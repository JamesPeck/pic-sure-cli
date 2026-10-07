package ops

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/jwt"
	"github.com/JamesPeck/pic-sure-cli/internal/log"
	"github.com/JamesPeck/pic-sure-cli/internal/sql"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

// StepSeed is §9.1 step 10's ID.
const StepSeed = "seed"

// TokenRenewBefore is how long before its expiry the introspection token is
// replaced (§9.3).
const TokenRenewBefore = 30 * 24 * time.Hour

const psama = "psama"

// SeedStep is §9.1 step 10, ID "seed", for init, up and update to add after
// the migrate step. It creates the admin user (auth.admin_email) with the
// Top Admin and User roles if no user has that email, and makes
// auth.application's PICSURE token equal secrets.yaml's introspection token.
// A stored token that is valid for TokenRenewBefore or longer is written to
// the database as it is; otherwise a new one is issued (jwt.Introspection)
// and written to the database first, then to secrets.yaml, updating sec. A
// running psama is restarted after a change.
//
// Check is done when the admin user exists, the token is valid for
// TokenRenewBefore and the database holds it, so a database that lost the
// token (after `reset`) is re-seeded. The migrations must have run: a
// database without them fails with exit 3.
//
// A changed token changes render.ComposeEnv(cfg, sec): a Composer whose env
// was computed before the step must recompute it.
func SeedStep(d *Deps, st *stack.Stack, cfg *stack.Config, sec *stack.Secrets) steps.Step {
	log.RegisterSecrets(cfg.Auth.AdminEmail)
	s := &seedStep{d: d, st: st, cfg: cfg, sec: sec}
	return steps.Step{ID: StepSeed, Title: "Seed the database", Check: s.check, Apply: s.apply}
}

type seedStep struct {
	d   *Deps
	st  *stack.Stack
	cfg *stack.Config
	sec *stack.Secrets
}

func (s *seedStep) check(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	t, err := s.target(ctx)
	if err != nil || t == nil {
		return false, err
	}
	if ok, err := migrated(ctx, s.d, *t); err != nil || !ok {
		return false, err
	}
	if !s.tokenValid() {
		return false, nil
	}
	if s.cfg.Auth.AdminEmail != "" {
		if n, err := countUsers(ctx, s.d, *t, s.cfg.Auth.AdminEmail); err != nil || n == 0 {
			return false, err
		}
	}
	stored, err := storedToken(ctx, s.d, *t)
	return err == nil && stored == string(s.sec.IntrospectionToken), err
}

func (s *seedStep) apply(ctx context.Context, sink events.Sink) error {
	t, err := s.target(ctx)
	if err != nil {
		return err
	}
	if t == nil {
		return exitcode.Precondition("%s is not running; run `pic-sure up`", picsureDB)
	}
	ok, err := migrated(ctx, s.d, *t)
	if err != nil {
		return err
	}
	if !ok {
		return exitcode.Precondition("the database migrations haven't been applied, so there is nothing to seed: " +
			"run `pic-sure migrate`. If a migration failed part-way, run `pic-sure migrate --repair`, then `pic-sure migrate`")
	}
	changed := false

	if email := s.cfg.Auth.AdminEmail; email != "" {
		n, err := countUsers(ctx, s.d, *t, email)
		if err != nil {
			return err
		}
		if n == 0 {
			var id [16]byte
			if _, err := s.d.Rand.Read(id[:]); err != nil {
				return fmt.Errorf("generating the admin user's id: %w", err)
			}
			if err := sql.ExecMySQL(ctx, s.d.Docker, *t, sql.SeedAdminUser(email, id)...); err != nil {
				return fmt.Errorf("creating the admin user: %w", err)
			}
			sink.Emit(events.Progress{ID: StepSeed, Text: "created the admin user with the Top Admin and User roles"})
			changed = true
		}
	}

	token, expiry := string(s.sec.IntrospectionToken), s.sec.IntrospectionTokenExpiry
	if !s.tokenValid() {
		token, expiry, err = jwt.Introspection(string(s.sec.Auth0ClientSecret), s.sec.ApplicationUUID, s.d.Clock.Now(), jwt.DefaultTTL)
		if err != nil {
			return exitcode.Precondition("issuing the introspection token: %w", err)
		}
		log.RegisterSecrets(token)
	}
	stored, err := storedToken(ctx, s.d, *t)
	if err != nil {
		return err
	}
	if stored != token {
		if err := sql.ExecMySQL(ctx, s.d.Docker, *t, sql.SetApplicationToken(token)); err != nil {
			return fmt.Errorf("storing the introspection token in auth.application: %w", err)
		}
		sink.Emit(events.Progress{ID: StepSeed, Text: "stored the introspection token in the database"})
		changed = true
	}
	// The database is written first: if saving fails, the next run still
	// finds secrets.yaml's token missing or expiring and issues another.
	if token != string(s.sec.IntrospectionToken) {
		old := *s.sec
		s.sec.IntrospectionToken, s.sec.IntrospectionTokenExpiry = stack.Secret(token), expiry
		if err := s.st.SaveSecrets(s.sec); err != nil {
			*s.sec = old
			return fmt.Errorf("saving the introspection token to secrets.yaml (the database already has it; re-run to issue another): %w", err)
		}
		sink.Emit(events.Progress{ID: StepSeed, Text: "issued an introspection token valid until " + expiry.Format(time.DateOnly)})
	}

	if changed {
		restartPsama(ctx, s.d, sink)
	}
	return nil
}

// tokenValid reports whether secrets.yaml's token is valid for at least
// TokenRenewBefore.
func (s *seedStep) tokenValid() bool {
	return s.sec.IntrospectionToken != "" && s.sec.IntrospectionTokenExpiry.After(s.d.Clock.Now().Add(TokenRenewBefore))
}

// target is root on the stack's MySQL, or nil when the local picsure-db
// isn't running.
func (s *seedStep) target(ctx context.Context) (*sql.MySQLTarget, error) {
	if s.cfg.DB.Mode == stack.DBRemote {
		t := mysqlTarget(s.cfg, s.sec, "")
		return &t, nil
	}
	svc, err := composeService(ctx, s.d, picsureDB)
	if err != nil || svc == nil || svc.State != "running" {
		return nil, err
	}
	t := mysqlTarget(s.cfg, s.sec, svc.ID)
	return &t, nil
}

// migrated reports whether both custom Flyway passes have applied a
// migration. AppliedMigrationsQuery fails on a missing history table, so
// the tables are looked up first.
func migrated(ctx context.Context, d *Deps, t sql.MySQLTarget) (bool, error) {
	n, err := queryCount(ctx, d, t, "SELECT COUNT(*) FROM information_schema.tables "+
		"WHERE table_schema IN ('auth', 'picsure') AND table_name = 'flyway_custom_schema_history'")
	if err != nil || n < 2 {
		return false, err
	}
	n, err = queryCount(ctx, d, t, sql.AppliedMigrationsQuery)
	return n > 0, err
}

func countUsers(ctx context.Context, d *Deps, t sql.MySQLTarget, email string) (int, error) {
	n, err := queryCount(ctx, d, t, sql.CountUsersWithEmail(email))
	if err != nil {
		return 0, fmt.Errorf("looking up the admin user: %w", err)
	}
	return n, nil
}

func queryCount(ctx context.Context, d *Deps, t sql.MySQLTarget, query string) (int, error) {
	rows, err := sql.QueryMySQL(ctx, d.Docker, t, query)
	if err != nil {
		return 0, err
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		return 0, fmt.Errorf("expected one count, got %d rows", len(rows))
	}
	return strconv.Atoi(rows[0][0])
}

// storedToken returns auth.application's PICSURE token; it fails when there
// is no PICSURE row, since an UPDATE couldn't store one.
func storedToken(ctx context.Context, d *Deps, t sql.MySQLTarget) (string, error) {
	rows, err := sql.QueryMySQL(ctx, d.Docker, t, "SELECT COALESCE(token, '') FROM auth.application WHERE name = 'PICSURE'")
	if err != nil {
		return "", fmt.Errorf("reading the introspection token from auth.application: %w", err)
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		return "", fmt.Errorf("auth.application has %d PICSURE rows, want 1; the migrations should have created it", len(rows))
	}
	return rows[0][0], nil
}

// restartPsama restarts psama if it is running, so it rereads the users and
// the token. A failure is a warning: Check would skip a re-run.
func restartPsama(ctx context.Context, d *Deps, sink events.Sink) {
	svc, err := composeService(ctx, d, psama)
	if err == nil && (svc == nil || svc.State != "running") {
		return
	}
	if err == nil {
		sink.Emit(events.Progress{ID: StepSeed, Text: "restarting psama to pick up the seeded data"})
		out := events.NewLogWriter(sink, StepSeed, events.StreamStderr)
		err = d.Compose.Restart(ctx, out, psama)
		_ = out.Close()
	}
	if err != nil && ctx.Err() == nil {
		sink.Emit(events.Warning{ID: StepSeed, Text: "the database was seeded, but restarting psama failed: " + err.Error() +
			"; run `pic-sure restart psama`"})
	}
}
