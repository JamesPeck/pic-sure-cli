package render

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/netproxy"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

// Dir is the stack-relative directory render writes to. ComposeFile is the
// compose file in it, and FilesDir the static files.
const (
	Dir         = ".pic-sure/render"
	ComposeFile = Dir + "/compose.yaml"
	FilesDir    = Dir + "/files"
)

// Input is everything a render depends on. Render reads nothing else.
type Input struct {
	// StackDir is the stack's absolute directory (stack.Stack.Dir).
	StackDir string
	Config   *stack.Config
	// State supplies the image tags: Images for every image the stack runs
	// (node's is the tag from the frontend's .nvmrc, needed only with
	// httpd-hmr), and DevImages for the services of enabled dev variants.
	State *stack.State
	// Sources maps a component to its host cache tree (src/<repo>/<sha>),
	// used when the config gives the component no local source. Render
	// needs pic-sure's and migrations'.
	Sources map[string]string
	// CustomTrust says the custom certs directory holds certs (§9.5).
	CustomTrust bool
	// SharedProfile is the HPDS profile recorded with the shared data set,
	// used with hpds.data: shared when hpds.profile is empty.
	SharedProfile string
}

// File is one file render produces. Path is relative to the stack dir.
type File struct {
	Path string
	Data []byte
	Perm fs.FileMode
}

// Render produces the stack's compose file, first, and its static files
// (§6.4). It does no I/O; Write saves the result.
func Render(in Input) ([]File, error) {
	d, mode, p, err := buildData(in)
	if err != nil {
		return nil, err
	}
	compose, err := renderCompose(d, composeFragments(mode, in.Config.Dev.Services))
	if err != nil {
		return nil, fmt.Errorf("render compose.yaml: %w", err)
	}
	if err := checkRendered(compose, d); err != nil {
		return nil, err
	}
	out := []File{{Path: ComposeFile, Data: compose, Perm: 0o644}}
	files, err := renderFiles(d)
	if err != nil {
		return nil, err
	}
	out = append(out, files...)
	if s := p.MavenSettings(); s != nil {
		// It holds the proxy credentials.
		out = append(out, File{Path: FilesDir + "/maven/settings.xml", Data: s, Perm: 0o600})
	}
	return out, nil
}

// buildData fills in templateData from the input.
func buildData(in Input) (templateData, catalog.Mode, *netproxy.Proxy, error) {
	cfg, st := in.Config, in.State
	if cfg == nil || st == nil {
		return templateData{}, catalog.Mode{}, nil, errors.New("render: no config or state")
	}
	mode := modeOf(cfg, in.CustomTrust)
	p, err := newProxy(cfg)
	if err != nil {
		return templateData{}, mode, nil, err
	}
	stackDir, err := bindSource("the stack directory", in.StackDir)
	if err != nil {
		return templateData{}, mode, nil, err
	}
	flags := stack.DeriveAuthFlags(cfg.Auth.Mode)
	d := templateData{
		Name:              cfg.Name,
		Labels:            map[string]string{stack.LabelStack: cfg.Name, stack.LabelStackDir: stackDir},
		FilesDir:          path.Join(stackDir, FilesDir),
		HTTPPort:          cfg.Network.HTTPPort,
		HTTPSPort:         cfg.Network.HTTPSPort,
		Images:            images(st.Images),
		DevImages:         map[string]string{},
		DevPorts:          map[string]int{},
		MigrationsProject: cfg.Components.Migrations.Project,
		DB:                dbTarget{Host: "picsure-db", Port: 3306, RootUser: "root"},
		Auth: authSettings{
			Auth0Tenant:          cfg.Auth.Auth0.Tenant,
			OpenIDP:              flags.OpenIDPProvider,
			GatewayOpenAccess:    flags.GatewayOpenAccess,
			PublicAccess:         flags.PublicAccess,
			ConsentAuthorization: cfg.Auth.ConsentAuthorization,
			TOS:                  cfg.Auth.TOS,
		},
		DocsEnabled: cfg.Frontend.DocsEnabled,
		EmailUser:   cfg.Email.User,
		HPDSProfile: cfg.HPDS.Profile,
		SharedHPDS:  cfg.HPDS.SharedName,
		JavaOpts:    map[string]string{"hpds": cfg.HPDS.JavaOpts},
		JVMExtra:    map[string]string{},
		Proxy:       p.Enabled(),
		ServiceEnv:  map[string]map[string]string{},
	}
	if d.HPDSProfile == "" && mode.SharedHPDS {
		d.HPDSProfile = in.SharedProfile
	}
	if mode.RemoteDB {
		r := cfg.DB.Remote
		d.DB = dbTarget{Host: r.Host, Port: r.Port, RootUser: r.RootUser}
	}
	if err := checkPathElem("components.migrations.project", d.MigrationsProject); err != nil {
		return d, mode, nil, err
	}
	if d.PicSureSrc, err = source(in, catalog.PicSure, cfg.Components.PicSure.Source); err != nil {
		return d, mode, nil, err
	}
	if d.MigrationsSrc, err = source(in, catalog.Migrations, cfg.Components.Migrations.Source); err != nil {
		return d, mode, nil, err
	}

	extra := map[string][]string{}
	if p.Enabled() {
		// psama is the one Java service that calls out, to Auth0 (§9.10).
		extra["psama"] = append(extra["psama"], p.JVMOpts()...)
	}
	if mode.CustomTrust {
		extra["psama"] = append(extra["psama"], trustJavaOpts)
	}
	if err := addDev(in, &d, extra); err != nil {
		return d, mode, nil, err
	}
	for svc, opts := range extra {
		d.JVMExtra[svc] = strings.Join(opts, " ")
	}

	have := map[string]bool{}
	for _, s := range catalog.ServicesIn(mode) {
		have[s.Name] = true
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Services)) {
		o := cfg.Services[name]
		if _, ok := catalog.LookupService(name); !ok {
			return d, mode, nil, fmt.Errorf("services.%s: no such service", name)
		}
		if !have[name] {
			continue // not in this stack's mode, e.g. picsure-db with a remote DB
		}
		if o.JavaOpts != "" {
			d.JavaOpts[name] = o.JavaOpts
		}
		if o.Env != nil {
			d.ServiceEnv[name] = o.Env
		}
	}
	return d, mode, p, nil
}

// addDev fills in the dev images, ports and frontend environment of the
// enabled dev variants, and adds their JDWP agents to extra.
func addDev(in Input, d *templateData, extra map[string][]string) error {
	cfg := in.Config
	enabled := cfg.Dev.Services
	if slices.Contains(enabled, "httpd") && slices.Contains(enabled, "httpd-hmr") {
		return errors.New("dev.services: httpd and httpd-hmr both replace httpd; enable one")
	}
	for _, name := range enabled {
		v, ok := catalog.LookupDevVariant(name)
		if !ok {
			return fmt.Errorf("dev.services: no dev variant %q", name)
		}
		if v.Port != catalog.NoPort {
			d.DevPorts[name] = cfg.Network.DevPorts.Base + v.Port
		}
		if v.Image != "" {
			continue // it runs that image, not its own build
		}
		for _, svc := range v.Services {
			s, _ := catalog.LookupService(svc)
			img, _ := catalog.LookupImage(s.Image)
			tag := in.State.DevImages[s.Image]
			if tag == "" {
				return fmt.Errorf("dev variant %s: no dev image of %s has been built", name, s.Image)
			}
			d.DevImages[s.Image] = img.Repository() + ":" + tag
			if v.Port != catalog.NoPort {
				extra[svc] = append(extra[svc], debugJavaOpts)
			}
		}
	}
	if port, ok := d.DevPorts["httpd-hmr"]; ok {
		src := cfg.Components.Frontend.Source
		if src == "" {
			return errors.New("dev variant httpd-hmr needs components.frontend.source")
		}
		var err error
		if d.FrontendSrc, err = bindSource("components.frontend.source", absolute(in.StackDir, src)); err != nil {
			return err
		}
		d.FrontendEnv = ViteEnv(cfg)
		d.FrontendEnv["VITE_ORIGIN"] = "http://localhost:" + strconv.Itoa(port)
	}
	return nil
}

// images returns the reference of every catalog image the stack has a tag
// for, plus the pinned third-party ones.
func images(tags map[string]string) map[string]string {
	out := map[string]string{}
	for _, img := range catalog.Images() {
		tag := tags[img.Name]
		switch {
		case img.Built():
			if tag != "" {
				out[img.Name] = img.Repository() + ":" + tag
			}
		case strings.Contains(img.Ref, ":"):
			out[img.Name] = img.Ref
		case tag != "":
			out[img.Name] = img.Ref + ":" + tag
		}
	}
	return out
}

// modeOf returns the catalog mode of a stack with the config cfg.
func modeOf(cfg *stack.Config, customTrust bool) catalog.Mode {
	return catalog.Mode{
		RemoteDB:    cfg.DB.Mode == stack.DBRemote,
		SharedHPDS:  cfg.HPDS.Data == stack.HPDSShared,
		CustomTrust: customTrust,
	}
}

// newProxy resolves the config's proxy for the stack's services. Render and
// ComposeEnv both use it, so JAVA_OPTS and NO_PROXY list the same services;
// custom trust adds no service, so it is left out.
func newProxy(cfg *stack.Config) (*netproxy.Proxy, error) {
	var names []string
	for _, s := range catalog.ServicesIn(modeOf(cfg, false)) {
		names = append(names, s.Name)
	}
	p, err := netproxy.New(netproxy.Config{HTTP: cfg.Proxy.HTTP, HTTPS: cfg.Proxy.HTTPS, NoProxy: cfg.Proxy.NoProxy}, names)
	if err != nil {
		return nil, fmt.Errorf("proxy: %w", err)
	}
	return p, nil
}

// source returns the tree to bind-mount for component: its configured local
// source, else its cache tree.
func source(in Input, component, configured string) (string, error) {
	what := "components." + component + ".source"
	if configured != "" {
		return bindSource(what, absolute(in.StackDir, configured))
	}
	tree := in.Sources[component]
	if tree == "" {
		return "", fmt.Errorf("render: no source tree for %s", component)
	}
	return bindSource("the "+component+" source tree", tree)
}

// absolute resolves p against the stack dir, as config paths are.
func absolute(stackDir, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(stackDir, p)
}

// bindSource checks that p can be a bind mount's source: absolute, and
// without a colon, which the short volume syntax would split on (§6.4), or a
// line break. It returns p cleaned.
func bindSource(what, p string) (string, error) {
	switch {
	case !filepath.IsAbs(p):
		return "", fmt.Errorf("%s %q is not an absolute path", what, p)
	case strings.ContainsAny(p, ":\r\n"):
		return "", fmt.Errorf("%s %q can't be bind-mounted: it contains a colon or line break", what, p)
	}
	return filepath.Clean(p), nil
}

// checkPathElem checks that s is one directory name.
func checkPathElem(what, s string) error {
	if s == "" || s == "." || s == ".." || strings.ContainsAny(s, `/\:`) {
		return fmt.Errorf("%s must be one directory name, got %q", what, s)
	}
	return nil
}

// checkRendered checks the merged compose file: every bind source is
// absolute and colon-free, so no template can introduce a bad one, and every
// service with configured java_opts reads JAVA_OPTS, so none is dropped
// silently.
func checkRendered(compose []byte, d templateData) error {
	var f struct {
		Services map[string]struct {
			Volumes     []yaml.Node
			Environment map[string]yaml.Node
		}
	}
	if err := yaml.Unmarshal(compose, &f); err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(d.JavaOpts)) {
		if _, ok := f.Services[name].Environment["JAVA_OPTS"]; !ok {
			return fmt.Errorf("services.%s.java_opts: %s doesn't take JAVA_OPTS", name, name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(f.Services)) {
		for _, v := range f.Services[name].Volumes {
			var m struct{ Type, Source string }
			if v.Kind != yaml.MappingNode {
				continue // short syntax, which the templates use for volumes only
			}
			if err := v.Decode(&m); err != nil {
				return err
			}
			if m.Type != "bind" {
				continue
			}
			if _, err := bindSource("service "+name+": bind source", m.Source); err != nil {
				return err
			}
		}
	}
	return nil
}

// renderFiles produces render/files/ from templates/files: a .tmpl file is
// executed and loses the suffix, any other file is copied.
func renderFiles(d templateData) ([]File, error) {
	var out []File
	tfs := templateFS()
	err := fs.WalkDir(tfs, "files", func(name string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		rel := strings.TrimPrefix(name, "files/")
		var data []byte
		if strings.HasSuffix(name, ".tmpl") {
			rel = strings.TrimSuffix(rel, ".tmpl")
			data, err = executeTemplate(name, d)
		} else {
			data, err = fs.ReadFile(tfs, name)
		}
		if err != nil {
			return fmt.Errorf("render %s: %w", name, err)
		}
		out = append(out, File{Path: FilesDir + "/" + rel, Data: data, Perm: 0o644})
		return nil
	})
	return out, err
}

// ViteEnv returns the frontend's VITE_* settings for the config, without
// VITE_ORIGIN, which depends on where the frontend is served. The frontend
// build bakes them in (§7.2), and httpd-hmr gets them as its environment.
// Ported from AIO's picsure_frontend_env and the VITE_* lines of init.sh.
func ViteEnv(cfg *stack.Config) map[string]string {
	flags := stack.DeriveAuthFlags(cfg.Auth.Mode)
	b := strconv.FormatBool
	env := map[string]string{
		"VITE_OPEN":                b(flags.ViteOpen),
		"VITE_OPEN_EXPLORER":       b(flags.ViteOpenExplorer),
		"VITE_DISCOVER":            b(flags.ViteDiscover),
		"VITE_ENABLE_TOS":          b(cfg.Auth.TOS),
		"VITE_ENFORCE_TOS_ACCEPT":  b(cfg.Auth.TOS),
		"VITE_CONFIG_MODE":         "override",
		"VITE_API_CONFIG_FEATURES": "ui:featureFlag",
		"VITE_API_CONFIG_SETTINGS": "ui:setting",
		"VITE_API_CONFIG_BRANDING": "ui:branding",
		"VITE_AUTH0_TENANT":        cfg.Auth.Auth0.Tenant,
		"VITE_THEME":               cfg.Frontend.Theme,
	}
	if env["VITE_THEME"] == "" {
		env["VITE_THEME"] = "picsure"
	}
	if id := cfg.Auth.Auth0.ClientID; id != "" {
		env["VITE_AUTH_PROVIDER_MODULE_GOOGLE"] = "true"
		env["VITE_AUTH_PROVIDER_MODULE_GOOGLE_TYPE"] = "AUTH0"
		env["VITE_AUTH_PROVIDER_MODULE_GOOGLE_CLIENTID"] = id
		env["VITE_AUTH_PROVIDER_MODULE_GOOGLE_CONNECTION"] = "google-oauth2"
		env["VITE_AUTH_PROVIDER_MODULE_GOOGLE_DESCRIPTION"] = "Login"
	}
	if id := cfg.Frontend.Analytics.GoogleAnalyticsID; id != "" {
		env["VITE_GOOGLE_ANALYTICS_ID"] = id
	}
	if id := cfg.Frontend.Analytics.GoogleTagManagerID; id != "" {
		env["VITE_GOOGLE_TAG_MANAGER_ID"] = id
	}
	return env
}

// ComposeEnv returns the environment every compose call needs (§6.4), as
// NAME=value entries for docker.NewCompose: each of secretVars, plus
// proxyVars when the config sets a proxy. The values are secrets.
func ComposeEnv(cfg *stack.Config, sec *stack.Secrets) ([]string, error) {
	root := sec.DBRootPassword
	if cfg.DB.Mode == stack.DBRemote {
		root = sec.DBRemoteRootPassword
	}
	vals := map[string]string{
		"DB_ROOT_PASSWORD":             string(root),
		"DB_PICSURE_PASSWORD":          string(sec.DBPicsurePassword),
		"DB_AUTH_PASSWORD":             string(sec.DBAuthPassword),
		"DB_AIRFLOW_PASSWORD":          string(sec.DBAirflowPassword),
		"DB_DICTIONARY_PASSWORD":       string(sec.DictionaryDBPassword),
		"AUTH0_CLIENT_SECRET":          string(sec.Auth0ClientSecret),
		"PICSURE_INTROSPECTION_TOKEN":  string(sec.IntrospectionToken),
		"QUERY_SERVICE_INTERNAL_TOKEN": string(sec.QueryServiceInternalToken),
		"PICSURE_APPLICATION_TOKEN":    string(sec.PicsureApplicationToken),
		"LOGGING_API_KEY":              string(sec.LoggingAPIKey),
		"AGGREGATE_OBFUSCATION_SALT":   string(sec.AggregateObfuscationSalt),
		"PICSURE_APPLICATION_ID":       sec.ApplicationUUID,
		"PICSURE_RESOURCE_ID":          sec.ResourceUUID,
		"PICSURE_VIZ_RESOURCE_ID":      sec.VisualizationUUID,
		"EMAIL_PASSWORD":               string(sec.EmailPassword),
	}
	env := make([]string, 0, len(secretVars)+len(proxyVars))
	for _, name := range secretVars {
		env = append(env, name+"="+vals[name])
	}
	p, err := newProxy(cfg)
	if err != nil {
		return nil, err
	}
	if p.Enabled() {
		pv := map[string]string{}
		for _, kv := range p.Env() {
			k, v, _ := strings.Cut(kv, "=")
			pv[k] = v
		}
		for _, name := range proxyVars {
			env = append(env, name+"="+pv[name])
		}
	}
	return env, nil
}

// Write saves a render's files in the stack, then removes the files an
// earlier render wrote that this one didn't produce, such as settings.xml
// after the proxy is turned off. Files under render/ the CLI didn't create
// are left alone.
func Write(st *stack.Stack, files []File) error {
	want := map[string]bool{}
	for _, f := range files {
		if err := st.MkdirAll(path.Dir(f.Path), 0o755); err != nil {
			return err
		}
		if err := st.WriteFile(f.Path, f.Data, f.Perm); err != nil {
			return err
		}
		want[f.Path] = true
	}
	var stale []string
	err := fs.WalkDir(st.FS(), Dir, func(name string, e fs.DirEntry, err error) error {
		if err == nil && e.Type().IsRegular() && !want[name] && !stack.IsTempName(e.Name()) {
			stale = append(stale, name)
		}
		return err
	})
	if err != nil {
		return err
	}
	for _, name := range stale {
		if err := st.Remove(name); err != nil && !errors.Is(err, stack.ErrNotCreated) {
			return err
		}
	}
	return nil
}
