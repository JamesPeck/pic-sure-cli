package ops_test

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/render"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

func busyHost(ports ...int) *fakeHost {
	h := &fakeHost{busy: map[int]bool{}}
	for _, p := range ports {
		h.busy[p] = true
	}
	return h
}

func TestChoosePorts(t *testing.T) {
	for _, tc := range []struct {
		name            string
		busy            []int
		http, https     int
		auto            bool
		wantHTTP, wantS int
		wantCode        int
	}{
		{name: "defaults free", wantHTTP: 80, wantS: 443},
		{name: "auto skips the defaults", auto: true, busy: []int{8443}, wantHTTP: 8081, wantS: 8444},
		{name: "busy defaults, no auto", busy: []int{443}, wantCode: exitcode.CodePrecondition},
		{name: "busy defaults, auto", busy: []int{80, 8080, 8444}, auto: true, wantHTTP: 8082, wantS: 8445},
		{name: "given and free", http: 8083, https: 8443, busy: []int{80, 443}, wantHTTP: 8083, wantS: 8443},
		{name: "given and busy", http: 8083, https: 8443, busy: []int{8443}, wantCode: exitcode.CodePrecondition},
		{name: "one given", https: 9443, wantHTTP: 80, wantS: 9443},
		{name: "one given, auto", https: 9443, auto: true, wantHTTP: 8080, wantS: 9443},
		{name: "one default busy", http: 8083, busy: []int{443}, wantCode: exitcode.CodePrecondition},
		{name: "one given equal to the other's default", http: 443, wantCode: exitcode.CodeUsage},
		{name: "one given equal to the other's default, auto", http: 443, auto: true, wantHTTP: 443, wantS: 8443},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, s, err := ops.ChoosePorts(busyHost(tc.busy...), tc.http, tc.https, tc.auto)
			if tc.wantCode != 0 {
				if exitcode.FromError(err) != tc.wantCode {
					t.Fatalf("err = %v, want exit %d", err, tc.wantCode)
				}
				return
			}
			if err != nil || h != tc.wantHTTP || s != tc.wantS {
				t.Fatalf("got %d/%d, %v; want %d/%d", h, s, err, tc.wantHTTP, tc.wantS)
			}
		})
	}
}

func TestChooseDevPortsBase(t *testing.T) {
	base, err := ops.ChooseDevPortsBase(busyHost(15003, 15016), 15020)
	if err != nil || base != 15030 {
		t.Fatalf("base = %d, %v; want 15030 (15000 and 15010 have a busy port, 15020 holds the HTTPS port)", base, err)
	}
}

func TestStackNameInUse(t *testing.T) {
	// The labels hold the directory with symlinks resolved, as Stack.Dir.
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(tmp, "demo")
	ps := `{"Names":"demo-httpd-1","Ports":"0.0.0.0:8443->443/tcp, [::]:8443->443/tcp, 0.0.0.0:8083->80/tcp",` +
		`"Labels":"com.docker.compose.project=demo,` + stack.LabelStackDir + `=` + dir + `"}` + "\n"
	for _, tc := range []struct {
		name, ps, vols string
		wantUser       string
		wantPorts      []int
	}{
		{name: "free", vols: "[]"},
		{name: "this stack's", ps: ps, vols: "[]", wantPorts: []int{8083, 8443}},
		{name: "another project's container", ps: `{"Names":"demo-web-1","Labels":"com.docker.compose.project=demo"}`, vols: "[]", wantUser: "container demo-web-1"},
		{name: "another stack's volume", vols: `[{"Name":"demo_hpds-data","Labels":{"` + stack.LabelStackDir + `":"/elsewhere"}}]`, wantUser: "volume demo_hpds-data"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fakerunner.New(t)
			f.On(fakerunner.Glob("docker ps --all --no-trunc --filter label=com.docker.compose.project=demo *")).Stdout(tc.ps)
			f.On(fakerunner.Glob("docker volume ls *")).Stdout(volumeLines(t, tc.vols))
			f.On(fakerunner.Glob("docker volume inspect *")).Stdout(tc.vols)
			d := &ops.Deps{Runner: f, Docker: docker.NewEngine(f)}
			user, published, err := ops.StackNameInUse(context.Background(), d, "demo", dir)
			ports := slices.Sorted(maps.Keys(published))
			if err != nil || user != tc.wantUser || !slices.Equal(ports, tc.wantPorts) {
				t.Fatalf("got %q, %v, %v; want %q, %v", user, ports, err, tc.wantUser, tc.wantPorts)
			}
		})
	}
}

// volumeLines is `docker volume ls --format` output naming the volumes in
// the JSON array vols.
func volumeLines(t *testing.T, vols string) string {
	t.Helper()
	if vols == "[]" {
		return ""
	}
	name := strings.SplitN(strings.SplitN(vols, `"Name":"`, 2)[1], `"`, 2)[0]
	return name + "\n"
}

func TestStartServicesLeaveOutTheOneShots(t *testing.T) {
	cfg := stack.DefaultConfig()
	got := ops.StartServices(&cfg)
	for _, want := range []string{"picsure-db", "psama", "hpds", "httpd"} {
		if !slices.Contains(got, want) {
			t.Errorf("StartServices = %v, missing %s", got, want)
		}
	}
	for _, not := range []string{"flyway-init", "flyway-dictionary-init", "hpds-genomic-seed"} {
		if slices.Contains(got, not) {
			t.Errorf("StartServices = %v, has %s", got, not)
		}
	}
	cfg.DB.Mode = stack.DBRemote
	if slices.Contains(ops.StartServices(&cfg), "picsure-db") {
		t.Error("a remote-DB stack starts picsure-db")
	}
}

func TestConvergeStepsBuildTheComposerOnce(t *testing.T) {
	st, err := stack.Create(filepath.Join(t.TempDir(), "demo"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := os.MkdirAll(st.Path(".pic-sure/render"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(st.Path(".pic-sure/render/compose.yaml"), []byte("name: demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := stack.DefaultConfig()
	cfg.Name = "demo"
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker compose * up -d --wait --wait-timeout 900 picsure-db *"))
	d := &ops.Deps{Runner: f, Sink: &events.Recorder{}}
	built := 0
	opts := ops.ConvergeOptions{Compose: func() (docker.Composer, error) {
		built++
		return docker.NewCompose(f, st.Dir, nil)
	}}
	list := ops.ConvergeSteps(d, st, &cfg, &stack.Secrets{}, opts)
	var ids []string
	for _, s := range list {
		ids = append(ids, s.ID)
	}
	if want := []string{"db", "migrate", "seed", "hpds-key", "start"}; !slices.Equal(ids, want) || !slices.Equal(ops.InitStepIDs(&cfg)[5:], want) {
		t.Fatalf("ConvergeSteps = %v, want %v", ids, want)
	}
	remote := cfg
	remote.DB.Mode = stack.DBRemote
	if got := ops.ConvergeSteps(d, st, &remote, &stack.Secrets{}, opts); got[1].ID != "db-bootstrap" || ops.InitStepIDs(&remote)[6] != "db-bootstrap" {
		t.Errorf("a remote database isn't bootstrapped: second step %s", got[1].ID)
	}
	start := list[len(list)-1]
	for range 2 {
		if err := start.Apply(context.Background(), d.Sink); err != nil {
			t.Fatal(err)
		}
	}
	if built != 1 {
		t.Errorf("the Composer was built %d times, want once", built)
	}
}

func TestInitStepIDsMatchInitSteps(t *testing.T) {
	st, err := stack.Create(filepath.Join(t.TempDir(), "demo"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, mode := range []stack.DBMode{stack.DBLocal, stack.DBRemote} {
		cfg := stack.DefaultConfig()
		cfg.Name, cfg.DB.Mode = "demo", mode
		var ids []string
		for _, s := range ops.InitSteps(&ops.Deps{}, st, &cfg, &stack.Secrets{}, &stack.State{}, ops.ConvergeOptions{}) {
			ids = append(ids, s.ID)
		}
		if want := ops.InitStepIDs(&cfg); !slices.Equal(ids, want) {
			t.Errorf("db.mode %s: InitSteps %v, InitStepIDs %v", mode, ids, want)
		}
	}
}

func TestRenderStepTakesTheSharedDataSetsProfile(t *testing.T) {
	x := newUpdateFixture(t)
	x.cfg.HPDS.Data, x.cfg.HPDS.SharedName = stack.HPDSShared, "set1"
	labels := map[string]string{ops.SharedDataLabel: "set1", ops.SharedDataProfileLabel: "bch-dev"}
	vols := map[string]bool{"set1_hpds-data": true, "set1_hpds-genomic": true}
	x.f.On(fakerunner.Glob("docker volume inspect *")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		name := c.Argv[len(c.Argv)-1]
		if !vols[name] {
			return docker.Result{Stderr: []byte("Error response from daemon: get " + name + ": no such volume\n"), ExitCode: 1}, nil
		}
		out, err := json.Marshal([]map[string]any{{"Name": name, "Labels": labels}})
		return docker.Result{Stdout: out}, err
	})
	x.f.On(fakerunner.Glob("docker run --rm --name pic-sure-shared-check-* *.picsure-published*"))
	apply := func() error {
		return ops.RenderStep(x.d, x.st, x.cfg, x.state, ops.ConvergeOptions{Cache: x.cache, Compose: x.compose}).Apply(context.Background(), x.rec)
	}
	if err := apply(); err != nil {
		t.Fatal(err)
	}
	compose, err := os.ReadFile(x.st.Path(render.ComposeFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`SPRING_PROFILES_ACTIVE: "bch-dev"`, `name: "set1_hpds-data"`, "shared-hpds-data:/opt/local/hpds:ro"} {
		if !strings.Contains(string(compose), want) {
			t.Errorf("compose.yaml lacks %s", want)
		}
	}

	// A set that isn't on the host stops the render before it writes.
	delete(vols, "set1_hpds-genomic")
	x.cfg.HPDS.Profile = "other"
	if err := apply(); exitcode.FromError(err) != exitcode.CodePrecondition || !strings.Contains(err.Error(), "no volume set1_hpds-genomic") {
		t.Fatalf("missing set: %v", err)
	}
	if again, _ := os.ReadFile(x.st.Path(render.ComposeFile)); string(again) != string(compose) {
		t.Error("the failed render rewrote compose.yaml")
	}
}

func TestSummaryInSharedModeSuggestsHydrateNotDemo(t *testing.T) {
	x := newUpdateFixture(t)
	x.cfg.HPDS.Data, x.cfg.HPDS.SharedName = stack.HPDSShared, "set1"
	steps := strings.Join(ops.Summary(x.st, x.cfg, nil).NextSteps, "\n")
	if strings.Contains(steps, "data demo") || !strings.Contains(steps, "dictionary hydrate") {
		t.Errorf("next steps:\n%s", steps)
	}
}

// writeRegisteredStack makes a stack in dir whose pic-sure.yaml sets the
// ports, and registers it in c.
func writeRegisteredStack(t *testing.T, c *cache.Cache, dir string, http, https, devBase int) {
	t.Helper()
	st, err := stack.Create(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	cfg := stack.DefaultConfig()
	cfg.Name, cfg.Auth.Mode, cfg.Auth.AdminEmail = filepath.Base(dir), stack.AuthOpen, "admin@example.com"
	cfg.Network.HTTPPort, cfg.Network.HTTPSPort, cfg.Network.DevPorts.Base = http, https, devBase
	doc, err := stack.NewConfigDoc(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	data, err := doc.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteConfig(data); err != nil {
		t.Fatal(err)
	}
	if err := c.RegisterStack(context.Background(), dir, cfg.Name); err != nil {
		t.Fatal(err)
	}
}

func TestReservedPortsAreSkipped(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := cache.Open(filepath.Join(tmp, "cache"), cache.Options{})
	if err != nil {
		t.Fatal(err)
	}
	writeRegisteredStack(t, c, filepath.Join(tmp, "a"), 8080, 8443, 15000)
	writeRegisteredStack(t, c, filepath.Join(tmp, "self"), 8081, 8444, 15010)
	// Gone: its directory is no longer a stack.
	writeRegisteredStack(t, c, filepath.Join(tmp, "gone"), 8082, 8445, 15020)
	if err := os.RemoveAll(filepath.Join(tmp, "gone", stack.CLIDir)); err != nil {
		t.Fatal(err)
	}
	// Unreadable: its pic-sure.yaml is missing.
	writeRegisteredStack(t, c, filepath.Join(tmp, "broken"), 8083, 8446, 15030)
	if err := os.Remove(filepath.Join(tmp, "broken", stack.ConfigFile)); err != nil {
		t.Fatal(err)
	}

	reserved, err := ops.ReservedPorts(c, filepath.Join(tmp, "self"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]bool{8080: true, 8443: true}
	for p := 15000; p < 15000+catalog.DevPortSpan; p++ {
		want[p] = true
	}
	if !maps.Equal(reserved, want) {
		t.Fatalf("reserved %v, want %v", slices.Sorted(maps.Keys(reserved)), slices.Sorted(maps.Keys(want)))
	}

	h := ops.ReservingHost{Host: busyHost(), Reserved: reserved}
	if hp, sp, err := ops.ChoosePorts(h, 0, 0, true); err != nil || hp != 8081 || sp != 8444 {
		t.Errorf("auto ports = %d/%d, %v; want 8081/8444", hp, sp, err)
	}
	if base, err := ops.ChooseDevPortsBase(h, 8081, 8444); err != nil || base != 15010 {
		t.Errorf("dev base = %d, %v; want 15010", base, err)
	}
	reserved[80] = true
	if _, _, err := ops.ChoosePorts(h, 0, 0, false); err == nil || !strings.Contains(err.Error(), "80 (another stack's)") {
		t.Errorf("default port another stack has: %v", err)
	}
}
