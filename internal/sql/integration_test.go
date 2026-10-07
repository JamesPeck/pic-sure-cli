//go:build integration

// These tests run the statements against real MySQL and Postgres servers in
// throwaway containers, to prove the escaping round-trips. They need a
// Docker daemon and skip without one:
//
//	go test -tags integration ./internal/sql/
package sql_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/sql"
)

// nastyValues are the strings each escaper must carry through intact.
var nastyValues = []string{
	"o'brien@example.org",
	`back\slash`,
	`trailing\`,
	`\'`,
	`\N`,
	"two\nlines\r\n",
	"ctrl\x1az",
	"tab\there",
	`"double" quotes`,
	"`backtick`",
	"'; SELECT 'pwned",
	"; DROP DATABASE auth; --",
	"%_wildcards",
	":var :'var' :\"var\"",
	"ünïcødé ✓ 🙂",
	"",
}

func TestMySQLIntegration(t *testing.T) {
	r := requireDocker(t)
	e := docker.NewEngine(r)
	const image = "mysql:8.0"
	// The image's entrypoint puts this password into SQL itself, so keep
	// it plain; the escaping under test is in the other values.
	rootPW := "RootPw" + randomHex(t)
	name := startContainer(t, r, image, []string{"MYSQL_ROOT_PASSWORD=" + rootPW})
	root := sql.MySQLTarget{Container: name, Host: "127.0.0.1", Password: rootPW}
	waitFor(t, func() error { return sql.ExecMySQL(context.Background(), e, root, "SELECT 1") })
	ctx := context.Background()

	t.Run("literals round-trip", func(t *testing.T) {
		values := append([]string{"nul\x00byte"}, nastyValues...)
		for _, v := range values {
			rows, err := sql.QueryMySQL(ctx, e, root, "SELECT HEX("+sql.QuoteMySQL(v)+"), "+sql.QuoteMySQL(v))
			if err != nil {
				t.Fatalf("%q: %v", v, err)
			}
			if len(rows) != 1 || len(rows[0]) != 2 {
				t.Fatalf("%q: rows = %q, want one row of two columns", v, rows)
			}
			if want := strings.ToUpper(hex.EncodeToString([]byte(v))); rows[0][0] != want {
				t.Errorf("%q: server got bytes %s, want %s", v, rows[0][0], want)
			}
			if rows[0][1] != v {
				t.Errorf("%q: read back %q", v, rows[0][1])
			}
		}
	})

	t.Run("literals stay closed under NO_BACKSLASH_ESCAPES", func(t *testing.T) {
		for _, v := range nastyValues {
			rows, err := sql.QueryMySQL(ctx, e, root,
				"SET SESSION sql_mode = 'NO_BACKSLASH_ESCAPES'; SELECT 'ok', LENGTH("+sql.QuoteMySQL(v)+")")
			if err != nil {
				t.Fatalf("%q: %v", v, err)
			}
			if len(rows) != 1 || rows[0][0] != "ok" {
				t.Errorf("%q: rows = %q, want one row", v, rows)
			}
		}
	})

	t.Run("values survive a server-wide NO_BACKSLASH_ESCAPES", func(t *testing.T) {
		orig := query(t, e, root, "SELECT @@GLOBAL.sql_mode")
		mustExec(t, e, root, "SET GLOBAL sql_mode = CONCAT_WS(',', NULLIF(@@GLOBAL.sql_mode, ''), 'NO_BACKSLASH_ESCAPES')")
		t.Cleanup(func() { mustExec(t, e, root, "SET GLOBAL sql_mode = "+sql.QuoteMySQL(orig)) })
		if got := query(t, e, root, "SELECT @@SESSION.sql_mode"); strings.Contains(got, "NO_BACKSLASH_ESCAPES") {
			t.Fatalf("session sql_mode = %s, want NO_BACKSLASH_ESCAPES cleared", got)
		}
		for _, v := range nastyValues {
			got := query(t, e, root, "SELECT HEX("+sql.QuoteMySQL(v)+")")
			if want := strings.ToUpper(hex.EncodeToString([]byte(v))); got != want {
				t.Errorf("%q: server got bytes %s, want %s", v, got, want)
			}
		}
	})

	t.Run("seed is idempotent", func(t *testing.T) {
		mustExec(t, e, root,
			"CREATE DATABASE auth",
			"CREATE TABLE auth.connection (uuid binary(16) NOT NULL PRIMARY KEY, label varchar(255) NOT NULL)",
			"CREATE TABLE auth.role (uuid binary(16) NOT NULL PRIMARY KEY)",
			"CREATE TABLE auth.user (uuid binary(16) NOT NULL PRIMARY KEY, auth0_metadata longtext, general_metadata longtext, "+
				"acceptedTOS datetime, connectionId binary(16), email varchar(255), matched bit(1) NOT NULL DEFAULT FALSE, "+
				"subject varchar(255), is_active bit(1) NOT NULL DEFAULT TRUE, long_term_token varchar(4000), "+
				"FOREIGN KEY (connectionId) REFERENCES auth.connection (uuid)) DEFAULT CHARSET=utf8 COLLATE=utf8_bin",
			"CREATE TABLE auth.user_role (user_id binary(16) NOT NULL, role_id binary(16) NOT NULL, PRIMARY KEY (user_id, role_id), "+
				"FOREIGN KEY (user_id) REFERENCES auth.user (uuid), FOREIGN KEY (role_id) REFERENCES auth.role (uuid))",
			"CREATE TABLE auth.application (uuid binary(16) NOT NULL PRIMARY KEY, name varchar(255), token varchar(2000))",
			"INSERT INTO auth.connection VALUES (0x97FD002DC366B0D8420F998F885D0ED7, 'Google')",
			"INSERT INTO auth.role VALUES (0x002DC366B0D8420F998F885D0ED797FD), (0x797FD002DC366B0D8420F998F885D0ED)",
			"INSERT INTO auth.application VALUES (0x01, 'PICSURE', NULL)",
		)
		email := `o'brien"\@example.org`
		var id [16]byte
		id[0] = 1
		mustExec(t, e, root, sql.SeedAdminUser(email, id)...)
		// A replay must not restore a role revoked in between.
		mustExec(t, e, root, "DELETE FROM auth.user_role WHERE role_id = 0x002DC366B0D8420F998F885D0ED797FD")
		mustExec(t, e, root, sql.SeedAdminUser(email, id)...)
		if got := query(t, e, root, "SELECT COUNT(*) FROM auth.user_role"); got != "1" {
			t.Errorf("roles after a replay = %s, want the 1 left after the revocation", got)
		}
		mustExec(t, e, root, "INSERT INTO auth.user_role VALUES (UNHEX('01000000000000000000000000000000'), 0x002DC366B0D8420F998F885D0ED797FD)")
		id[0] = 2
		mustExec(t, e, root, sql.SeedAdminUser(email, id)...)

		got := query(t, e, root, sql.CountUsersWithEmail(email))
		if got != "1" {
			t.Errorf("users with the email = %s, want 1", got)
		}
		got = query(t, e, root, "SELECT COUNT(*) FROM auth.user_role ur JOIN auth.user u ON u.uuid = ur.user_id WHERE u.email = "+sql.QuoteMySQL(email))
		if got != "2" {
			t.Errorf("admin roles = %s, want 2", got)
		}
		got = query(t, e, root, "SELECT JSON_UNQUOTE(JSON_EXTRACT(general_metadata, '$.email')) FROM auth.user")
		if got != email {
			t.Errorf("metadata email = %q, want %q", got, email)
		}
		got = query(t, e, root, "SELECT HEX(connectionId) FROM auth.user")
		if got != "97FD002DC366B0D8420F998F885D0ED7" {
			t.Errorf("connectionId = %s, want the Google connection", got)
		}

		tok := `tok'en\` + randomHex(t)
		mustExec(t, e, root, sql.SetApplicationToken(tok))
		if got := query(t, e, root, "SELECT token FROM auth.application WHERE name = 'PICSURE'"); got != tok {
			t.Errorf("token = %q, want %q", got, tok)
		}
	})

	t.Run("applied migrations ignore baseline rows", func(t *testing.T) {
		history := "(installed_rank int NOT NULL PRIMARY KEY, version varchar(50), type varchar(20) NOT NULL, success tinyint(1) NOT NULL)"
		mustExec(t, e, root,
			"CREATE DATABASE IF NOT EXISTS auth",
			"CREATE DATABASE IF NOT EXISTS picsure",
			"CREATE TABLE auth.flyway_custom_schema_history "+history,
			"CREATE TABLE picsure.flyway_custom_schema_history "+history,
			"INSERT INTO auth.flyway_custom_schema_history VALUES (1, '1', 'BASELINE', 1), (2, '2', 'SQL', 1), (3, '3', 'SQL', 1)",
			"INSERT INTO picsure.flyway_custom_schema_history VALUES (1, '1', 'BASELINE', 1), (2, '2', 'SQL', 0)",
		)
		if got := query(t, e, root, sql.AppliedMigrationsQuery); got != "0" {
			t.Errorf("with only a baseline and a failed row: %s, want 0", got)
		}
		mustExec(t, e, root, "INSERT INTO picsure.flyway_custom_schema_history VALUES (3, '2', 'SQL', 1)")
		if got := query(t, e, root, sql.AppliedMigrationsQuery); got != "1" {
			t.Errorf("after one applied migration in each: %s, want 1", got)
		}
	})

	t.Run("bootstrap, sync and rotate over a remote target", func(t *testing.T) {
		ip := strings.TrimSpace(string(mustRun(t, r, "docker", "inspect", "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", name)))
		if ip == "" {
			t.Skip("the server has no default-bridge IP")
		}
		remote := sql.MySQLTarget{Host: ip, Port: 3306, Password: rootPW}
		first := sql.AppPasswords{Picsure: `pic'pw\` + randomHex(t), Auth: "auth-pw", Airflow: "air\"pw"}
		mustExec(t, e, remote, sql.Bootstrap(sql.AppUsers(first), false)...)
		mustExec(t, e, remote, sql.Bootstrap(sql.AppUsers(first), false)...)
		connect := func(user, pw, db string) error {
			return sql.ExecMySQL(ctx, e, sql.MySQLTarget{Host: ip, User: user, Password: pw, Database: db}, "SELECT 1")
		}
		if err := connect("picsure", first.Picsure, "picsure"); err != nil {
			t.Fatalf("picsure cannot connect after bootstrap: %v", err)
		}
		if err := connect("airflow", first.Airflow, "auth"); err != nil {
			t.Fatalf("airflow cannot connect after bootstrap: %v", err)
		}

		second := sql.AppPasswords{Picsure: `new'pic\pw`, Auth: "new-auth", Airflow: "new-air"}
		mustExec(t, e, remote, sql.Bootstrap(sql.AppUsers(second), false)...)
		if err := connect("picsure", first.Picsure, "picsure"); err != nil {
			t.Errorf("bootstrap without sync changed the password: %v", err)
		}
		mustExec(t, e, remote, sql.Bootstrap(sql.AppUsers(second), true)...)
		if err := connect("picsure", second.Picsure, "picsure"); err != nil {
			t.Errorf("picsure cannot connect after sync: %v", err)
		}

		third := `third'\pw`
		mustExec(t, e, root, sql.AlterUserPassword(sql.Account{User: "auth", Host: "%"}, third))
		if err := connect("auth", third, "auth"); err != nil {
			t.Errorf("auth cannot connect after rotation: %v", err)
		}
	})
}

func TestPostgresIntegration(t *testing.T) {
	r := requireDocker(t)
	e := docker.NewEngine(r)
	const image = "postgres:16-alpine"
	pw := "PgPw" + randomHex(t)
	name := startContainer(t, r, image, []string{"POSTGRES_USER=picsure", "POSTGRES_PASSWORD=" + pw, "POSTGRES_DB=dictionary"})
	pg := sql.PostgresTarget{Container: name, User: "picsure", Password: pw, Database: "dictionary"}
	ctx := context.Background()
	// The image's init runs a socket-only server first, so wait for the
	// one listening on TCP.
	waitFor(t, func() error {
		_, err := docker.RunChecked(ctx, r, docker.Cmd{Argv: []string{"docker", "exec", name, "pg_isready", "-h", "127.0.0.1", "-U", "picsure"}})
		return err
	})

	t.Run("literals round-trip", func(t *testing.T) {
		for _, conforming := range []string{"on", "off"} {
			for _, v := range nastyValues {
				lit, err := sql.QuotePostgres(v)
				if err != nil {
					t.Fatal(err)
				}
				// A mismatch divides by zero, which fails the run. (A bare
				// ELSE 1/0 would fail every time: the planner folds it.)
				check := fmt.Sprintf("SELECT 1 / CASE WHEN %s = convert_from(decode('%s', 'hex'), 'UTF8') THEN 1 ELSE 0 END",
					lit, hex.EncodeToString([]byte(v)))
				if err := sql.ExecPostgres(ctx, e, pg, "SET standard_conforming_strings = "+conforming, check); err != nil {
					t.Errorf("standard_conforming_strings=%s, %q: %v", conforming, v, err)
				}
			}
		}
	})

	t.Run("rotate", func(t *testing.T) {
		ip := strings.TrimSpace(string(mustRun(t, r, "docker", "inspect", "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", name)))
		if ip == "" {
			t.Skip("the server has no default-bridge IP")
		}
		newPW := `new'pg\pw` + randomHex(t)
		stmt, err := sql.AlterPostgresPassword("picsure", newPW)
		if err != nil {
			t.Fatal(err)
		}
		if err := sql.ExecPostgres(ctx, e, pg, stmt); err != nil {
			t.Fatal(err)
		}
		// Connections from inside the container are trusted, so log in
		// from another container, where the password is checked.
		login := func(pw string) error {
			_, err := docker.RunChecked(ctx, r, docker.Cmd{
				Argv: []string{"docker", "run", "--rm", "-e", "PGPASSWORD", image,
					"psql", "--no-password", "--host=" + ip, "--username=picsure", "--dbname=dictionary", "--command=SELECT 1"},
				Env: []string{"PGPASSWORD=" + pw},
			})
			return err
		}
		if err := login(newPW); err != nil {
			t.Errorf("login with the new password: %v", err)
		}
		if err := login(pw); err == nil {
			t.Error("the old password still works")
		}
	})
}

func requireDocker(t *testing.T) docker.Runner {
	t.Helper()
	r := execRunner{}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	if _, err := docker.RunChecked(context.Background(), r, docker.Cmd{Argv: []string{"docker", "info"}}); err != nil {
		t.Skipf("the Docker daemon is unavailable: %v", err)
	}
	return r
}

// startContainer runs image detached with env passed by name, and removes
// it when the test ends.
func startContainer(t *testing.T, r docker.Runner, image string, env []string) string {
	t.Helper()
	name := "picsure-sql-it-" + randomHex(t)
	argv := []string{"docker", "run", "--detach", "--name", name}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		argv = append(argv, "-e", k)
	}
	mustRunCmd(t, r, docker.Cmd{Argv: append(argv, image), Env: env})
	t.Cleanup(func() {
		_, _ = r.Run(context.Background(), docker.Cmd{Argv: []string{"docker", "rm", "--force", "--volumes", name}})
	})
	return name
}

func waitFor(t *testing.T, ready func() error) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		err := ready()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("server not ready: %v", err)
		}
		time.Sleep(time.Second)
	}
}

func mustExec(t *testing.T, e docker.Engine, target sql.MySQLTarget, statements ...string) {
	t.Helper()
	if err := sql.ExecMySQL(context.Background(), e, target, statements...); err != nil {
		t.Fatal(err)
	}
}

// query runs a query expected to return at most one value, and returns it.
func query(t *testing.T, e docker.Engine, target sql.MySQLTarget, q string) string {
	t.Helper()
	rows, err := sql.QueryMySQL(context.Background(), e, target, q)
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case len(rows) == 0:
		return ""
	case len(rows) == 1 && len(rows[0]) == 1:
		return rows[0][0]
	}
	t.Fatalf("%s: got %q, want one value", q, rows)
	return ""
}

func mustRun(t *testing.T, r docker.Runner, argv ...string) []byte {
	t.Helper()
	return mustRunCmd(t, r, docker.Cmd{Argv: argv})
}

func mustRunCmd(t *testing.T, r docker.Runner, c docker.Cmd) []byte {
	t.Helper()
	res, err := docker.RunChecked(context.Background(), r, c)
	if err != nil {
		t.Fatal(err)
	}
	return res.Stdout
}

func randomHex(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// execRunner is a bare docker.Runner over os/exec, enough for these tests.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, c docker.Cmd) (docker.Result, error) {
	var stdout, stderr bytes.Buffer
	code, err := execRunner{}.Stream(ctx, c, &stdout, &stderr)
	return docker.Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: code}, err
}

func (execRunner) Stream(ctx context.Context, c docker.Cmd, stdout, stderr io.Writer) (int, error) {
	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...)
	cmd.Env = append(os.Environ(), c.Env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = c.Stdin, stdout, stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return 0, err
}
