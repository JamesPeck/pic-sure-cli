package ops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/jwt"
	"github.com/JamesPeck/pic-sure-cli/internal/log"
	"github.com/JamesPeck/pic-sure-cli/internal/sql"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

// The secrets `secrets rotate` takes (§9.11).
const (
	SecretDBRoot             = "db-root"
	SecretDBPicsure          = "db-picsure"
	SecretDBAuth             = "db-auth"
	SecretDBAirflow          = "db-airflow"
	SecretDictionaryDB       = "dictionary-db"
	SecretIntrospectionToken = "introspection-token"
	SecretQueryServiceToken  = "query-service-token"
	SecretApplicationToken   = "application-token"
	SecretLoggingKey         = "logging-key"
	SecretObfuscationSalt    = "obfuscation-salt"
	SecretHPDSKey            = "hpds-key"
	SecretAuth0ClientSecret  = "auth0-client-secret"
)

// RotateNames lists the secrets `secrets rotate` takes.
func RotateNames() []string {
	return []string{
		SecretDBRoot, SecretDBPicsure, SecretDBAuth, SecretDBAirflow, SecretDictionaryDB,
		SecretIntrospectionToken, SecretQueryServiceToken, SecretApplicationToken, SecretLoggingKey, SecretObfuscationSalt,
		SecretHPDSKey, SecretAuth0ClientSecret,
	}
}

// RotateReadsStdin reports whether name's new value is the operator's,
// read from stdin, rather than generated: the Auth0 client secret, and with
// a remote database the root password, which belongs to its DBA.
func RotateReadsStdin(cfg *stack.Config, name string) bool {
	return name == SecretAuth0ClientSecret || name == SecretDBRoot && cfg.DB.Mode == stack.DBRemote
}

// Step IDs of a rotation.
const (
	RotateSaveStepID    = "rotate-save"
	RotateRestartStepID = "rotate-restart"
	RotateHPDSDataID    = "hpds-data"
	RotateHPDSStopID    = "hpds-stop"
	RotateHPDSWipeID    = "hpds-wipe"
	RotateHPDSStartID   = "hpds-start"
)

// RotateOptions configures RotateSecret.
type RotateOptions struct {
	// Name is one of RotateNames.
	Name string
	// Value is the new secret for a name RotateReadsStdin; it must be
	// empty for the others, which are generated.
	Value stack.Secret
	// DiscardData lets hpds-key delete the HPDS data, which the new key
	// can't read. The command sets it only with --discard-data --yes.
	DiscardData bool
}

// Format prints o with the secret redacted, as stack.Secrets.Format does.
func (o RotateOptions) Format(f fmt.State, _ rune) {
	type noFormat RotateOptions
	_, _ = fmt.Fprintf(f, "%+v", noFormat(o))
}

// RotateReport is what a rotation did.
type RotateReport struct {
	Stack  string `json:"stack"`
	Secret string `json:"secret"`
	// Restarted are the services recreated or restarted with the new value.
	Restarted []string `json:"restarted"`
	// DiscardedData is set when hpds-key deleted the HPDS data.
	DiscardedData bool `json:"discarded_data,omitempty"`
	// TokenExpiry is the new introspection token's expiry, when one was
	// issued.
	TokenExpiry *time.Time `json:"introspection_token_expiry,omitempty"`
}

// RotateSecret replaces one secret everywhere it is used (§9.11). The
// database changes first (ALTER USER, or auth.application's token), then
// secrets.yaml, then the running services whose rendered compose config
// references the secret's variable are recreated with `compose up -d
// --wait`, which a plain restart wouldn't do: compose passes the new value
// only to a new container. The render holds no secret values (§6.4), so it
// isn't redone. The database change and the save run as one step that a
// signal doesn't cut short. If saving secrets.yaml fails, the database
// change is undone, so the two never disagree; a failed database change
// leaves both as they were.
//
// The caller holds the stack lock and sets d.Compose to a Composer whose
// env is computed on each call from sec, which is updated in place once
// secrets.yaml is saved. A database secret needs its database running.
//
// hpds-key deletes the HPDS data the old key encrypted, refusing with exit
// 4 unless DiscardData when there is any, then writes a new key file,
// copies it into the hpds-data volume and restarts hpds if it was running.
func RotateSecret(ctx context.Context, d *Deps, st *stack.Stack, cfg *stack.Config, sec *stack.Secrets, opts RotateOptions) (*RotateReport, error) {
	r := &rotator{d: d, st: st, cfg: cfg, sec: sec, opts: opts,
		report: &RotateReport{Stack: cfg.Name, Secret: opts.Name, Restarted: []string{}}}
	if err := CheckRotateOptions(cfg, opts); err != nil {
		return nil, err
	}
	var plan []steps.Step
	if opts.Name == SecretHPDSKey {
		plan = r.hpdsPlan()
	} else {
		if err := r.prepare(); err != nil {
			return nil, err
		}
		plan = r.plan()
	}
	if err := steps.Run(ctx, d.Sink, plan, steps.Options{}); err != nil {
		return nil, r.runError(err)
	}
	slices.Sort(r.report.Restarted)
	return r.report, nil
}

// CheckRotateOptions checks opts before anything changes: a value for
// exactly the names read from stdin, and an Auth0 client secret PSAMA can
// sign with.
func CheckRotateOptions(cfg *stack.Config, opts RotateOptions) error {
	switch {
	case RotateReadsStdin(cfg, opts.Name) && opts.Value == "":
		return exitcode.Usage("%s's new value must be given on stdin", opts.Name)
	case !RotateReadsStdin(cfg, opts.Name) && opts.Value != "":
		return exitcode.Usage("%s is generated, not read from stdin", opts.Name)
	case opts.Name == SecretAuth0ClientSecret && len(opts.Value) < jwt.MinSecretLen:
		return exitcode.Usage("the Auth0 client secret must be at least %d bytes; PSAMA can't sign with a shorter one", jwt.MinSecretLen)
	}
	return nil
}

// runError replaces the step engine's advice to re-run the command, which
// for a rotation would rotate again: a failed step says what to do itself,
// and an interrupted run says whether the new secret was saved.
func (r *rotator) runError(err error) error {
	var se *steps.Error
	if !errors.As(err, &se) {
		return err
	}
	if !se.Interrupted {
		return se.Err
	}
	var hint string
	switch {
	case r.saved && r.opts.Name == SecretHPDSKey:
		hint = "the new key is saved; run `pic-sure up` to install it and start hpds"
	case r.saved:
		hint = "the new secret is saved; run `pic-sure up` to restart the services that use it"
	case r.report.DiscardedData:
		hint = "the HPDS data was deleted but the key wasn't replaced; run the command again to finish"
	case r.hpdsWasRunning:
		hint = "the key wasn't replaced; run `pic-sure up` to start hpds again"
	default:
		hint = "nothing was changed"
	}
	return &rotateInterrupted{err: se, hint: hint}
}

// rotateInterrupted is an interrupted rotation. It unwraps to the
// *steps.Error, so the exit code still comes from the signal.
type rotateInterrupted struct {
	err  *steps.Error
	hint string
}

func (e *rotateInterrupted) Error() string {
	return fmt.Sprintf("stopped at step %s: %v; %s", e.err.Step, e.err.Err, e.hint)
}

func (e *rotateInterrupted) Unwrap() error { return e.err }

type rotator struct {
	d      *Deps
	st     *stack.Stack
	cfg    *stack.Config
	sec    *stack.Secrets
	opts   RotateOptions
	report *RotateReport

	// next is sec with the new value.
	next *stack.Secrets
	// vars are the compose variables holding the secret, whose consumers
	// are recreated.
	vars []string
	// extra are services to restart that compose doesn't show using the
	// secret: psama reads the token from the database.
	extra []string
	// change makes the database change from sec to next, and returns how
	// to undo it. Nil for a secret only the services read.
	change func(ctx context.Context) (undo func(context.Context) error, err error)
	undo   func(context.Context) error
	// saved is set once secrets.yaml holds the new value.
	saved bool

	hpdsVolume     string
	hpdsData       bool
	hpdsWasRunning bool
}

// prepare works out the new value, where it goes and who reads it.
func (r *rotator) prepare() error {
	next := *r.sec
	r.next = &next
	var err error
	gen := func(dst *stack.Secret, g func() (stack.Secret, error)) {
		if err == nil {
			*dst, err = g()
		}
	}
	password := func() (stack.Secret, error) { return stack.GeneratePassword(r.d.Rand) }
	token := func(n int) func() (stack.Secret, error) {
		return func() (stack.Secret, error) { return stack.GenerateHexToken(r.d.Rand, n) }
	}
	switch r.opts.Name {
	case SecretDBRoot:
		r.vars = []string{"DB_ROOT_PASSWORD"}
		if r.cfg.DB.Mode == stack.DBRemote {
			next.DBRemoteRootPassword = r.opts.Value
			r.change = r.checkRemoteRoot
		} else {
			gen(&next.DBRootPassword, password)
			r.change = r.alterRoot
		}
	case SecretDBPicsure:
		gen(&next.DBPicsurePassword, password)
		r.vars, r.change = []string{"DB_PICSURE_PASSWORD"}, r.alterAppUser("picsure", func(s *stack.Secrets) stack.Secret { return s.DBPicsurePassword })
	case SecretDBAuth:
		gen(&next.DBAuthPassword, password)
		r.vars, r.change = []string{"DB_AUTH_PASSWORD"}, r.alterAppUser("auth", func(s *stack.Secrets) stack.Secret { return s.DBAuthPassword })
	case SecretDBAirflow:
		gen(&next.DBAirflowPassword, password)
		r.vars, r.change = []string{"DB_AIRFLOW_PASSWORD"}, r.alterAppUser("airflow", func(s *stack.Secrets) stack.Secret { return s.DBAirflowPassword })
	case SecretDictionaryDB:
		gen(&next.DictionaryDBPassword, password)
		r.vars, r.change = []string{"DB_DICTIONARY_PASSWORD"}, r.alterDictionary
	case SecretQueryServiceToken:
		gen(&next.QueryServiceInternalToken, token(32))
		r.vars = []string{"QUERY_SERVICE_INTERNAL_TOKEN"}
	case SecretApplicationToken:
		gen(&next.PicsureApplicationToken, token(32))
		r.vars = []string{"PICSURE_APPLICATION_TOKEN"}
	case SecretLoggingKey:
		gen(&next.LoggingAPIKey, token(32))
		r.vars = []string{"LOGGING_API_KEY"}
	case SecretObfuscationSalt:
		gen(&next.AggregateObfuscationSalt, token(16))
		r.vars = []string{"AGGREGATE_OBFUSCATION_SALT"}
	case SecretIntrospectionToken:
		err = r.mint()
	case SecretAuth0ClientSecret:
		next.Auth0ClientSecret, next.Auth0ClientSecretGenerated = r.opts.Value, false
		err = r.mint()
		r.vars = append(r.vars, "AUTH0_CLIENT_SECRET")
	default:
		return exitcode.Usage("unknown secret %q; secrets rotate takes %s", r.opts.Name, strings.Join(RotateNames(), ", "))
	}
	return err
}

// mint issues a new introspection token, signed with next's client secret,
// for auth.application first.
func (r *rotator) mint() error {
	if r.next.Auth0ClientSecret == "" {
		return exitcode.Precondition("secrets.yaml has no Auth0 client secret to sign the introspection token with; " +
			"pipe one to `pic-sure secrets rotate auth0-client-secret`")
	}
	token, expiry, err := jwt.Introspection(string(r.next.Auth0ClientSecret), r.next.ApplicationUUID, r.d.Clock.Now(), jwt.DefaultTTL)
	if err != nil {
		return exitcode.Precondition("issuing the introspection token: %w", err)
	}
	log.RegisterSecrets(token)
	r.next.IntrospectionToken, r.next.IntrospectionTokenExpiry = stack.Secret(token), expiry
	r.report.TokenExpiry = &expiry
	r.vars = append(r.vars, "PICSURE_INTROSPECTION_TOKEN")
	r.extra = []string{psama}
	r.change = r.setToken
	return nil
}

// rotateSaveTimeout bounds the database change and the save, which a
// signal doesn't cancel.
const rotateSaveTimeout = 2 * time.Minute

// plan is the rotation of a secret in secrets.yaml.
func (r *rotator) plan() []steps.Step {
	title := "Save the new secret to secrets.yaml"
	if r.change != nil {
		title = "Change the secret in the database, then in secrets.yaml"
	}
	return []steps.Step{
		{ID: RotateSaveStepID, Title: title, Apply: r.save},
		{ID: RotateRestartStepID, Title: "Restart the services that use it", Apply: r.restart},
	}
}

// save changes the database, then saves secrets.yaml. Once the database
// may have changed, an interruption would leave the new value only in
// memory, so the step runs to the end whatever ctx does.
func (r *rotator) save(ctx context.Context, sink events.Sink) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rotateSaveTimeout)
	defer cancel()
	if r.change != nil {
		undo, err := r.change(ctx)
		if err != nil {
			return fmt.Errorf("%w; secrets.yaml is unchanged", err)
		}
		r.undo = undo
	}
	// Mark the restarts compose can't see first, so no failure from here
	// on loses them: `up` makes them.
	if err := markPendingRestarts(r.st, r.extra...); err != nil {
		return r.rollback(ctx, sink, err)
	}
	if err := r.st.SaveSecrets(r.next); err != nil && !r.holdsNext() {
		return r.rollback(ctx, sink, fmt.Errorf("saving secrets.yaml: %w", err))
	} else if err != nil {
		// The write landed and only syncing the directory failed.
		sink.Emit(events.Warning{ID: RotateSaveStepID, Text: "secrets.yaml holds the new secret, but saving it reported: " + err.Error()})
	}
	*r.sec = *r.next
	r.saved = true
	return nil
}

// holdsNext reports whether secrets.yaml reads back as next.
func (r *rotator) holdsNext() bool {
	saved, err := r.st.LoadSecrets()
	if err != nil {
		return false
	}
	a, aerr := yaml.Marshal(saved)
	b, berr := yaml.Marshal(r.next)
	return aerr == nil && berr == nil && bytes.Equal(a, b)
}

// rollback undoes the database change after err, so the database keeps
// matching secrets.yaml.
func (r *rotator) rollback(ctx context.Context, sink events.Sink, err error) error {
	if r.undo == nil {
		return err
	}
	sink.Emit(events.Progress{ID: RotateSaveStepID, Text: "undoing the database change"})
	if uerr := r.undo(ctx); uerr != nil {
		return fmt.Errorf("%w; undoing the database change also failed (%v), so the database no longer matches secrets.yaml", err, uerr)
	}
	return fmt.Errorf("%w; the database change was undone", err)
}

// restart recreates the running services whose compose config uses the
// secret, then restarts the running extra services compose didn't.
func (r *rotator) restart(ctx context.Context, sink events.Sink) error {
	consumers, err := secretConsumers(ctx, r.d.Compose, r.vars)
	if err != nil {
		return restartError(err)
	}
	running, err := runningServices(ctx, r.d)
	if err != nil {
		return restartError(err)
	}
	var recreate, restart []string
	for _, s := range consumers {
		if running[s] {
			recreate = append(recreate, s)
		}
	}
	for _, s := range r.extra {
		if running[s] && !slices.Contains(recreate, s) {
			restart = append(restart, s)
		}
	}
	out := events.NewLogWriter(sink, RotateRestartStepID, events.StreamStderr)
	defer func() { _ = out.Close() }()
	if len(recreate) > 0 {
		sink.Emit(events.Progress{ID: RotateRestartStepID, Text: "recreating " + strings.Join(recreate, ", ") + " with the new value"})
		if err := r.d.Compose.Up(ctx, docker.ComposeUpOpts{Services: recreate, Wait: true, WaitTimeout: StartWaitTimeout, Out: out}); err != nil {
			return restartError(err)
		}
	}
	if len(restart) > 0 {
		sink.Emit(events.Progress{ID: RotateRestartStepID, Text: "restarting " + strings.Join(restart, ", ")})
		if err := r.d.Compose.Restart(ctx, out, restart...); err != nil {
			return restartError(err)
		}
	}
	r.report.Restarted = append(recreate, restart...)
	return clearPendingRestarts(r.st, r.extra...)
}

func restartError(err error) error {
	return fmt.Errorf("the new secret is saved, but restarting the services that use it failed: %w; run `pic-sure up` to finish", err)
}

// rootTarget is root on the stack's MySQL, connecting with sec's password.
// A local picsure-db must be running.
func (r *rotator) rootTarget(ctx context.Context, sec *stack.Secrets) (sql.MySQLTarget, error) {
	if r.cfg.DB.Mode == stack.DBRemote {
		return mysqlTarget(r.cfg, sec, ""), nil
	}
	svc, err := composeService(ctx, r.d, picsureDB)
	if err != nil {
		return sql.MySQLTarget{}, err
	}
	if svc == nil || svc.State != "running" {
		return sql.MySQLTarget{}, exitcode.Precondition("%s isn't running, and the password must change in the database first; start the stack with `pic-sure up`", picsureDB)
	}
	return mysqlTarget(r.cfg, sec, svc.ID), nil
}

// alterRoot changes the local MySQL's root password, for every root
// account (the image makes root@'%' and root@localhost).
func (r *rotator) alterRoot(ctx context.Context) (func(context.Context) error, error) {
	t, err := r.rootTarget(ctx, r.sec)
	if err != nil {
		return nil, err
	}
	rows, err := sql.QueryMySQL(ctx, r.d.Docker, t, "SELECT host FROM mysql.user WHERE user = 'root'")
	if err != nil {
		return nil, fmt.Errorf("listing the root accounts: %w", err)
	}
	var accounts []sql.Account
	for _, row := range rows {
		accounts = append(accounts, sql.Account{User: "root", Host: row[0]})
	}
	if len(accounts) == 0 {
		return nil, errors.New("the database has no root account")
	}
	alter := func(ctx context.Context, t sql.MySQLTarget, password stack.Secret) error {
		var stmts []string
		for _, a := range accounts {
			stmts = append(stmts, sql.AlterUserPassword(a, string(password)))
		}
		return sql.ExecMySQL(ctx, r.d.Docker, t, stmts...)
	}
	if err := alter(ctx, t, r.next.DBRootPassword); err != nil {
		return nil, fmt.Errorf("changing the root password: %w", err)
	}
	return func(ctx context.Context) error {
		t.Password = string(r.next.DBRootPassword)
		return alter(ctx, t, r.sec.DBRootPassword)
	}, nil
}

// checkRemoteRoot makes sure the new remote root password logs in. The
// account is the DBA's: pic-sure only records its new password.
func (r *rotator) checkRemoteRoot(ctx context.Context) (func(context.Context) error, error) {
	t := mysqlTarget(r.cfg, r.next, "")
	if err := sql.ExecMySQL(ctx, r.d.Docker, t, "SELECT 1"); err != nil {
		return nil, remoteDBError(r.cfg, "logging in with the new password to", err)
	}
	return nil, nil
}

// alterAppUser changes an application account's password, user@'%', as
// root.
func (r *rotator) alterAppUser(user string, password func(*stack.Secrets) stack.Secret) func(context.Context) (func(context.Context) error, error) {
	return func(ctx context.Context) (func(context.Context) error, error) {
		t, err := r.rootTarget(ctx, r.sec)
		if err != nil {
			return nil, err
		}
		a := sql.Account{User: user, Host: "%"}
		if err := sql.ExecMySQL(ctx, r.d.Docker, t, sql.AlterUserPassword(a, string(password(r.next)))); err != nil {
			return nil, fmt.Errorf("changing %s's password: %w", user, err)
		}
		return func(ctx context.Context) error {
			return sql.ExecMySQL(ctx, r.d.Docker, t, sql.AlterUserPassword(a, string(password(r.sec))))
		}, nil
	}
}

// alterDictionary changes the dictionary database's picsure role's
// password, as that role.
func (r *rotator) alterDictionary(ctx context.Context) (func(context.Context) error, error) {
	svc, err := composeService(ctx, r.d, dictionaryDB)
	if err != nil {
		return nil, err
	}
	if svc == nil || svc.State != "running" {
		return nil, exitcode.Precondition("%s isn't running, and the password must change in the database first; start the stack with `pic-sure up`", dictionaryDB)
	}
	alter := func(ctx context.Context, from, to stack.Secret) error {
		stmt, err := sql.AlterPostgresPassword(dictionaryRole, string(to))
		if err != nil {
			return err
		}
		t := sql.PostgresTarget{Container: svc.ID, User: dictionaryRole, Password: string(from), Database: "dictionary"}
		return sql.ExecPostgres(ctx, r.d.Docker, t, stmt)
	}
	if err := alter(ctx, r.sec.DictionaryDBPassword, r.next.DictionaryDBPassword); err != nil {
		return nil, fmt.Errorf("changing the dictionary database password: %w", err)
	}
	return func(ctx context.Context) error {
		return alter(ctx, r.next.DictionaryDBPassword, r.sec.DictionaryDBPassword)
	}, nil
}

// setToken stores next's introspection token in auth.application.
func (r *rotator) setToken(ctx context.Context) (func(context.Context) error, error) {
	t, err := r.rootTarget(ctx, r.sec)
	if err != nil {
		return nil, err
	}
	old, err := storedToken(ctx, r.d, t)
	if err != nil {
		return nil, err
	}
	if err := sql.ExecMySQL(ctx, r.d.Docker, t, sql.SetApplicationToken(string(r.next.IntrospectionToken))); err != nil {
		return nil, fmt.Errorf("storing the introspection token in auth.application: %w", err)
	}
	return func(ctx context.Context) error {
		return sql.ExecMySQL(ctx, r.d.Docker, t, sql.SetApplicationToken(old))
	}, nil
}

// dictionaryRole is dictionary-db's POSTGRES_USER.
const dictionaryRole = "picsure"

// hpdsPlan re-keys HPDS: refuse while it has data unless DiscardData, stop
// it, delete the data, write a new key file, copy it into the volume and
// start hpds again if it was running.
func (r *rotator) hpdsPlan() []steps.Step {
	key := HPDSKeyStep(r.d, r.st, r.cfg)
	return []steps.Step{
		{ID: RotateHPDSDataID, Title: "Look for loaded HPDS data", Apply: r.hpdsCheck},
		{ID: RotateHPDSStopID, Title: "Stop hpds", Apply: r.hpdsStop},
		{ID: RotateHPDSWipeID, Title: "Delete the HPDS data", Apply: r.hpdsWipe},
		{ID: RotateSaveStepID, Title: "Write the new HPDS key", Apply: r.hpdsSave},
		{ID: key.ID, Title: key.Title, Apply: key.Apply},
		{ID: RotateHPDSStartID, Title: "Start hpds", Apply: r.hpdsStart},
	}
}

// hpdsDataFiles lists the phenotype data in hpds-data, which the loader
// encrypts with the key, and its provenance marker. all/ is the mountpoint
// of hpds-genomic, whose store HPDS doesn't encrypt, so it is kept.
const hpdsDataFiles = `find /data -mindepth 1 -maxdepth 1 ! -name encryption_key ! -name .encryption_key.incoming ! -name all`

func (r *rotator) hpdsCheck(ctx context.Context, _ events.Sink) error {
	if r.cfg.HPDS.Data != stack.HPDSLocal {
		return exitcode.Precondition("hpds.data is %s: the HPDS key belongs to the shared data set, not to this stack", r.cfg.HPDS.Data)
	}
	v, _ := catalog.LookupVolume(hpdsDataVolume)
	r.hpdsVolume = v.DockerName(r.cfg.Name)
	_, err := r.d.Docker.VolumeInspect(ctx, r.hpdsVolume)
	if errors.Is(err, docker.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	// Refuses a volume labelled for another stack, before it is wiped.
	if _, err := r.st.EnsureVolume(ctx, r.d.Docker, r.cfg.Name, r.hpdsVolume, hpdsDataVolume); err != nil {
		return err
	}
	var out bytes.Buffer
	if err := volumeHelper(ctx, r.d, r.st, r.cfg, r.hpdsVolume, "hpds-data", hpdsDataFiles, nil, &out); err != nil {
		return fmt.Errorf("listing volume %s: %w", r.hpdsVolume, err)
	}
	r.hpdsData = strings.TrimSpace(out.String()) != ""
	if r.hpdsData && !r.opts.DiscardData {
		return exitcode.ConfirmRequired("HPDS has data loaded, which a new key can't read; pass --discard-data --yes to delete it and re-key, then load the data again. Nothing was changed.")
	}
	return nil
}

func (r *rotator) hpdsStop(ctx context.Context, sink events.Sink) error {
	svc, err := composeService(ctx, r.d, hpdsService)
	if err != nil || svc == nil || svc.State != "running" {
		return err
	}
	r.hpdsWasRunning = true
	out := events.NewLogWriter(sink, RotateHPDSStopID, events.StreamStderr)
	defer func() { _ = out.Close() }()
	if err := r.d.Compose.Stop(ctx, out, hpdsService); err != nil {
		return fmt.Errorf("stopping hpds: %w", err)
	}
	return nil
}

func (r *rotator) hpdsWipe(ctx context.Context, _ events.Sink) error {
	if !r.hpdsData {
		return nil
	}
	script := hpdsDataFiles + ` -exec rm -rf {} +`
	if err := volumeHelper(ctx, r.d, r.st, r.cfg, r.hpdsVolume, "hpds-wipe", script, nil, nil); err != nil {
		return fmt.Errorf("deleting the HPDS data in volume %s: %w", r.hpdsVolume, err)
	}
	r.report.DiscardedData = true
	return nil
}

func (r *rotator) hpdsSave(_ context.Context, _ events.Sink) error {
	if err := r.st.ReplaceHPDSKey(r.d.Rand); err != nil {
		return err
	}
	r.saved = true
	return nil
}

func (r *rotator) hpdsStart(ctx context.Context, sink events.Sink) error {
	if !r.hpdsWasRunning {
		return nil
	}
	out := events.NewLogWriter(sink, RotateHPDSStartID, events.StreamStderr)
	defer func() { _ = out.Close() }()
	if err := r.d.Compose.Up(ctx, docker.ComposeUpOpts{Services: []string{hpdsService}, Wait: true, WaitTimeout: HPDSStartTimeout, Out: out}); err != nil {
		return fmt.Errorf("the HPDS key is replaced, but starting hpds failed: %w; run `pic-sure up`", err)
	}
	r.report.Restarted = []string{hpdsService}
	return nil
}

// secretConsumers returns the services whose compose config, as `compose
// config --no-interpolate` gives it with the overrides, references any of
// vars: in its environment, command or anywhere else. Compose recreates
// exactly these when a variable's value changes.
func secretConsumers(ctx context.Context, c docker.Composer, vars []string) ([]string, error) {
	if len(vars) == 0 {
		return nil, nil
	}
	out, err := c.Config(ctx, false)
	if err != nil {
		return nil, fmt.Errorf("reading the compose config: %w", err)
	}
	var doc struct {
		Services map[string]yaml.Node `yaml:"services"`
	}
	if err := yaml.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("reading the compose config: %w", err)
	}
	names := strings.Join(vars, "|")
	re := regexp.MustCompile(`\$(?:\{(?:` + names + `)(?:[:?+-][^}]*)?\}|(?:` + names + `)\b)`)
	var svcs []string
	for name, node := range doc.Services {
		if references(&node, re) {
			svcs = append(svcs, name)
		}
	}
	slices.Sort(svcs)
	return svcs, nil
}

// references reports whether a scalar under n matches re, once `$$`
// escapes are taken out.
func references(n *yaml.Node, re *regexp.Regexp) bool {
	if n.Kind == yaml.ScalarNode {
		return re.MatchString(strings.ReplaceAll(n.Value, "$$", ""))
	}
	for _, c := range n.Content {
		if references(c, re) {
			return true
		}
	}
	return false
}

// runningServices returns the stack's services that are running.
func runningServices(ctx context.Context, d *Deps) (map[string]bool, error) {
	svcs, err := d.Compose.Ps(ctx)
	if err != nil {
		return nil, err
	}
	running := map[string]bool{}
	for _, s := range svcs {
		if s.State == "running" {
			running[s.Service] = true
		}
	}
	return running, nil
}

// markPendingRestarts adds services to state.json's PendingRestarts, which
// up's restart step restarts.
func markPendingRestarts(st *stack.Stack, services ...string) error {
	return updatePendingRestarts(st, func(p []string) []string {
		for _, s := range services {
			if !slices.Contains(p, s) {
				p = append(p, s)
			}
		}
		return p
	})
}

// clearPendingRestarts takes services off state.json's PendingRestarts.
func clearPendingRestarts(st *stack.Stack, services ...string) error {
	return updatePendingRestarts(st, func(p []string) []string {
		return slices.DeleteFunc(p, func(s string) bool { return slices.Contains(services, s) })
	})
}

func updatePendingRestarts(st *stack.Stack, f func([]string) []string) error {
	state, err := st.LoadState()
	if err != nil {
		return err
	}
	before := slices.Clone(state.PendingRestarts)
	state.PendingRestarts = f(state.PendingRestarts)
	if slices.Equal(before, state.PendingRestarts) {
		return nil
	}
	return st.SaveState(state)
}

// volumeHelper runs script in an alpine container with volume at /data and
// no network.
func volumeHelper(ctx context.Context, d *Deps, st *stack.Stack, cfg *stack.Config, volume, prefix, script string, stdin io.Reader, stdout io.Writer) error {
	name, err := docker.UniqueName(cfg.Name+"-"+prefix, d.Rand)
	if err != nil {
		return err
	}
	alpine, _ := catalog.LookupImage("alpine")
	var stderr bytes.Buffer
	code, err := d.Docker.Run(ctx, docker.RunOpts{
		Image:   alpine.Ref,
		Name:    name,
		Remove:  true,
		Network: "none",
		Labels:  st.Labels(cfg.Name),
		Mounts:  []docker.Mount{{Source: volume, Target: "/data"}},
		Args:    []string{"sh", "-c", "set -eu; " + script},
		Stdin:   stdin,
		Stdout:  stdout,
		Stderr:  &stderr,
	})
	if err != nil {
		_ = d.Docker.Rm(context.WithoutCancel(ctx), name, true)
		return err
	}
	if code != 0 {
		err = fmt.Errorf("the helper container exited %d", code)
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			err = fmt.Errorf("%w: %s", err, msg[strings.LastIndexByte(msg, '\n')+1:])
		}
	}
	return err
}
