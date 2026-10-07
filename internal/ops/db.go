package ops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/sql"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

// StepDBBootstrap is the ID of the step that prepares a remote database.
const StepDBBootstrap = "db-bootstrap"

// BootstrapOptions configures the bootstrap step.
type BootstrapOptions struct {
	// SyncPasswords sets each application user's password to the one in
	// secrets.yaml. Without it, a user that already exists keeps its
	// password, and a mismatch fails the step.
	SyncPasswords bool
}

// BootstrapReport is `db bootstrap --check`: whether the remote server has
// the databases, users and grants the stack needs, and whether each user
// logs in with its password from secrets.yaml.
type BootstrapReport struct {
	OK bool `json:"ok"`
	// Server is the remote database's host:port.
	Server string `json:"server"`
	// Version is the server's version.
	Version string           `json:"version"`
	Checks  []BootstrapCheck `json:"checks"`
}

// BootstrapCheck is one thing the stack needs on the remote server.
type BootstrapCheck struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Problem string `json:"problem,omitempty"`
}

// DBSteps is §9.1 step 8: the db step, then, for a remote database, the
// bootstrap step. Init, up, update and migrate put it before the migrate
// step.
func DBSteps(d *Deps, cfg *stack.Config, sec *stack.Secrets, opts DBOptions) []steps.Step {
	plan := []steps.Step{DBStep(d, cfg, sec, opts)}
	if cfg.DB.Mode == stack.DBRemote {
		plan = append(plan, BootstrapStep(d, cfg, sec, BootstrapOptions{}))
	}
	return plan
}

// Bootstrap is the `db bootstrap` command: probe the remote database, then
// bootstrap it unless CheckBootstrap finds everything in place.
func Bootstrap(ctx context.Context, d *Deps, cfg *stack.Config, sec *stack.Secrets, opts BootstrapOptions, skip []string) error {
	plan := []steps.Step{DBStep(d, cfg, sec, DBOptions{}), BootstrapStep(d, cfg, sec, opts)}
	return steps.Run(ctx, d.Sink, plan, steps.Options{Skip: skip})
}

// BootstrapStep prepares a remote database, ID "db-bootstrap": as the
// remote root user, it creates the auth and picsure databases and the
// picsure, auth and airflow users from secrets.yaml, and grants each user
// all privileges on its databases, as AIO's bootstrap-remote-db.sh does.
// What exists is left alone, so a user keeps its password unless
// opts.SyncPasswords is set. Afterwards every user must log in with its
// password from secrets.yaml; a mismatch is exit 3, suggesting
// --sync-passwords. Check is done when CheckBootstrap finds nothing
// missing.
func BootstrapStep(d *Deps, cfg *stack.Config, sec *stack.Secrets, opts BootstrapOptions) steps.Step {
	return steps.Step{
		ID:    StepDBBootstrap,
		Title: "Prepare the remote database",
		Check: func(ctx context.Context) (bool, error) {
			ctx, cancel := context.WithTimeout(ctx, checkTimeout)
			defer cancel()
			r, err := CheckBootstrap(ctx, d, cfg, sec)
			return err == nil && r.OK, nil
		},
		Apply: func(ctx context.Context, sink events.Sink) error {
			if err := requireRemoteSecrets(sec); err != nil {
				return err
			}
			stmts := sql.Bootstrap(sql.AppUsers(appPasswords(sec)), opts.SyncPasswords)
			if err := sql.ExecMySQL(ctx, d.Docker, mysqlTarget(cfg, sec, ""), stmts...); err != nil {
				return remoteDBError(cfg, "bootstrapping", err)
			}
			r, err := CheckBootstrap(ctx, d, cfg, sec)
			if err != nil {
				return err
			}
			if r.OK {
				return nil
			}
			var problems []string
			for _, c := range r.Checks {
				if !c.OK {
					problems = append(problems, c.Name+": "+c.Problem)
				}
			}
			msg := "the remote database is still not ready after bootstrap: " + strings.Join(problems, "; ")
			if r.NeedsSync() {
				return exitcode.Precondition("%s. Run `pic-sure db bootstrap --sync-passwords` "+
					"to set the users' passwords to the ones in secrets.yaml", msg)
			}
			return errors.New(msg)
		},
	}
}

// problemPassword is a user's login check failing on its password.
const problemPassword = "its password doesn't match secrets.yaml"

// NeedsSync reports whether a user's password differs from secrets.yaml,
// which --sync-passwords fixes.
func (r *BootstrapReport) NeedsSync() bool {
	for _, c := range r.Checks {
		if c.Problem == problemPassword {
			return true
		}
	}
	return false
}

// requireRemoteSecrets refuses secrets without a password Bootstrap needs:
// an empty one would create, or with --sync-passwords leave, an account
// that logs in without a password.
func requireRemoteSecrets(sec *stack.Secrets) error {
	var missing []string
	for _, s := range []struct {
		name string
		v    stack.Secret
	}{
		{"db_remote_root_password", sec.DBRemoteRootPassword},
		{"db_picsure_password", sec.DBPicsurePassword},
		{"db_auth_password", sec.DBAuthPassword},
		{"db_airflow_password", sec.DBAirflowPassword},
	} {
		if s.v == "" {
			missing = append(missing, s.name)
		}
	}
	if missing != nil {
		return exitcode.Precondition("secrets.yaml lacks %s, which the remote database needs", strings.Join(missing, ", "))
	}
	return nil
}

// CheckBootstrap reports what the remote database has of what BootstrapStep
// creates, and whether each existing user logs in with its password from
// secrets.yaml and reaches its databases. It changes nothing. An error
// means it couldn't read the server as the root user.
func CheckBootstrap(ctx context.Context, d *Deps, cfg *stack.Config, sec *stack.Secrets) (*BootstrapReport, error) {
	if err := requireRemoteSecrets(sec); err != nil {
		return nil, err
	}
	users := sql.AppUsers(appPasswords(sec))
	root := mysqlTarget(cfg, sec, "")
	r := &BootstrapReport{Server: net.JoinHostPort(cfg.DB.Remote.Host, strconv.Itoa(cfg.DB.Remote.Port)),
		Checks: []BootstrapCheck{}}
	rows, err := sql.QueryMySQL(ctx, d.Docker, root, sql.BootstrapStateQuery(users))
	if err != nil {
		return nil, remoteDBError(cfg, "reading", err)
	}
	have := map[string]bool{}
	others := map[string][]string{} // user → its accounts other than user@'%'
	for _, row := range rows {
		if len(row) != 2 {
			return nil, fmt.Errorf("reading the remote database: unexpected row %q", row)
		}
		switch row[0] {
		case "version":
			r.Version = row[1]
		case "account":
			user, host, _ := strings.Cut(row[1], " ")
			others[user] = append(others[user], "'"+user+"'@'"+host+"'")
		}
		have[row[0]+" "+row[1]] = true
	}
	add := func(name, problem string) {
		r.Checks = append(r.Checks, BootstrapCheck{Name: name, OK: problem == "", Problem: problem})
	}
	for _, db := range sql.Databases(users) {
		problem := ""
		if !have["database "+db] {
			problem = "missing"
		}
		add("database "+db, problem)
	}
	for _, u := range users {
		if !have["user "+u.Name] {
			add("user "+u.Name, "missing")
			continue
		}
		add("user "+u.Name, "")
		for _, db := range u.Databases {
			problem := ""
			if !have["grant "+u.Name+" "+db] {
				problem = "missing ALL PRIVILEGES"
			}
			add("grants "+u.Name+" on "+db, problem)
		}
		problem := loginProblem(ctx, d, cfg, u)
		// The server matches the most specific host first, so another
		// account can refuse the login even when user@'%' has the right
		// password.
		if problem == problemPassword && len(others[u.Name]) > 0 {
			problem = "access denied; check the password of " + strings.Join(others[u.Name], ", ") +
				", which the server may match before '" + u.Name + "'@'%'"
		}
		add("login "+u.Name, problem)
	}
	r.OK = true
	for _, c := range r.Checks {
		r.OK = r.OK && c.OK
	}
	return r, nil
}

// loginProblem logs in as u with its password and selects each of its
// databases, returning "" when that works.
func loginProblem(ctx context.Context, d *Deps, cfg *stack.Config, u sql.AppUser) string {
	t := sql.MySQLTarget{Host: cfg.DB.Remote.Host, Port: cfg.DB.Remote.Port, User: u.Name, Password: u.Password}
	var stmts []string
	for _, db := range u.Databases {
		stmts = append(stmts, sql.UseDatabase(db))
	}
	err := sql.ExecMySQL(ctx, d.Docker, t, stmts...)
	var xe *docker.ExitError
	switch {
	case err == nil:
		return ""
	case accessDenied(err):
		return problemPassword
	case errors.As(err, &xe) && bytes.Contains(xe.Stderr, []byte("ERROR 1044")):
		return "it can't use all of " + strings.Join(u.Databases, ", ")
	case errors.As(err, &xe):
		return "login failed: " + firstLine(xe.Stderr)
	default:
		return "login failed: " + err.Error()
	}
}

func firstLine(b []byte) string {
	s, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	return s
}

func appPasswords(sec *stack.Secrets) sql.AppPasswords {
	return sql.AppPasswords{Picsure: string(sec.DBPicsurePassword), Auth: string(sec.DBAuthPassword),
		Airflow: string(sec.DBAirflowPassword)}
}

// remoteDBError wraps a failure to reach the remote server as root, adding
// LoopbackHint; refused credentials are exit 3.
func remoteDBError(cfg *stack.Config, doing string, err error) error {
	server := net.JoinHostPort(cfg.DB.Remote.Host, strconv.Itoa(cfg.DB.Remote.Port))
	err = fmt.Errorf("%s the remote database at %s as %s: %w%s", doing, server, cfg.DB.Remote.RootUser, err,
		LoopbackHint(cfg.DB.Remote.Host))
	if accessDenied(err) {
		return exitcode.Precondition("%w", err)
	}
	return err
}

// LoopbackHint explains, for a loopback db.remote.host, that the stack's
// containers can't reach the Docker host's database that way. It is "" for
// any other host.
func LoopbackHint(host string) string {
	ip := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback() && !ip.IsUnspecified()) {
		return ""
	}
	return ". db.remote.host " + host + " is the container itself when a container connects to it; " +
		"for a database on the Docker host, use host.docker.internal (Docker Desktop, OrbStack) " +
		"or the host's address on the Docker network"
}
