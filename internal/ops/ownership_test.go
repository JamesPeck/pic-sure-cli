package ops_test

import (
	"context"
	"maps"
	"path/filepath"
	"slices"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

// noStackResources answers the ownership check's docker calls (the
// containers, volumes and networks a stack's name selects) with none.
func noStackResources(f *fakerunner.Runner) {
	f.On(fakerunner.Glob("docker ps --all --no-trunc --filter label=com.docker.compose.project=* --format *"))
	f.On(fakerunner.Glob("docker volume ls -q --filter label=*"))
	f.On(fakerunner.Glob("docker network ls -q --no-trunc --filter label=com.docker.compose.project=*"))
}

func TestStackResources(t *testing.T) {
	// The labels hold the directory with symlinks resolved, as Stack.Dir.
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(tmp, "demo")
	const id = "0123456789abcdef0123456789abcdef"
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker ps --all --no-trunc --filter label=com.docker.compose.project=demo *")).Stdout(
		`{"Names":"demo-httpd-1","Ports":"0.0.0.0:8443->443/tcp, [::]:8443->443/tcp, 0.0.0.0:8083->80/tcp",` +
			`"Labels":{"` + stack.LabelStack + `":"demo","` + stack.LabelStackDir + `":"` + dir + `","` + stack.LabelStackID + `":"` + id + `"}}` + "\n" +
			`{"Names":"demo-web-1","Ports":"0.0.0.0:9000->80/tcp","Labels":{"` + stack.LabelStack + `":"","` + stack.LabelStackDir + `":"","` + stack.LabelStackID + `":""}}` + "\n")
	f.On(fakerunner.Glob("docker volume ls -q --filter label=com.docker.compose.project=demo")).Stdout("demo_hpds-data\n")
	f.On(fakerunner.Glob("docker volume ls -q --filter label=" + stack.LabelStack + "=demo")).Stdout("demo_hpds-data\ndemo_old\n")
	f.On(fakerunner.Glob("docker volume inspect demo_hpds-data")).Stdout(`[{"Name":"demo_hpds-data","Labels":{"` + stack.LabelStackDir + `":"` + dir + `"}}]`)
	f.On(fakerunner.Glob("docker volume inspect demo_hpds-data demo_old")).Stdout(`[{"Name":"demo_hpds-data","Labels":{"` + stack.LabelStackDir + `":"` + dir + `"}},` +
		`{"Name":"demo_old","Labels":{"` + stack.LabelStack + `":"demo","` + stack.LabelStackDir + `":"/stacks/gone","` + stack.LabelStackID + `":"` + id + `"}}]`)
	f.On(fakerunner.Glob("docker network ls *")).Stdout("n1\n")
	f.On(fakerunner.Glob("docker network inspect n1")).Stdout(`[{"Name":"demo_default","Labels":{"` + stack.LabelStackDir + `":"/elsewhere"}}]`)
	d := &ops.Deps{Runner: f, Docker: docker.NewEngine(f)}

	o, err := ops.StackResources(context.Background(), d, "demo", id, dir)
	if err != nil {
		t.Fatal(err)
	}
	claims := map[string]stack.Claim{}
	for _, r := range o.Resources {
		claims[r.String()] = r.Claim
	}
	want := map[string]stack.Claim{
		"container demo-httpd-1": stack.Own,
		// A compose project of the same name, not pic-sure's.
		"container demo-web-1": stack.Foreign,
		// No ID label: its directory decides.
		"volume demo_hpds-data": stack.Own,
		"volume demo_old":       stack.Moved,
		"network demo_default":  stack.Foreign,
	}
	if !maps.Equal(claims, want) {
		t.Errorf("claims %v, want %v", claims, want)
	}
	if ports := slices.Sorted(maps.Keys(o.Published)); !slices.Equal(ports, []int{8083, 8443}) {
		t.Errorf("published %v, want only this stack's 8083 and 8443", ports)
	}
	if err := o.Err("demo"); err == nil {
		t.Error("Err = nil with foreign resources")
	}

	// A stack init hasn't made owns nothing.
	o, err = ops.StackResources(context.Background(), d, "demo", "", "")
	if err != nil || len(o.Foreign()) != len(o.Resources) || len(o.Published) != 0 {
		t.Errorf("for a new stack: %d of %d foreign, published %v, err %v; want all foreign", len(o.Foreign()), len(o.Resources), o.Published, err)
	}
}
