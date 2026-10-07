package ops

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/render"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

// StatusOptions are what Status needs from the cli layer.
type StatusOptions struct {
	// CLIVersion is this binary's version, for the version gate.
	CLIVersion string
	// Migrations is the config migration registry this binary runs.
	Migrations stack.Registry
	// ComposeErr is why the command couldn't build Deps.Compose, when it
	// is nil for a reason other than a stack that hasn't been rendered.
	ComposeErr error
	// Deep runs the probes inside the containers (StatusDeep).
	Deep bool
}

// StatusReport is `status --json` (spec §9.9). docs/json-schemas.md
// documents it; changes within schema_version 2 are additive only. A
// section that couldn't be read says why in its error field, and status
// still exits 0.
type StatusReport struct {
	Stack    StatusStack    `json:"stack"`
	Config   StatusConfig   `json:"config"`
	Versions StatusVersions `json:"versions"`
	// StateError is why state.json couldn't be read; release, components,
	// image tags and last_operation are then empty.
	StateError string            `json:"state_error,omitempty"`
	Release    StatusRelease     `json:"release"`
	Components []StatusComponent `json:"components"`
	Images     []StatusImage     `json:"images"`
	// ImagesError is why image presence couldn't be checked (usually an
	// unreachable daemon); present is then null.
	ImagesError string          `json:"images_error,omitempty"`
	Services    []StatusService `json:"services"`
	// ServicesError is why compose ps couldn't run. An empty services list
	// is unknown, not down: with no error, compose reported no containers.
	ServicesError string           `json:"services_error,omitempty"`
	DB            *StatusDB        `json:"db"`
	Migrations    StatusMigrations `json:"migrations"`
	Token         StatusToken      `json:"token"`
	Auth0         *StatusAuth0     `json:"auth0"`
	LastOperation *StatusOperation `json:"last_operation"`
	// Deep is set by status --deep.
	Deep *StatusDeep `json:"deep,omitempty"`
}

// StatusOperation is the last mutating command state.json records.
type StatusOperation struct {
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
}

// StatusStack identifies the stack.
type StatusStack struct {
	Name string `json:"name"`
	Dir  string `json:"dir"`
}

// StatusConfig is whether pic-sure.yaml is valid.
type StatusConfig struct {
	Valid    bool            `json:"valid"`
	Problems []StatusProblem `json:"problems"`
	// Error is set when the file couldn't be read or decoded at all, such
	// as a schema this pic-sure can't read.
	Error string `json:"error,omitempty"`
}

// StatusProblem is one config problem.
type StatusProblem struct {
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Message string `json:"message"`
}

// Version gate states.
const (
	GateOK                = "ok"
	GateStackNewer        = "stack_newer"
	GateMigrationsPending = "migrations_pending"
	GateUnsupportedSchema = "unsupported_schema"
	GateUnknown           = "unknown"
)

// StatusVersions is the version gate state (§10.6).
type StatusVersions struct {
	CLI          string `json:"cli"`
	Schema       int    `json:"schema"`
	StackCLI     string `json:"stack_cli"`
	StackSchema  int    `json:"stack_schema"`
	ConfigSchema int    `json:"config_schema"`
	Gate         string `json:"gate"`
	// PendingMigrations summarizes the config migrations update would run.
	PendingMigrations []string `json:"pending_migrations"`
	Error             string   `json:"error,omitempty"`
}

// StatusRelease is the release-control commit state.json records.
type StatusRelease struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	Commit string `json:"commit"`
}

// StatusComponent is one component's recorded source commit.
type StatusComponent struct {
	Name   string `json:"name"`
	Ref    string `json:"ref"`
	Commit string `json:"commit"`
}

// StatusImage is one built image at the tag the stack runs.
type StatusImage struct {
	Name      string `json:"name"`
	Component string `json:"component"`
	// Ref is repository:tag, empty when state.json records no tag.
	Ref string `json:"ref"`
	// Present is null when unknown: no recorded tag, or docker failed.
	Present *bool `json:"present"`
	// Dev is set when a dev variant replaces the image: Ref is then the
	// local build's tag (§7.3), or empty for a variant that runs a
	// third-party image instead (httpd-hmr runs node).
	Dev bool `json:"dev"`
}

// StatusService is one container from compose ps.
type StatusService struct {
	Service   string `json:"service"`
	Container string `json:"container"`
	State     string `json:"state"`
	Health    string `json:"health"`
	Status    string `json:"status"`
	ExitCode  int    `json:"exit_code"`
}

// StatusDB is the database mode.
type StatusDB struct {
	Mode string `json:"mode"`
	// Host and Port are set for a remote database.
	Host string `json:"host,omitempty"`
	Port int    `json:"port,omitempty"`
}

// StatusMigrations is the Flyway migration state (MigrationsUpToDate).
type StatusMigrations struct {
	// Status is up_to_date, pending or unknown.
	Status string `json:"status"`
	// Error says why Status is unknown.
	Error string `json:"error,omitempty"`
}

// StatusToken is the introspection token's expiry, from secrets.yaml.
type StatusToken struct {
	// ExpiresAt is null when no token has been issued.
	ExpiresAt *time.Time `json:"expires_at"`
	Expired   bool       `json:"expired"`
	Error     string     `json:"error,omitempty"`
}

// StatusAuth0 is what to register in the Auth0 application for this
// stack's HTTPS origin (D35).
type StatusAuth0 struct {
	// Needed is false in open mode, where nobody has to log in.
	Needed      bool   `json:"needed"`
	CallbackURL string `json:"callback_url"`
	LogoutURL   string `json:"logout_url"`
	WebOrigin   string `json:"web_origin"`
	// The Dev URLs are httpd-hmr's Vite origin, set while that dev
	// variant is on.
	DevCallbackURL string `json:"dev_callback_url,omitempty"`
	DevLogoutURL   string `json:"dev_logout_url,omitempty"`
	DevWebOrigin   string `json:"dev_web_origin,omitempty"`
}

// Status reports on st without changing anything: no lock, no writes, no
// network beyond the local docker daemon and, with Deep, the containers'
// own localhost.
func Status(ctx context.Context, d *Deps, st *stack.Stack, opts StatusOptions) *StatusReport {
	r := &StatusReport{
		Stack:      StatusStack{Dir: st.Dir},
		Migrations: StatusMigrations{Status: MigrationsStatusUnknown},
	}
	cfg := statusConfig(r, st, opts.Migrations)
	statusVersions(r, st, opts)
	state, err := st.LoadState()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		state = &stack.State{}
	case err != nil:
		r.StateError = err.Error()
		state = &stack.State{}
	}
	r.Release = StatusRelease(state.Release)
	if op := state.LastOperation; op != nil {
		r.LastOperation = &StatusOperation{Name: op.Name, Status: string(op.Status), StartedAt: op.StartedAt.UTC(), FinishedAt: op.FinishedAt.UTC()}
	}
	for _, c := range catalog.Components() {
		rec := state.Components[c.Name]
		r.Components = append(r.Components, StatusComponent{Name: c.Name, Ref: rec.Ref, Commit: rec.Commit})
	}
	statusImages(ctx, d, r, state, cfg)
	statusServices(ctx, d, r, opts.ComposeErr)
	if opts.Deep {
		r.Deep = statusDeep(ctx, d, r)
	}
	statusToken(d, r, st)
	statusMigrations(ctx, d, r, st, cfg)
	if cfg != nil {
		r.Stack.Name = cfg.Name
		r.DB = &StatusDB{Mode: string(cfg.DB.Mode)}
		if cfg.DB.Mode == stack.DBRemote {
			r.DB.Host, r.DB.Port = cfg.DB.Remote.Host, cfg.DB.Remote.Port
		}
		r.Auth0 = auth0URLs(cfg)
	}
	return r
}

// statusConfig fills in the config section and returns the decoded config,
// or nil if it can't be decoded. Problems found by CheckFiles leave it
// usable.
func statusConfig(r *StatusReport, st *stack.Stack, migrations stack.Registry) *stack.Config {
	r.Config.Problems = []StatusProblem{}
	doc, err := st.ReadConfigDoc()
	if err != nil {
		configProblems(r, err)
		return nil
	}
	if name, err := doc.Raw("name"); err == nil {
		if s, ok := name.(string); ok {
			r.Stack.Name = s
		}
	}
	if _, err := migrations.Migrate(doc); err != nil {
		configProblems(r, err)
		return nil
	}
	cfg, err := doc.Config()
	if err != nil {
		configProblems(r, err)
		return nil
	}
	if err := cfg.CheckFiles(st.Dir); err != nil {
		configProblems(r, err)
		return cfg
	}
	r.Config.Valid = true
	return cfg
}

func configProblems(r *StatusReport, err error) {
	var ce *stack.ConfigError
	if !errors.As(err, &ce) {
		r.Config.Error = err.Error()
		return
	}
	for _, p := range ce.Problems {
		r.Config.Problems = append(r.Config.Problems, StatusProblem{Path: p.Path, Line: p.Line, Message: p.Msg})
	}
}

func statusVersions(r *StatusReport, st *stack.Stack, opts StatusOptions) {
	r.Versions = StatusVersions{CLI: opts.CLIVersion, Schema: opts.Migrations.Target, Gate: GateUnknown, PendingMigrations: []string{}}
	v, err := st.CheckVersions(opts.CLIVersion, opts.Migrations)
	if err != nil {
		r.Versions.Error = err.Error()
		return
	}
	r.Versions.StackCLI, r.Versions.StackSchema, r.Versions.ConfigSchema = v.StackCLI, v.StackSchema, v.ConfigSchema
	for _, m := range v.Pending {
		r.Versions.PendingMigrations = append(r.Versions.PendingMigrations, m.Summary)
	}
	switch {
	case v.Newer():
		r.Versions.Gate = GateStackNewer
	case v.MigrationErr() != nil:
		r.Versions.Gate = GateUnsupportedSchema
	case len(v.Pending) > 0:
		r.Versions.Gate = GateMigrationsPending
	default:
		r.Versions.Gate = GateOK
	}
}

// statusImages checks each built image at the tag state.json records, or
// at its dev build's tag while a dev variant runs it. The first docker
// failure stops the checks, since the rest would fail the same way. Each
// check has Ps's timeout, so a wedged daemon can't hang status.
func statusImages(ctx context.Context, d *Deps, r *StatusReport, state *stack.State, cfg *stack.Config) {
	r.Images = []StatusImage{}
	dev := devImages(cfg)
	for _, img := range catalog.Images() {
		if !img.Built() {
			continue
		}
		built, isDev := dev[img.Name]
		si := StatusImage{Name: img.Name, Component: img.Component, Dev: isDev}
		tag := state.Images[img.Name]
		switch {
		case isDev && built:
			tag = state.DevImages[img.Name]
		case isDev:
			tag = ""
		}
		if tag != "" {
			si.Ref = img.Repository() + ":" + tag
		}
		if si.Ref != "" && r.ImagesError == "" {
			ictx, cancel := context.WithTimeout(ctx, docker.PsTimeout)
			ok, err := d.Docker.ImageExists(ictx, si.Ref)
			if ctx.Err() == nil && errors.Is(ictx.Err(), context.DeadlineExceeded) {
				err = fmt.Errorf("docker image inspect did not finish within %s", docker.PsTimeout)
			}
			cancel()
			if err != nil {
				r.ImagesError = err.Error()
			} else {
				si.Present = &ok
			}
		}
		r.Images = append(r.Images, si)
	}
}

// devImages are the images that the config's dev variants replace, mapped
// to whether the variant runs the image's local build (true) or a
// third-party image (false), as render picks them.
func devImages(cfg *stack.Config) map[string]bool {
	images := map[string]bool{}
	if cfg == nil {
		return images
	}
	for _, name := range cfg.Dev.Services {
		v, ok := catalog.LookupDevVariant(name)
		if !ok {
			continue
		}
		for _, svc := range v.Services {
			if s, ok := catalog.LookupService(svc); ok {
				images[s.Image] = v.Image == ""
			}
		}
	}
	return images
}

func statusServices(ctx context.Context, d *Deps, r *StatusReport, composeErr error) {
	r.Services = []StatusService{}
	switch {
	case composeErr != nil:
		r.ServicesError = composeErr.Error()
		return
	case d.Compose == nil:
		r.ServicesError = "the stack hasn't been rendered yet; run pic-sure up"
		return
	}
	ps, err := d.Compose.Ps(ctx)
	if err != nil {
		r.ServicesError = err.Error()
		return
	}
	r.Services = StatusServices(ps)
}

// StatusServices is compose ps's containers as status reports them, sorted
// by service, then container. `ps --json` shares the shape.
func StatusServices(ps []docker.ComposeService) []StatusService {
	services := []StatusService{}
	for _, s := range ps {
		services = append(services, StatusService{
			Service: s.Service, Container: s.Name, State: s.State,
			Health: s.Health, Status: s.Status, ExitCode: s.ExitCode,
		})
	}
	sort.Slice(services, func(i, j int) bool {
		a, b := services[i], services[j]
		return a.Service < b.Service || a.Service == b.Service && a.Container < b.Container
	})
	return services
}

// statusMigrationsTimeout bounds the migration check's compose and
// database calls, so a wedged daemon can't hang status.
const statusMigrationsTimeout = 30 * time.Second

// statusMigrations checks the Flyway histories when both databases are
// running and healthy. A remote database isn't queried, since status reaches no
// further than the local daemon.
func statusMigrations(ctx context.Context, d *Deps, r *StatusReport, st *stack.Stack, cfg *stack.Config) {
	if cfg == nil || d.Compose == nil || r.ServicesError != "" {
		return
	}
	if cfg.DB.Mode == stack.DBRemote {
		r.Migrations.Error = "not checked for a remote database"
		return
	}
	healthy := func(service string) bool {
		return slices.ContainsFunc(r.Services, func(s StatusService) bool {
			return s.Service == service && s.State == "running" && s.Health == "healthy"
		})
	}
	if !healthy(picsureDB) || !healthy(dictionaryDB) {
		r.Migrations.Error = "the databases aren't running and healthy"
		return
	}
	sec, err := st.LoadSecrets()
	if err != nil {
		r.Migrations.Error = err.Error()
		return
	}
	ctx, cancel := context.WithTimeout(ctx, statusMigrationsTimeout)
	defer cancel()
	switch ok, err := MigrationsUpToDate(ctx, d, cfg, sec); {
	case err != nil:
		r.Migrations.Error = err.Error()
	case ok:
		r.Migrations.Status = MigrationsStatusUpToDate
	default:
		r.Migrations.Status = MigrationsStatusPending
	}
}

func statusToken(d *Deps, r *StatusReport, st *stack.Stack) {
	sec, err := st.LoadSecrets()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return
	case err != nil:
		r.Token.Error = err.Error()
		return
	}
	if exp := sec.IntrospectionTokenExpiry; !exp.IsZero() {
		exp = exp.UTC()
		r.Token.ExpiresAt = &exp
		r.Token.Expired = !d.Clock.Now().Before(exp)
	}
}

// auth0URLs are the URLs the frontend sends Auth0 for this stack: it logs
// in through /login/loading/ on its own origin.
func auth0URLs(cfg *stack.Config) *StatusAuth0 {
	host := cfg.Network.Hostname
	if cfg.Network.HTTPSPort != 443 {
		host = net.JoinHostPort(host, strconv.Itoa(cfg.Network.HTTPSPort))
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	origin := "https://" + host
	a := &StatusAuth0{
		Needed:      cfg.Auth.Mode != stack.AuthOpen,
		CallbackURL: origin + "/login/loading/",
		LogoutURL:   origin,
		WebOrigin:   origin,
	}
	if v, ok := catalog.LookupDevVariant("httpd-hmr"); ok && slices.Contains(cfg.Dev.Services, v.Name) {
		dev := render.HMROrigin(cfg.Network.DevPorts.Base + v.Port)
		a.DevCallbackURL, a.DevLogoutURL, a.DevWebOrigin = dev+"/login/loading/", dev, dev
	}
	return a
}
