package ops_test

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
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
		{name: "one given equal to the other's default", http: 443, wantCode: exitcode.CodePrecondition},
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
