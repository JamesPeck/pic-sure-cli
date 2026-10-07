package ops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/sql"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

// Step IDs of §9.1 steps 8 and 9.
const (
	StepDB      = "db"
	StepMigrate = "migrate"
)

// The services and Flyway one-shots the steps drive.
const (
	picsureDB            = "picsure-db"
	dictionaryDB         = "dictionary-db"
	flywayInit           = "flyway-init"
	flywayDictionaryInit = "flyway-dictionary-init"
)

// Defaults for DBOptions.
const (
	DefaultDBTimeout  = 5 * time.Minute
	DefaultDBInterval = 2 * time.Second
)

// DBOptions bounds the wait for the database.
type DBOptions struct {
	// Timeout is how long to wait for picsure-db to be healthy and accept
	// the root password over TCP; zero means DefaultDBTimeout.
	Timeout time.Duration
	// Interval is the pause between polls; zero means DefaultDBInterval.
	Interval time.Duration
}

// MigrateAction is what Flyway does: FlywayMigrate or FlywayRepair.
type MigrateAction string

const (
	FlywayMigrate MigrateAction = "migrate"
	FlywayRepair  MigrateAction = "repair"
)

// MigrateOptions configures the migrate step.
type MigrateOptions struct {
	// Action is FlywayMigrate (the zero value) or FlywayRepair. Repair has
	// no Check, so it always runs.
	Action MigrateAction
	// NoRestart leaves psama and dictionary-api alone after a migrate.
	NoRestart bool
}

// DBStep is §9.1 step 8 for a local database, ID "db": `compose up -d
// picsure-db`, a wait for its exact `healthy` status, then an authenticated
// `SELECT 1` over TCP, since the healthcheck passes on access denied and on
// the entrypoint's temporary server, which listens only on the socket.
// Access denied fails at once: the volume was initialised with another
// root password. Check is done when the container is healthy and the probe
// succeeds. With a remote database the step only probes it; creating its
// schemas and users is `db bootstrap` (054).
func DBStep(d *Deps, cfg *stack.Config, sec *stack.Secrets, opts DBOptions) steps.Step {
	s := &dbStep{d: d, cfg: cfg, sec: sec, opts: opts}
	if s.opts.Timeout <= 0 {
		s.opts.Timeout = DefaultDBTimeout
	}
	if s.opts.Interval <= 0 {
		s.opts.Interval = DefaultDBInterval
	}
	return steps.Step{ID: StepDB, Title: "Start the database", Check: s.check, Apply: s.apply}
}

type dbStep struct {
	d    *Deps
	cfg  *stack.Config
	sec  *stack.Secrets
	opts DBOptions
}

func (s *dbStep) check(ctx context.Context) (bool, error) {
	if s.cfg.DB.Mode == stack.DBRemote {
		return s.probe(ctx, "") == nil, nil
	}
	svc, err := s.picsureDB(ctx)
	if err != nil || svc == nil || svc.State != "running" || svc.Health != "healthy" {
		return false, err
	}
	return s.probe(ctx, svc.ID) == nil, nil
}

func (s *dbStep) apply(ctx context.Context, sink events.Sink) error {
	if s.cfg.DB.Mode == stack.DBRemote {
		host := fmt.Sprintf("%s:%d", s.cfg.DB.Remote.Host, s.cfg.DB.Remote.Port)
		if err := s.probe(ctx, ""); err != nil {
			return exitcode.Precondition("the remote database at %s refused SELECT 1: %w", host, err)
		}
		return nil
	}
	out := events.NewLogWriter(sink, StepDB, events.StreamStderr)
	err := s.d.Compose.Up(ctx, docker.ComposeUpOpts{Services: []string{picsureDB}, Out: out})
	_ = out.Close()
	if err != nil {
		return fmt.Errorf("starting %s: %w", picsureDB, err)
	}
	sink.Emit(events.Progress{ID: StepDB, Text: "waiting for " + picsureDB + " to be healthy"})
	var last string // why the last poll wasn't ready
	for range s.opts.Timeout / s.opts.Interval {
		svc, err := s.picsureDB(ctx)
		switch {
		case err != nil:
			return err
		case svc == nil:
			last = "it has no container"
		case svc.State == "exited" || svc.State == "dead":
			s.logTail(ctx, sink)
			return fmt.Errorf("%s stopped (exit %d) before it was ready", picsureDB, svc.ExitCode)
		case svc.Health != "healthy":
			last = fmt.Sprintf("its state is %s, health %q", svc.State, svc.Health)
		default:
			err := s.probe(ctx, svc.ID)
			if err == nil {
				return nil
			}
			if accessDenied(err) {
				return exitcode.Precondition("%s refused the stack's root password: %w. "+
					"MySQL sets the root password only when it initialises an empty volume, so %s "+
					"was probably created by another stack, or before secrets.yaml changed. Restore "+
					"the matching secrets.yaml, or remove the volume if its data is disposable",
					picsureDB, err, s.volume())
			}
			// The entrypoint's temporary server listens only on the socket.
			last = "it is healthy but doesn't accept TCP connections yet: " + err.Error()
		}
		if err := sleep(ctx, s.opts.Interval); err != nil {
			return err
		}
	}
	s.logTail(ctx, sink)
	return fmt.Errorf("%s wasn't ready after %s: %s", picsureDB, s.opts.Timeout, last)
}

// picsureDB returns the picsure-db container compose knows about, or nil.
func (s *dbStep) picsureDB(ctx context.Context) (*docker.ComposeService, error) {
	return composeService(ctx, s.d, picsureDB)
}

// probe runs SELECT 1 as root: over TCP inside the container (local), or
// from a client container (remote, when container is "").
func (s *dbStep) probe(ctx context.Context, container string) error {
	t := mysqlTarget(s.cfg, s.sec, container)
	if container != "" {
		t.Host = "127.0.0.1"
	}
	rows, err := sql.QueryMySQL(ctx, s.d.Docker, t, "SELECT 1")
	if err != nil {
		return err
	}
	if len(rows) != 1 || len(rows[0]) != 1 || rows[0][0] != "1" {
		return fmt.Errorf("SELECT 1 returned %q", rows)
	}
	return nil
}

func (s *dbStep) volume() string {
	v, _ := catalog.LookupVolume("picsure-db-data")
	return v.DockerName(s.cfg.Name)
}

// logTail shows picsure-db's last log lines, for a failure.
func (s *dbStep) logTail(ctx context.Context, sink events.Sink) {
	out := events.NewLogWriter(sink, StepDB, events.StreamStderr)
	_ = s.d.Compose.Logs(ctx, docker.ComposeLogsOpts{Services: []string{picsureDB}, Tail: 30, Out: out})
	_ = out.Close()
}

// accessDenied reports whether err is the mysql client's ERROR 1045.
func accessDenied(err error) bool {
	var xe *docker.ExitError
	return errors.As(err, &xe) && bytes.Contains(xe.Stderr, []byte("ERROR 1045"))
}

// mysqlTarget is root on the stack's MySQL: in container for a local
// database, or the remote server.
func mysqlTarget(cfg *stack.Config, sec *stack.Secrets, container string) sql.MySQLTarget {
	if cfg.DB.Mode == stack.DBRemote {
		return sql.MySQLTarget{Host: cfg.DB.Remote.Host, Port: cfg.DB.Remote.Port,
			User: cfg.DB.Remote.RootUser, Password: string(sec.DBRemoteRootPassword)}
	}
	return sql.MySQLTarget{Container: container, Password: string(sec.DBRootPassword)}
}

// composeService returns the stack's container for service, or nil.
func composeService(ctx context.Context, d *Deps, service string) (*docker.ComposeService, error) {
	svcs, err := d.Compose.Ps(ctx, service)
	if err != nil {
		return nil, err
	}
	for i := range svcs {
		if svcs[i].Service == service {
			return &svcs[i], nil
		}
	}
	return nil, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-t.C:
		return nil
	}
}

// MigrateStep is §9.1 step 9, ID "migrate": `compose run --rm flyway-init`
// (four passes: auth core, picsure core, custom picsure, custom auth), then
// `compose run --rm flyway-dictionary-init`. Each run starts the database
// it depends on. A non-zero exit fails the step and shows the output's
// last lines. After a migrate, psama and dictionary-api are restarted if
// they are running, since they cache what the migrations change. For a
// migrate, Check is MigrationsUpToDate.
func MigrateStep(d *Deps, cfg *stack.Config, sec *stack.Secrets, opts MigrateOptions) steps.Step {
	if opts.Action == "" {
		opts.Action = FlywayMigrate
	}
	s := steps.Step{ID: StepMigrate, Title: "Run the database migrations", Apply: func(ctx context.Context, sink events.Sink) error {
		return migrate(ctx, d, sink, opts)
	}}
	if opts.Action == FlywayMigrate {
		s.Check = func(ctx context.Context) (bool, error) { return MigrationsUpToDate(ctx, d, cfg, sec) }
	} else {
		s.Title = "Repair the Flyway history"
	}
	return s
}

func migrate(ctx context.Context, d *Deps, sink events.Sink, opts MigrateOptions) error {
	var env []string
	if opts.Action != FlywayMigrate {
		env = []string{"FLYWAY_ACTION=" + string(opts.Action)}
	}
	for _, svc := range []string{flywayInit, flywayDictionaryInit} {
		sink.Emit(events.Progress{ID: StepMigrate, Text: "running " + svc})
		out, _ := newPartOutput("", svc, func(l string) {
			if d.Log != nil {
				d.Log.Debug(svc, "line", l)
			}
		})
		code, err := d.Compose.Run(ctx, docker.ComposeRunOpts{Service: svc, Rm: true, Env: env, Stdout: out, Stderr: out})
		_ = out.Close()
		if err == nil && code != 0 {
			err = fmt.Errorf("exit %d", code)
		}
		if err != nil {
			out.emitTail(sink, StepMigrate)
			return fmt.Errorf("%s (Flyway %s) failed: %w", svc, opts.Action, err)
		}
	}
	if opts.Action != FlywayMigrate || opts.NoRestart {
		return nil
	}
	svcs, err := d.Compose.Ps(ctx)
	if err != nil {
		return fmt.Errorf("finding the services to restart: %w", err)
	}
	var restart []string
	for _, c := range catalog.Services() {
		if c.RestartAfterMigrate && slices.ContainsFunc(svcs, func(s docker.ComposeService) bool {
			return s.Service == c.Name && s.State == "running"
		}) {
			restart = append(restart, c.Name)
		}
	}
	if len(restart) == 0 {
		return nil
	}
	sink.Emit(events.Progress{ID: StepMigrate, Text: "restarting " + strings.Join(restart, ", ") + " to pick up the migrated data"})
	out := events.NewLogWriter(sink, StepMigrate, events.StreamStderr)
	defer func() { _ = out.Close() }()
	return d.Compose.Restart(ctx, out, restart...)
}

// Migrate is the `migrate` command: the db step, then the migrate step, so
// a database that is already migrated is skipped.
func Migrate(ctx context.Context, d *Deps, cfg *stack.Config, sec *stack.Secrets, opts MigrateOptions, skip []string) error {
	plan := []steps.Step{DBStep(d, cfg, sec, DBOptions{}), MigrateStep(d, cfg, sec, opts)}
	return steps.Run(ctx, d.Sink, plan, steps.Options{Skip: skip})
}

// historyTable is one Flyway history and the migrations it records: the
// directory a one-shot mounts at target.
type historyTable struct {
	service, target string
	table           string // schema.table
}

// historyTables lists every Flyway pass, in the order they run.
var historyTables = []historyTable{
	{flywayInit, "/migrations/auth", "auth.flyway_schema_history"},
	{flywayInit, "/migrations/picsure", "picsure.flyway_schema_history"},
	{flywayInit, "/migrations/custom/picsure", "picsure.flyway_custom_schema_history"},
	{flywayInit, "/migrations/custom/auth", "auth.flyway_custom_schema_history"},
	{flywayDictionaryInit, "/migrations/dictionary", "public.flyway_schema_history"},
}

// MigrationsUpToDate reports whether every Flyway history records every
// versioned migration in the directories the one-shots mount, with no
// failed entries. A migration at or below a pass's baseline counts as
// applied, as it does for Flyway. A missing history table, or a database
// that isn't running, is not up to date; neither is a directory with a
// repeatable (R__) migration, whose checksum this doesn't compare.
func MigrationsUpToDate(ctx context.Context, d *Deps, cfg *stack.Config, sec *stack.Secrets) (bool, error) {
	mounts, err := composeMounts(ctx, d)
	if err != nil {
		return false, err
	}
	var container string
	if cfg.DB.Mode != stack.DBRemote {
		svc, err := composeService(ctx, d, picsureDB)
		if err != nil || svc == nil || svc.State != "running" {
			return false, err
		}
		container = svc.ID
	}
	dict, err := composeService(ctx, d, dictionaryDB)
	if err != nil || dict == nil || dict.State != "running" {
		return false, err
	}
	mysqlHist, err := mysqlHistory(ctx, d, mysqlTarget(cfg, sec, container))
	if err != nil {
		return false, err
	}
	pgHist, err := postgresHistory(ctx, d, sql.PostgresTarget{Container: dict.ID, User: "picsure",
		Password: string(sec.DictionaryDBPassword), Database: "dictionary"})
	if err != nil {
		return false, err
	}
	for _, h := range historyTables {
		src := mounts[h.service][h.target]
		if src == "" {
			return false, fmt.Errorf("%s mounts nothing at %s", h.service, h.target)
		}
		files, err := migrationVersions(src)
		if err != nil {
			return false, err
		}
		rows, ok := mysqlHist[h.table]
		if h.service == flywayDictionaryInit {
			rows, ok = pgHist, pgHist != nil
		}
		if files == nil || !ok || !historyCovers(rows, files) {
			return false, nil
		}
	}
	return true, nil
}

// historyRow is one versioned row of a Flyway history table.
type historyRow struct {
	version  string
	baseline bool
	success  bool
}

// mysqlHistory reads the four MySQL histories, keyed by schema.table. A
// table that doesn't exist has no key.
func mysqlHistory(ctx context.Context, d *Deps, t sql.MySQLTarget) (map[string][]historyRow, error) {
	rows, err := sql.QueryMySQL(ctx, d.Docker, t, "SELECT CONCAT(table_schema, '.', table_name) "+
		"FROM information_schema.tables WHERE table_schema IN ('auth', 'picsure') "+
		"AND table_name IN ('flyway_schema_history', 'flyway_custom_schema_history')")
	if err != nil {
		return nil, fmt.Errorf("listing the Flyway history tables: %w", err)
	}
	hist := map[string][]historyRow{}
	var selects []string
	for _, r := range rows {
		hist[r[0]] = []historyRow{}
		// The names come from the fixed list in the WHERE clause above.
		selects = append(selects, fmt.Sprintf("SELECT '%s', version, type, success FROM %s WHERE version IS NOT NULL", r[0], r[0]))
	}
	if len(selects) == 0 {
		return hist, nil
	}
	rows, err = sql.QueryMySQL(ctx, d.Docker, t, strings.Join(selects, " UNION ALL "))
	if err != nil {
		return nil, fmt.Errorf("reading the Flyway history: %w", err)
	}
	for _, r := range rows {
		if len(r) != 4 {
			return nil, fmt.Errorf("reading the Flyway history: unexpected row %q", r)
		}
		hist[r[0]] = append(hist[r[0]], historyRow{version: r[1], baseline: r[2] == "BASELINE", success: r[3] == "1"})
	}
	return hist, nil
}

// postgresHistory reads the dictionary's history; nil when there is no
// history table yet.
func postgresHistory(ctx context.Context, d *Deps, t sql.PostgresTarget) ([]historyRow, error) {
	rows, err := sql.QueryPostgres(ctx, d.Docker, t, "SELECT to_regclass('public.flyway_schema_history') IS NOT NULL")
	if err != nil {
		return nil, fmt.Errorf("finding the dictionary Flyway history: %w", err)
	}
	if len(rows) != 1 || rows[0][0] != "t" {
		return nil, nil
	}
	rows, err = sql.QueryPostgres(ctx, d.Docker, t,
		"SELECT version, type, success FROM public.flyway_schema_history WHERE version IS NOT NULL")
	if err != nil {
		return nil, fmt.Errorf("reading the dictionary Flyway history: %w", err)
	}
	hist := []historyRow{}
	for _, r := range rows {
		if len(r) != 3 {
			return nil, fmt.Errorf("reading the dictionary Flyway history: unexpected row %q", r)
		}
		hist = append(hist, historyRow{version: r[0], baseline: r[1] == "BASELINE", success: r[2] == "t"})
	}
	return hist, nil
}

// historyCovers reports whether rows record every version in files, with
// no failed row.
func historyCovers(rows []historyRow, files []string) bool {
	applied := map[string]bool{}
	var baseline []string
	for _, r := range rows {
		if !r.success {
			return false
		}
		v := versionParts(r.version)
		applied[strings.Join(v, ".")] = true
		if r.baseline && compareVersions(v, baseline) > 0 {
			baseline = v
		}
	}
	for _, f := range files {
		v := versionParts(f)
		if !applied[strings.Join(v, ".")] && (baseline == nil || compareVersions(v, baseline) > 0) {
			return false
		}
	}
	return true
}

var (
	versionedRE   = regexp.MustCompile(`^V([0-9][0-9._]*)__.*\.sql$`)
	repeatableRE  = regexp.MustCompile(`^R__.*\.sql$`)
	errRepeatable = errors.New("repeatable migration")
)

// migrationVersions lists the versions of the V<version>__*.sql files
// under dir, as Flyway's filesystem location finds them. It returns nil
// when there are none, or when there is a repeatable migration.
func migrationVersions(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(_ string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			return nil
		}
		if repeatableRE.MatchString(e.Name()) {
			return errRepeatable
		}
		if m := versionedRE.FindStringSubmatch(e.Name()); m != nil {
			out = append(out, m[1])
		}
		return nil
	})
	if errors.Is(err, errRepeatable) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing the migrations: %w", err)
	}
	return out, nil
}

// versionParts splits a Flyway version ("1.2", or "1_2" in a file name)
// into its numbers, without leading zeros or trailing zero parts, so equal
// versions have equal parts.
func versionParts(v string) []string {
	parts := strings.FieldsFunc(v, func(r rune) bool { return r == '.' || r == '_' })
	for i, p := range parts {
		if p = strings.TrimLeft(p, "0"); p == "" {
			p = "0"
		}
		parts[i] = p
	}
	for len(parts) > 0 && parts[len(parts)-1] == "0" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// compareVersions orders two versionParts results numerically.
func compareVersions(a, b []string) int {
	for i := range max(len(a), len(b)) {
		x, y := "0", "0"
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if c := len(x) - len(y); c != 0 {
			return c
		}
		if c := strings.Compare(x, y); c != 0 {
			return c
		}
	}
	return 0
}

// composeMounts reads the bind mounts of the Flyway one-shots and
// dictionary-db from `compose config`, so the migrations checked are the
// ones Flyway will see, overrides included: service → target → host path.
func composeMounts(ctx context.Context, d *Deps) (map[string]map[string]string, error) {
	out, err := d.Compose.Config(ctx, false)
	if err != nil {
		return nil, fmt.Errorf("reading the compose config: %w", err)
	}
	var doc struct {
		Services map[string]struct {
			Volumes []struct {
				Type   string `yaml:"type"`
				Source string `yaml:"source"`
				Target string `yaml:"target"`
			} `yaml:"volumes"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("reading the compose config: %w", err)
	}
	mounts := map[string]map[string]string{}
	for _, name := range []string{flywayInit, flywayDictionaryInit, dictionaryDB} {
		m := map[string]string{}
		for _, v := range doc.Services[name].Volumes {
			if v.Type == "bind" {
				m[v.Target] = v.Source
			}
		}
		mounts[name] = m
	}
	return mounts, nil
}

// MigrateCheckReport is `migrate --check`: whether the migrations' inputs
// are in place, ported from AIO's `run-migrations.sh --check`.
type MigrateCheckReport struct {
	OK     bool                `json:"ok"`
	Inputs []MigrateCheckInput `json:"inputs"`
	// Warnings don't fail the check.
	Warnings []string `json:"warnings"`
}

// MigrateCheckInput is one input the migrations need.
type MigrateCheckInput struct {
	Name string `json:"name"`
	// Path is the host path checked, when the input is a file or directory.
	Path    string `json:"path,omitempty"`
	OK      bool   `json:"ok"`
	Problem string `json:"problem,omitempty"`
}

// legacyTokenRE matches the Jenkins-era UUID tokens that flyway-init
// substitutes in a copy of the project migrations.
var legacyTokenRE = regexp.MustCompile(`__(APPLICATION|RESOURCE|VISUALIZATION_RESOURCE)_UUID__`)

// MigrateCheck verifies the migrations' inputs without running them: the
// SQL directories the Flyway one-shots mount (the pic-sure source's core
// auth, core picsure and dictionary migrations, and the migrations
// project's picsure and auth), the dictionary's schema.sql, the UUIDs the
// migrations substitute, the remote database settings, and that `compose
// config` accepts the stack. It asks docker for nothing else.
func MigrateCheck(ctx context.Context, d *Deps, cfg *stack.Config, sec *stack.Secrets) *MigrateCheckReport {
	r := &MigrateCheckReport{Inputs: []MigrateCheckInput{}, Warnings: []string{}}
	add := func(in MigrateCheckInput) {
		in.OK = in.Problem == ""
		r.Inputs = append(r.Inputs, in)
	}
	if _, err := d.Compose.Config(ctx, true); err != nil {
		add(MigrateCheckInput{Name: "compose config", Problem: err.Error()})
	} else {
		add(MigrateCheckInput{Name: "compose config"})
	}
	mounts, err := composeMounts(ctx, d)
	project := cfg.Components.Migrations.Project
	dirs := []struct{ name, service, target string }{
		{"core auth migrations", flywayInit, "/migrations/auth"},
		{"core picsure migrations", flywayInit, "/migrations/picsure"},
		{"project picsure migrations (" + project + ")", flywayInit, "/migrations/custom/picsure"},
		{"project auth migrations (" + project + ")", flywayInit, "/migrations/custom/auth"},
		{"dictionary migrations", flywayDictionaryInit, "/migrations/dictionary"},
	}
	for _, dir := range dirs {
		in := MigrateCheckInput{Name: dir.name}
		switch {
		case err != nil:
			in.Problem = err.Error()
		default:
			in.Path = mounts[dir.service][dir.target]
			in.Problem = sqlDirProblem(in.Path, dir.service, dir.target)
		}
		if in.Problem == "" && strings.HasPrefix(dir.target, "/migrations/custom/") && hasLegacyTokens(in.Path) {
			r.Warnings = append(r.Warnings, dir.name+" contain legacy Jenkins UUID tokens; Flyway substitutes them in a temporary copy")
		}
		add(in)
	}
	schema := MigrateCheckInput{Name: "dictionary baseline schema"}
	if err != nil {
		schema.Problem = err.Error()
	} else if schema.Path = mounts[dictionaryDB]["/docker-entrypoint-initdb.d/schema.sql"]; schema.Path == "" {
		schema.Problem = dictionaryDB + " mounts no schema.sql"
	} else if fi, err := os.Stat(schema.Path); err != nil || !fi.Mode().IsRegular() {
		schema.Problem = "not a file"
	}
	add(schema)

	var missing []string
	for _, u := range []struct{ name, v string }{
		{"application_uuid", sec.ApplicationUUID},
		{"resource_uuid", sec.ResourceUUID},
		{"visualization_uuid", sec.VisualizationUUID},
	} {
		if u.v == "" {
			missing = append(missing, u.name)
		}
	}
	uuids := MigrateCheckInput{Name: "project UUIDs"}
	if missing != nil {
		uuids.Problem = "secrets.yaml lacks " + strings.Join(missing, ", ")
	}
	add(uuids)

	if cfg.DB.Mode == stack.DBRemote {
		var missing []string
		if cfg.DB.Remote.Host == "" {
			missing = append(missing, "db.remote.host")
		}
		if cfg.DB.Remote.Port == 0 {
			missing = append(missing, "db.remote.port")
		}
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
		remote := MigrateCheckInput{Name: "remote database settings"}
		if missing != nil {
			remote.Problem = "missing " + strings.Join(missing, ", ")
		}
		add(remote)
	}

	r.OK = !slices.ContainsFunc(r.Inputs, func(in MigrateCheckInput) bool { return !in.OK })
	return r
}

// sqlDirProblem says what is wrong with a mounted migrations directory:
// nothing mounted, missing, or without a top-level .sql file.
func sqlDirProblem(dir, service, target string) string {
	if dir == "" {
		return service + " mounts nothing at " + target
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "missing"
		}
		return err.Error()
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			return ""
		}
	}
	return "no .sql files"
}

// hasLegacyTokens reports whether any .sql file under dir holds a legacy
// UUID token.
func hasLegacyTokens(dir string) bool {
	found := false
	_ = filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			return nil
		}
		if b, err := os.ReadFile(p); err == nil && legacyTokenRE.Match(b) {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found
}
