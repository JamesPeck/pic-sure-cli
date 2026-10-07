package ops_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

// rotateComposeConfig is a cut-down `compose config --no-interpolate`.
// httpd's $$ is an escape, not a reference, and flyway-init is a one-shot
// that isn't running.
const rotateComposeConfig = `name: demo
services:
  httpd:
    healthcheck:
      test: ["CMD-SHELL", "echo $$DB_PICSURE_PASSWORD"]
  picsure-db:
    environment:
      MYSQL_ROOT_PASSWORD: ${DB_ROOT_PASSWORD}
      DB_PICSURE_PASSWORD: ${DB_PICSURE_PASSWORD}
      DB_AUTH_PASSWORD: ${DB_AUTH_PASSWORD}
  pic-sure-operations-service:
    environment:
      SPRING_DATASOURCE_PASSWORD: ${DB_PICSURE_PASSWORD:-}
      OTHER: $DB_PICSURE_PASSWORD_OLD
  psama:
    environment:
      DATASOURCE_PASSWORD: ${DB_AUTH_PASSWORD}
      APPLICATION_CLIENT_SECRET: ${AUTH0_CLIENT_SECRET}
  gateway:
    environment:
      TOKEN_INTROSPECTION_TOKEN: ${PICSURE_INTROSPECTION_TOKEN}
      LOGGING_API_KEY: ${LOGGING_API_KEY}
  dictionary-db:
    environment:
      POSTGRES_PASSWORD: ${DB_DICTIONARY_PASSWORD}
  dictionary-api:
    environment:
      POSTGRES_PASSWORD: ${DB_DICTIONARY_PASSWORD}
  flyway-init:
    environment:
      DB_ROOT_PASSWORD: ${DB_ROOT_PASSWORD}
`

var (
	rotateMySQL    = fakerunner.Glob("docker exec -i -e MYSQL_PWD id-picsure-db mysql *")
	rotatePsql     = fakerunner.Glob("docker exec -i * id-dictionary-db psql *")
	rotateUp       = fakerunner.Glob("docker compose * up -d --wait *")
	rotateRestart  = fakerunner.Glob("docker compose * restart *")
	alterRE        = regexp.MustCompile(`ALTER USER '([^']*)'@'([^']*)' IDENTIFIED BY '([^']*)'`)
	rotateRunning  = []string{"httpd", "picsure-db", "pic-sure-operations-service", "psama", "gateway", "dictionary-db", "dictionary-api", "hpds"}
	rotateTokenSet = regexp.MustCompile(`SET token = '([^']*)'`)
)

type rotateFixture struct {
	f   *fakerunner.Runner
	d   *ops.Deps
	st  *stack.Stack
	cfg *stack.Config
	sec *stack.Secrets

	mu sync.Mutex
	// alters are the MySQL ALTER USERs run, as user@host=password.
	alters []string
	// alterExit fails every ALTER USER with this exit code.
	alterExit int
	// token is auth.application's PICSURE token.
	token string
	// composeConfig is what `compose config` prints.
	composeConfig string
	// savedAtChange is secrets.yaml as it was when the database changed.
	savedAtChange []byte
}

func newRotateFixture(t *testing.T) *rotateFixture {
	t.Helper()
	st, err := stack.Create(filepath.Join(t.TempDir(), "demo"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sec, err := st.EnsureSecrets(rand.Reader, stack.EnsureOptions{OpenAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	sec.IntrospectionToken = "old-token"
	if err := st.SaveSecrets(sec); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveState(&stack.State{PendingRestarts: []string{"httpd"}}); err != nil {
		t.Fatal(err)
	}
	cfg := stack.DefaultConfig()
	cfg.Name = "demo"
	x := &rotateFixture{f: fakerunner.New(t), st: st, cfg: &cfg, sec: sec, token: "old-token", composeConfig: rotateComposeConfig}
	var ps strings.Builder
	for _, s := range rotateRunning {
		ps.WriteString(psLine(s, "running", "healthy"))
	}
	x.f.On(fakerunner.Glob("docker compose * ps --all --format json picsure-db")).Stdout(psLine("picsure-db", "running", "healthy"))
	x.f.On(fakerunner.Glob("docker compose * ps --all --format json dictionary-db")).Stdout(psLine("dictionary-db", "running", "healthy"))
	x.f.On(fakerunner.Glob("docker compose * ps --all --format json hpds")).Stdout(psLine("hpds", "running", "healthy"))
	x.f.On(fakerunner.Glob("docker compose * ps --all --format json")).Stdout(ps.String())
	x.f.On(fakerunner.Glob("docker compose * config --no-interpolate")).Do(func(context.Context, fakerunner.Call) (docker.Result, error) {
		return docker.Result{Stdout: []byte(x.composeConfig)}, nil
	})
	x.f.On(rotateMySQL).Do(x.mysql)
	x.f.On(rotateUp)
	x.f.On(rotateRestart)
	x.d = &ops.Deps{
		Runner:  x.f,
		Docker:  docker.NewEngine(x.f),
		Compose: &docker.Compose{Runner: x.f, Files: []string{"/stack/.pic-sure/render/compose.yaml"}, ProjectDir: "/stack"},
		Clock:   ops.FixedClock(t0),
		Rand:    rand.Reader,
		Sink:    &events.Recorder{},
	}
	return x
}

func (x *rotateFixture) mysql(_ context.Context, c fakerunner.Call) (docker.Result, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	in := string(c.Stdin)
	switch {
	case strings.Contains(in, "SELECT host FROM mysql.user"):
		return docker.Result{Stdout: []byte("%\nlocalhost\n")}, nil
	case strings.Contains(in, "FROM auth.application"):
		return docker.Result{Stdout: []byte(x.token + "\n")}, nil
	case strings.Contains(in, "UPDATE auth.application"):
		x.noteChange()
		x.token = rotateTokenSet.FindStringSubmatch(in)[1]
		return docker.Result{}, nil
	case strings.Contains(in, "ALTER USER"):
		if x.alterExit != 0 {
			return docker.Result{Stderr: []byte("ERROR 1396 (HY000): Operation ALTER USER failed\n"), ExitCode: x.alterExit}, nil
		}
		x.noteChange()
		for _, m := range alterRE.FindAllStringSubmatch(in, -1) {
			x.alters = append(x.alters, m[1]+"@"+m[2]+"="+m[3])
		}
		return docker.Result{}, nil
	}
	return docker.Result{ExitCode: 1, Stderr: []byte("unexpected SQL")}, nil
}

// noteChange records secrets.yaml the first time the database changes.
func (x *rotateFixture) noteChange() {
	if x.savedAtChange == nil {
		x.savedAtChange, _ = x.st.ReadFile(stack.SecretsFile)
	}
}

func (x *rotateFixture) rotate(opts ops.RotateOptions) (*ops.RotateReport, error) {
	return ops.RotateSecret(context.Background(), x.d, x.st, x.cfg, x.sec, opts)
}

func (x *rotateFixture) saved(t *testing.T) *stack.Secrets {
	t.Helper()
	s, err := x.st.LoadSecrets()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (x *rotateFixture) upServices() [][]string {
	var out [][]string
	for _, c := range x.f.CallsMatching(rotateUp) {
		i := slices.Index(c.Argv, "--wait-timeout")
		out = append(out, c.Argv[i+2:])
	}
	return out
}

func TestRotateDBPicsureChangesTheDatabaseFirst(t *testing.T) {
	x := newRotateFixture(t)
	old := x.sec.DBPicsurePassword
	before, _ := x.st.ReadFile(stack.SecretsFile)

	report, err := x.rotate(ops.RotateOptions{Name: ops.SecretDBPicsure})
	if err != nil {
		t.Fatal(err)
	}
	saved := x.saved(t).DBPicsurePassword
	if saved == old || len(saved) != 24 {
		t.Fatalf("secrets.yaml's password wasn't replaced with a new one")
	}
	if want := []string{"picsure@%=" + string(saved)}; !slices.Equal(x.alters, want) {
		t.Errorf("ALTER USERs = %d, want picsure@%% to the saved password", len(x.alters))
	}
	if !bytes.Equal(x.savedAtChange, before) {
		t.Error("secrets.yaml changed before the database did")
	}
	if x.sec.DBPicsurePassword != saved {
		t.Error("the caller's secrets weren't updated, so compose would get the old value")
	}
	// httpd's $$ is an escape and flyway-init isn't running; ${..._OLD} is
	// another variable.
	if got, want := x.upServices(), [][]string{{"pic-sure-operations-service", "picsure-db"}}; !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("compose up for %v, want %v", got, want)
	}
	if !slices.Equal(report.Restarted, []string{"pic-sure-operations-service", "picsure-db"}) {
		t.Errorf("Restarted = %v", report.Restarted)
	}
	x.f.AssertOrder(rotateMySQL, rotateUp)
	x.f.AssertNotCalled(rotateRestart)
	for _, c := range x.f.Calls() {
		if strings.Contains(strings.Join(c.Argv, " "), string(saved)) {
			t.Errorf("the password reached argv: %s", c)
		}
	}
}

func TestRotateFailedDBChangeLeavesSecretsUnchanged(t *testing.T) {
	x := newRotateFixture(t)
	x.alterExit = 1
	before, _ := x.st.ReadFile(stack.SecretsFile)

	_, err := x.rotate(ops.RotateOptions{Name: ops.SecretDBAuth})
	if err == nil || !strings.Contains(err.Error(), "secrets.yaml is unchanged") {
		t.Fatalf("err = %v", err)
	}
	if after, _ := x.st.ReadFile(stack.SecretsFile); !bytes.Equal(after, before) {
		t.Error("secrets.yaml changed")
	}
	x.f.AssertNotCalled(rotateUp)
}

func TestRotateUndoesTheDBChangeWhenSavingFails(t *testing.T) {
	x := newRotateFixture(t)
	old := x.sec.DBAirflowPassword
	x.breakSecretsFile(t)

	_, err := x.rotate(ops.RotateOptions{Name: ops.SecretDBAirflow})
	if err == nil || !strings.Contains(err.Error(), "the database change was undone") {
		t.Fatalf("err = %v", err)
	}
	if len(x.alters) != 2 || x.alters[1] != "airflow@%="+string(old) {
		t.Errorf("the second ALTER USER should restore the old password; got %d", len(x.alters))
	}
	if x.sec.DBAirflowPassword != old {
		t.Error("the caller's secrets changed although secrets.yaml didn't")
	}
	x.f.AssertNotCalled(rotateUp)
}

// breakSecretsFile puts a directory where secrets.yaml goes, so the atomic
// rename that saves it fails.
func (x *rotateFixture) breakSecretsFile(t *testing.T) {
	t.Helper()
	path := x.st.Path(stack.SecretsFile)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(path, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestRotateUndoesRootAndTokenChangesWhenSavingFails(t *testing.T) {
	t.Run("db-root", func(t *testing.T) {
		x := newRotateFixture(t)
		old := string(x.sec.DBRootPassword)
		x.breakSecretsFile(t)
		if _, err := x.rotate(ops.RotateOptions{Name: ops.SecretDBRoot}); err == nil {
			t.Fatal("want an error")
		}
		if len(x.alters) != 4 || x.alters[2] != "root@%="+old || x.alters[3] != "root@localhost="+old {
			t.Errorf("want both root accounts set back to the old password, got %d alters", len(x.alters))
		}
		if x.sec.DBRootPassword != stack.Secret(old) {
			t.Error("the caller's secrets changed")
		}
	})
	t.Run("introspection-token", func(t *testing.T) {
		x := newRotateFixture(t)
		x.breakSecretsFile(t)
		if _, err := x.rotate(ops.RotateOptions{Name: ops.SecretIntrospectionToken}); err == nil {
			t.Fatal("want an error")
		}
		if x.token != "old-token" {
			t.Error("auth.application should hold the old token again")
		}
	})
}

func TestRotateSignalDuringTheDBChangeStillSaves(t *testing.T) {
	x := newRotateFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	x.f = fakerunner.New(t)
	x.f.On(fakerunner.Glob("docker compose * ps --all --format json picsure-db")).Stdout(psLine("picsure-db", "running", "healthy"))
	x.f.On(rotateMySQL).Do(func(c context.Context, call fakerunner.Call) (docker.Result, error) {
		res, err := x.mysql(c, call)
		cancel() // the signal arrives while the server applies the change
		return res, err
	})
	x.d.Runner, x.d.Docker, x.d.Compose = x.f, docker.NewEngine(x.f), &docker.Compose{Runner: x.f, Files: []string{"/s/c.yaml"}, ProjectDir: "/s"}

	_, err := ops.RotateSecret(ctx, x.d, x.st, x.cfg, x.sec, ops.RotateOptions{Name: ops.SecretDBPicsure})
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "the new secret is saved; run `pic-sure up`") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "re-run") {
		t.Errorf("a re-run would rotate again: %v", err)
	}
	if got := x.saved(t).DBPicsurePassword; len(x.alters) != 1 || x.alters[0] != "picsure@%="+string(got) {
		t.Error("secrets.yaml should hold the password the database now has")
	}
}

func TestRotateDBRootChangesEveryRootAccount(t *testing.T) {
	x := newRotateFixture(t)
	if _, err := x.rotate(ops.RotateOptions{Name: ops.SecretDBRoot}); err != nil {
		t.Fatal(err)
	}
	pw := string(x.saved(t).DBRootPassword)
	if want := []string{"root@%=" + pw, "root@localhost=" + pw}; !slices.Equal(x.alters, want) {
		t.Errorf("alters = %d, want both root accounts", len(x.alters))
	}
	if got := x.upServices(); len(got) != 1 || !slices.Equal(got[0], []string{"picsure-db"}) {
		t.Errorf("compose up for %v, want picsure-db only", got)
	}
}

func TestRotateDatabaseSecretNeedsTheDatabaseRunning(t *testing.T) {
	x := newRotateFixture(t)
	x.f = fakerunner.New(t)
	x.f.On(fakerunner.Glob("docker compose * ps --all --format json picsure-db")).Stdout(psLine("picsure-db", "exited", ""))
	x.d.Runner, x.d.Docker, x.d.Compose = x.f, docker.NewEngine(x.f), &docker.Compose{Runner: x.f, Files: []string{"/s/c.yaml"}, ProjectDir: "/s"}
	before, _ := x.st.ReadFile(stack.SecretsFile)

	_, err := x.rotate(ops.RotateOptions{Name: ops.SecretDBPicsure})
	if exitcode.FromError(err) != exitcode.CodePrecondition {
		t.Fatalf("err = %v, want exit 3", err)
	}
	if after, _ := x.st.ReadFile(stack.SecretsFile); !bytes.Equal(after, before) {
		t.Error("secrets.yaml changed")
	}
}

func TestRotateRemoteDBRootRecordsTheDBAsPassword(t *testing.T) {
	x := newRotateFixture(t)
	x.cfg.DB.Mode = stack.DBRemote
	x.cfg.DB.Remote.Host, x.cfg.DB.Remote.Port, x.cfg.DB.Remote.RootUser = "db.example.org", 3306, "admin"
	remote := fakerunner.Glob("docker run * mysql:8.0 mysql *")
	x.f.On(remote)
	newPW := stack.Secret("synthetic-new-remote-root-password")
	// A remote database's stack has no picsure-db.
	x.composeConfig = strings.Replace(rotateComposeConfig, "picsure-db:", "unused:", 1)

	if _, err := x.rotate(ops.RotateOptions{Name: ops.SecretDBRoot}); exitcode.FromError(err) != exitcode.CodeUsage {
		t.Fatalf("without a value: err = %v, want exit 2", err)
	}
	if _, err := x.rotate(ops.RotateOptions{Name: ops.SecretDBRoot, Value: newPW}); err != nil {
		t.Fatal(err)
	}
	if x.saved(t).DBRemoteRootPassword != newPW {
		t.Error("the new remote root password wasn't saved")
	}
	if len(x.alters) != 0 {
		t.Error("the DBA's root account was altered")
	}
	calls := x.f.CallsMatching(remote)
	if len(calls) != 1 || !strings.Contains(string(calls[0].Stdin), "SELECT 1") {
		t.Errorf("want one login check with the new password, got %d calls", len(calls))
	}
	// Only flyway-init uses it, and it isn't running.
	x.f.AssertNotCalled(rotateUp)
}

func TestRotateDictionaryDB(t *testing.T) {
	x := newRotateFixture(t)
	old := x.sec.DictionaryDBPassword
	var stmt string
	x.f.On(rotatePsql).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		stmt = string(c.Stdin)
		return docker.Result{}, nil
	})
	if _, err := x.rotate(ops.RotateOptions{Name: ops.SecretDictionaryDB}); err != nil {
		t.Fatal(err)
	}
	pw := x.saved(t).DictionaryDBPassword
	if pw == old || !strings.Contains(stmt, `ALTER ROLE "picsure" WITH PASSWORD '`+string(pw)+`'`) {
		t.Errorf("psql got %q", stmt)
	}
	if got := x.upServices(); len(got) != 1 || !slices.Equal(got[0], []string{"dictionary-api", "dictionary-db"}) {
		t.Errorf("compose up for %v", got)
	}
}

func TestRotateIntrospectionToken(t *testing.T) {
	x := newRotateFixture(t)
	report, err := x.rotate(ops.RotateOptions{Name: ops.SecretIntrospectionToken})
	if err != nil {
		t.Fatal(err)
	}
	saved := x.saved(t)
	if saved.IntrospectionToken == "old-token" || string(saved.IntrospectionToken) != x.token {
		t.Error("auth.application and secrets.yaml should both hold a new token")
	}
	if report.TokenExpiry == nil || !saved.IntrospectionTokenExpiry.Equal(*report.TokenExpiry) {
		t.Error("the report should carry the saved expiry")
	}
	if !strings.Contains(string(x.savedAtChange), "old-token") {
		t.Error("secrets.yaml changed before the database did")
	}
	// The gateway reads the token from its env; psama from the database.
	if got := x.upServices(); len(got) != 1 || !slices.Equal(got[0], []string{"gateway"}) {
		t.Errorf("compose up for %v", got)
	}
	restarts := x.f.CallsMatching(rotateRestart)
	if len(restarts) != 1 || restarts[0].Argv[len(restarts[0].Argv)-1] != "psama" {
		t.Errorf("want psama restarted once, got %v", restarts)
	}
	x.f.AssertOrder(rotateMySQL, rotateUp, rotateRestart)
	state, err := x.st.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(state.PendingRestarts, []string{"httpd"}) {
		t.Errorf("pending restarts = %v, want only the earlier httpd", state.PendingRestarts)
	}
}

func TestRotateAuth0ClientSecretLeavesOpenMode(t *testing.T) {
	x := newRotateFixture(t)
	if !x.sec.Auth0ClientSecretGenerated {
		t.Fatal("fixture: want a generated open-mode secret")
	}
	if _, err := x.rotate(ops.RotateOptions{Name: ops.SecretAuth0ClientSecret, Value: "too-short"}); exitcode.FromError(err) != exitcode.CodeUsage {
		t.Fatalf("short secret: err = %v, want exit 2", err)
	}
	if _, err := x.rotate(ops.RotateOptions{Name: ops.SecretAuth0ClientSecret}); exitcode.FromError(err) != exitcode.CodeUsage {
		t.Fatalf("no value: err = %v, want exit 2", err)
	}
	secret := stack.Secret("synthetic-auth0-client-secret-0123456789abcdef")
	if _, err := x.rotate(ops.RotateOptions{Name: ops.SecretAuth0ClientSecret, Value: secret}); err != nil {
		t.Fatal(err)
	}
	saved := x.saved(t)
	if saved.Auth0ClientSecret != secret || saved.Auth0ClientSecretGenerated {
		t.Error("the supplied secret should replace the generated one and clear the marker")
	}
	if saved.IntrospectionToken == "old-token" || string(saved.IntrospectionToken) != x.token {
		t.Error("the token should be re-issued, in the database and secrets.yaml")
	}
	// psama uses the secret, so it is recreated rather than restarted.
	if got := x.upServices(); len(got) != 1 || !slices.Equal(got[0], []string{"gateway", "psama"}) {
		t.Errorf("compose up for %v", got)
	}
	x.f.AssertNotCalled(rotateRestart)
}

func TestRotateConfigOnlySecret(t *testing.T) {
	x := newRotateFixture(t)
	old := x.sec.LoggingAPIKey
	if _, err := x.rotate(ops.RotateOptions{Name: ops.SecretLoggingKey, Value: "x"}); exitcode.FromError(err) != exitcode.CodeUsage {
		t.Fatalf("a value for a generated secret: err = %v, want exit 2", err)
	}
	if _, err := x.rotate(ops.RotateOptions{Name: ops.SecretLoggingKey}); err != nil {
		t.Fatal(err)
	}
	if k := x.saved(t).LoggingAPIKey; k == old || len(k) != 64 {
		t.Error("the logging key wasn't replaced with a new 32-byte hex one")
	}
	x.f.AssertNotCalled(rotateMySQL)
	if got := x.upServices(); len(got) != 1 || !slices.Equal(got[0], []string{"gateway"}) {
		t.Errorf("compose up for %v", got)
	}
}

func TestRotateHPDSKeyRefusesLoadedDataWithoutDiscard(t *testing.T) {
	x := newRotateFixture(t)
	h := x.hpdsFakes("columnMeta.javabin\n")
	oldKey, _ := x.st.LoadHPDSKey()

	_, err := x.rotate(ops.RotateOptions{Name: ops.SecretHPDSKey})
	if exitcode.FromError(err) != exitcode.CodeConfirmRequired {
		t.Fatalf("err = %v, want exit 4", err)
	}
	if k, _ := x.st.LoadHPDSKey(); k != oldKey {
		t.Error("the key file changed")
	}
	x.f.AssertNotCalled(fakerunner.Glob("docker compose * stop hpds"))
	if h.wiped {
		t.Error("the data was deleted")
	}
}

func TestRotateHPDSKeyDiscardsDataAndRekeys(t *testing.T) {
	x := newRotateFixture(t)
	h := x.hpdsFakes("columnMeta.javabin\nall\n")
	oldKey, _ := x.st.LoadHPDSKey()

	report, err := x.rotate(ops.RotateOptions{Name: ops.SecretHPDSKey, DiscardData: true})
	if err != nil {
		t.Fatal(err)
	}
	key, err := x.st.LoadHPDSKey()
	if err != nil || key == oldKey {
		t.Fatalf("the key file wasn't replaced: %v", err)
	}
	if string(h.key) != string(key)+"\n" {
		t.Error("the volume didn't get the new key")
	}
	if !h.wiped || !report.DiscardedData {
		t.Error("the data wasn't deleted")
	}
	stop := fakerunner.Glob("docker compose * stop hpds")
	wipe := fakerunner.Glob("docker run * demo-hpds-wipe-* * -exec rm -rf {} +")
	keyCopy := fakerunner.Glob("docker run * demo-hpds-key-* *")
	x.f.AssertOrder(stop, wipe, keyCopy, rotateUp)
	if got := x.upServices(); len(got) != 1 || !slices.Equal(got[0], []string{"hpds"}) {
		t.Errorf("compose up for %v", got)
	}
	state, _ := x.st.LoadState()
	if state.HPDSKey == nil {
		t.Error("state.json doesn't record the new key's copy")
	}
}

func TestRotateHPDSKeyWithoutDataNeedsNoDiscard(t *testing.T) {
	x := newRotateFixture(t)
	h := x.hpdsFakes("")
	report, err := x.rotate(ops.RotateOptions{Name: ops.SecretHPDSKey})
	if err != nil {
		t.Fatal(err)
	}
	if h.wiped || report.DiscardedData {
		t.Error("nothing was loaded, so nothing should be deleted")
	}
	if h.key == nil {
		t.Error("the new key wasn't copied")
	}
}

func TestRotateHPDSKeyRefusesAnotherStacksVolume(t *testing.T) {
	x := newRotateFixture(t)
	x.f = fakerunner.New(t)
	x.f.On(fakerunner.Exact("docker", "volume", "inspect", "demo_hpds-data")).
		Stdout(`[{"Name":"demo_hpds-data","CreatedAt":"2026-10-07T12:00:00Z","Labels":{"` + stack.LabelStack + `":"other"}}]`)
	x.d.Runner, x.d.Docker = x.f, docker.NewEngine(x.f)
	oldKey, _ := x.st.LoadHPDSKey()

	_, err := x.rotate(ops.RotateOptions{Name: ops.SecretHPDSKey, DiscardData: true})
	if exitcode.FromError(err) != exitcode.CodePrecondition {
		t.Fatalf("err = %v, want exit 3", err)
	}
	if k, _ := x.st.LoadHPDSKey(); k != oldKey {
		t.Error("the key file changed")
	}
}

type hpdsRotateFakes struct {
	wiped bool
	key   []byte
}

// hpdsFakes fakes the hpds-data volume, holding files besides the key.
func (x *rotateFixture) hpdsFakes(files string) *hpdsRotateFakes {
	h := &hpdsRotateFakes{}
	x.f.On(fakerunner.Exact("docker", "volume", "inspect", "demo_hpds-data")).
		Stdout(`[{"Name":"demo_hpds-data","CreatedAt":"2026-10-07T12:00:00Z","Labels":{"` + stack.LabelStack + `":"demo"}}]`)
	x.f.On(fakerunner.Glob("docker compose * stop hpds"))
	x.f.On(fakerunner.Glob("docker run * demo-hpds-data-* *")).Do(func(context.Context, fakerunner.Call) (docker.Result, error) {
		if h.wiped {
			return docker.Result{}, nil
		}
		return docker.Result{Stdout: []byte(files)}, nil
	})
	x.f.On(fakerunner.Glob("docker run * demo-hpds-wipe-* *")).Do(func(context.Context, fakerunner.Call) (docker.Result, error) {
		h.wiped = true
		return docker.Result{}, nil
	})
	x.f.On(fakerunner.Glob("docker run * demo-hpds-key-* *")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		h.key = c.Stdin
		return docker.Result{}, nil
	})
	return h
}

func TestRotateHPDSKeyRefusesSharedData(t *testing.T) {
	x := newRotateFixture(t)
	x.cfg.HPDS.Data = stack.HPDSShared
	_, err := x.rotate(ops.RotateOptions{Name: ops.SecretHPDSKey, DiscardData: true})
	if exitcode.FromError(err) != exitcode.CodePrecondition {
		t.Fatalf("err = %v, want exit 3", err)
	}
}

func TestRotateRestartFailureSaysHowToFinish(t *testing.T) {
	x := newRotateFixture(t)
	x.f = fakerunner.New(t)
	x.f.On(fakerunner.Glob("docker compose * config --no-interpolate")).Err(errors.New("boom"))
	x.d.Runner, x.d.Docker, x.d.Compose = x.f, docker.NewEngine(x.f), &docker.Compose{Runner: x.f, Files: []string{"/s/c.yaml"}, ProjectDir: "/s"}
	old := x.sec.QueryServiceInternalToken
	_, err := x.rotate(ops.RotateOptions{Name: ops.SecretQueryServiceToken})
	if err == nil || !strings.Contains(err.Error(), "run `pic-sure up` to finish") || strings.Contains(err.Error(), "re-run") {
		t.Fatalf("err = %v", err)
	}
	if x.saved(t).QueryServiceInternalToken == old {
		t.Error("the new token should be saved before the restart")
	}
}
