package sql_test

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/sql"
)

// Synthetic secrets. Each is distinctive enough that finding it as a
// substring of argv means it leaked.
const (
	rootPassword = "R00tPassw0rd-synthetic"
	appPassword  = "AppPassw0rd'with\\quote"
	adminEmail   = "o'brien@example.org"
	token        = "eyJhbGciOiJIUzI1NiJ9.synthetic.token"
)

var ctx = context.Background()

func TestExecMySQLLocalRunsTheClientInThePicsureDBContainer(t *testing.T) {
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker exec *"))
	target := sql.MySQLTarget{Container: "0123abcd", Password: rootPassword}

	if err := sql.ExecMySQL(ctx, docker.NewEngine(f), target, "SELECT 1", "SELECT 2;"); err != nil {
		t.Fatal(err)
	}

	c := onlyCall(t, f)
	assertArgv(t, c, "docker", "exec", "-i", "-e", "MYSQL_PWD", "0123abcd",
		"mysql", "--batch", "--skip-column-names", "--default-character-set=utf8mb4", "--user=root")
	if !reflect.DeepEqual(c.Env, []string{"MYSQL_PWD"}) {
		t.Errorf("env names = %q, want [MYSQL_PWD]", c.Env)
	}
	if got, want := string(c.Stdin), "SELECT 1;\nSELECT 2;\n"; got != want {
		t.Errorf("stdin = %q, want %q", got, want)
	}
}

func TestExecMySQLLocalWithHostConnectsOverTCP(t *testing.T) {
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker exec *"))
	target := sql.MySQLTarget{Container: "0123abcd", Host: "127.0.0.1", Password: rootPassword}

	if err := sql.ExecMySQL(ctx, docker.NewEngine(f), target, "SELECT 1"); err != nil {
		t.Fatal(err)
	}

	assertArgv(t, onlyCall(t, f), "docker", "exec", "-i", "-e", "MYSQL_PWD", "0123abcd",
		"mysql", "--batch", "--skip-column-names", "--default-character-set=utf8mb4",
		"--host=127.0.0.1", "--user=root")
}

func TestExecMySQLRemoteRunsAThrowawayClientContainer(t *testing.T) {
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker run *"))
	target := sql.MySQLTarget{Host: "db.example.org", Port: 3307, User: "admin", Password: rootPassword, Database: "auth"}

	if err := sql.ExecMySQL(ctx, docker.NewEngine(f), target, "SELECT 1"); err != nil {
		t.Fatal(err)
	}

	c := onlyCall(t, f)
	assertArgv(t, c, "docker", "run", "-i", "--rm", "-e", "MYSQL_PWD", "mysql:8.0",
		"mysql", "--batch", "--skip-column-names", "--default-character-set=utf8mb4",
		"--host=db.example.org", "--port=3307", "--user=admin", "--database=auth")
	if !c.HasEnv("MYSQL_PWD") {
		t.Errorf("env names = %q, want MYSQL_PWD", c.Env)
	}
}

func TestExecMySQLWithoutPasswordPassesNoEnv(t *testing.T) {
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker exec *"))

	if err := sql.ExecMySQL(ctx, docker.NewEngine(f), sql.MySQLTarget{Container: "0123abcd"}, "SELECT 1"); err != nil {
		t.Fatal(err)
	}

	c := onlyCall(t, f)
	if len(c.Env) != 0 || containsArg(c.Argv, "-e") {
		t.Errorf("argv %s, env %q: want no -e and no env", c, c.Env)
	}
}

// The fake runner records env names only, so this runner checks the value.
func TestExecMySQLPutsThePasswordInTheEnvironment(t *testing.T) {
	var got docker.Cmd
	r := runnerFunc(func(c docker.Cmd) (docker.Result, error) {
		got = c
		return docker.Result{}, nil
	})

	if err := sql.ExecMySQL(ctx, docker.NewEngine(r), sql.MySQLTarget{Container: "0123abcd", Password: rootPassword}, "SELECT 1"); err != nil {
		t.Fatal(err)
	}

	if want := []string{"MYSQL_PWD=" + rootPassword}; !reflect.DeepEqual(got.Env, want) {
		t.Errorf("Env = %q, want %q", got.Env, want)
	}
}

func TestSecretsAndEmailNeverReachArgv(t *testing.T) {
	statements := map[string][]string{
		"seed":      sql.SeedAdminUser(adminEmail, [16]byte{1, 2, 3}),
		"count":     {sql.CountUsersWithEmail(adminEmail)},
		"token":     {sql.SetApplicationToken(token)},
		"bootstrap": sql.Bootstrap(sql.AppUsers(sql.AppPasswords{Picsure: appPassword, Auth: appPassword, Airflow: appPassword}), true),
		"rotate":    {sql.AlterUserPassword(sql.Account{User: "picsure", Host: "%"}, appPassword)},
	}
	targets := map[string]sql.MySQLTarget{
		"local":  {Container: "0123abcd", Password: rootPassword},
		"remote": {Host: "db.example.org", Port: 3306, Password: rootPassword},
	}
	// Fragments of the secrets that survive escaping, so neither the raw
	// nor the escaped form may appear.
	secrets := []string{"Passw0rd", "brien", "synthetic"}

	for sName, stmts := range statements {
		for tName, target := range targets {
			t.Run(sName+"/"+tName, func(t *testing.T) {
				f := fakerunner.New(t)
				f.On(fakerunner.Regex(`^docker (exec|run) `))

				if err := sql.ExecMySQL(ctx, docker.NewEngine(f), target, stmts...); err != nil {
					t.Fatal(err)
				}

				c := onlyCall(t, f)
				for _, a := range c.Argv {
					for _, s := range secrets {
						if strings.Contains(a, s) {
							t.Errorf("argv element %q contains secret %q", a, s)
						}
					}
					if strings.HasPrefix(a, "MYSQL_PWD=") {
						t.Errorf("argv has %q; want a bare -e MYSQL_PWD", a)
					}
				}
				assertBareEnvFlag(t, c.Argv, "MYSQL_PWD")
				if want := strings.Join(stmts, ";\n") + ";\n"; string(c.Stdin) != want {
					t.Errorf("stdin = %q, want %q", c.Stdin, want)
				}
			})
		}
	}
}

func TestExecMySQLReportsTheClientError(t *testing.T) {
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker exec *")).Exit(1).
		Stderr("ERROR 1045 (28000): Access denied for user 'root'@'localhost' (using password: YES)\n")

	err := sql.ExecMySQL(ctx, docker.NewEngine(f), sql.MySQLTarget{Container: "0123abcd", Password: rootPassword}, "SELECT 1")

	var exitErr *docker.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode != 1 {
		t.Fatalf("err = %v, want an *ExitError with code 1", err)
	}
	if !strings.Contains(err.Error(), "Access denied") {
		t.Errorf("err = %v, want the client's error line", err)
	}
}

func TestExecMySQLWithNoStatementsRunsNothing(t *testing.T) {
	f := fakerunner.New(t) // any call fails the test
	if err := sql.ExecMySQL(ctx, docker.NewEngine(f), sql.MySQLTarget{Container: "0123abcd"}, "", " ; "); err != nil {
		t.Fatal(err)
	}
}

func TestExecMySQLNeedsAContainerOrAHost(t *testing.T) {
	f := fakerunner.New(t)
	if err := sql.ExecMySQL(ctx, docker.NewEngine(f), sql.MySQLTarget{Password: rootPassword}, "SELECT 1"); err == nil {
		t.Fatal("want an error for a target with neither a container nor a host")
	}
}

func TestQueryMySQLParsesBatchOutput(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stdout string
		want   [][]string
	}{
		{"no rows", "", nil},
		{"scalar", "3\n", [][]string{{"3"}}},
		{"empty value", "\n", [][]string{{""}}},
		{"columns and rows", "root@%\t8.0.40\nNULL\t\n", [][]string{{"root@%", "8.0.40"}, {"NULL", ""}}},
		{"escapes", `a\tb` + "\t" + `c\nd\\e\0` + "\t" + `\\n` + "\n", [][]string{{"a\tb", "c\nd\\e\x00", `\n`}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fakerunner.New(t)
			f.On(fakerunner.Glob("docker exec *")).Stdout(tc.stdout)

			rows, err := sql.QueryMySQL(ctx, docker.NewEngine(f), sql.MySQLTarget{Container: "0123abcd"}, sql.CountUsersWithEmail(adminEmail))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(rows, tc.want) {
				t.Errorf("rows = %q, want %q", rows, tc.want)
			}
			if got := string(onlyCall(t, f).Stdin); !strings.Contains(got, `'o''brien@example.org'`) {
				t.Errorf("stdin = %q, want the escaped email", got)
			}
		})
	}
}

func TestQueryMySQLReturnsTheClientError(t *testing.T) {
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker exec *")).Exit(1).Stderr("ERROR 1146 (42S02): Table 'auth.user' doesn't exist\n")

	if _, err := sql.QueryMySQL(ctx, docker.NewEngine(f), sql.MySQLTarget{Container: "0123abcd"}, "SELECT 1"); err == nil {
		t.Fatal("want an error")
	}
}

func TestExecPostgresRunsPsqlInTheDictionaryDBContainer(t *testing.T) {
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker exec *"))
	stmt, err := sql.AlterPostgresPassword("picsure", appPassword)
	if err != nil {
		t.Fatal(err)
	}
	target := sql.PostgresTarget{Container: "4567cdef", User: "picsure", Password: rootPassword, Database: "dictionary"}

	if err := sql.ExecPostgres(ctx, docker.NewEngine(f), target, stmt); err != nil {
		t.Fatal(err)
	}

	c := onlyCall(t, f)
	assertArgv(t, c, "docker", "exec", "-i", "-e", "PGCLIENTENCODING", "-e", "PGPASSWORD", "4567cdef",
		"psql", "--no-psqlrc", "--no-password", "--quiet", "--set=ON_ERROR_STOP=1",
		"--username=picsure", "--dbname=dictionary")
	assertBareEnvFlag(t, c.Argv, "PGPASSWORD")
	if !reflect.DeepEqual(c.Env, []string{"PGCLIENTENCODING", "PGPASSWORD"}) {
		t.Errorf("env names = %q, want [PGCLIENTENCODING PGPASSWORD]", c.Env)
	}
	for _, a := range c.Argv {
		if strings.Contains(a, rootPassword) || strings.Contains(a, "Passw0rd") {
			t.Errorf("argv element %q contains a secret", a)
		}
	}
	if got, want := string(c.Stdin), stmt+";\n"; got != want {
		t.Errorf("stdin = %q, want %q", got, want)
	}
}

func TestExecPostgresPutsThePasswordInTheEnvironment(t *testing.T) {
	var got docker.Cmd
	r := runnerFunc(func(c docker.Cmd) (docker.Result, error) {
		got = c
		return docker.Result{}, nil
	})

	if err := sql.ExecPostgres(ctx, docker.NewEngine(r), sql.PostgresTarget{Container: "4567cdef", User: "picsure", Password: rootPassword}, "SELECT 1"); err != nil {
		t.Fatal(err)
	}

	if want := []string{"PGCLIENTENCODING=UTF8", "PGPASSWORD=" + rootPassword}; !reflect.DeepEqual(got.Env, want) {
		t.Errorf("Env = %q, want %q", got.Env, want)
	}
	if containsArg(got.Argv, "--dbname=") {
		t.Errorf("argv %q: want no --dbname without a Database", got.Argv)
	}
}

func TestExecPostgresReportsFailure(t *testing.T) {
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker exec *")).Exit(3).Stderr(`ERROR:  role "nobody" does not exist` + "\n")

	err := sql.ExecPostgres(ctx, docker.NewEngine(f), sql.PostgresTarget{Container: "4567cdef", User: "picsure"}, "SELECT 1")

	var exitErr *docker.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode != 3 {
		t.Fatalf("err = %v, want an *ExitError with code 3", err)
	}
}

func TestExecPostgresNeedsAContainerAndAUser(t *testing.T) {
	f := fakerunner.New(t)
	for _, target := range []sql.PostgresTarget{{Container: "4567cdef"}, {User: "picsure"}} {
		if err := sql.ExecPostgres(ctx, docker.NewEngine(f), target, "SELECT 1"); err == nil {
			t.Errorf("target %+v: want an error", target)
		}
	}
}

func onlyCall(t *testing.T, f *fakerunner.Runner) fakerunner.Call {
	t.Helper()
	calls := f.Calls()
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1: %v", len(calls), calls)
	}
	return calls[0]
}

func assertArgv(t *testing.T, c fakerunner.Call, want ...string) {
	t.Helper()
	if !reflect.DeepEqual(c.Argv, want) {
		t.Errorf("argv:\n got %s\nwant %s", c, docker.FormatArgv(want))
	}
}

// assertBareEnvFlag checks that argv passes name to the container as a bare
// `-e NAME`, so docker takes the value from its own environment.
func assertBareEnvFlag(t *testing.T, argv []string, name string) {
	t.Helper()
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "-e" && argv[i+1] == name {
			return
		}
	}
	t.Errorf("argv %q has no bare -e %s", argv, name)
}

func containsArg(argv []string, prefix string) bool {
	for _, a := range argv {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}

type runnerFunc func(docker.Cmd) (docker.Result, error)

func (f runnerFunc) Run(_ context.Context, c docker.Cmd) (docker.Result, error) { return f(c) }

func (f runnerFunc) Stream(_ context.Context, c docker.Cmd, _, _ io.Writer) (int, error) {
	res, err := f(c)
	return res.ExitCode, err
}
