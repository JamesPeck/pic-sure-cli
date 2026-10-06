package docker_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
)

const (
	noSuchVolume  = "Error response from daemon: get demo_hpds-data: no such volume\n"
	inspectHpds   = `{"CreatedAt":"2026-10-06T20:20:28Z","Driver":"local","Labels":{"org.hms-dbmi.picsure.stack":"demo"},"Mountpoint":"/var/lib/docker/volumes/demo_hpds-data/_data","Name":"demo_hpds-data","Options":null,"Scope":"local"}`
	inspectDictDB = `{"CreatedAt":"2026-10-06T20:21:00Z","Driver":"local","Labels":{"org.hms-dbmi.picsure.stack":"demo"},"Mountpoint":"/var/lib/docker/volumes/demo_dictionary-db/_data","Name":"demo_dictionary-db","Options":null,"Scope":"local"}`
)

func TestVolumeCreate(t *testing.T) {
	f, e := newEngine(t)
	f.On(fakerunner.Exact("docker", "volume", "create",
		"--label", "org.hms-dbmi.picsure.shared-hpds-data=picsure-demo",
		"--label", "org.hms-dbmi.picsure.shared-hpds-data.created=2026-10-06T20:20:28Z",
		"picsure-demo_hpds-data"))
	f.On(fakerunner.Exact("docker", "volume", "create", "pic-sure-m2"))
	ctx := context.Background()

	err := e.VolumeCreate(ctx, "picsure-demo_hpds-data", map[string]string{
		"org.hms-dbmi.picsure.shared-hpds-data.created": "2026-10-06T20:20:28Z",
		"org.hms-dbmi.picsure.shared-hpds-data":         "picsure-demo",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.VolumeCreate(ctx, "pic-sure-m2", nil); err != nil {
		t.Fatal(err)
	}
}

func TestVolumeInspect(t *testing.T) {
	f, e := newEngine(t)
	f.On(fakerunner.Exact("docker", "volume", "inspect", "demo_hpds-data")).Stdout("[" + inspectHpds + "]\n").Times(1)
	f.On(fakerunner.Exact("docker", "volume", "inspect", "demo_hpds-data")).Stdout("[]\n").Stderr(noSuchVolume).Exit(1)
	ctx := context.Background()

	v, err := e.VolumeInspect(ctx, "demo_hpds-data")
	if err != nil {
		t.Fatal(err)
	}
	want := docker.Volume{
		Name:       "demo_hpds-data",
		Driver:     "local",
		Labels:     map[string]string{"org.hms-dbmi.picsure.stack": "demo"},
		Mountpoint: "/var/lib/docker/volumes/demo_hpds-data/_data",
		CreatedAt:  "2026-10-06T20:20:28Z",
	}
	if !reflect.DeepEqual(v, want) {
		t.Errorf("VolumeInspect = %+v, want %+v", v, want)
	}

	_, err = e.VolumeInspect(ctx, "demo_hpds-data")
	if !errors.Is(err, docker.ErrNotFound) {
		t.Errorf("missing volume: err = %v, want ErrNotFound", err)
	}
	var exitErr *docker.ExitError
	if !errors.As(err, &exitErr) || !strings.Contains(err.Error(), "no such volume") {
		t.Errorf("missing volume: err = %v, want the *ExitError with docker's message", err)
	}
}

func TestVolumeList(t *testing.T) {
	f, e := newEngine(t)
	f.On(fakerunner.Exact("docker", "volume", "ls", "-q",
		"--filter", "label=org.hms-dbmi.picsure.stack=demo",
		"--filter", "label=org.hms-dbmi.picsure.stack-dir")).
		Stdout("demo_hpds-data\ndemo_dictionary-db\n")
	f.On(fakerunner.Exact("docker", "volume", "inspect", "demo_hpds-data", "demo_dictionary-db")).
		Stdout("[" + inspectHpds + "," + inspectDictDB + "]")

	vols, err := e.VolumeList(context.Background(), "org.hms-dbmi.picsure.stack=demo", "org.hms-dbmi.picsure.stack-dir")
	if err != nil {
		t.Fatal(err)
	}
	if len(vols) != 2 || vols[0].Name != "demo_dictionary-db" || vols[1].Name != "demo_hpds-data" {
		t.Fatalf("VolumeList = %+v, want both, sorted by name", vols)
	}
	if vols[1].Labels["org.hms-dbmi.picsure.stack"] != "demo" {
		t.Errorf("labels = %v", vols[1].Labels)
	}
}

func TestVolumeListEmpty(t *testing.T) {
	f, e := newEngine(t)
	f.On(fakerunner.Exact("docker", "volume", "ls", "-q"))
	vols, err := e.VolumeList(context.Background())
	if err != nil || len(vols) != 0 {
		t.Errorf("VolumeList = %v, %v", vols, err)
	}
	f.AssertNotCalled(fakerunner.Glob("docker volume inspect*"))
}

func TestVolumeListRetriesWhenAVolumeVanishes(t *testing.T) {
	f, e := newEngine(t)
	ls := fakerunner.Glob("docker volume ls *")
	f.On(ls).Stdout("demo_dictionary-db\ndemo_hpds-data\n").Times(1)
	f.On(ls).Stdout("demo_hpds-data\n")
	f.On(fakerunner.Exact("docker", "volume", "inspect", "demo_dictionary-db", "demo_hpds-data")).
		Stdout("[" + inspectHpds + "]").Stderr("Error response from daemon: get demo_dictionary-db: no such volume\n").Exit(1)
	f.On(fakerunner.Exact("docker", "volume", "inspect", "demo_hpds-data")).Stdout("[" + inspectHpds + "]")

	vols, err := e.VolumeList(context.Background(), "org.hms-dbmi.picsure.stack=demo")
	if err != nil || len(vols) != 1 || vols[0].Name != "demo_hpds-data" {
		t.Errorf("VolumeList = %+v, %v, want just the remaining volume", vols, err)
	}
}

func TestVolumeListGivesUpAfterOneRetry(t *testing.T) {
	f, e := newEngine(t)
	f.On(fakerunner.Glob("docker volume ls *")).Stdout("ghost\n")
	f.On(fakerunner.Exact("docker", "volume", "inspect", "ghost")).Stderr("Error response from daemon: get ghost: no such volume\n").Exit(1)

	if _, err := e.VolumeList(context.Background(), "x"); !errors.Is(err, docker.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if n := len(f.CallsMatching(fakerunner.Glob("docker volume ls *"))); n != 2 {
		t.Errorf("listed %d times, want 2", n)
	}
}

func TestVolumeRemove(t *testing.T) {
	f, e := newEngine(t)
	rm := fakerunner.Exact("docker", "volume", "rm", "demo_hpds-data")
	f.On(rm).Times(1)
	f.On(rm).Stderr(noSuchVolume).Exit(1).Times(1)
	f.On(rm).Stderr("Error response from daemon: remove demo_hpds-data: volume is in use - [5f3a]\n").Exit(1)
	ctx := context.Background()

	if err := e.VolumeRemove(ctx, "demo_hpds-data"); err != nil {
		t.Errorf("present: %v", err)
	}
	if err := e.VolumeRemove(ctx, "demo_hpds-data"); err != nil {
		t.Errorf("already gone: %v, want nil", err)
	}
	if err := e.VolumeRemove(ctx, "demo_hpds-data"); err == nil || !strings.Contains(err.Error(), "volume is in use") {
		t.Errorf("in use: %v, want docker's message", err)
	}
}

func TestContainersUsingVolume(t *testing.T) {
	f, e := newEngine(t)
	// Real `docker ps --format '{{json .}}'` rows, trimmed.
	f.On(fakerunner.Exact("docker", "ps", "-a", "--no-trunc", "--filter", "volume=demo_hpds-data", "--format", "{{json .}}")).
		Stdout(`{"Command":"\"sh -c 'exit 3'\"","ID":"c167f0dc2ebc","Image":"alpine","Labels":"desktop.docker.io/ports.scheme=v2","LocalVolumes":"1","Mounts":"demo_hpds-data","Names":"demo-hpds-load-1a2b3c4d","State":"exited","Status":"Exited (3) 2 minutes ago"}
{"ID":"9a8b7c6d5e4f","Image":"hms-dbmi/pic-sure-hpds:0123456789ab","Names":"demo-other/hpds,demo-hpds-1","State":"running","Status":"Up 2 hours (healthy)"}
`)
	f.On(fakerunner.Glob("docker ps * volume=unused *"))
	ctx := context.Background()

	cs, err := e.ContainersUsingVolume(ctx, "demo_hpds-data")
	if err != nil {
		t.Fatal(err)
	}
	want := []docker.Container{
		{ID: "c167f0dc2ebc", Name: "demo-hpds-load-1a2b3c4d", Image: "alpine", State: "exited"},
		{ID: "9a8b7c6d5e4f", Name: "demo-hpds-1", Image: "hms-dbmi/pic-sure-hpds:0123456789ab", State: "running"},
	}
	if !reflect.DeepEqual(cs, want) {
		t.Errorf("ContainersUsingVolume = %+v, want %+v", cs, want)
	}

	if cs, err := e.ContainersUsingVolume(ctx, "unused"); err != nil || len(cs) != 0 {
		t.Errorf("unused volume: %v, %v", cs, err)
	}
}

func TestContainersUsingVolumeBadOutput(t *testing.T) {
	f, e := newEngine(t)
	f.On(fakerunner.Glob("docker ps *")).Stdout("CONTAINER ID   IMAGE\n")
	if _, err := e.ContainersUsingVolume(context.Background(), "v1"); err == nil {
		t.Error("no error for non-JSON output")
	}
}
