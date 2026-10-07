package ops_test

import (
	"context"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

// remoteServer fakes a remote MySQL reached through the mysql:8.0 client
// container: the root account answers the bootstrap state query and runs
// the bootstrap statements, and each application user logs in only while
// its password matches secrets.yaml.
type remoteServer struct {
	f         *fakerunner.Runner
	databases map[string]bool
	users     map[string]bool // exists as name@'%'
	matches   map[string]bool // its password is the one in secrets.yaml
	grants    map[string]bool // "user db"
	accounts  []string        // "user host" for hosts other than '%'

	scripts []string // what root ran, besides the state query
}

const remoteClient = "docker run -i --rm -e MYSQL_PWD mysql:8.0 mysql * --host=db.example.org --port=3307 "

func newRemoteServer(t *testing.T) *remoteServer {
	s := &remoteServer{f: fakerunner.New(t), databases: map[string]bool{}, users: map[string]bool{},
		matches: map[string]bool{}, grants: map[string]bool{}}
	s.f.On(fakerunner.Glob(remoteClient + "--user=admin")).Do(s.root)
	for _, u := range []string{"picsure", "auth", "airflow"} {
		s.f.On(fakerunner.Glob(remoteClient + "--user=" + u)).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
			if !s.users[u] || !s.matches[u] {
				return docker.Result{ExitCode: 1, Stderr: []byte("ERROR 1045 (28000): Access denied for user '" + u + "'@'172.17.0.3'\n")}, nil
			}
			for _, line := range strings.Split(string(c.Stdin), "\n") {
				if db, ok := strings.CutPrefix(line, "USE "); ok && !s.grants[u+" "+strings.Trim(db, "`")] {
					return docker.Result{ExitCode: 1, Stderr: []byte("ERROR 1044 (42000): Access denied for user '" + u + "'@'%' to database '" + strings.Trim(db, "`") + "'\n")}, nil
				}
			}
			return docker.Result{}, nil
		})
	}
	return s
}

func (s *remoteServer) full() {
	for _, db := range []string{"auth", "picsure"} {
		s.databases[db] = true
	}
	for _, g := range []string{"picsure picsure", "auth auth", "airflow auth", "airflow picsure"} {
		u, _, _ := strings.Cut(g, " ")
		s.users[u], s.matches[u], s.grants[g] = true, true, true
	}
}

func (s *remoteServer) root(_ context.Context, c fakerunner.Call) (docker.Result, error) {
	in := string(c.Stdin)
	if strings.Contains(in, "UNION ALL") {
		out := "version\t8.0.40\n"
		for db := range s.databases {
			out += "database\t" + db + "\n"
		}
		for u := range s.users {
			out += "user\t" + u + "\n"
		}
		for g := range s.grants {
			out += "grant\t" + g + "\n"
		}
		for _, a := range s.accounts {
			out += "account\t" + a + "\n"
		}
		return docker.Result{Stdout: []byte(out)}, nil
	}
	if strings.Contains(in, "\nSELECT 1\n") {
		return docker.Result{Stdout: []byte("1\n")}, nil
	}
	s.scripts = append(s.scripts, in)
	for _, line := range strings.Split(in, "\n") {
		f := strings.Fields(line)
		switch {
		case strings.HasPrefix(line, "CREATE DATABASE IF NOT EXISTS "):
			s.databases[strings.Trim(f[5], "`")] = true
		case strings.HasPrefix(line, "CREATE USER IF NOT EXISTS "):
			u := strings.Trim(strings.Split(f[5], "@")[0], "'")
			if !s.users[u] {
				s.users[u], s.matches[u] = true, true
			}
		case strings.HasPrefix(line, "ALTER USER "):
			s.matches[strings.Trim(strings.Split(f[2], "@")[0], "'")] = true
		case strings.HasPrefix(line, "GRANT ALL PRIVILEGES ON "):
			s.grants[strings.Trim(strings.Split(f[6], "@")[0], "'")+" "+strings.Trim(strings.TrimSuffix(f[4], ".*"), "`")] = true
		}
	}
	return docker.Result{}, nil
}

func remoteConfig() (*stack.Config, *stack.Secrets) {
	cfg, sec := migrateConfig()
	cfg.DB.Mode = stack.DBRemote
	cfg.DB.Remote = stack.RemoteDB{Host: "db.example.org", Port: 3307, RootUser: "admin"}
	sec.DBRemoteRootPassword = migrateRootPassword
	sec.DBPicsurePassword = "Picsure-synthetic-pw"
	sec.DBAuthPassword = "Auth-synthetic-pw"
	sec.DBAirflowPassword = "Airflow-synthetic-pw"
	return cfg, sec
}

func failing(r *ops.BootstrapReport) map[string]string {
	got := map[string]string{}
	for _, c := range r.Checks {
		if !c.OK {
			got[c.Name] = c.Problem
		}
	}
	return got
}

func TestCheckBootstrapOnAnEmptyServer(t *testing.T) {
	s := newRemoteServer(t)
	d, _ := migrateDeps(s.f)
	cfg, sec := remoteConfig()

	r, err := ops.CheckBootstrap(context.Background(), d, cfg, sec)
	if err != nil {
		t.Fatal(err)
	}
	if r.OK || r.Version != "8.0.40" || r.Server != "db.example.org:3307" {
		t.Errorf("report %+v", r)
	}
	want := map[string]string{"database auth": "missing", "database picsure": "missing",
		"user picsure": "missing", "user auth": "missing", "user airflow": "missing"}
	if got := failing(r); len(got) != len(want) || got["user airflow"] != "missing" || got["database auth"] != "missing" {
		t.Errorf("failing checks %v, want %v", got, want)
	}
	if len(s.f.Calls()) != 1 {
		t.Errorf("calls %v, want only the state query", s.f.Calls())
	}
	if len(s.scripts) != 0 {
		t.Errorf("--check ran %q", s.scripts)
	}
}

func TestCheckBootstrapFindsAPasswordMismatchAndAMissingGrant(t *testing.T) {
	s := newRemoteServer(t)
	s.full()
	s.matches["auth"] = false
	delete(s.grants, "airflow picsure")
	d, _ := migrateDeps(s.f)
	cfg, sec := remoteConfig()

	r, err := ops.CheckBootstrap(context.Background(), d, cfg, sec)
	if err != nil {
		t.Fatal(err)
	}
	got := failing(r)
	if r.OK || len(got) != 3 || got["login auth"] != "its password doesn't match secrets.yaml" ||
		got["grants airflow on picsure"] != "missing ALL PRIVILEGES" || !strings.Contains(got["login airflow"], "picsure") {
		t.Errorf("failing checks %v", got)
	}
	noPasswordInArgv(t, s.f, sec)
}

func TestBootstrapStepCreatesWhatIsMissing(t *testing.T) {
	s := newRemoteServer(t)
	s.databases["auth"] = true
	d, rec := migrateDeps(s.f)
	cfg, sec := remoteConfig()

	if err := ops.Bootstrap(context.Background(), d, cfg, sec, ops.BootstrapOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	if len(s.scripts) != 1 {
		t.Fatalf("root ran %d scripts, want 1", len(s.scripts))
	}
	in := s.scripts[0]
	for _, want := range []string{"CREATE DATABASE IF NOT EXISTS `picsure`",
		"CREATE USER IF NOT EXISTS 'airflow'@'%' IDENTIFIED BY 'Airflow-synthetic-pw'",
		"GRANT ALL PRIVILEGES ON `auth`.* TO 'airflow'@'%'"} {
		if !strings.Contains(in, want) {
			t.Errorf("bootstrap script lacks %q:\n%s", want, in)
		}
	}
	if strings.Contains(in, "ALTER USER") {
		t.Errorf("bootstrap without --sync-passwords altered a user:\n%s", in)
	}
	if got := stepStatuses(rec); got[ops.StepDB] != events.StepSkipped || got[ops.StepDBBootstrap] != events.StepOK {
		t.Errorf("steps %v", got)
	}
	noPasswordInArgv(t, s.f, sec)

	// A second run finds everything in place.
	*rec = events.Recorder{}
	if err := ops.Bootstrap(context.Background(), d, cfg, sec, ops.BootstrapOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	if got := stepStatuses(rec); got[ops.StepDBBootstrap] != events.StepSkipped {
		t.Errorf("second run: steps %v", got)
	}
}

func TestBootstrapStepNeedsSyncPasswordsForAnExistingUser(t *testing.T) {
	s := newRemoteServer(t)
	s.full()
	s.matches["picsure"] = false
	d, _ := migrateDeps(s.f)
	cfg, sec := remoteConfig()

	err := ops.Bootstrap(context.Background(), d, cfg, sec, ops.BootstrapOptions{}, nil)
	if exitcode.FromError(err) != exitcode.CodePrecondition || !strings.Contains(err.Error(), "--sync-passwords") ||
		!strings.Contains(err.Error(), "login picsure") {
		t.Fatalf("err %v, want exit 3 suggesting --sync-passwords", err)
	}

	if err := ops.Bootstrap(context.Background(), d, cfg, sec, ops.BootstrapOptions{SyncPasswords: true}, nil); err != nil {
		t.Fatal(err)
	}
	last := s.scripts[len(s.scripts)-1]
	if !strings.Contains(last, "ALTER USER 'picsure'@'%' IDENTIFIED BY 'Picsure-synthetic-pw'") {
		t.Errorf("--sync-passwords script:\n%s", last)
	}
	if !s.matches["picsure"] {
		t.Error("picsure's password still doesn't match")
	}
}

func TestCheckBootstrapNamesAnAccountThatShadowsTheUser(t *testing.T) {
	s := newRemoteServer(t)
	s.full()
	s.matches["auth"] = false
	s.accounts = []string{"auth 172.17.%"}
	d, _ := migrateDeps(s.f)
	cfg, sec := remoteConfig()

	r, err := ops.CheckBootstrap(context.Background(), d, cfg, sec)
	if err != nil {
		t.Fatal(err)
	}
	if got := failing(r)["login auth"]; !strings.Contains(got, "'auth'@'172.17.%'") || r.NeedsSync() {
		t.Errorf("login auth: %q, needs sync %v", got, r.NeedsSync())
	}
}

func TestBootstrapRefusesAnEmptyPassword(t *testing.T) {
	s := newRemoteServer(t)
	d, _ := migrateDeps(s.f)
	cfg, sec := remoteConfig()
	sec.DBAuthPassword = ""

	err := ops.Bootstrap(context.Background(), d, cfg, sec, ops.BootstrapOptions{SyncPasswords: true}, nil)
	if exitcode.FromError(err) != exitcode.CodePrecondition || !strings.Contains(err.Error(), "db_auth_password") {
		t.Fatalf("err %v, want exit 3 naming db_auth_password", err)
	}
	if len(s.scripts) != 0 {
		t.Errorf("bootstrap ran %q", s.scripts)
	}
	if _, err := ops.CheckBootstrap(context.Background(), d, cfg, sec); exitcode.FromError(err) != exitcode.CodePrecondition {
		t.Errorf("check: %v, want exit 3", err)
	}
}

func TestBootstrapRefusedRootIsAPrecondition(t *testing.T) {
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker run * mysql:8.0 mysql *")).Exit(1).
		Stderr("ERROR 1045 (28000): Access denied for user 'admin'@'172.17.0.3' (using password: YES)\n")
	d, _ := migrateDeps(f)
	cfg, sec := remoteConfig()
	cfg.DB.Remote.Host = "127.0.0.1"

	_, err := ops.CheckBootstrap(context.Background(), d, cfg, sec)
	if exitcode.FromError(err) != exitcode.CodePrecondition || !strings.Contains(err.Error(), "host.docker.internal") {
		t.Fatalf("err %v, want exit 3 with the loopback hint", err)
	}
}

func TestDBStepsAddBootstrapForARemoteDatabase(t *testing.T) {
	d, _ := migrateDeps(fakerunner.New(t))
	cfg, sec := migrateConfig()
	ids := func() []string {
		var ids []string
		for _, s := range ops.DBSteps(d, cfg, sec, ops.DBOptions{}) {
			ids = append(ids, s.ID)
		}
		return ids
	}
	if got := ids(); strings.Join(got, ",") != "db" {
		t.Errorf("local: %v", got)
	}
	cfg.DB.Mode = stack.DBRemote
	if got := ids(); strings.Join(got, ",") != "db,db-bootstrap" {
		t.Errorf("remote: %v", got)
	}
}

func TestLoopbackHint(t *testing.T) {
	for host, want := range map[string]bool{"localhost": true, "LOCALHOST": true, "127.0.0.1": true, "127.1.2.3": true,
		"::1": true, "0.0.0.0": true, "host.docker.internal": false, "db.example.org": false, "10.0.0.5": false} {
		if got := ops.LoopbackHint(host) != ""; got != want {
			t.Errorf("LoopbackHint(%q) set: %v, want %v", host, got, want)
		}
	}
}

func noPasswordInArgv(t *testing.T, f *fakerunner.Runner, sec *stack.Secrets) {
	t.Helper()
	for _, c := range f.Calls() {
		argv := strings.Join(c.Argv, " ")
		for _, pw := range []stack.Secret{sec.DBRemoteRootPassword, sec.DBPicsurePassword, sec.DBAuthPassword, sec.DBAirflowPassword} {
			if strings.Contains(argv, string(pw)) {
				t.Errorf("a password reached argv: %q", c.Argv)
			}
		}
	}
}

func stepStatuses(rec *events.Recorder) map[string]events.StepStatus {
	got := map[string]events.StepStatus{}
	for _, e := range rec.Events() {
		if done, ok := e.(events.StepDone); ok {
			got[done.ID] = done.Status
		}
	}
	return got
}
