package ops_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

const migrateRootPassword = "R00t-synthetic-password"

func migrateDeps(f *fakerunner.Runner) (*ops.Deps, *events.Recorder) {
	var rec events.Recorder
	return &ops.Deps{
		Runner: f,
		Docker: docker.NewEngine(f),
		Compose: &docker.Compose{Runner: f, Files: []string{"/stack/.pic-sure/render/compose.yaml"}, ProjectDir: "/stack",
			Env: func() []string { return []string{"DB_ROOT_PASSWORD=" + migrateRootPassword} }},
		Clock: ops.FixedClock(t0),
		Sink:  &rec,
	}, &rec
}

func migrateConfig() (*stack.Config, *stack.Secrets) {
	cfg := stack.DefaultConfig()
	cfg.Name = "demo"
	return &cfg, &stack.Secrets{
		DBRootPassword:       migrateRootPassword,
		DictionaryDBPassword: "Dict-synthetic-password",
		ApplicationUUID:      "8b5722c9-62fd-48d6-b0bf-4f67e53efb2b",
		ResourceUUID:         "02e23f52-f354-4e8b-992c-d37c8b9ba140",
		VisualizationUUID:    "ca0ad4a9-130a-3a8a-ae00-e35b07f1108b",
	}
}

func psLine(service, state, health string) string {
	return fmt.Sprintf(`{"ID":"id-%s","Service":%q,"State":%q,"Health":%q}`+"\n", service, service, state, health)
}

var fastDB = ops.DBOptions{Timeout: 50 * time.Millisecond, Interval: time.Millisecond}

func noSecretInArgv(t *testing.T, f *fakerunner.Runner) {
	t.Helper()
	for _, c := range f.Calls() {
		if strings.Contains(strings.Join(c.Argv, " "), migrateRootPassword) {
			t.Errorf("a password reached argv: %q", c.Argv)
		}
	}
}

func TestDBStepWaitsForHealthyThenProbesOverTCP(t *testing.T) {
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker compose * up -d picsure-db"))
	ps := fakerunner.Glob("docker compose * ps --all --format json picsure-db")
	f.On(ps).Stdout(psLine("picsure-db", "running", "starting")).Times(1)
	f.On(ps).Stdout(psLine("picsure-db", "running", "healthy"))
	probe := fakerunner.Glob("docker exec -i -e MYSQL_PWD id-picsure-db mysql * --host=127.0.0.1 --user=root")
	// The entrypoint's temporary server listens only on the socket.
	f.On(probe).Stderr("ERROR 2003 (HY000): Can't connect to MySQL server on '127.0.0.1:3306' (111)\n").Exit(1).Times(1)
	f.On(probe).Stdout("1\n")
	d, rec := migrateDeps(f)
	cfg, sec := migrateConfig()

	if err := steps.Run(context.Background(), d.Sink, []steps.Step{ops.DBStep(d, cfg, sec, fastDB)}, steps.Options{}); err != nil {
		t.Fatal(err)
	}
	f.AssertOrder(fakerunner.Glob("docker compose * up -d picsure-db"), ps, probe)
	if n := len(f.CallsMatching(probe)); n != 2 {
		t.Errorf("%d probes, want 2", n)
	}
	if c := f.CallsMatching(probe)[1]; !c.HasEnv("MYSQL_PWD") || string(c.Stdin) == "" || !strings.Contains(string(c.Stdin), "SELECT 1") {
		t.Errorf("probe: env %v, stdin %q", c.Env, c.Stdin)
	}
	noSecretInArgv(t, f)
	assertStepDone(t, rec, ops.StepDB, events.StepOK)
}

func TestDBStepCheckSkipsAHealthyDatabase(t *testing.T) {
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker compose * ps *")).Stdout(psLine("picsure-db", "running", "healthy"))
	f.On(fakerunner.Glob("docker exec *")).Stdout("1\n")
	d, rec := migrateDeps(f)
	cfg, sec := migrateConfig()

	if err := steps.Run(context.Background(), d.Sink, []steps.Step{ops.DBStep(d, cfg, sec, fastDB)}, steps.Options{}); err != nil {
		t.Fatal(err)
	}
	f.AssertNotCalled(fakerunner.Glob("docker compose * up *"))
	assertStepDone(t, rec, ops.StepDB, events.StepSkipped)
}

func TestDBStepFailsAtOnceOnAccessDenied(t *testing.T) {
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker compose * up -d picsure-db"))
	f.On(fakerunner.Glob("docker compose * ps *")).Stdout(psLine("picsure-db", "running", "healthy"))
	probe := fakerunner.Glob("docker exec *")
	f.On(probe).Stderr("ERROR 1045 (28000): Access denied for user 'root'@'127.0.0.1' (using password: YES)\n").Exit(1)
	d, _ := migrateDeps(f)
	cfg, sec := migrateConfig()

	err := steps.Run(context.Background(), d.Sink, []steps.Step{ops.DBStep(d, cfg, sec, fastDB)}, steps.Options{})
	if exitcode.FromError(err) != exitcode.CodePrecondition {
		t.Fatalf("err %v, want exit 3", err)
	}
	if !strings.Contains(err.Error(), "demo_picsure-db-data") || !strings.Contains(err.Error(), "Access denied") {
		t.Errorf("err %q, want the volume named and the cause", err)
	}
	// One probe in Check, one in Apply: no retry after access denied.
	if n := len(f.CallsMatching(probe)); n != 2 {
		t.Errorf("%d probes, want 2", n)
	}
}

func TestDBStepReportsAStoppedContainerWithItsLogs(t *testing.T) {
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker compose * up -d picsure-db"))
	f.On(fakerunner.Glob("docker compose * ps *")).Stdout(`{"ID":"x","Service":"picsure-db","State":"exited","ExitCode":1}` + "\n")
	f.On(fakerunner.Glob("docker compose * logs --tail 30 picsure-db")).Stdout("[ERROR] [MY-010119] Aborting\n")
	d, rec := migrateDeps(f)
	cfg, sec := migrateConfig()

	err := steps.Run(context.Background(), d.Sink, []steps.Step{ops.DBStep(d, cfg, sec, fastDB)}, steps.Options{})
	if err == nil || !strings.Contains(err.Error(), "stopped (exit 1)") {
		t.Fatalf("err %v", err)
	}
	if !hasLog(rec, "Aborting") {
		t.Error("the container's log tail wasn't shown")
	}
}

func TestDBStepTimesOut(t *testing.T) {
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker compose * up -d picsure-db"))
	f.On(fakerunner.Glob("docker compose * ps *")).Stdout(psLine("picsure-db", "running", "unhealthy"))
	f.On(fakerunner.Glob("docker compose * logs *"))
	d, _ := migrateDeps(f)
	cfg, sec := migrateConfig()

	err := steps.Run(context.Background(), d.Sink, []steps.Step{ops.DBStep(d, cfg, sec, fastDB)}, steps.Options{})
	if err == nil || !strings.Contains(err.Error(), `health "unhealthy"`) {
		t.Fatalf("err %v", err)
	}
}

func TestDBStepProbesARemoteDatabase(t *testing.T) {
	f := fakerunner.New(t)
	probe := fakerunner.Glob("docker run -i --rm -e MYSQL_PWD mysql:8.0 mysql * --host=db.example.org --port=3307 --user=admin")
	f.On(probe).Exit(1).Stderr("ERROR 2005 (HY000): Unknown MySQL server host\n").Times(1)
	f.On(probe).Exit(1).Stderr("ERROR 2005 (HY000): Unknown MySQL server host\n").Times(1)
	d, _ := migrateDeps(f)
	cfg, sec := migrateConfig()
	cfg.DB.Mode = stack.DBRemote
	cfg.DB.Remote = stack.RemoteDB{Host: "db.example.org", Port: 3307, RootUser: "admin"}
	sec.DBRemoteRootPassword = migrateRootPassword

	err := steps.Run(context.Background(), d.Sink, []steps.Step{ops.DBStep(d, cfg, sec, fastDB)}, steps.Options{})
	if exitcode.FromError(err) != exitcode.CodePrecondition || !strings.Contains(err.Error(), "db.example.org:3307") {
		t.Fatalf("err %v, want exit 3 naming the host", err)
	}
	f.AssertNotCalled(fakerunner.Glob("docker compose *"))
	noSecretInArgv(t, f)
}

// migrationsWorld fakes a stack whose Flyway one-shots mount temp
// directories, with MySQL and Postgres histories the test sets.
type migrationsWorld struct {
	f       *fakerunner.Runner
	dirs    map[string]string // target → host dir
	tables  map[string]string // schema.table → rows "version\ttype\tsuccess"
	pgTable string            // "" for no history table
	dictUp  bool
}

func newMigrationsWorld(t *testing.T) *migrationsWorld {
	root := t.TempDir()
	w := &migrationsWorld{f: fakerunner.New(t), dirs: map[string]string{}, tables: map[string]string{}, dictUp: true}
	for target, files := range map[string][]string{
		"/migrations/auth":           {"V1__a.sql", "V2__b.sql"},
		"/migrations/picsure":        {"V1__a.sql", "V1_1__b.sql"},
		"/migrations/custom/picsure": {"V1__a.sql", "V2__b.sql"},
		"/migrations/custom/auth":    {"V1__a.sql"},
		"/migrations/dictionary":     {"V1__a.sql", "V10__b.sql"},
	} {
		dir := filepath.Join(root, strings.ReplaceAll(strings.TrimPrefix(target, "/migrations/"), "/", "-"))
		writeFiles(t, dir, files...)
		w.dirs[target] = dir
	}
	schema := filepath.Join(root, "schema.sql")
	writeFile(t, schema, "CREATE SCHEMA dict;")
	w.dirs["schema"] = schema
	w.tables = map[string]string{
		"auth.flyway_schema_history":           "1\tSQL\t1\n2\tSQL\t1\n",
		"picsure.flyway_schema_history":        "1\tSQL\t1\n1.1\tSQL\t1\n",
		"picsure.flyway_custom_schema_history": "1\tBASELINE\t1\n2\tSQL\t1\n",
		"auth.flyway_custom_schema_history":    "1\tSQL\t1\n",
	}
	w.pgTable = "1\tSQL\tt\n10\tSQL\tt\n"

	w.f.On(fakerunner.Glob("docker compose * config --no-interpolate")).Do(func(context.Context, fakerunner.Call) (docker.Result, error) {
		var b strings.Builder
		b.WriteString("services:\n  flyway-init:\n    volumes:\n")
		for _, tgt := range []string{"/migrations/auth", "/migrations/picsure", "/migrations/custom/auth", "/migrations/custom/picsure"} {
			fmt.Fprintf(&b, "      - type: bind\n        source: %s\n        target: %s\n", w.dirs[tgt], tgt)
		}
		fmt.Fprintf(&b, "  flyway-dictionary-init:\n    volumes:\n      - type: bind\n        source: %s\n        target: /migrations/dictionary\n", w.dirs["/migrations/dictionary"])
		fmt.Fprintf(&b, "  dictionary-db:\n    volumes:\n      - type: volume\n        source: dictionary-db-data\n        target: /var/lib/postgresql/data\n"+
			"      - type: bind\n        source: %s\n        target: /docker-entrypoint-initdb.d/schema.sql\n", w.dirs["schema"])
		return docker.Result{Stdout: []byte(b.String())}, nil
	})
	w.f.On(fakerunner.Glob("docker compose * ps --all --format json picsure-db")).Stdout(psLine("picsure-db", "running", "healthy"))
	w.f.On(fakerunner.Glob("docker compose * ps --all --format json dictionary-db")).Do(func(context.Context, fakerunner.Call) (docker.Result, error) {
		if !w.dictUp {
			return docker.Result{}, nil
		}
		return docker.Result{Stdout: []byte(psLine("dictionary-db", "running", "healthy"))}, nil
	})
	w.f.On(fakerunner.Glob("docker exec -i -e MYSQL_PWD id-picsure-db mysql *")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		in := string(c.Stdin)
		var out strings.Builder
		if strings.HasSuffix(in, "\nSELECT 1\n;\n") {
			return docker.Result{Stdout: []byte("1\n")}, nil
		}
		if strings.Contains(in, "information_schema.tables") {
			for name := range w.tables {
				out.WriteString(name + "\n")
			}
			return docker.Result{Stdout: []byte(out.String())}, nil
		}
		for name, rows := range w.tables {
			if !strings.Contains(in, "FROM "+name+" ") {
				continue
			}
			for _, r := range strings.Split(strings.TrimSpace(rows), "\n") {
				out.WriteString(name + "\t" + r + "\n")
			}
		}
		return docker.Result{Stdout: []byte(out.String())}, nil
	})
	w.f.On(fakerunner.Glob("docker exec -i -e PGCLIENTENCODING -e PGPASSWORD id-dictionary-db psql *")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		if strings.Contains(string(c.Stdin), "to_regclass") {
			if w.pgTable == "" {
				return docker.Result{Stdout: []byte("f\n")}, nil
			}
			return docker.Result{Stdout: []byte("t\n")}, nil
		}
		return docker.Result{Stdout: []byte(w.pgTable)}, nil
	})
	return w
}

func writeFiles(t *testing.T, dir string, names ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		writeFile(t, filepath.Join(dir, n), "SELECT 1;")
	}
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationsUpToDate(t *testing.T) {
	tests := []struct {
		name  string
		setup func(w *migrationsWorld)
		want  bool
	}{
		{"every migration applied", func(*migrationsWorld) {}, true},
		{"a new migration file", func(w *migrationsWorld) { writeFiles(t, w.dirs["/migrations/custom/auth"], "V2__new.sql") }, false},
		{"a failed row", func(w *migrationsWorld) { w.tables["auth.flyway_schema_history"] += "3\tSQL\t0\n" }, false},
		{"a missing history table", func(w *migrationsWorld) { delete(w.tables, "auth.flyway_custom_schema_history") }, false},
		{"no dictionary history", func(w *migrationsWorld) { w.pgTable = "" }, false},
		{"dictionary-db not running", func(w *migrationsWorld) { w.dictUp = false }, false},
		{"a version applied in another spelling", func(w *migrationsWorld) {
			w.tables["picsure.flyway_schema_history"] = "1.0\tSQL\t1\n01.1\tSQL\t1\n"
		}, true},
		{"versions compare numerically", func(w *migrationsWorld) { w.pgTable = "1\tSQL\tt\n9\tSQL\tt\n" }, false},
		{"a baseline above the files", func(w *migrationsWorld) {
			w.tables["auth.flyway_custom_schema_history"] = "1.5\tBASELINE\t1\n"
		}, true},
		{"a repeatable migration", func(w *migrationsWorld) { writeFiles(t, w.dirs["/migrations/picsure"], "R__view.sql") }, false},
		{"a pass with no migrations", func(w *migrationsWorld) {
			if err := os.RemoveAll(w.dirs["/migrations/custom/auth"]); err != nil {
				t.Fatal(err)
			}
			writeFiles(t, w.dirs["/migrations/custom/auth"])
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newMigrationsWorld(t)
			tt.setup(w)
			d, _ := migrateDeps(w.f)
			cfg, sec := migrateConfig()
			got, err := ops.MigrationsUpToDate(context.Background(), d, cfg, sec)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("up to date = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMigrationsUpToDateOnARemoteDatabase(t *testing.T) {
	w := newMigrationsWorld(t)
	w.f.On(fakerunner.Glob("docker run -i --rm -e MYSQL_PWD mysql:8.0 mysql * --host=db.example.org *")).Stdout("")
	d, _ := migrateDeps(w.f)
	cfg, sec := migrateConfig()
	cfg.DB.Mode = stack.DBRemote
	cfg.DB.Remote = stack.RemoteDB{Host: "db.example.org", Port: 3306, RootUser: "admin"}
	sec.DBRemoteRootPassword = migrateRootPassword

	got, err := ops.MigrationsUpToDate(context.Background(), d, cfg, sec)
	if err != nil || got {
		t.Fatalf("up to date %v, err %v; want false with no history tables", got, err)
	}
	w.f.AssertNotCalled(fakerunner.Glob("docker compose * ps --all --format json picsure-db"))
}

func TestMigrateRunsBothOneShotsThenRestartsRunningCaches(t *testing.T) {
	f := fakerunner.New(t)
	initRun := fakerunner.Glob("docker compose * run --rm -T flyway-init")
	dictRun := fakerunner.Glob("docker compose * run --rm -T flyway-dictionary-init")
	f.On(initRun).Stdout("[flyway] All migrations complete.\n")
	f.On(dictRun)
	f.On(fakerunner.Glob("docker compose * ps --all --format json")).
		Stdout(psLine("psama", "running", "healthy") + psLine("dictionary-api", "exited", "") + psLine("picsure-db", "running", "healthy"))
	restart := fakerunner.Glob("docker compose * restart psama")
	f.On(restart)
	d, _ := migrateDeps(f)
	cfg, sec := migrateConfig()
	step := ops.MigrateStep(d, cfg, sec, ops.MigrateOptions{})

	if err := step.Apply(context.Background(), d.Sink); err != nil {
		t.Fatal(err)
	}
	f.AssertOrder(initRun, dictRun, restart)
	for _, c := range f.Calls() {
		if strings.Contains(strings.Join(c.Argv, " "), "FLYWAY_ACTION") {
			t.Errorf("a migrate passed FLYWAY_ACTION: %q", c.Argv)
		}
	}
}

func TestMigrateRepairPassesTheActionAndRestartsNothing(t *testing.T) {
	f := fakerunner.New(t)
	initRun := fakerunner.Glob("docker compose * run --rm -T -e FLYWAY_ACTION flyway-init")
	dictRun := fakerunner.Glob("docker compose * run --rm -T -e FLYWAY_ACTION flyway-dictionary-init")
	f.On(initRun)
	f.On(dictRun)
	d, _ := migrateDeps(f)
	cfg, sec := migrateConfig()
	step := ops.MigrateStep(d, cfg, sec, ops.MigrateOptions{Action: ops.FlywayRepair})
	if step.Check != nil {
		t.Error("repair has a Check, so it could be skipped")
	}

	if err := step.Apply(context.Background(), d.Sink); err != nil {
		t.Fatal(err)
	}
	f.AssertOrder(initRun, dictRun)
	if c := f.CallsMatching(initRun)[0]; !c.HasEnv("FLYWAY_ACTION") {
		t.Errorf("env names %v, want FLYWAY_ACTION", c.Env)
	}
}

func TestMigrateFailureShowsTheOutputTail(t *testing.T) {
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker compose * run --rm -T flyway-init")).
		Stdout("[flyway] Running auth schema migrations...\n").
		Stderr("ERROR: Validate failed: Detected failed migration to version 3\n").Exit(1)
	d, rec := migrateDeps(f)
	cfg, sec := migrateConfig()

	err := ops.MigrateStep(d, cfg, sec, ops.MigrateOptions{}).Apply(context.Background(), d.Sink)
	if err == nil || !strings.Contains(err.Error(), "flyway-init (Flyway migrate) failed: exit 1") {
		t.Fatalf("err %v", err)
	}
	if !hasLog(rec, "Detected failed migration") {
		t.Error("the output's tail wasn't shown")
	}
	f.AssertNotCalled(fakerunner.Glob("docker compose * run --rm -T flyway-dictionary-init"))
}

func TestMigrateSkipsAnUpToDateStack(t *testing.T) {
	w := newMigrationsWorld(t)
	d, rec := migrateDeps(w.f)
	cfg, sec := migrateConfig()

	if err := ops.Migrate(context.Background(), d, cfg, sec, ops.MigrateOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	assertStepDone(t, rec, ops.StepDB, events.StepSkipped)
	assertStepDone(t, rec, ops.StepMigrate, events.StepSkipped)
	w.f.AssertNotCalled(fakerunner.Glob("docker compose * run *"))
}

func TestMigrateCheck(t *testing.T) {
	w := newMigrationsWorld(t)
	w.f.On(fakerunner.Glob("docker compose * config --quiet"))
	d, _ := migrateDeps(w.f)
	cfg, sec := migrateConfig()

	r := ops.MigrateCheck(context.Background(), d, cfg, sec)
	if !r.OK || len(r.Warnings) != 0 {
		t.Fatalf("report %+v, want OK with no warnings", r)
	}
	for _, c := range w.f.Calls() {
		if c.Argv[0] != "docker" || c.Argv[1] != "compose" || !strings.Contains(strings.Join(c.Argv, " "), " config ") {
			t.Errorf("--check ran %q; it may only run compose config", c.Argv)
		}
	}

	if err := os.RemoveAll(w.dirs["/migrations/custom/picsure"]); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(w.dirs["/migrations/custom/auth"], "V2__legacy.sql"), "INSERT INTO x VALUES (UNHEX('__APPLICATION_UUID__'));")
	sec.ResourceUUID = ""
	r = ops.MigrateCheck(context.Background(), d, cfg, sec)
	if r.OK {
		t.Fatal("check passed with a missing directory and UUID")
	}
	failed := map[string]string{}
	for _, in := range r.Inputs {
		if !in.OK {
			failed[in.Name] = in.Problem
		}
	}
	if len(failed) != 2 || failed["project picsure migrations (Baseline)"] != "missing" || failed["project UUIDs"] != "secrets.yaml lacks resource_uuid" {
		t.Errorf("failed inputs %q", failed)
	}
	if len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "legacy Jenkins UUID tokens") {
		t.Errorf("warnings %q", r.Warnings)
	}
}

func TestMigrateCheckReportsAnInvalidComposeConfig(t *testing.T) {
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker compose * config *")).Stderr("yaml: line 3: did not find expected key\n").Exit(15)
	d, _ := migrateDeps(f)
	cfg, sec := migrateConfig()

	r := ops.MigrateCheck(context.Background(), d, cfg, sec)
	if r.OK || r.Inputs[0].Name != "compose config" || r.Inputs[0].OK {
		t.Fatalf("report %+v", r)
	}
}

func assertStepDone(t *testing.T, rec *events.Recorder, id string, want events.StepStatus) {
	t.Helper()
	for _, e := range rec.Events() {
		if done, ok := e.(events.StepDone); ok && done.ID == id {
			if done.Status != want {
				t.Errorf("step %s: %s, want %s", id, done.Status, want)
			}
			return
		}
	}
	t.Errorf("step %s never finished", id)
}

func hasLog(rec *events.Recorder, substr string) bool {
	for _, e := range rec.Events() {
		if l, ok := e.(events.Log); ok && strings.Contains(l.Line, substr) {
			return true
		}
	}
	return false
}

func TestStatusReportsTheMigrationState(t *testing.T) {
	for _, tt := range []struct {
		name   string
		setup  func(w *migrationsWorld)
		status string
		err    string
	}{
		{"up to date", func(*migrationsWorld) {}, "up_to_date", ""},
		{"pending", func(w *migrationsWorld) { w.pgTable = "" }, "pending", ""},
		{"databases down", func(w *migrationsWorld) { w.dictUp = false }, "unknown", "the databases aren't running"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := newStatusStack(t, "schema: 1\nname: demo\nauth: {mode: open, admin_email: admin@example.com}\n")
			_, sec := migrateConfig()
			if err := st.SaveSecrets(sec); err != nil {
				t.Fatal(err)
			}
			if err := st.MkdirAll(".pic-sure/render", 0o755); err != nil {
				t.Fatal(err)
			}
			if err := st.WriteFile(".pic-sure/render/compose.yaml", []byte("services: {}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			w := newMigrationsWorld(t)
			tt.setup(w)
			w.f.On(fakerunner.Glob("docker compose * ps --all --format json")).Do(func(context.Context, fakerunner.Call) (docker.Result, error) {
				out := psLine("picsure-db", "running", "healthy")
				if w.dictUp {
					out += psLine("dictionary-db", "running", "healthy")
				}
				return docker.Result{Stdout: []byte(out)}, nil
			})

			r := ops.Status(context.Background(), statusDeps(t, w.f, st), st, statusOpts())
			if r.Migrations.Status != tt.status || r.Migrations.Error != tt.err {
				t.Errorf("migrations %+v, want %s %q", r.Migrations, tt.status, tt.err)
			}
		})
	}
}
