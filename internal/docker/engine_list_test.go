package docker_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
)

func TestImageList(t *testing.T) {
	f, e := newEngine(t)
	f.On(fakerunner.Exact("docker", "image", "ls", "--filter", "reference=hms-dbmi/*", "--format", "{{.Repository}}:{{.Tag}}")).
		Stdout("hms-dbmi/pic-sure-hpds:0123456789ab\nhms-dbmi/pic-sure-hpds:<none>\nhms-dbmi/dictionary-etl:dev-s-0123456789ab\n")
	f.On(fakerunner.Exact("docker", "image", "inspect", "hms-dbmi/pic-sure-hpds:0123456789ab", "hms-dbmi/dictionary-etl:dev-s-0123456789ab")).
		Stdout(`[{"Id":"sha256:a","RepoTags":["hms-dbmi/pic-sure-hpds:0123456789ab","hms-dbmi/pic-sure-hpds:x"],"Size":424693812,"Created":"2026-10-05T21:56:53.5Z","Config":{"Labels":{"k":"v"}}},` +
			`{"Id":"sha256:b","RepoTags":["hms-dbmi/dictionary-etl:dev-s-0123456789ab"],"Size":5,"Created":"2026-10-06T00:00:00Z","Config":{"Labels":null}}]`)
	imgs, err := e.ImageList(context.Background(), "hms-dbmi/*")
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) != 2 {
		t.Fatalf("got %+v, want 2 images", imgs)
	}
	etl, hpds := imgs[0], imgs[1] // sorted by ref
	if hpds.Ref != "hms-dbmi/pic-sure-hpds:0123456789ab" || hpds.ID != "sha256:a" || hpds.Size != 424693812 ||
		!slices.Contains(hpds.RepoTags, "hms-dbmi/pic-sure-hpds:x") || hpds.Labels["k"] != "v" ||
		!hpds.Created.Equal(time.Date(2026, 10, 5, 21, 56, 53, 5e8, time.UTC)) {
		t.Errorf("hpds = %+v", hpds)
	}
	if etl.Ref != "hms-dbmi/dictionary-etl:dev-s-0123456789ab" || etl.ID != "sha256:b" {
		t.Errorf("etl = %+v", etl)
	}
}

func TestImageListRetriesWhenAnImageGoesMeanwhile(t *testing.T) {
	f, e := newEngine(t)
	f.On(fakerunner.Glob("docker image ls *")).Times(1).Stdout("a:1\nb:1\n")
	f.On(fakerunner.Exact("docker", "image", "inspect", "a:1", "b:1")).Exit(1).Stderr("Error response from daemon: No such image: b:1\n")
	f.On(fakerunner.Glob("docker image ls *")).Stdout("a:1\n")
	f.On(fakerunner.Exact("docker", "image", "inspect", "a:1")).Stdout(`[{"Id":"sha256:a"}]`)
	imgs, err := e.ImageList(context.Background(), "*")
	if err != nil || len(imgs) != 1 || imgs[0].Ref != "a:1" {
		t.Errorf("ImageList = %+v, %v", imgs, err)
	}
}

func TestContainerListIncludesStoppedContainersAndMounts(t *testing.T) {
	f, e := newEngine(t)
	f.On(fakerunner.Exact("docker", "ps", "-a", "-q", "--no-trunc")).Stdout("id2\nid1\n")
	f.On(fakerunner.Exact("docker", "container", "inspect", "id2", "id1")).Stdout(`[
{"Id":"id2","Name":"/zeta","Image":"sha256:z","Config":{"Image":"alpine:3","Labels":{"l":"1"}},"State":{"Status":"exited"},
 "Mounts":[{"Type":"bind","Source":"/home/u/.cache/pic-sure/src/pic-sure/abc","Name":""},{"Type":"volume","Name":"demo_hpds-data","Source":"/var/lib/docker/volumes/demo_hpds-data/_data"}]},
{"Id":"id1","Name":"/alpha","Image":"sha256:a","Config":{"Image":"hms-dbmi/pic-sure-hpds:0123456789ab"},"State":{"Status":"running","Running":true}}]`)
	cs, err := e.ContainerList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 || cs[0].Name != "alpha" || cs[1].Name != "zeta" {
		t.Fatalf("ContainerList = %+v", cs)
	}
	z := cs[1]
	if z.ImageID != "sha256:z" || z.Image != "alpine:3" || z.Status != "exited" || z.Labels["l"] != "1" ||
		!slices.Equal(z.Mounts, []string{"/home/u/.cache/pic-sure/src/pic-sure/abc", "demo_hpds-data"}) {
		t.Errorf("zeta = %+v", z)
	}
}

func TestContainerListWithNoContainers(t *testing.T) {
	f, e := newEngine(t)
	f.On(fakerunner.Exact("docker", "ps", "-a", "-q", "--no-trunc"))
	cs, err := e.ContainerList(context.Background())
	if err != nil || cs != nil {
		t.Errorf("ContainerList = %+v, %v", cs, err)
	}
}

func TestNetworkListRetriesWhenANetworkGoesMeanwhile(t *testing.T) {
	f, e := newEngine(t)
	f.On(fakerunner.Exact("docker", "network", "ls", "-q", "--no-trunc", "--filter", "label=org.hms-dbmi.picsure.stack-dir")).Times(1).Stdout("n1\nn2\n")
	f.On(fakerunner.Exact("docker", "network", "inspect", "n1", "n2")).Exit(1).Stderr("Error response from daemon: network n2 not found\n")
	f.On(fakerunner.Glob("docker network ls *")).Stdout("n1\n")
	f.On(fakerunner.Exact("docker", "network", "inspect", "n1")).Stdout(`[{"Name":"demo_default","Id":"n1","Labels":{"org.hms-dbmi.picsure.stack":"demo"}}]`)
	nets, err := e.NetworkList(context.Background(), "org.hms-dbmi.picsure.stack-dir")
	if err != nil || len(nets) != 1 || nets[0].Name != "demo_default" || nets[0].Labels["org.hms-dbmi.picsure.stack"] != "demo" {
		t.Errorf("NetworkList = %+v, %v", nets, err)
	}
}
