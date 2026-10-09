package render

import (
	"bytes"
	"errors"
	"flag"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rogpeppe/go-internal/txtar"
	"go.yaml.in/yaml/v3"

	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

var update = flag.Bool("update", false, "rewrite the render goldens in testdata/golden")

// goldenCase is one stack of the golden matrix (§11).
type goldenCase struct {
	name       string
	auth       stack.AuthMode
	dev        []string
	remoteDB   bool
	sharedHPDS bool
	proxy      bool
	overrides  bool // services.* overrides, plus testdata/overrides.yaml for compose config
	trust      bool
}

// goldenCases cover every pair of values of auth mode, dev services
// (none, hpds, httpd-hmr), db mode, hpds data, proxy and overrides, in nine
// stacks, plus one with everything on and one with the httpd dev variant.
var goldenCases = []goldenCase{
	{"required-remote-overrides", stack.AuthRequired, nil, true, false, false, true, false},
	{"required-dev-hpds-shared-proxy", stack.AuthRequired, []string{"hpds"}, false, true, true, false, false},
	{"required-dev-hmr-remote-proxy-overrides", stack.AuthRequired, []string{"httpd-hmr"}, true, false, true, true, false},
	{"open-remote-proxy", stack.AuthOpen, nil, true, false, true, false, false},
	{"open-dev-hpds-proxy-overrides", stack.AuthOpen, []string{"hpds"}, false, false, true, true, false},
	{"open-dev-hmr-remote-shared", stack.AuthOpen, []string{"httpd-hmr"}, true, true, false, false, false},
	{"explore-shared", stack.AuthExplore, nil, false, true, false, false, false},
	{"explore-dev-hpds-remote-overrides", stack.AuthExplore, []string{"hpds"}, true, false, false, true, false},
	{"explore-dev-hmr-shared-proxy-overrides", stack.AuthExplore, []string{"httpd-hmr"}, false, true, true, true, false},
	{"everything", stack.AuthRequired, allDev(), true, true, true, true, true},
	{"open-dev-httpd-trust", stack.AuthOpen, []string{"httpd"}, false, false, false, false, true},
}

// allDev is every dev variant but httpd, which can't be on with httpd-hmr.
func allDev() []string {
	var out []string
	for _, v := range catalog.DevVariants() {
		if v.Name != "httpd" {
			out = append(out, v.Name)
		}
	}
	return out
}

const goldenDir = "/stacks/golden"

func goldenConfig(c goldenCase) *stack.Config {
	cfg := stack.DefaultConfig()
	cfg.Name = "golden"
	cfg.Network.HTTPPort = 8080
	cfg.Network.HTTPSPort = 8443
	cfg.Auth.Mode = c.auth
	cfg.Auth.Auth0.ClientID = "golden-client-id"
	cfg.Auth.AdminEmail = "admin@example.com"
	cfg.Email.User = "ops@example.org"
	cfg.Dev.Services = slices.Clone(c.dev)
	cfg.Components.Frontend.Source = "../PIC-SURE-Frontend"
	if c.remoteDB {
		cfg.DB.Mode = stack.DBRemote
		cfg.DB.Remote = stack.RemoteDB{Host: "db.example.org", Port: 3307, RootUser: "admin"}
	}
	if c.sharedHPDS {
		cfg.HPDS.Data = stack.HPDSShared
		cfg.HPDS.SharedName = "nhanes"
	}
	if c.proxy {
		cfg.Proxy = stack.Proxy{HTTP: "http://proxy.example.org:3128", HTTPS: "http://proxy.example.org:3128", NoProxy: "intranet.example.org"}
	}
	if c.overrides {
		cfg.Services = map[string]stack.ServiceOverride{
			"hpds":       {JavaOpts: "-Xmx2g", Env: map[string]string{"ID_BATCH_SIZE": "500"}},
			"psama":      {Env: map[string]string{"SYSTEM_NAME": "Golden $HOME"}},
			"picsure-db": {Env: map[string]string{"MYSQL_EXTRA": "1"}},
		}
	}
	return &cfg
}

func goldenInput(c goldenCase) Input {
	st := &stack.State{StackID: "0123456789abcdef0123456789abcdef", Images: map[string]string{"node": "24.19.0-alpine3.23"}, DevImages: map[string]string{}}
	for _, img := range catalog.Images() {
		if img.Built() {
			st.Images[img.Name] = "abc123def456"
			st.DevImages[img.Name] = "dev-golden-abc123def456"
		}
	}
	in := Input{
		StackDir: goldenDir,
		Config:   goldenConfig(c),
		State:    st,
		Sources: map[string]string{
			catalog.PicSure:    "/home/op/.cache/pic-sure/src/pic-sure/0123456789abcdef0123456789abcdef01234567",
			catalog.Migrations: "/home/op/.cache/pic-sure/src/PIC-SURE-Migrations/89abcdef0123456789abcdef0123456789abcdef",
		},
		CustomTrust: c.trust,
		HostUser:    "1000:1000",
	}
	if c.sharedHPDS {
		// hpds.profile is empty, so this is the profile hpds runs with.
		in.SharedProfile = "bch-dev"
	}
	return in
}

// goldenSecrets are distinctive values for every secret, so a test can look
// for them in the output.
func goldenSecrets() *stack.Secrets {
	return &stack.Secrets{
		DBRootPassword:            "SECRETdbroot0000000000000",
		DBRemoteRootPassword:      "SECRETdbremoteroot0000000",
		DBPicsurePassword:         "SECRETdbpicsure000000000",
		DBAuthPassword:            "SECRETdbauth000000000000",
		DBAirflowPassword:         "SECRETdbairflow000000000",
		DictionaryDBPassword:      "SECRETdbdictionary000000",
		Auth0ClientSecret:         "SECRETauth0clientsecret0000000000000000",
		QueryServiceInternalToken: "SECRETquerytoken",
		PicsureApplicationToken:   "SECRETapptoken",
		LoggingAPIKey:             "SECRETloggingkey",
		AggregateObfuscationSalt:  "SECRETsalt",
		IntrospectionToken:        "SECRETintrospection",
		ApplicationUUID:           "5ec2e7a1-0000-4000-8000-000000000001",
		ResourceUUID:              "5ec2e7a1-0000-4000-8000-000000000002",
		VisualizationUUID:         "5ec2e7a1-0000-4000-8000-000000000003",
		EmailPassword:             "SECRETemail",
	}
}

func TestGoldenConfigsValidate(t *testing.T) {
	for _, c := range goldenCases {
		if err := goldenConfig(c).Validate(); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

func TestGoldens(t *testing.T) {
	var secrets []string
	for _, c := range []goldenCase{{}, {remoteDB: true}} { // the local and the remote root password
		env, err := ComposeEnv(goldenConfig(c), goldenSecrets())
		if err != nil {
			t.Fatal(err)
		}
		for _, kv := range env {
			if _, v, _ := strings.Cut(kv, "="); v != "" {
				secrets = append(secrets, v)
			}
		}
	}
	names := map[string]bool{}
	for _, c := range goldenCases {
		names[c.name+".txtar"] = true
		t.Run(c.name, func(t *testing.T) {
			files, err := Render(goldenInput(c))
			if err != nil {
				t.Fatal(err)
			}
			got := txtar.Format(goldenArchive(t, files))
			for _, s := range secrets {
				if bytes.Contains(got, []byte(s)) {
					t.Errorf("the render holds the secret %q", s)
				}
			}
			file := filepath.Join("testdata", "golden", c.name+".txtar")
			if *update {
				if err := os.WriteFile(file, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("%v (run go test ./internal/render -run TestGoldens -update)", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s differs from the render; if the change is intended, rerun with -update", file)
			}
		})
	}
	entries, err := os.ReadDir(filepath.Join("testdata", "golden"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !names[e.Name()] {
			t.Errorf("testdata/golden/%s belongs to no case; delete it", e.Name())
		}
	}
}

// TestGoldensMatchCatalog checks each golden stack's compose file against
// the catalog, as templates_test does for the sample data.
func TestGoldensMatchCatalog(t *testing.T) {
	for _, c := range goldenCases {
		in := goldenInput(c)
		d, m, _, err := buildData(in)
		if err != nil {
			t.Fatal(err)
		}
		files, err := Render(in)
		if err != nil {
			t.Fatal(err)
		}
		var f composeFile
		if err := yaml.Unmarshal(files[0].Data, &f); err != nil {
			t.Fatal(err)
		}
		checkCompose(t, f, d, m, c.dev)
	}
}

// goldenArchive is the render as one txtar file, paths relative to render/.
// It leaves out the files copied unchanged from templates/files, after
// checking they are copies.
func goldenArchive(t *testing.T, files []File) *txtar.Archive {
	t.Helper()
	a := &txtar.Archive{}
	for _, f := range files {
		rel := strings.TrimPrefix(f.Path, Dir+"/")
		if src, err := fs.ReadFile(templateFS(), rel); err == nil {
			if !bytes.Equal(src, f.Data) {
				t.Errorf("%s isn't a copy of its template", rel)
			}
			continue
		}
		a.Files = append(a.Files, txtar.File{Name: rel, Data: f.Data})
	}
	return a
}

// TestGoldensComposeConfig runs docker compose config --quiet on every
// golden compose file, with the case's overrides file. It needs only the
// docker CLI, no daemon state.
func TestGoldensComposeConfig(t *testing.T) {
	needCompose(t)
	overrides, err := filepath.Abs(filepath.Join("testdata", "overrides.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range goldenCases {
		a, err := txtar.ParseFile(filepath.Join("testdata", "golden", c.name+".txtar"))
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		file := filepath.Join(dir, "compose.yaml")
		if err := os.WriteFile(file, a.Files[0].Data, 0o600); err != nil {
			t.Fatal(err)
		}
		args := []string{"compose", "-f", file}
		if c.overrides {
			args = append(args, "-f", overrides)
		}
		cmd := exec.Command("docker", append(args, "--project-directory", dir, "--env-file", os.DevNull, "config", "--quiet")...)
		env, err := ComposeEnv(goldenConfig(c), goldenSecrets())
		if err != nil {
			t.Fatal(err)
		}
		cmd.Env = append(os.Environ(), env...)
		if b, err := cmd.CombinedOutput(); err != nil || len(b) > 0 {
			t.Errorf("%s: docker compose config: %v\n%s", c.name, err, b)
		}
	}
}

func TestComposeEnv(t *testing.T) {
	sec := goldenSecrets()
	vars := func(c goldenCase) map[string]string {
		t.Helper()
		env, err := ComposeEnv(goldenConfig(c), sec)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]string{}
		for _, kv := range env {
			k, v, _ := strings.Cut(kv, "=")
			m[k] = v
		}
		return m
	}

	local := vars(goldenCase{})
	for _, name := range secretVars {
		if local[name] == "" {
			t.Errorf("%s is empty", name)
		}
	}
	if len(local) != len(secretVars) {
		t.Errorf("without a proxy the env should be just the secrets: %v", slices.Sorted(maps.Keys(local)))
	}
	if local["DB_ROOT_PASSWORD"] != string(sec.DBRootPassword) || local["PICSURE_RESOURCE_ID"] != sec.ResourceUUID {
		t.Errorf("local env: %v", local)
	}
	if remote := vars(goldenCase{remoteDB: true}); remote["DB_ROOT_PASSWORD"] != string(sec.DBRemoteRootPassword) {
		t.Errorf("remote DB_ROOT_PASSWORD %q, want the remote root password", remote["DB_ROOT_PASSWORD"])
	}

	proxied := vars(goldenCase{proxy: true})
	if proxied["HTTP_PROXY"] != "http://proxy.example.org:3128" || !strings.Contains(proxied["NO_PROXY"], "psama") {
		t.Errorf("proxy env: HTTP_PROXY %q NO_PROXY %q", proxied["HTTP_PROXY"], proxied["NO_PROXY"])
	}

	// With only proxy.http, HTTPS_PROXY and ALL_PROXY are still set, empty,
	// in both cases, so neither the compose file's ${HTTPS_PROXY} nor compose
	// itself uses a proxy from the caller's env.
	cfg := goldenConfig(goldenCase{})
	cfg.Proxy.HTTP = "http://proxy.example.org:3128"
	env, err := ComposeEnv(cfg, sec)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"HTTPS_PROXY=", "https_proxy=", "ALL_PROXY=", "all_proxy="} {
		if !slices.Contains(env, want) {
			t.Errorf("%s should be set empty: %v", strings.TrimSuffix(want, "="), env[len(secretVars):])
		}
	}
}

func TestProxyCredentialsStayOutOfCompose(t *testing.T) {
	in := goldenInput(goldenCase{})
	in.Config.Proxy = stack.Proxy{HTTP: "http://op:pr0xyPass@proxy.example.org:3128", HTTPS: "http://op:pr0xyPass@proxy.example.org:3128"}
	files, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if bytes.Contains(f.Data, []byte("pr0xyPass")) {
			t.Errorf("%s holds the proxy password", f.Path)
		}
	}
	compose := string(files[0].Data)
	if !strings.Contains(compose, "-Dhttps.proxyHost=proxy.example.org") {
		t.Errorf("psama's JAVA_OPTS lacks the proxy properties")
	}
}

func TestRenderValues(t *testing.T) {
	c := goldenCase{remoteDB: true, overrides: true, trust: true, proxy: true, dev: []string{"psama", "httpd-hmr"}}
	files, err := Render(goldenInput(c))
	if err != nil {
		t.Fatal(err)
	}
	var f composeFile
	if err := yaml.Unmarshal(files[0].Data, &f); err != nil {
		t.Fatal(err)
	}
	psama := f.Services["psama"]
	if psama.Image != "hms-dbmi/pic-sure-psama:dev-golden-abc123def456" {
		t.Errorf("dev psama image %q", psama.Image)
	}
	java := psama.Environment["JAVA_OPTS"]
	iProxy, iTrust, iDebug := strings.Index(java, "-Dhttp.proxyHost"), strings.Index(java, trustJavaOpts), strings.Index(java, debugJavaOpts)
	if iProxy < 0 || iTrust < iProxy || iDebug < iTrust {
		t.Errorf("psama JAVA_OPTS should end with proxy, trust and debug options in that order: %q", java)
	}
	if psama.Environment["SYSTEM_NAME"] != "Golden $$HOME" {
		t.Errorf("psama SYSTEM_NAME %q, want the override", psama.Environment["SYSTEM_NAME"])
	}
	if _, ok := f.Services["picsure-db"]; ok {
		t.Errorf("an override for picsure-db added it to a remote-DB stack")
	}
	if hpds := f.Services["hpds"]; hpds.Environment["JAVA_OPTS"] != "-Xmx2g" || hpds.Image != "hms-dbmi/pic-sure-hpds:abc123def456" {
		t.Errorf("hpds: image %q JAVA_OPTS %q", hpds.Image, hpds.Environment["JAVA_OPTS"])
	}
	hmr := f.Services["httpd"]
	if hmr.Image != "node:24.19.0-alpine3.23" || hmr.User != "1000:1000" || hmr.Environment["VITE_ORIGIN"] != "http://127.0.0.1:3000" ||
		hmr.Environment["VITE_AUTH_PROVIDER_MODULE_GOOGLE_CLIENTID"] != "golden-client-id" {
		t.Errorf("httpd-hmr: image %q env %v", hmr.Image, hmr.Environment)
	}
	if !slices.Contains(hmr.Ports, "127.0.0.1:15006:3000") {
		t.Errorf("httpd-hmr ports %v", hmr.Ports)
	}
	if !bytes.Contains(files[0].Data, []byte(`"/stacks/PIC-SURE-Frontend"`)) {
		t.Errorf("the relative frontend source isn't resolved against the stack dir")
	}
}

// A volume that already exists keeps its labels, so its compose config hash
// doesn't change and compose doesn't offer to recreate it. Every volume the
// templates declare takes them.
func TestExistingVolumesKeepTheirLabels(t *testing.T) {
	c := goldenCase{dev: allDev(), sharedHPDS: true, trust: true}
	parse := func(in Input) composeFile {
		t.Helper()
		files, err := Render(in)
		if err != nil {
			t.Fatal(err)
		}
		var f composeFile
		if err := yaml.Unmarshal(files[0].Data, &f); err != nil {
			t.Fatal(err)
		}
		return f
	}
	in := goldenInput(c)
	in.ExistingVolumeLabels = map[string]map[string]string{}
	for key, v := range parse(goldenInput(c)).Volumes {
		if !v.External {
			in.ExistingVolumeLabels[key] = map[string]string{stack.LabelStack: "golden", stack.LabelStackDir: "/old/" + key}
		}
	}
	if len(in.ExistingVolumeLabels) < 10 {
		t.Fatalf("only %d volumes", len(in.ExistingVolumeLabels))
	}
	for key, v := range parse(in).Volumes {
		if want, ok := in.ExistingVolumeLabels[key]; ok && !maps.Equal(v.Labels, want) {
			t.Errorf("%s labels %v, want its existing %v", key, v.Labels, want)
		}
	}

	in = goldenInput(c)
	in.ExistingVolumeLabels = map[string]map[string]string{"hpds-data": {stack.LabelStack: "golden"}}
	if got := parse(in).Volumes["hpds-csv"].Labels[stack.LabelStackDir]; got != goldenDir {
		t.Errorf("a new volume's stack-dir %q, want %q", got, goldenDir)
	}
}

// busybox wget ignores no_proxy, so a wget healthcheck in a container that
// has the proxy variables must turn the proxy off, or the probe goes to the
// proxy and the service never becomes healthy.
func TestProxiedHealthchecksSkipTheProxy(t *testing.T) {
	files, err := Render(goldenInput(goldenCase{proxy: true, dev: []string{"httpd-hmr"}}))
	if err != nil {
		t.Fatal(err)
	}
	var f composeFile
	if err := yaml.Unmarshal(files[0].Data, &f); err != nil {
		t.Fatal(err)
	}
	proxied := 0
	for name, svc := range f.Services {
		if svc.Environment["HTTP_PROXY"] == "" {
			continue
		}
		proxied++
		if test := strings.Join(svc.Healthcheck.Test, " "); strings.Contains(test, "wget") && !strings.Contains(test, "wget -Y off") {
			t.Errorf("%s: healthcheck %q would go through the proxy", name, test)
		}
	}
	if proxied < 2 {
		t.Errorf("%d services have the proxy variables, want psama and httpd-hmr at least", proxied)
	}
}

func TestSharedProfile(t *testing.T) {
	for _, c := range []struct{ configured, recorded, want string }{
		{"", "genomic", "genomic"},
		{"other", "genomic", "other"},
	} {
		in := goldenInput(goldenCase{sharedHPDS: true})
		in.Config.HPDS.Profile = c.configured
		in.SharedProfile = c.recorded
		files, err := Render(in)
		if err != nil {
			t.Fatal(err)
		}
		var f composeFile
		if err := yaml.Unmarshal(files[0].Data, &f); err != nil {
			t.Fatal(err)
		}
		if got := f.Services["hpds"].Environment["SPRING_PROFILES_ACTIVE"]; got != c.want {
			t.Errorf("hpds.profile %q, recorded %q: profile %q, want %q", c.configured, c.recorded, got, c.want)
		}
	}
}

func TestViteEnv(t *testing.T) {
	cfg := stack.DefaultConfig()
	env := ViteEnv(&cfg)
	if env["VITE_OPEN"] != "false" || env["VITE_THEME"] != "picsure" || env["VITE_AUTH0_TENANT"] != "avillachlab" || env["VITE_ORIGIN"] != "http://localhost" {
		t.Errorf("required mode: %v", env)
	}
	if _, ok := env["VITE_AUTH_PROVIDER_MODULE_GOOGLE"]; ok {
		t.Errorf("no client id, but the Google login is configured")
	}
	cfg.Auth.Mode = stack.AuthExplore
	cfg.Auth.TOS = true
	cfg.Frontend.Analytics.GoogleTagManagerID = "GTM-X"
	env = ViteEnv(&cfg)
	if env["VITE_OPEN_EXPLORER"] != "true" || env["VITE_DISCOVER"] != "false" || env["VITE_ENFORCE_TOS_ACCEPT"] != "true" || env["VITE_GOOGLE_TAG_MANAGER_ID"] != "GTM-X" {
		t.Errorf("explore mode: %v", env)
	}
}

func TestRenderRefuses(t *testing.T) {
	cases := []struct {
		name  string
		edit  func(*Input)
		error string
	}{
		{"relative stack dir", func(in *Input) { in.StackDir = "stacks/golden" }, "not an absolute path"},
		{"colon in stack dir", func(in *Input) { in.StackDir = "/stacks/a:b" }, "colon"},
		{"colon in a source", func(in *Input) { in.Config.Components.PicSure.Source = "/src/a:b" }, "colon"},
		{"no cache tree", func(in *Input) { delete(in.Sources, catalog.Migrations) }, "no source tree for migrations"},
		{"bad project", func(in *Input) { in.Config.Components.Migrations.Project = "../x" }, "one directory name"},
		{"hmr without source", func(in *Input) {
			in.Config.Dev.Services = []string{"httpd-hmr"}
			in.Config.Components.Frontend.Source = ""
		}, "components.frontend.source"},
		{"httpd and hmr", func(in *Input) { in.Config.Dev.Services = []string{"httpd", "httpd-hmr"} }, "enable one"},
		{"unknown variant", func(in *Input) { in.Config.Dev.Services = []string{"nope"} }, `no dev variant "nope"`},
		{"unbuilt dev image", func(in *Input) {
			in.Config.Dev.Services = []string{"query"}
			in.State.DevImages = nil
		}, "no dev image of pic-sure-hpds-query-service"},
		{"unknown service override", func(in *Input) {
			in.Config.Services = map[string]stack.ServiceOverride{"nope": {JavaOpts: "-Xmx1g"}}
		}, "services.nope"},
		{"java_opts nothing reads", func(in *Input) {
			in.Config.Services = map[string]stack.ServiceOverride{"dictionary-api": {JavaOpts: "-Xmx8g"}}
		}, "services.dictionary-api.java_opts"},
		{"no image tag", func(in *Input) { delete(in.State.Images, "pic-sure-gateway") }, "no image pic-sure-gateway"},
	}
	for _, c := range cases {
		in := goldenInput(goldenCase{})
		c.edit(&in)
		_, err := Render(in)
		if err == nil || !strings.Contains(err.Error(), c.error) {
			t.Errorf("%s: error %v, want one containing %q", c.name, err, c.error)
		}
	}
}

func TestWrite(t *testing.T) {
	st, err := stack.Create(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	in := goldenInput(goldenCase{})
	in.StackDir = st.Dir
	files, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	// Older versions rendered settings.xml; the next render removes it.
	settings := FilesDir + "/maven/settings.xml"
	if err := Write(st, append(slices.Clone(files), File{Path: settings, Data: []byte("<settings/>"), Perm: 0o600})); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(st.Path(settings)); err != nil {
		t.Fatal(err)
	}
	// A file under render/ that pic-sure didn't create survives a re-render.
	if err := os.WriteFile(st.Path(FilesDir+"/mine.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Write(st, files); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(st.Path(settings)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a file the last render didn't produce should be gone: %v", err)
	}
	if _, err := os.Stat(st.Path(FilesDir + "/mine.txt")); err != nil {
		t.Errorf("an operator's file was removed: %v", err)
	}
	got, err := os.ReadFile(st.Path(ComposeFile))
	if err != nil || !bytes.Equal(got, files[0].Data) {
		t.Errorf("compose.yaml not written: %v", err)
	}
}

func TestShellScriptsAreExecutable(t *testing.T) {
	files, err := Render(goldenInput(goldenCase{}))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, f := range files {
		if strings.HasSuffix(f.Path, ".sh") {
			n++
			if f.Perm != 0o755 {
				t.Errorf("%s: mode %o, want 0755", f.Path, f.Perm)
			}
		}
	}
	if n == 0 {
		t.Error("no shell scripts rendered")
	}
}
