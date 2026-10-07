package ops_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

// sharedFixture fakes a daemon holding volumes, keyed by name, with their
// labels.
type sharedFixture struct {
	f     *fakerunner.Runner
	d     *ops.Deps
	st    *stack.Stack
	cfg   *stack.Config
	state *stack.State

	mu       sync.Mutex
	vols     map[string]map[string]string
	users    map[string]string // volume -> docker ps output
	removed  []string
	hpds     string // hpds's compose state
	probe    string // the probe helper's stdout
	copyExit int
	copies   [][]string
	// probeFn, if set, answers the probe instead of probe.
	probeFn func(ctx context.Context, argv []string) ([]byte, error)
	// inspectFail makes the next inspect of that volume fail.
	inspectFail string
	// onCreate runs before a volume is created, under mu.
	onCreate func(name string)
}

const goodProbe = "dataset phenotype:abc\n" +
	"partition synth\ncontig synth/chr21\ncontig synth/chr22\n" +
	"partition -dash\ncontig -dash/chr21\n"

func newSharedFixture(t *testing.T) *sharedFixture {
	st, err := stack.Create(filepath.Join(t.TempDir(), "demo"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := stack.DefaultConfig()
	cfg.Name = "demo"
	fx := &sharedFixture{
		f: fakerunner.New(t), st: st, cfg: &cfg,
		state: &stack.State{
			Images:     map[string]string{"pic-sure-hpds-etl": "abc123abc123"},
			Components: map[string]stack.Component{"pic-sure": {Commit: "statecommit"}},
		},
		vols: map[string]map[string]string{
			"demo_hpds-data":    {stack.LabelStack: "demo"},
			"demo_hpds-genomic": {stack.LabelStack: "demo"},
		},
		users: map[string]string{},
		hpds:  "running",
		probe: goodProbe,
	}
	f := fx.f
	f.On(fakerunner.Glob("docker volume inspect *")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		fx.mu.Lock()
		defer fx.mu.Unlock()
		var vols []map[string]any
		for _, name := range c.Argv[3:] {
			labels, ok := fx.vols[name]
			if !ok {
				return docker.Result{Stderr: []byte("Error response from daemon: get " + name + ": no such volume\n"), ExitCode: 1}, nil
			}
			if name == fx.inspectFail {
				fx.inspectFail = ""
				return docker.Result{Stderr: []byte("Error response from daemon: context canceled\n"), ExitCode: 1}, nil
			}
			vols = append(vols, map[string]any{"Name": name, "Labels": labels})
		}
		out, err := json.Marshal(vols)
		return docker.Result{Stdout: out}, err
	})
	f.On(fakerunner.Glob("docker volume create *")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		fx.mu.Lock()
		defer fx.mu.Unlock()
		name := c.Argv[len(c.Argv)-1]
		if fx.onCreate != nil {
			fx.onCreate(name)
		}
		if _, ok := fx.vols[name]; !ok {
			labels := map[string]string{}
			for i, a := range c.Argv {
				if a == "--label" {
					k, v, _ := strings.Cut(c.Argv[i+1], "=")
					labels[k] = v
				}
			}
			fx.vols[name] = labels
		}
		return docker.Result{Stdout: []byte(name + "\n")}, nil
	})
	f.On(fakerunner.Glob("docker volume rm *")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		fx.mu.Lock()
		defer fx.mu.Unlock()
		delete(fx.vols, c.Argv[3])
		fx.removed = append(fx.removed, c.Argv[3])
		return docker.Result{}, nil
	})
	f.On(fakerunner.Glob("docker volume ls -q --filter label=" + ops.SharedDataLabel)).Do(func(context.Context, fakerunner.Call) (docker.Result, error) {
		fx.mu.Lock()
		defer fx.mu.Unlock()
		var names []string
		for n, l := range fx.vols {
			if _, ok := l[ops.SharedDataLabel]; ok {
				names = append(names, n)
			}
		}
		return docker.Result{Stdout: []byte(strings.Join(names, "\n"))}, nil
	})
	f.On(fakerunner.Glob("docker ps -a --no-trunc --filter volume=* --format {{json .}}")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		return docker.Result{Stdout: []byte(fx.users[strings.TrimPrefix(c.Argv[5], "volume=")])}, nil
	})
	f.On(fakerunner.Exact("docker", "image", "inspect", "hms-dbmi/pic-sure-hpds-etl:abc123abc123")).
		Stdout(`[{"Id":"sha256:1","Config":{"Labels":{"` + ops.ReactorSrcLabel + `":"imagecommit"}}}]`)
	f.On(fakerunner.Glob("docker compose * ps --all --format json hpds")).Do(func(context.Context, fakerunner.Call) (docker.Result, error) {
		return docker.Result{Stdout: []byte(psLine("hpds", fx.hpds, "healthy"))}, nil
	})
	f.On(fakerunner.Glob("docker compose * stop hpds"))
	f.On(fakerunner.Glob("docker compose * up -d --wait --wait-timeout 900 hpds"))
	f.On(fakerunner.Glob("docker run --rm --name demo-shared-probe-* --network none *")).Do(func(ctx context.Context, c fakerunner.Call) (docker.Result, error) {
		if fx.probeFn != nil {
			out, err := fx.probeFn(ctx, c.Argv)
			return docker.Result{Stdout: out}, err
		}
		return docker.Result{Stdout: []byte(fx.probe)}, nil
	})
	f.On(fakerunner.Glob("docker run --rm --name demo-shared-copy-* --network none *")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		fx.copies = append(fx.copies, c.Argv)
		return docker.Result{Stderr: []byte("cp: write error: No space left on device\n"), ExitCode: fx.copyExit}, nil
	})
	fx.d = &ops.Deps{
		Runner: f, Docker: docker.NewEngine(f), Rand: rand.Reader, Clock: ops.FixedClock(t0), Sink: &events.Recorder{},
		Compose: &docker.Compose{Runner: f, Files: []string{"/stack/.pic-sure/render/compose.yaml"}, ProjectDir: "/stack"},
	}
	return fx
}

func (fx *sharedFixture) publish(name string) (ops.SharedDataSet, error) {
	return ops.PublishSharedData(context.Background(), fx.d, fx.st, fx.cfg, fx.state, ops.PublishOptions{Name: name, CLIVersion: "v2.0.0-test"})
}

func TestPublishSharedData(t *testing.T) {
	fx := newSharedFixture(t)
	set, err := fx.publish("nhanes-v2")
	if err != nil {
		t.Fatal(err)
	}
	fx.f.AssertOrder(
		fakerunner.Glob("docker run * demo-shared-probe-* -v demo_hpds-data:/d:ro -v demo_hpds-genomic:/g:ro alpine:* sh -c *"),
		fakerunner.Glob("docker compose * stop hpds"),
		fakerunner.Glob("docker volume create * nhanes-v2_hpds-data"),
		fakerunner.Glob("docker volume create * nhanes-v2_hpds-genomic"),
		fakerunner.Glob("docker run * demo-shared-copy-* -v demo_hpds-data:/sd:ro -v demo_hpds-genomic:/sg:ro "+
			"-v nhanes-v2_hpds-data:/dd -v nhanes-v2_hpds-genomic:/dg alpine:* sh -c * sh nhanes-v2 2026-10-06T12:00:00Z synth -dash"),
		fakerunner.Glob("docker compose * up -d --wait --wait-timeout 900 hpds"),
	)
	want := ops.SharedDataSet{
		Name: "nhanes-v2", Contents: "phenotype=phenotype:abc genomic=synth,-dash", HPDSProfile: ops.GenomicProfile,
		PicsureCommit: "imagecommit", CLIVersion: "v2.0.0-test", SourceStack: "demo", Created: "2026-10-06T12:00:00Z",
		Volumes: []string{"nhanes-v2_hpds-data", "nhanes-v2_hpds-genomic"},
	}
	if !equalSet(set, want) {
		t.Errorf("set = %+v, want %+v", set, want)
	}
	for vol, kind := range map[string]string{"nhanes-v2_hpds-data": "hpds-data", "nhanes-v2_hpds-genomic": "hpds-genomic"} {
		l := fx.vols[vol]
		// The publishing stack's labels would make its destroy remove the set.
		for k := range l {
			if strings.HasPrefix(k, "org.hms-dbmi.picsure.stack") || strings.HasPrefix(k, "com.docker.compose.") {
				t.Errorf("%s has stack label %s", vol, k)
			}
		}
		checks := map[string]string{
			ops.SharedDataLabel: "nhanes-v2", ops.SharedDataKindLabel: kind, ops.SharedDataContentsLabel: want.Contents,
			ops.SharedDataProfileLabel: ops.GenomicProfile, ops.SharedDataCommitLabel: "imagecommit",
			ops.SharedDataCLIVersionLabel: "v2.0.0-test", ops.SharedDataSourceStackLabel: "demo", ops.SharedDataCreatedLabel: want.Created,
		}
		for k, v := range checks {
			if l[k] != v {
				t.Errorf("%s label %s = %q, want %q", vol, k, l[k], v)
			}
		}
	}
	script := fx.copies[0][slices.Index(fx.copies[0], "-c")+1]
	for _, s := range []string{"for f in encryption_key allObservationsStore.javabin columnMeta.javabin columnMeta.csv columnMetaErrors.csv .picsure-dataset;",
		"mkdir -p /dd/all", `cp -a "./$p" /dg/`, "/.picsure-published"} {
		if !strings.Contains(script, s) {
			t.Errorf("copy script lacks %q:\n%s", s, script)
		}
	}
}

func equalSet(a, b ops.SharedDataSet) bool {
	return slices.Equal(a.Volumes, b.Volumes) && a.Name == b.Name && a.Contents == b.Contents && a.HPDSProfile == b.HPDSProfile &&
		a.PicsureCommit == b.PicsureCommit && a.CLIVersion == b.CLIVersion && a.SourceStack == b.SourceStack && a.Created == b.Created
}

func TestPublishSharedDataPhenotypeOnlyWithHPDSStopped(t *testing.T) {
	fx := newSharedFixture(t)
	fx.hpds = "exited"
	fx.probe = ""
	fx.f.On(fakerunner.Exact("docker", "image", "inspect", "hms-dbmi/pic-sure-hpds-etl:abc123abc123")).Stdout(`[{"Id":"sha256:1"}]`)
	fx.state.Images = nil
	set, err := fx.publish("pheno")
	if err != nil {
		t.Fatal(err)
	}
	// Stopped all the same, in case it is about to restart; not started.
	fx.f.AssertCalled(fakerunner.Glob("docker compose * stop hpds"))
	fx.f.AssertNotCalled(fakerunner.Glob("docker compose * up *"))
	if set.Contents != "phenotype=unknown genomic=none" || set.HPDSProfile != "" || set.PicsureCommit != "statecommit" {
		t.Errorf("set = %+v", set)
	}
	if args := fx.copies[0][len(fx.copies[0])-2:]; !slices.Equal(args, []string{"pheno", "2026-10-06T12:00:00Z"}) {
		t.Errorf("copy args end %q, want no partitions", args)
	}
}

func TestPublishSharedDataRefusals(t *testing.T) {
	eleven := ""
	for i := range 11 {
		eleven += fmt.Sprintf("partition p%d\ncontig p%d/chr1\n", i, i)
	}
	tests := []struct {
		name  string
		set   string
		setup func(*sharedFixture)
		code  int
		want  string
	}{
		{"bad name", "Bad.Name", nil, exitcode.CodeUsage, "must match"},
		{"leading dash", "-x", nil, exitcode.CodeUsage, "must match"},
		{"shared mode", "x", func(fx *sharedFixture) {
			fx.cfg.HPDS.Data, fx.cfg.HPDS.SharedName = stack.HPDSShared, "other"
		}, 1, `uses the shared data set "other"`},
		{"set exists", "x", func(fx *sharedFixture) {
			fx.vols["x_hpds-genomic"] = map[string]string{ops.SharedDataLabel: "x"}
		}, exitcode.CodePrecondition, "x_hpds-genomic already exists. Data sets are immutable"},
		{"no source volume", "x", func(fx *sharedFixture) { delete(fx.vols, "demo_hpds-genomic") }, exitcode.CodePrecondition, "demo_hpds-genomic doesn't exist"},
		{"phenotype missing", "x", func(fx *sharedFixture) {
			fx.probe = "missing allObservationsStore.javabin\nmissing columnMeta.csv\n"
		}, exitcode.CodePrecondition, "missing allObservationsStore.javabin, columnMeta.csv"},
		{"no contig", "x", func(fx *sharedFixture) { fx.probe = goodProbe + "partition empty\n" }, exitcode.CodePrecondition, "partition empty in volume"},
		{"unindexed", "x", func(fx *sharedFixture) {
			fx.probe = goodProbe + "unindexed synth/chr21/variantIndex_fbbis.javabin\n"
		}, exitcode.CodePrecondition, "lacks the indexes HPDS writes on its first start: synth/chr21/variantIndex_fbbis.javabin"},
		{"too many partitions", "x", func(fx *sharedFixture) { fx.probe = eleven }, exitcode.CodePrecondition, "holds 11 genomic partitions"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newSharedFixture(t)
			if tt.setup != nil {
				tt.setup(fx)
			}
			_, err := fx.publish(tt.set)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			if got := exitcode.FromError(err); got != tt.code {
				t.Errorf("exit code %d, want %d", got, tt.code)
			}
			fx.f.AssertNotCalled(fakerunner.Glob("docker compose * stop hpds"))
			fx.f.AssertNotCalled(fakerunner.Glob("docker volume create *"))
			fx.f.AssertNotCalled(fakerunner.Glob("docker volume rm *"))
		})
	}
}

func TestPublishSharedDataCopyFailureRemovesItsVolumes(t *testing.T) {
	fx := newSharedFixture(t)
	fx.copyExit = 1
	_, err := fx.publish("x")
	if err == nil || !strings.Contains(err.Error(), "step shared-copy failed") || !strings.Contains(err.Error(), "No space left on device") {
		t.Fatalf("err = %v", err)
	}
	if !slices.Equal(fx.removed, []string{"x_hpds-data", "x_hpds-genomic"}) {
		t.Errorf("removed %q", fx.removed)
	}
	// HPDS was running, so it is put back.
	fx.f.AssertOrder(fakerunner.Glob("docker volume rm x_hpds-genomic"), fakerunner.Glob("docker compose * up -d --wait --wait-timeout 900 hpds"))
}

func TestPublishSharedDataRemovesOnlyItsOwnVolumes(t *testing.T) {
	fx := newSharedFixture(t)
	// Another publish of x creates the genomic volume after the check.
	fx.onCreate = func(name string) {
		if name == "x_hpds-genomic" {
			fx.vols[name] = map[string]string{ops.SharedDataLabel: "x"}
		}
	}
	_, err := fx.publish("x")
	if exitcode.FromError(err) != exitcode.CodePrecondition || !strings.Contains(err.Error(), "appeared while publishing") {
		t.Fatalf("err = %v", err)
	}
	if !slices.Equal(fx.removed, []string{"x_hpds-data"}) {
		t.Errorf("removed %q, want only x_hpds-data", fx.removed)
	}
	if _, ok := fx.vols["x_hpds-genomic"]; !ok {
		t.Error("the other publish's volume was removed")
	}
}

func TestListSharedData(t *testing.T) {
	fx := newSharedFixture(t)
	fx.vols["b_hpds-genomic"] = map[string]string{ops.SharedDataLabel: "b", ops.SharedDataContentsLabel: "phenotype=demo:nhanes genomic=false"}
	// AIO's sets name their source project in .source-project.
	fx.vols["b_hpds-data"] = map[string]string{ops.SharedDataLabel: "b", ops.SharedDataContentsLabel: "phenotype=demo:nhanes genomic=false",
		ops.SharedDataLabel + ".source-project": "picsure"}
	fx.vols["a_hpds-data"] = map[string]string{ops.SharedDataLabel: "a", ops.SharedDataProfileLabel: "bch-dev"}
	sets, err := ops.ListSharedData(context.Background(), fx.d)
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 2 || sets[0].Name != "a" || sets[1].Name != "b" {
		t.Fatalf("sets = %+v", sets)
	}
	if !slices.Equal(sets[0].Volumes, []string{"a_hpds-data"}) || sets[0].HPDSProfile != "bch-dev" {
		t.Errorf("a = %+v", sets[0])
	}
	if !slices.Equal(sets[1].Volumes, []string{"b_hpds-data", "b_hpds-genomic"}) || sets[1].Contents != "phenotype=demo:nhanes genomic=false" || sets[1].SourceStack != "picsure" {
		t.Errorf("b = %+v", sets[1])
	}
}

func TestRemoveSharedData(t *testing.T) {
	setLabels := map[string]string{ops.SharedDataLabel: "x"}
	tests := []struct {
		name    string
		setup   func(*sharedFixture)
		code    int
		want    string
		removed []string
	}{
		{"removes both", nil, 0, "", []string{"x_hpds-data", "x_hpds-genomic"}},
		{"one left", func(fx *sharedFixture) { delete(fx.vols, "x_hpds-data") }, 0, "", []string{"x_hpds-genomic"}},
		{"in use by a stopped container", func(fx *sharedFixture) {
			fx.users["x_hpds-genomic"] = `{"ID":"1","Names":"b-hpds-genomic-seed-1","State":"exited"}` + "\n"
		}, exitcode.CodePrecondition, "in use by b-hpds-genomic-seed-1 (exited)", nil},
		{"not a data set", func(fx *sharedFixture) {
			fx.vols["x_hpds-data"] = map[string]string{stack.LabelStack: "x"}
		}, exitcode.CodePrecondition, "x_hpds-data isn't part of a published data set", nil},
		{"missing", func(fx *sharedFixture) {
			delete(fx.vols, "x_hpds-data")
			delete(fx.vols, "x_hpds-genomic")
		}, exitcode.CodePrecondition, "there is no data set x", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newSharedFixture(t)
			fx.vols["x_hpds-data"], fx.vols["x_hpds-genomic"] = setLabels, setLabels
			if tt.setup != nil {
				tt.setup(fx)
			}
			removed, err := ops.RemoveSharedData(context.Background(), fx.d, "x")
			if tt.want == "" && err != nil || tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			if got := exitcode.FromError(err); err != nil && got != tt.code {
				t.Errorf("exit code %d, want %d", got, tt.code)
			}
			if !slices.Equal(removed, tt.removed) || !slices.Equal(fx.removed, tt.removed) {
				t.Errorf("removed %q (daemon %q), want %q", removed, fx.removed, tt.removed)
			}
		})
	}
	if _, err := ops.RemoveSharedData(context.Background(), newSharedFixture(t).d, "../x"); exitcode.FromError(err) != exitcode.CodeUsage {
		t.Errorf("bad name: %v", err)
	}
}

func TestPublishSharedDataRemovesAVolumeItCouldNotConfirm(t *testing.T) {
	fx := newSharedFixture(t)
	fx.inspectFail = "x_hpds-genomic"
	if _, err := fx.publish("x"); err == nil {
		t.Fatal("publish succeeded")
	}
	if !slices.Equal(fx.removed, []string{"x_hpds-data", "x_hpds-genomic"}) {
		t.Errorf("removed %q, want both", fx.removed)
	}
}

func TestPublishSharedDataRestartsARestartingHPDS(t *testing.T) {
	fx := newSharedFixture(t)
	fx.hpds = "restarting"
	if _, err := fx.publish("x"); err != nil {
		t.Fatal(err)
	}
	fx.f.AssertOrder(fakerunner.Glob("docker compose * stop hpds"), fakerunner.Glob("docker run * demo-shared-copy-*"),
		fakerunner.Glob("docker compose * up -d --wait --wait-timeout 900 hpds"))
}

// TestPublishSharedDataProbeScript runs the probe's script with the local
// sh over a directory tree standing in for the two volumes.
func TestPublishSharedDataProbeScript(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	data, genomic := t.TempDir(), t.TempDir()
	files := map[string]string{
		"encryption_key": "k", "allObservationsStore.javabin": "o", "columnMeta.javabin": "m", "columnMeta.csv": "",
		".picsure-dataset":                       "demo:nhanes\n",
		"stray.txt":                              "x",
		"synth/chr21/variantIndex_fbbis.javabin": "i", "synth/chr21/BucketIndexBySample.javabin": "b",
		"-dash/chr22/variantIndex_fbbis.javabin":  "i",
		".hidden/chr1/variantIndex_fbbis.javabin": "i", ".hidden/chr1/BucketIndexBySample.javabin": "b",
		"all-bak/chr21/x": "", ".promote-synth/chr21/x": "",
	}
	for name, content := range files {
		dir := genomic
		if !strings.Contains(name, "/") && name != "stray.txt" {
			dir = data
		}
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(genomic, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	fx := newSharedFixture(t)
	fx.probeFn = func(ctx context.Context, argv []string) ([]byte, error) {
		return exec.CommandContext(ctx, "sh", "-c", argv[slices.Index(argv, "-c")+1], "sh", data, genomic).Output()
	}
	write := func(path, content string) {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	steps := []struct {
		want string
		fix  func()
	}{
		{"is missing columnMeta.csv", func() { write(filepath.Join(data, "columnMeta.csv"), "c") }},
		{"genomic partition empty", func() { _ = os.Remove(filepath.Join(genomic, "empty")) }},
		{"lacks the indexes HPDS writes on its first start: -dash/chr22/BucketIndexBySample.javabin",
			func() { write(filepath.Join(genomic, "-dash/chr22/BucketIndexBySample.javabin"), "b") }},
	}
	for _, step := range steps {
		if _, err := fx.publish("x"); err == nil || !strings.Contains(err.Error(), step.want) {
			t.Fatalf("err = %v, want %q", err, step.want)
		}
		step.fix()
	}
	set, err := fx.publish("x")
	if err != nil {
		t.Fatal(err)
	}
	// all-bak and the unfinished promote are left out; hidden partitions aren't.
	if want := "phenotype=demo:nhanes genomic=-dash,synth,.hidden"; set.Contents != want {
		t.Errorf("contents %q, want %q", set.Contents, want)
	}
	if args := fx.copies[0][len(fx.copies[0])-3:]; !slices.Equal(args, []string{"-dash", "synth", ".hidden"}) {
		t.Errorf("copied partitions %q", args)
	}
}

func TestSharedDataProfile(t *testing.T) {
	set := map[string]string{ops.SharedDataLabel: "x", ops.SharedDataProfileLabel: "bch-dev"}
	tests := []struct {
		name   string
		setup  func(*sharedFixture)
		helper int // the marker check's exit code
		want   string
		code   int
	}{
		{"published", nil, 0, "", 0},
		{"no genomic volume", func(fx *sharedFixture) { delete(fx.vols, "x_hpds-genomic") }, 0, "shared data set x isn't on this Docker host (no volume x_hpds-genomic)", exitcode.CodePrecondition},
		{"not a data set", func(fx *sharedFixture) {
			fx.vols["x_hpds-data"] = map[string]string{stack.LabelStack: "x"}
		}, 0, "x_hpds-data isn't part of a published data set", exitcode.CodePrecondition},
		{"publish unfinished", nil, 42, "shared data set x is incomplete", exitcode.CodePrecondition},
		{"helper fails", nil, 1, "the helper container exited 1", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newSharedFixture(t)
			fx.vols["x_hpds-data"], fx.vols["x_hpds-genomic"] = set, map[string]string{ops.SharedDataLabel: "x"}
			if tt.setup != nil {
				tt.setup(fx)
			}
			fx.f.On(fakerunner.Glob("docker run --rm --name pic-sure-shared-check-* --network none -v x_hpds-data:/d:ro -v x_hpds-genomic:/g:ro alpine:* sh -c *.picsure-published*")).
				Exit(tt.helper)
			profile, err := ops.SharedDataProfile(context.Background(), fx.d, "x")
			if tt.want == "" {
				if err != nil || profile != "bch-dev" {
					t.Fatalf("profile %q, err %v; want bch-dev", profile, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) || exitcode.FromError(err) != tt.code {
				t.Fatalf("err = %v, want exit %d with %q", err, tt.code, tt.want)
			}
		})
	}
}
