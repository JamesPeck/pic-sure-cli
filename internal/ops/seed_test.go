package ops_test

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/jwt"
	"github.com/JamesPeck/pic-sure-cli/internal/log"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

const (
	seedAdminEmail   = "seed.admin@example.com"
	seedClientSecret = "synthetic-client-secret-0123456789abcdef"
)

// fakeDB answers the seed's SQL the way picsure-db would.
type fakeDB struct {
	t          *testing.T
	historyTbl int // custom history tables present
	applied    int // AppliedMigrationsQuery's result
	users      map[string]bool
	token      string // auth.application's PICSURE token
	insertErr  string // stderr for a failing admin insert
	// onTokenWrite runs when the token is updated, before the call returns.
	onTokenWrite func(token string)
}

var (
	emailRE = regexp.MustCompile(`email = '([^']*)'`)
	tokenRE = regexp.MustCompile(`SET token = '([^']*)'`)
)

func newFakeDB(t *testing.T) *fakeDB {
	return &fakeDB{t: t, historyTbl: 2, applied: 7, users: map[string]bool{}}
}

func (db *fakeDB) do(_ context.Context, c fakerunner.Call) (docker.Result, error) {
	in := string(c.Stdin)
	switch {
	case strings.Contains(in, "information_schema.tables"):
		return docker.Result{Stdout: []byte(strconv.Itoa(db.historyTbl) + "\n")}, nil
	case strings.Contains(in, "SELECT LEAST("):
		return docker.Result{Stdout: []byte(strconv.Itoa(db.applied) + "\n")}, nil
	case strings.Contains(in, "SELECT COUNT(*) FROM auth.user"):
		n := 0
		if db.users[emailRE.FindStringSubmatch(in)[1]] {
			n = 1
		}
		return docker.Result{Stdout: []byte(strconv.Itoa(n) + "\n")}, nil
	case strings.Contains(in, "INSERT INTO auth.user "):
		email := emailRE.FindStringSubmatch(in)[1]
		if db.insertErr != "" {
			return docker.Result{Stderr: []byte(strings.ReplaceAll(db.insertErr, "EMAIL", email)), ExitCode: 1}, nil
		}
		db.users[email] = true
		return docker.Result{}, nil
	case strings.Contains(in, "FROM auth.application"):
		return docker.Result{Stdout: []byte(db.token + "\n")}, nil
	case strings.Contains(in, "UPDATE auth.application"):
		db.token = tokenRE.FindStringSubmatch(in)[1]
		if db.onTokenWrite != nil {
			db.onTokenWrite(db.token)
		}
		return docker.Result{}, nil
	}
	db.t.Errorf("unexpected SQL %q", in)
	return docker.Result{ExitCode: 1}, nil
}

type seedFixture struct {
	f   *fakerunner.Runner
	db  *fakeDB
	d   *ops.Deps
	rec *events.Recorder
	st  *stack.Stack
	cfg *stack.Config
	sec *stack.Secrets

	psamaRunning bool
}

var mysqlExec = fakerunner.Glob("docker exec -i -e MYSQL_PWD id-picsure-db mysql *")

func newSeedFixture(t *testing.T) *seedFixture {
	t.Helper()
	f := fakerunner.New(t)
	db := newFakeDB(t)
	f.On(fakerunner.Glob("docker compose * ps --all --format json picsure-db")).Stdout(psLine("picsure-db", "running", "healthy"))
	x := &seedFixture{f: f, db: db}
	f.On(fakerunner.Glob("docker compose * ps --all --format json psama")).Do(func(context.Context, fakerunner.Call) (docker.Result, error) {
		if !x.psamaRunning {
			return docker.Result{}, nil
		}
		return docker.Result{Stdout: []byte(psLine("psama", "running", "healthy"))}, nil
	})
	f.On(mysqlExec).Do(db.do)
	d, rec := migrateDeps(f)
	d.Rand = bytes.NewReader(bytes.Repeat([]byte{7}, 64))
	cfg, sec := migrateConfig()
	cfg.Auth.AdminEmail = seedAdminEmail
	sec.Auth0ClientSecret = seedClientSecret
	st, err := stack.Create(filepath.Join(t.TempDir(), "demo"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	x.d, x.rec, x.st, x.cfg, x.sec = d, rec, st, cfg, sec
	return x
}

func (x *seedFixture) run() error {
	*x.rec = events.Recorder{}
	return steps.Run(context.Background(), x.d.Sink, []steps.Step{ops.SeedStep(x.d, x.st, x.cfg, x.sec)}, steps.Options{})
}

func (x *seedFixture) savedToken(t *testing.T) string {
	t.Helper()
	saved, err := x.st.LoadSecrets()
	if err != nil {
		return ""
	}
	return string(saved.IntrospectionToken)
}

// noSeedSecretInArgv checks that no call's argv holds the email, the
// token or a password.
func (x *seedFixture) noSeedSecretInArgv(t *testing.T) {
	t.Helper()
	for _, c := range x.f.Calls() {
		argv := strings.Join(c.Argv, " ")
		for _, s := range []string{seedAdminEmail, string(x.sec.IntrospectionToken), migrateRootPassword, seedClientSecret} {
			if s != "" && strings.Contains(argv, s) {
				t.Errorf("a secret reached argv: %q", c.Argv)
			}
		}
	}
}

func TestSeedCreatesTheAdminAndIssuesTheTokenThenSkips(t *testing.T) {
	x := newSeedFixture(t)
	x.db.onTokenWrite = func(token string) {
		// The database is written before secrets.yaml.
		if got := x.savedToken(t); got == token {
			t.Error("secrets.yaml had the token before the database")
		}
	}

	if err := x.run(); err != nil {
		t.Fatal(err)
	}
	assertStepDone(t, x.rec, ops.StepSeed, events.StepOK)
	if !x.db.users[seedAdminEmail] {
		t.Error("the admin user wasn't created")
	}
	want, wantExp, err := jwt.Introspection(seedClientSecret, x.sec.ApplicationUUID, t0, jwt.DefaultTTL)
	if err != nil {
		t.Fatal(err)
	}
	if x.db.token != want || string(x.sec.IntrospectionToken) != want || !x.sec.IntrospectionTokenExpiry.Equal(wantExp) {
		t.Errorf("db token %q, secrets token %q expiry %s; want %q until %s", x.db.token, x.sec.IntrospectionToken, x.sec.IntrospectionTokenExpiry, want, wantExp)
	}
	saved, err := x.st.LoadSecrets()
	if err != nil || string(saved.IntrospectionToken) != want || !saved.IntrospectionTokenExpiry.Equal(wantExp) {
		t.Errorf("secrets.yaml: %v, err %v", saved, err)
	}
	x.noSeedSecretInArgv(t)

	writes := len(x.f.Calls())
	if err := x.run(); err != nil {
		t.Fatal(err)
	}
	assertStepDone(t, x.rec, ops.StepSeed, events.StepSkipped)
	for _, c := range x.f.Calls()[writes:] {
		if in := string(c.Stdin); strings.Contains(in, "INSERT") || strings.Contains(in, "UPDATE") {
			t.Errorf("the second run wrote %q", in)
		}
	}
}

func TestSeedRestoresAValidTokenTheDatabaseLost(t *testing.T) {
	x := newSeedFixture(t)
	x.db.users[seedAdminEmail] = true
	token, exp, err := jwt.Introspection(seedClientSecret, x.sec.ApplicationUUID, t0.Add(-100*24*time.Hour), jwt.DefaultTTL)
	if err != nil {
		t.Fatal(err)
	}
	x.sec.IntrospectionToken, x.sec.IntrospectionTokenExpiry = stack.Secret(token), exp
	x.db.token = "" // after reset: a fresh database

	if err := x.run(); err != nil {
		t.Fatal(err)
	}
	assertStepDone(t, x.rec, ops.StepSeed, events.StepOK)
	if x.db.token != token || string(x.sec.IntrospectionToken) != token {
		t.Errorf("db token %q, secrets %q; want the stored token kept", x.db.token, x.sec.IntrospectionToken)
	}
	if _, err := os.Stat(x.st.Path(stack.SecretsFile)); err == nil {
		t.Error("secrets.yaml was written though its token didn't change")
	}
}

func TestSeedRenewsATokenWithin30DaysOfExpiry(t *testing.T) {
	x := newSeedFixture(t)
	x.db.users[seedAdminEmail] = true
	x.sec.IntrospectionToken, x.sec.IntrospectionTokenExpiry = "old-synthetic-token", t0.Add(29*24*time.Hour)
	x.db.token = "old-synthetic-token"

	if err := x.run(); err != nil {
		t.Fatal(err)
	}
	if x.db.token == "old-synthetic-token" || x.db.token != string(x.sec.IntrospectionToken) || x.savedToken(t) != x.db.token {
		t.Errorf("db %q, secrets %q, saved %q; want one renewed token", x.db.token, x.sec.IntrospectionToken, x.savedToken(t))
	}
	if !x.sec.IntrospectionTokenExpiry.Equal(t0.Add(jwt.DefaultTTL)) {
		t.Errorf("expiry %s", x.sec.IntrospectionTokenExpiry)
	}
}

func TestSeedConvergesAfterSecretsYAMLFailedToSave(t *testing.T) {
	x := newSeedFixture(t)
	dir := filepath.Dir(x.st.Path(stack.SecretsFile))
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err := x.run()
	if err == nil || !strings.Contains(err.Error(), "secrets.yaml") {
		t.Fatalf("err %v, want the save failure", err)
	}
	if x.db.token == "" || x.sec.IntrospectionToken != "" {
		t.Fatalf("db token %q, in-memory token %q; want the db written and sec left as on disk", x.db.token, x.sec.IntrospectionToken)
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	x.d.Clock = ops.FixedClock(t0.Add(time.Hour))
	if err := x.run(); err != nil {
		t.Fatal(err)
	}
	if x.db.token != string(x.sec.IntrospectionToken) || x.savedToken(t) != x.db.token {
		t.Errorf("db %q, secrets %q, saved %q; want them equal", x.db.token, x.sec.IntrospectionToken, x.savedToken(t))
	}
	if err := x.run(); err != nil {
		t.Fatal(err)
	}
	assertStepDone(t, x.rec, ops.StepSeed, events.StepSkipped)
}

func TestSeedNeedsTheMigrations(t *testing.T) {
	for name, set := range map[string]func(*fakeDB){
		"no history tables": func(db *fakeDB) { db.historyTbl = 0 },
		"one history table": func(db *fakeDB) { db.historyTbl = 1 },
		"only baselines":    func(db *fakeDB) { db.applied = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			x := newSeedFixture(t)
			set(x.db)
			err := x.run()
			if exitcode.FromError(err) != exitcode.CodePrecondition || !strings.Contains(err.Error(), "pic-sure migrate --repair") {
				t.Fatalf("err %v, want exit 3 with the repair hint", err)
			}
			for _, c := range x.f.Calls() {
				if strings.Contains(string(c.Stdin), "SELECT LEAST(") && x.db.historyTbl < 2 {
					t.Error("AppliedMigrationsQuery ran without its tables")
				}
			}
		})
	}
}

func TestSeedNeedsARunningDatabase(t *testing.T) {
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker compose * ps *"))
	d, _ := migrateDeps(f)
	cfg, sec := migrateConfig()
	st, err := stack.Create(filepath.Join(t.TempDir(), "demo"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	err = steps.Run(context.Background(), d.Sink, []steps.Step{ops.SeedStep(d, st, cfg, sec)}, steps.Options{})
	if exitcode.FromError(err) != exitcode.CodePrecondition || !strings.Contains(err.Error(), "picsure-db is not running") {
		t.Fatalf("err %v", err)
	}
}

func TestSeedRestartsARunningPsama(t *testing.T) {
	x := newSeedFixture(t)
	x.psamaRunning = true
	restart := fakerunner.Glob("docker compose * restart psama")
	x.f.On(restart)

	if err := x.run(); err != nil {
		t.Fatal(err)
	}
	// After every write.
	if calls := x.f.Calls(); !restart.Match(calls[len(calls)-1].Argv) {
		t.Errorf("last call %s, want the restart", calls[len(calls)-1])
	}
}

func TestSeedNeedsAClientSecretForTheToken(t *testing.T) {
	x := newSeedFixture(t)
	x.sec.Auth0ClientSecret = ""
	err := x.run()
	if exitcode.FromError(err) != exitcode.CodePrecondition || !strings.Contains(err.Error(), "open mode") {
		t.Fatalf("err %v, want exit 3 explaining open mode", err)
	}
}

func TestSeedKeepsTheEmailAndTokenOutOfLogs(t *testing.T) {
	x := newSeedFixture(t)
	x.db.insertErr = "ERROR 1062 (23000) at line 3: Duplicate entry 'EMAIL' for key 'user.email'\n"
	var stderr bytes.Buffer
	run := log.New(log.Options{Level: slog.LevelDebug, Stderr: &stderr, File: true})
	logFile, err := run.OpenFile(logStore{x.st}, t0)
	if err != nil {
		t.Fatal(err)
	}
	x.d.Log = run.Logger()

	err = x.run()
	if err == nil || !strings.Contains(err.Error(), "creating the admin user") {
		t.Fatalf("err %v", err)
	}
	x.d.Log.Error("seed failed", "err", err)

	x.db.insertErr = ""
	if err := x.run(); err != nil {
		t.Fatal(err)
	}
	token := string(x.sec.IntrospectionToken)
	x.d.Log.Info("seeded", "detail", "token "+token+" for "+seedAdminEmail)
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	for name, out := range map[string]string{"stderr": stderr.String(), "run log": string(file)} {
		if !strings.Contains(out, "[REDACTED]") {
			t.Errorf("%s has nothing redacted: %s", name, out)
		}
		for _, s := range []string{seedAdminEmail, token} {
			if strings.Contains(out, s) {
				t.Errorf("%s holds %q: %s", name, s, out)
			}
		}
	}
	x.noSeedSecretInArgv(t)
}

// logStore is the stack as a log.Store that owns every file.
type logStore struct{ *stack.Stack }

func (logStore) Owns(string) bool { return true }
