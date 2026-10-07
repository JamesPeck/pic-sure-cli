package render

import (
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
)

// sampleData returns template data for a stack in mode m with the dev
// variants dev on, the way render fills it in.
func sampleData(m catalog.Mode, dev []string, proxy bool) templateData {
	d := templateData{
		Name:              "demo",
		Labels:            map[string]string{"org.hms-dbmi.picsure.stack": "demo", "org.hms-dbmi.picsure.stack-dir": "/stacks/demo $HOME"},
		FilesDir:          "/stacks/demo/.pic-sure/render/files",
		HTTPPort:          8080,
		HTTPSPort:         8443,
		Images:            map[string]string{},
		DevImages:         map[string]string{},
		DevPorts:          map[string]int{},
		PicSureSrc:        "/cache/src/pic-sure/abc",
		MigrationsSrc:     "/cache/src/PIC-SURE-Migrations/def",
		MigrationsProject: "Baseline",
		FrontendSrc:       "/home/dev/PIC-SURE-Frontend",
		DB:                dbTarget{Host: "picsure-db", Port: 3306, RootUser: "root"},
		Auth:              authSettings{Auth0Tenant: "avillachlab", OpenIDP: true, GatewayOpenAccess: true},
		DocsEnabled:       true,
		EmailUser:         "ops@example.org",
		SharedHPDS:        "picsure-demo",
		JavaOpts:          map[string]string{"hpds": "-Xmx2g"},
		JVMExtra:          map[string]string{},
		Proxy:             proxy,
		HostUser:          "1000:1000",
		FrontendEnv:       map[string]string{"VITE_ORIGIN": "http://127.0.0.1:3000"},
		ServiceEnv:        map[string]map[string]string{"hpds": {"ID_BATCH_SIZE": "0"}},
	}
	if m.RemoteDB {
		d.DB = dbTarget{Host: "db.example.org", Port: 3307, RootUser: "admin"}
	}
	for _, img := range catalog.Images() {
		d.Images[img.Name] = img.Repository() + ":abc123def456"
		if img.Ref != "" {
			d.Images[img.Name] = img.Ref
		}
	}
	d.Images["node"] = "node:24.19.0-alpine3.23"
	if m.CustomTrust {
		d.JVMExtra["psama"] = trustJavaOpts
	}
	for _, name := range dev {
		v, _ := catalog.LookupDevVariant(name)
		for _, svc := range v.Services {
			s, _ := catalog.LookupService(svc)
			img, _ := catalog.LookupImage(s.Image)
			d.DevImages[s.Image] = img.Repository() + ":dev-demo-abc123def456"
		}
		if v.Port != catalog.NoPort {
			d.DevPorts[name] = 15000 + v.Port
			if v.Image == "" {
				for _, svc := range v.Services {
					d.JVMExtra[svc] = strings.TrimSpace(d.JVMExtra[svc] + " " + debugJavaOpts)
				}
			}
		}
	}
	return d
}

// modes returns every combination of the catalog's mode switches.
func modes() []catalog.Mode {
	var out []catalog.Mode
	for i := range 8 {
		out = append(out, catalog.Mode{RemoteDB: i&1 != 0, SharedHPDS: i&2 != 0, CustomTrust: i&4 != 0})
	}
	return out
}

// devSets returns the dev variant selections to test: none, each one alone,
// and every variant at once except httpd, which can't be on with httpd-hmr.
func devSets() [][]string {
	sets := [][]string{nil}
	var all []string
	for _, v := range catalog.DevVariants() {
		sets = append(sets, []string{v.Name})
		if v.Name != "httpd" {
			all = append(all, v.Name)
		}
	}
	return append(sets, all)
}

type composeFile struct {
	Name     string
	Services map[string]struct {
		Image       string
		User        string
		Labels      map[string]string
		Profiles    []string
		Restart     string
		NetworkMode string `yaml:"network_mode"`
		Networks    []string
		Ports       []string
		Volumes     []yaml.Node
		Environment map[string]string
	}
	Networks map[string]struct {
		Internal bool
		Labels   map[string]string
	}
	Volumes map[string]struct {
		External bool
		Name     string
		Labels   map[string]string
	}
}

func render(t *testing.T, m catalog.Mode, dev []string, proxy bool) (composeFile, []byte) {
	t.Helper()
	d := sampleData(m, dev, proxy)
	out, err := renderCompose(d, composeFragments(m, dev))
	if err != nil {
		t.Fatalf("mode %+v dev %v: %v", m, dev, err)
	}
	var f composeFile
	if err := yaml.Unmarshal(out, &f); err != nil {
		t.Fatalf("mode %+v dev %v: %v\n%s", m, dev, err, out)
	}
	return f, out
}

func TestTemplatesParse(t *testing.T) {
	n := 0
	err := fs.WalkDir(templateFS(), ".", func(name string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() || !strings.HasSuffix(name, ".tmpl") {
			return err
		}
		n++
		_, err = parseTemplate(name)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no templates embedded")
	}
}

func TestEveryDevVariantHasAFragment(t *testing.T) {
	for _, v := range catalog.DevVariants() {
		if _, err := fs.Stat(templateFS(), devFragment(v.Name)); err != nil {
			t.Errorf("dev variant %s: %v", v.Name, err)
		}
	}
	entries, err := fs.ReadDir(templateFS(), "compose/dev")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if _, ok := catalog.LookupDevVariant(strings.TrimSuffix(e.Name(), ".yaml.tmpl")); !ok {
			t.Errorf("compose/dev/%s isn't a catalog dev variant", e.Name())
		}
	}
}

// TestComposeMatchesCatalog renders every mode and dev selection and checks
// the result against the catalog: services, images, networks, volumes and
// labels.
func TestComposeMatchesCatalog(t *testing.T) {
	for _, m := range modes() {
		for _, dev := range devSets() {
			t.Run(fmt.Sprintf("%+v/%v", m, dev), func(t *testing.T) {
				f, _ := render(t, m, dev, false)
				checkCompose(t, f, sampleData(m, dev, false), m, dev)
			})
		}
	}
}

func checkCompose(t *testing.T, f composeFile, d templateData, m catalog.Mode, dev []string) {
	// The file holds values as compose reads them, before interpolation.
	labels := map[string]string{}
	for k, v := range d.Labels {
		labels[k] = strings.ReplaceAll(v, "$", "$$")
	}
	if f.Name != d.Name {
		t.Errorf("name %q, want %q", f.Name, d.Name)
	}

	variantOf := map[string]catalog.DevVariant{}
	for _, name := range dev {
		v, _ := catalog.LookupDevVariant(name)
		for _, s := range v.Services {
			variantOf[s] = v
		}
	}

	var want []string
	for _, s := range catalog.ServicesIn(m) {
		want = append(want, s.Name)
	}
	if got := slices.Sorted(maps.Keys(f.Services)); !slices.Equal(got, slices.Sorted(slices.Values(want))) {
		t.Fatalf("services %v, want %v", got, want)
	}

	mounted := map[string]bool{}
	for _, s := range catalog.ServicesIn(m) {
		got := f.Services[s.Name]
		v, inDev := variantOf[s.Name]

		wantImage := d.Images[s.Image]
		switch {
		case inDev && v.Image != "":
			wantImage = d.Images[v.Image]
		case inDev:
			wantImage = d.DevImages[s.Image]
		}
		if got.Image != wantImage {
			t.Errorf("%s: image %q, want %q", s.Name, got.Image, wantImage)
		}
		if !maps.Equal(got.Labels, labels) {
			t.Errorf("%s: labels %v, want %v", s.Name, got.Labels, labels)
		}

		wantNets := s.Networks
		if inDev {
			wantNets = append(slices.Clone(wantNets), v.Networks...)
		}
		if len(wantNets) == 0 {
			if got.NetworkMode != "none" || len(got.Networks) != 0 {
				t.Errorf("%s: networks %v, network_mode %q; want network_mode none", s.Name, got.Networks, got.NetworkMode)
			}
		} else if !slices.Equal(slices.Sorted(slices.Values(got.Networks)), slices.Sorted(slices.Values(wantNets))) {
			t.Errorf("%s: networks %v, want %v", s.Name, got.Networks, wantNets)
		}

		// A variant that runs another image mounts only its own volumes.
		wantVols := s.VolumesIn(m)
		if inDev && v.Image != "" {
			wantVols = nil
		}
		if inDev {
			wantVols = append(slices.Clone(wantVols), v.Volumes...)
		}
		gotVols := checkMounts(t, s.Name, got.Volumes, d)
		if !slices.Equal(slices.Sorted(slices.Values(gotVols)), slices.Sorted(slices.Values(wantVols))) {
			t.Errorf("%s: volumes %v, want %v", s.Name, gotVols, wantVols)
		}
		for _, v := range gotVols {
			mounted[v] = true
		}

		if s.Phase == catalog.PhaseMigrate {
			if !slices.Equal(got.Profiles, []string{"migrate"}) {
				t.Errorf("%s: profiles %v, want [migrate]", s.Name, got.Profiles)
			}
		} else if len(got.Profiles) != 0 {
			t.Errorf("%s: profiles %v, want none", s.Name, got.Profiles)
		}
		if wantRestart := map[bool]string{true: "no", false: "always"}[s.OneShot]; got.Restart != wantRestart {
			t.Errorf("%s: restart %q, want %q", s.Name, got.Restart, wantRestart)
		}

		var wantPorts []string
		switch {
		case inDev && v.Port != catalog.NoPort:
			container := "5005"
			if v.Image != "" {
				container = "3000"
			}
			wantPorts = []string{fmt.Sprintf("127.0.0.1:%d:%s", d.DevPorts[v.Name], container)}
		case s.Name == "httpd":
			wantPorts = []string{"8080:80", "8443:443"}
		}
		if !slices.Equal(got.Ports, wantPorts) {
			t.Errorf("%s: ports %v, want %v", s.Name, got.Ports, wantPorts)
		}
	}

	for name, vol := range f.Volumes {
		cv, ok := catalog.LookupVolume(name)
		switch {
		case !ok:
			t.Errorf("volume %s isn't in the catalog", name)
		case !cv.When.In(m):
			t.Errorf("volume %s declared in mode %+v", name, m)
		case cv.Scope == catalog.SharedData:
			if !vol.External || vol.Name != cv.DockerName(d.SharedHPDS) || vol.Labels != nil {
				t.Errorf("shared volume %s: %+v, want external %s without labels", name, vol, cv.DockerName(d.SharedHPDS))
			}
		case cv.Scope == catalog.StackScoped:
			if vol.External || vol.Name != "" || !maps.Equal(vol.Labels, labels) {
				t.Errorf("volume %s: %+v, want a stack volume with the stack labels", name, vol)
			}
		default:
			t.Errorf("volume %s: scope %v can't be in a stack's compose file", name, cv.Scope)
		}
	}
	for name := range mounted {
		if _, ok := f.Volumes[name]; !ok {
			t.Errorf("volume %s is mounted but not declared", name)
		}
	}

	if len(f.Networks) != len(catalog.Networks()) {
		t.Errorf("networks %v, want %v", slices.Sorted(maps.Keys(f.Networks)), catalog.Networks())
	}
	for _, n := range catalog.Networks() {
		got, ok := f.Networks[n.Name]
		if !ok || got.Internal != n.Internal || !maps.Equal(got.Labels, labels) {
			t.Errorf("network %s: %+v, want internal=%v with the stack labels", n.Name, got, n.Internal)
		}
	}
}

// checkMounts returns the named volumes in a service's volume list, and
// checks that every bind mount is long syntax with an absolute source under
// a directory render controls.
func checkMounts(t *testing.T, service string, vols []yaml.Node, d templateData) []string {
	var named []string
	for _, n := range vols {
		if n.Kind == yaml.ScalarNode {
			src, _, _ := strings.Cut(n.Value, ":")
			if strings.HasPrefix(src, "/") || strings.HasPrefix(src, ".") {
				t.Errorf("%s: short-syntax bind %q", service, n.Value)
			}
			named = append(named, src)
			continue
		}
		var long struct {
			Type, Source, Target string
			Bind                 struct {
				CreateHostPath *bool `yaml:"create_host_path"`
			}
		}
		if err := n.Decode(&long); err != nil {
			t.Fatal(err)
		}
		if long.Type != "bind" {
			t.Errorf("%s: long-syntax %s mount; use the short syntax for volumes", service, long.Type)
			continue
		}
		ok := false
		for _, root := range []string{d.FilesDir, d.PicSureSrc, d.MigrationsSrc, d.FrontendSrc} {
			ok = ok || long.Source == root || strings.HasPrefix(long.Source, root+"/")
		}
		if !ok {
			t.Errorf("%s: bind source %q isn't under the render, source or frontend dirs", service, long.Source)
		}
		// Otherwise compose uses the bind API, and Docker creates a
		// missing source as an empty directory.
		if c := long.Bind.CreateHostPath; c == nil || *c {
			t.Errorf("%s: bind %s doesn't set create_host_path: false", service, long.Target)
		}
	}
	return named
}

var varRef = regexp.MustCompile(`\$+\{([^}]*)\}`)

// TestComposeReferencesOnlySecrets checks that the only ${NAME} references
// are the secrets and, with a proxy, the proxy variables, and that every
// secret is used.
func TestComposeReferencesOnlySecrets(t *testing.T) {
	used := map[string]bool{}
	for _, proxy := range []bool{false, true} {
		allowed := slices.Clone(secretVars)
		if proxy {
			allowed = append(allowed, proxyVars...)
		}
		for _, m := range modes() {
			for _, dev := range devSets() {
				_, out := render(t, m, dev, proxy)
				var doc yaml.Node
				if err := yaml.Unmarshal(out, &doc); err != nil {
					t.Fatal(err)
				}
				for _, ref := range varRef.FindAllStringSubmatch(strings.Join(scalars(&doc), "\n"), -1) {
					if dollars := len(ref[0]) - len(ref[1]) - 2; dollars%2 == 0 {
						continue // $$ is an escaped $, so an even run is literal
					}
					if !slices.Contains(allowed, ref[1]) {
						t.Errorf("proxy=%v mode %+v dev %v: ${%s} isn't a secret", proxy, m, dev, ref[1])
					}
					used[ref[1]] = true
				}
			}
		}
	}
	for _, name := range append(slices.Clone(secretVars), proxyVars...) {
		if !used[name] {
			t.Errorf("%s is never referenced", name)
		}
	}
}

// scalars returns every scalar in the YAML tree, so comments don't count.
func scalars(n *yaml.Node) []string {
	if n.Kind == yaml.ScalarNode {
		return []string{n.Value}
	}
	var out []string
	for _, c := range n.Content {
		out = append(out, scalars(c)...)
	}
	return out
}

func TestComposeValues(t *testing.T) {
	m := catalog.Mode{RemoteDB: true, CustomTrust: true}
	f, out := render(t, m, []string{"psama", "httpd-hmr"}, true)

	psama := f.Services["psama"].Environment
	wantJava := "-Xms2g -Xmx4g -XX:MetaspaceSize=96M -XX:MaxMetaspaceSize=256m -Djava.net.preferIPv4Stack=true " + trustJavaOpts + " " + debugJavaOpts
	if psama["JAVA_OPTS"] != wantJava {
		t.Errorf("psama JAVA_OPTS %q, want %q", psama["JAVA_OPTS"], wantJava)
	}
	if want := "jdbc:mysql://db.example.org:3307/auth?"; !strings.HasPrefix(psama["DATASOURCE_URL"], want) {
		t.Errorf("psama DATASOURCE_URL %q, want prefix %q", psama["DATASOURCE_URL"], want)
	}
	if psama["APPLICATION_CLIENT_SECRET_IS_BASE_64"] != "false" || psama["OPEN_IDP_PROVIDER_IS_ENABLED"] != "true" || psama["ENABLE_PUBLIC_ACCESS"] != "false" {
		t.Errorf("psama auth switches: %v", psama)
	}
	if psama["HTTPS_PROXY"] != "${HTTPS_PROXY}" || psama["no_proxy"] != "${NO_PROXY}" {
		t.Errorf("psama proxy env: %v", psama)
	}

	flyway := f.Services["flyway-init"].Environment
	if flyway["DB_HOST"] != "db.example.org" || flyway["DB_PORT"] != "3307" || flyway["DB_ROOT_USER"] != "admin" {
		t.Errorf("flyway-init DB env: %v", flyway)
	}

	hpds := f.Services["hpds"].Environment
	if hpds["JAVA_OPTS"] != "-Xmx2g" {
		t.Errorf("hpds JAVA_OPTS %q, want the configured -Xmx2g", hpds["JAVA_OPTS"])
	}
	if hpds["ID_BATCH_SIZE"] != "0" {
		t.Errorf("hpds ID_BATCH_SIZE %q: services.hpds.env should override it", hpds["ID_BATCH_SIZE"])
	}

	if u := f.Services["httpd"].User; u != "1000:1000" {
		t.Errorf("httpd-hmr user %q", u)
	}
	hmr := f.Services["httpd"].Environment
	if hmr["VITE_ORIGIN"] != "http://127.0.0.1:3000" || hmr["HTTP_PROXY"] != "${HTTP_PROXY}" || hmr["GATEWAY_DOCS_ENABLED"] != "true" {
		t.Errorf("httpd-hmr env: %v", hmr)
	}

	// The $ in the stack dir is escaped for compose.
	if !strings.Contains(string(out), `"/stacks/demo $$HOME"`) {
		t.Errorf("labels don't escape $:\n%s", out)
	}
}

func TestVhostRedirectKeepsHTTPSPort(t *testing.T) {
	for port, want := range map[int]string{
		443:  "RewriteRule ^ https://%{SERVER_NAME}/ [L,NE,R=301]",
		8443: "RewriteRule ^ https://%{SERVER_NAME}:8443/ [L,NE,R=301]",
	} {
		d := sampleData(catalog.Mode{}, nil, false)
		d.HTTPSPort = port
		out, err := executeTemplate("files/httpd/httpd-vhosts.conf.tmpl", d)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(out), want) {
			t.Errorf("port %d: no %q in the vhost", port, want)
		}
	}
}

func TestVhostSetsTheCSPFloor(t *testing.T) {
	out, err := executeTemplate("files/httpd/httpd-vhosts.conf.tmpl", sampleData(catalog.Mode{}, nil, false))
	if err != nil {
		t.Fatal(err)
	}
	want := `Header always set Content-Security-Policy "` + CSPFloorPolicy + `" "expr=-z %{resp:Content-Security-Policy}"`
	if !strings.Contains(string(out), want) {
		t.Errorf("no %q in the vhost", want)
	}
}

func TestMissingValuesFailExecution(t *testing.T) {
	d := sampleData(catalog.Mode{}, nil, false)
	delete(d.Images, "pic-sure-psama")
	if _, err := renderCompose(d, composeFragments(catalog.Mode{}, nil)); err == nil || !strings.Contains(err.Error(), "pic-sure-psama") {
		t.Errorf("err = %v, want a missing-image error", err)
	}
	d = sampleData(catalog.Mode{}, nil, false)
	if _, err := renderCompose(d, composeFragments(catalog.Mode{}, []string{"psama"})); err == nil {
		t.Error("a dev variant without a dev image and port rendered")
	}
}

func TestQuote(t *testing.T) {
	for _, s := range []string{"plain", "$HOME and ${X}", `"quoted" \ back`, "tab\tnew\nline", "# not a comment: x", "ünïcode", ""} {
		var got string
		if err := yaml.Unmarshal([]byte("v: "+quote(s)), &struct{ V *string }{&got}); err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if want := strings.ReplaceAll(s, "$", "$$"); got != want {
			t.Errorf("quote(%q) reads back as %q, want %q", s, got, want)
		}
	}
	if got := quote(true); got != `"true"` {
		t.Errorf("quote(true) = %s", got)
	}
}

func TestMergeMapping(t *testing.T) {
	base := "a:\n  x: 1\n  list: [1, 2]\n  m: {k: v}\nb: 2\n"
	over := "a:\n  list: [3]\n  m: {j: w}\n  y: 2\nc: 3\nb: [x]\n"
	var bn, on yaml.Node
	if err := yaml.Unmarshal([]byte(base), &bn); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal([]byte(over), &on); err != nil {
		t.Fatal(err)
	}
	if err := mergeMapping(bn.Content[0], on.Content[0], ""); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := bn.Decode(&got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"a": map[string]any{"x": 1, "list": []any{3}, "m": map[string]any{"k": "v", "j": "w"}, "y": 2},
		"b": []any{"x"},
		"c": 3,
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("merged %v, want %v", got, want)
	}
}

func TestMergeMappingRefusesToReplaceAMapping(t *testing.T) {
	for _, over := range []string{"a:\n", "a: [1]\n", "a: x\n", "b: {c: ~}\n"} {
		var bn, on yaml.Node
		if err := yaml.Unmarshal([]byte("a: {x: 1}\nb: {c: {d: 1}}\n"), &bn); err != nil {
			t.Fatal(err)
		}
		if err := yaml.Unmarshal([]byte(over), &on); err != nil {
			t.Fatal(err)
		}
		if err := mergeMapping(bn.Content[0], on.Content[0], ""); err == nil {
			t.Errorf("merging %q replaced a mapping", over)
		}
	}
}

func TestEmptyServiceEnvChangesNothing(t *testing.T) {
	m := catalog.Mode{}
	d := sampleData(m, nil, false)
	want, err := renderCompose(d, composeFragments(m, nil)[:len(composeFragments(m, nil))-1])
	if err != nil {
		t.Fatal(err)
	}
	for _, env := range []map[string]map[string]string{nil, {}, {"psama": {}}, {"psama": nil, "hpds": {}}} {
		d.ServiceEnv = env
		got, err := renderCompose(d, composeFragments(m, nil))
		if err != nil {
			t.Fatalf("ServiceEnv %v: %v", env, err)
		}
		if string(got) != string(want) {
			t.Errorf("ServiceEnv %v changed the compose file:\n%s", env, got)
		}
	}
}

// needCompose skips the test in short mode or without the docker CLI and
// its compose plugin. With PICSURE_REQUIRE_COMPOSE=1, as in CI's compose
// validation job, it fails instead of skipping.
func needCompose(t *testing.T) {
	t.Helper()
	required := os.Getenv("PICSURE_REQUIRE_COMPOSE") == "1"
	if testing.Short() && !required {
		t.Skip("short mode")
	}
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		if required {
			t.Fatalf("docker compose unavailable: %v", err)
		}
		t.Skipf("docker compose unavailable: %v", err)
	}
}

// TestComposeConfigAccepts runs docker compose config over rendered files,
// when the docker CLI with the compose plugin is installed. It needs no
// daemon.
func TestComposeConfigAccepts(t *testing.T) {
	needCompose(t)
	cases := []struct {
		m     catalog.Mode
		dev   []string
		proxy bool
	}{
		{catalog.Mode{}, nil, false},
		{catalog.Mode{RemoteDB: true, SharedHPDS: true, CustomTrust: true}, nil, true},
		{catalog.Mode{}, devSets()[len(devSets())-1], true},
		{catalog.Mode{SharedHPDS: true}, []string{"httpd"}, false},
	}
	for _, c := range cases {
		_, out := render(t, c.m, c.dev, c.proxy)
		file := filepath.Join(t.TempDir(), "compose.yaml")
		if err := os.WriteFile(file, out, 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("docker", "compose", "-f", file, "config", "--quiet")
		cmd.Env = os.Environ()
		for _, v := range append(slices.Clone(secretVars), proxyVars...) {
			cmd.Env = append(cmd.Env, v+"=x")
		}
		if b, err := cmd.CombinedOutput(); err != nil || len(b) > 0 {
			t.Errorf("mode %+v dev %v: docker compose config: %v\n%s", c.m, c.dev, err, b)
		}
	}
}

func TestHMRWithoutAHostUserRunsAsTheImagesUser(t *testing.T) {
	m := catalog.Mode{}
	d := sampleData(m, []string{"httpd-hmr"}, false)
	d.HostUser = ""
	out, err := renderCompose(d, composeFragments(m, []string{"httpd-hmr"}))
	if err != nil {
		t.Fatal(err)
	}
	var f composeFile
	if err := yaml.Unmarshal(out, &f); err != nil {
		t.Fatal(err)
	}
	if httpd := f.Services["httpd"]; httpd.User != "" || httpd.Image != "node:24.19.0-alpine3.23" {
		t.Errorf("httpd-hmr: user %q, image %q", httpd.User, httpd.Image)
	}
}
