package sql

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
)

// MySQLClientImage runs the mysql client for a remote server.
const MySQLClientImage = "mysql:8.0"

// MySQLTarget says where the mysql client runs and how it connects.
type MySQLTarget struct {
	// Container is the ID of the stack's picsure-db container. When it is
	// set, the client runs inside that container with `docker exec`.
	// Otherwise the client runs in a throwaway MySQLClientImage container
	// (`docker run --rm`) and connects to Host.
	Container string
	// Host is the server to connect to, and a remote target needs it. On a
	// local target, setting it (to 127.0.0.1, say) makes the client connect
	// over TCP instead of the socket.
	Host string
	// Port is the server's port; 0 leaves the client's default, 3306.
	Port int
	// User is the account to connect as; empty means root.
	User string
	// Password reaches the client as MYSQL_PWD in its environment, never in
	// argv. Empty means no password.
	Password string
	// Database is the default database; empty means none.
	Database string
}

// ExecMySQL runs statements in order in one mysql client session and stops
// at the first one that fails. The statements go to the client on stdin, so
// the values escaped into them never appear in argv. A trailing semicolon
// on a statement is optional. With no statements it runs nothing.
//
// A client that exits non-zero gives a *docker.ExitError for the client's
// own argv, carrying its stderr, such as
// "ERROR 1045 (28000): Access denied ...". When docker itself fails, the
// error is the engine's.
func ExecMySQL(ctx context.Context, e docker.Engine, t MySQLTarget, statements ...string) error {
	_, err := runMySQL(ctx, e, t, statements)
	return err
}

// QueryMySQL runs query like ExecMySQL and returns the rows it prints, each
// a slice of column values. A NULL reads as "NULL".
func QueryMySQL(ctx context.Context, e docker.Engine, t MySQLTarget, query string) ([][]string, error) {
	out, err := runMySQL(ctx, e, t, []string{query})
	if err != nil {
		return nil, err
	}
	return parseBatch(out), nil
}

func runMySQL(ctx context.Context, e docker.Engine, t MySQLTarget, statements []string) ([]byte, error) {
	in := script(statements)
	if in == "" {
		return nil, nil
	}
	if t.Container == "" && t.Host == "" {
		return nil, errors.New("sql: a MySQL target needs a container or a host")
	}
	// --batch prints rows tab-separated, one per line, and stops at the
	// first error. utf8mb4 is what QuoteMySQL's escaping assumes.
	args := []string{"mysql", "--batch", "--skip-column-names", "--default-character-set=utf8mb4"}
	if t.Host != "" {
		args = append(args, "--host="+t.Host)
	}
	if t.Port != 0 {
		args = append(args, "--port="+strconv.Itoa(t.Port))
	}
	user := t.User
	if user == "" {
		user = "root"
	}
	args = append(args, "--user="+user)
	if t.Database != "" {
		args = append(args, "--database="+t.Database)
	}
	var env []string
	if t.Password != "" {
		env = []string{"MYSQL_PWD=" + t.Password}
	}
	var stdout, stderr bytes.Buffer
	var code int
	var err error
	if t.Container != "" {
		code, err = e.Exec(ctx, docker.ExecOpts{Container: t.Container, Args: args, Env: env,
			Stdin: strings.NewReader(in), Stdout: &stdout, Stderr: &stderr})
	} else {
		code, err = e.Run(ctx, docker.RunOpts{Image: MySQLClientImage, Remove: true, Args: args, Env: env,
			Stdin: strings.NewReader(in), Stdout: &stdout, Stderr: &stderr})
	}
	return stdout.Bytes(), clientError(args, code, stderr.Bytes(), err)
}

// clientError is the error for a client that ran with the engine's result:
// the engine's own error when docker failed, otherwise an *docker.ExitError
// naming the client for a non-zero exit.
func clientError(args []string, code int, stderr []byte, err error) error {
	if err != nil {
		return err
	}
	if code != 0 {
		return &docker.ExitError{Argv: args, ExitCode: code, Stderr: stderr}
	}
	return nil
}

// PostgresTarget says which Postgres container the psql client runs in and
// how it connects. The dictionary DB is always local to the stack.
type PostgresTarget struct {
	// Container is the ID of the stack's dictionary-db container; the
	// client runs inside it with `docker exec`.
	Container string
	// User is the role to connect as.
	User string
	// Password reaches the client as PGPASSWORD in its environment, never
	// in argv. Empty means no password.
	Password string
	// Database is the database to connect to; empty means the one named
	// after User.
	Database string
}

// ExecPostgres runs statements in order in one psql session and stops at
// the first one that fails. The statements go to psql on stdin, so the
// values escaped into them never appear in argv. A trailing semicolon on a
// statement is optional. With no statements it runs nothing.
func ExecPostgres(ctx context.Context, e docker.Engine, t PostgresTarget, statements ...string) error {
	in := script(statements)
	if in == "" {
		return nil
	}
	if t.Container == "" || t.User == "" {
		return errors.New("sql: a Postgres target needs a container and a user")
	}
	// UTF8 is a client encoding in which no multibyte character contains a
	// quote or backslash byte, which QuotePostgres relies on.
	env := []string{"PGCLIENTENCODING=UTF8"}
	if t.Password != "" {
		env = append(env, "PGPASSWORD="+t.Password)
	}
	// ON_ERROR_STOP makes psql stop and exit non-zero at the first error;
	// without it psql runs on and exits 0.
	args := []string{"psql", "--no-psqlrc", "--no-password", "--quiet", "--set=ON_ERROR_STOP=1",
		"--username=" + t.User}
	if t.Database != "" {
		args = append(args, "--dbname="+t.Database)
	}
	var stderr bytes.Buffer
	code, err := e.Exec(ctx, docker.ExecOpts{Container: t.Container, Args: args, Env: env,
		Stdin: strings.NewReader(in), Stderr: &stderr})
	return clientError(args, code, stderr.Bytes(), err)
}

// script joins statements into client input, one per line, each ending in
// a semicolon. Empty statements are dropped.
func script(statements []string) string {
	var b strings.Builder
	for _, s := range statements {
		s = strings.TrimSuffix(strings.TrimSpace(s), ";")
		if strings.TrimSpace(s) == "" {
			continue
		}
		b.WriteString(s)
		b.WriteString(";\n")
	}
	return b.String()
}

// parseBatch splits the mysql client's --batch output into rows of columns,
// undoing the escapes it writes for NUL, tab, newline and backslash.
func parseBatch(out []byte) [][]string {
	if len(out) == 0 {
		return nil
	}
	out = bytes.TrimSuffix(out, []byte("\n"))
	var rows [][]string
	for _, line := range strings.Split(string(out), "\n") {
		cols := strings.Split(line, "\t")
		for i, c := range cols {
			cols[i] = unescapeBatch(c)
		}
		rows = append(rows, cols)
	}
	return rows
}

var batchUnescaper = strings.NewReplacer(`\\`, `\`, `\0`, "\x00", `\t`, "\t", `\n`, "\n")

func unescapeBatch(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	return batchUnescaper.Replace(s)
}
