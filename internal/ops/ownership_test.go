package ops_test

import (
	"context"
	"maps"
	"path/filepath"
	"slices"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
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

func TestNoteMovedUntilTheStackRunsInItsNewDirectory(t *testing.T) {
	res := func(kind string, claim stack.Claim) ops.Resource {
		return ops.Resource{ResourceRef: ops.ResourceRef{Kind: kind, StackDir: "/stacks/a"}, Claim: claim}
	}
	var rec events.Recorder
	// Moved after down: only the volumes are there, and they are noted.
	ops.NoteMoved(&rec, &ops.Ownership{Resources: []ops.Resource{res("volume", stack.Moved), res("volume", stack.Moved)}})
	if ev := rec.Events(); len(ev) != 1 || ev[0].(events.Warning).Text != "stack moved from /stacks/a; adopting its resources" {
		t.Errorf("notes %v, want one", ev)
	}
	// After up the containers carry the new directory; the volumes keep
	// the old one for good, and aren't noted again.
	ops.NoteMoved(&rec, &ops.Ownership{Resources: []ops.Resource{res("container", stack.Own), res("volume", stack.Moved)}})
	if n := len(rec.Events()); n != 1 {
		t.Errorf("%d notes once the stack runs in its new directory, want none more", n-1)
	}
}

func TestDevImages(t *testing.T) {
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker image ls --filter reference=hms-dbmi/* *")).Stdout(
		"hms-dbmi/pic-sure-hpds:0123456789ab\nhms-dbmi/pic-sure-psama:dev-demo-0123456789ab\nhms-dbmi/pic-sure-httpd:dev-demo-0123456789ab\nhms-dbmi/pic-sure-psama:dev-demo2-0123456789ab\n")
	f.On(fakerunner.Glob("docker image inspect *")).Stdout(`[{"Id":"sha256:a","RepoTags":["hms-dbmi/pic-sure-hpds:0123456789ab"]},` +
		`{"Id":"sha256:b","RepoTags":["hms-dbmi/pic-sure-psama:dev-demo-0123456789ab"],"Config":{"Labels":{"` + stack.LabelStack + `":"demo","` + stack.LabelStackDir + `":"/stacks/demo"}}},` +
		`{"Id":"sha256:c","RepoTags":["hms-dbmi/pic-sure-httpd:dev-demo-0123456789ab"]},` +
		`{"Id":"sha256:d","RepoTags":["hms-dbmi/pic-sure-psama:dev-demo2-0123456789ab"],"Config":{"Labels":{"` + stack.LabelStackDir + `":"/stacks/demo2"}}}]`)
	d := &ops.Deps{Runner: f, Docker: docker.NewEngine(f)}
	rs, err := ops.DevImages(context.Background(), d, "demo")
	if err != nil {
		t.Fatal(err)
	}
	// The commit-tagged image is shared, httpd's predates stack labels, and
	// demo2's is another stack's name.
	if len(rs) != 1 || rs[0].String() != "image hms-dbmi/pic-sure-psama:dev-demo-0123456789ab" || rs[0].StackDir != "/stacks/demo" || rs[0].Claim != stack.Foreign {
		t.Errorf("DevImages = %+v, want only demo's labelled psama image", rs)
	}
}
