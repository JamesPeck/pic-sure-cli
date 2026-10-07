package ops_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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

const loaderCSV = "PATIENT_NUM,CONCEPT_PATH,NUMERIC_VALUE,TEXT_VALUE\n1,\\demo\\age\\,42,\n"

// envSpy records the environment of each docker run, which the fake
// runner keeps only the names of.
type envSpy struct {
	docker.Runner
	mu   sync.Mutex
	envs map[string][]string // by container name
}

func (s *envSpy) record(c docker.Cmd) {
	if len(c.Argv) < 2 || c.Argv[1] != "run" {
		return
	}
	if i := slices.Index(c.Argv, "--name"); i > 0 {
		s.mu.Lock()
		s.envs[c.Argv[i+1]] = c.Env
		s.mu.Unlock()
	}
}

func (s *envSpy) Run(ctx context.Context, c docker.Cmd) (docker.Result, error) {
	s.record(c)
	return s.Runner.Run(ctx, c)
}

func (s *envSpy) Stream(ctx context.Context, c docker.Cmd, stdout, stderr io.Writer) (int, error) {
	s.record(c)
	return s.Runner.Stream(ctx, c, stdout, stderr)
}

func (s *envSpy) env(prefix string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, env := range s.envs {
		if strings.HasPrefix(name, prefix) {
			return env
		}
	}
	return nil
}

type loaderFixture struct {
	*hpdsKeyFixture
	state        *stack.State
	csv          string
	spy          *envSpy
	rec          *events.Recorder
	imageMissing bool
	hidden       string // the daemon sees no file under this host dir
	denied       bool   // and refuses the mount, as Docker Desktop does
	probeFails   bool   // docker can't run the probe at all
	probed       []string
	loaderExit   int
	health       string
	marker       []byte
}

func newLoaderFixture(t *testing.T) *loaderFixture {
	fx := &loaderFixture{hpdsKeyFixture: newHPDSKeyFixture(t), health: "healthy"}
	fx.csv = filepath.Join(t.TempDir(), "pheno.csv")
	if err := os.WriteFile(fx.csv, []byte(loaderCSV), 0o644); err != nil {
		t.Fatal(err)
	}
	fx.state = &stack.State{Images: map[string]string{"pic-sure-hpds-etl": "abc123abc123"}}
	f := fx.f
	f.On(fakerunner.Exact("docker", "image", "inspect", "hms-dbmi/pic-sure-hpds-etl:abc123abc123")).Do(func(context.Context, fakerunner.Call) (docker.Result, error) {
		if fx.imageMissing {
			return docker.Result{Stderr: []byte("Error response from daemon: No such image: hms-dbmi/pic-sure-hpds-etl:abc123abc123\n"), ExitCode: 1}, nil
		}
		return docker.Result{Stdout: []byte(`[{"Id":"sha256:1"}]`)}, nil
	})
	f.On(fakerunner.Glob("docker run --rm --name demo-hpds-input-* --network none * alpine:* sh -c *")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		var src string
		for _, a := range c.Argv {
			if s, ok := strings.CutSuffix(a, ":/input.csv:ro"); ok {
				src = s
			}
		}
		fx.probed = append(fx.probed, src)
		if fx.probeFails {
			return docker.Result{Stderr: []byte("docker: Error response from daemon: pull access denied for alpine\n"), ExitCode: 125}, nil
		}
		if fx.hidden != "" && strings.HasPrefix(src, fx.hidden+string(filepath.Separator)) {
			if fx.denied {
				return docker.Result{Stderr: []byte("docker: Error response from daemon: Mounts denied: \nThe path " + src + " is not shared from the host and is not known to Docker.\n"), ExitCode: 125}, nil
			}
			return docker.Result{ExitCode: 1}, nil
		}
		return docker.Result{Stdout: []byte(strconv.Itoa(len(loaderCSV)) + "\n")}, nil
	})
	f.On(fakerunner.Glob("docker rm -v -f demo-hpds-input-*"))
	f.On(fakerunner.Glob("docker compose * stop hpds"))
	f.On(fakerunner.Glob("docker run --rm --name demo-hpds-wipe-* --network none * -v demo_hpds-data:/data alpine:* sh -c *"))
	f.On(fakerunner.Glob("docker run --rm --name demo-hpds-etl-* --user 0:0 --network none *")).Do(func(context.Context, fakerunner.Call) (docker.Result, error) {
		return docker.Result{Stdout: []byte("loading\n"), ExitCode: fx.loaderExit}, nil
	})
	f.On(fakerunner.Glob("docker run -i --rm --name demo-hpds-marker-* --network none * -v demo_hpds-data:/data alpine:* sh -c *")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		fx.marker = c.Stdin
		return docker.Result{}, nil
	})
	f.On(fakerunner.Glob("docker compose * up -d --wait --wait-timeout 900 hpds"))
	f.On(fakerunner.Glob("docker compose * ps --all --format json hpds")).Do(func(context.Context, fakerunner.Call) (docker.Result, error) {
		return docker.Result{Stdout: []byte(psLine("hpds", "running", fx.health))}, nil
	})
	fx.spy = &envSpy{Runner: f, envs: map[string][]string{}}
	fx.rec = &events.Recorder{}
	fx.d = &ops.Deps{
		Runner: fx.spy, Docker: docker.NewEngine(fx.spy), Rand: rand.Reader, Clock: ops.FixedClock(t0), Sink: fx.rec,
		Compose: &docker.Compose{Runner: fx.spy, Files: []string{"/stack/.pic-sure/render/compose.yaml"}, ProjectDir: "/stack"},
	}
	return fx
}

func (fx *loaderFixture) load(opts ops.PhenotypeLoadOptions) (string, error) {
	if opts.CSV == "" {
		opts.CSV = fx.csv
	}
	return ops.LoadPhenotype(context.Background(), fx.d, fx.st, fx.cfg, fx.state, opts)
}

func TestLoadPhenotypeRunsTheLoaderBetweenStopAndStart(t *testing.T) {
	fx := newLoaderFixture(t)
	dataset, err := fx.load(ops.PhenotypeLoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	fx.f.AssertOrder(
		fakerunner.Glob("docker run * demo-hpds-input-* -v "+fx.csv+":/input.csv:ro *"),
		fakerunner.Glob("docker compose * stop hpds"),
		fakerunner.Glob("docker run * demo-hpds-wipe-* sh -c set -eu; cd /data && rm -f allObservationsStore.javabin allObservationsTemp.javabin columnMeta.javabin columnMeta.csv columnMetaErrors.csv .picsure-dataset"),
		fakerunner.Glob("docker run * demo-hpds-key-*"),
		fakerunner.Glob("docker run --rm --name demo-hpds-etl-* --user 0:0 --network none * -e HEAPSIZE -e LOADER_NAME -e LOADER_ARGS -v demo_hpds-data:/opt/local/hpds -v "+fx.csv+":/opt/local/hpds/allConcepts.csv:ro hms-dbmi/pic-sure-hpds-etl:abc123abc123"),
		fakerunner.Glob("docker run -i * demo-hpds-marker-* sh -c set -eu; rm -f /data/allConcepts.csv; cat > /data/.picsure-dataset"),
		fakerunner.Glob("docker compose * up -d --wait --wait-timeout 900 hpds"),
		fakerunner.Glob("docker compose * ps --all --format json hpds"),
	)
	sum := sha256.Sum256([]byte(loaderCSV))
	want := "phenotype:" + hex.EncodeToString(sum[:])
	if dataset != want || string(fx.marker) != want+"\n" {
		t.Errorf("dataset %q, marker %q; want %q", dataset, fx.marker, want)
	}
	env := fx.spy.env("demo-hpds-etl-")
	for _, e := range []string{"HEAPSIZE=4096", "LOADER_NAME=CSVLoaderNewSearch", "LOADER_ARGS="} {
		if !slices.Contains(env, e) {
			t.Errorf("loader env %q lacks %s", env, e)
		}
	}
}

func TestLoadPhenotypeDemoOptions(t *testing.T) {
	fx := newLoaderFixture(t)
	dataset, err := fx.load(ops.PhenotypeLoadOptions{Dataset: "demo:nhanes", HeapMB: 1024, LoaderArgs: ops.DemoLoaderArgs})
	if err != nil {
		t.Fatal(err)
	}
	if dataset != "demo:nhanes" || string(fx.marker) != "demo:nhanes\n" {
		t.Errorf("dataset %q, marker %q", dataset, fx.marker)
	}
	env := fx.spy.env("demo-hpds-etl-")
	for _, e := range []string{"HEAPSIZE=1024", "LOADER_ARGS=ROLLUP"} {
		if !slices.Contains(env, e) {
			t.Errorf("loader env %q lacks %s", env, e)
		}
	}
}

func TestLoadPhenotypeLoaderFailureLeavesHPDSStopped(t *testing.T) {
	fx := newLoaderFixture(t)
	fx.loaderExit = 1
	_, err := fx.load(ops.PhenotypeLoadOptions{})
	if err == nil || !strings.Contains(err.Error(), "the HPDS loader exited 1") || !strings.Contains(err.Error(), "HPDS is stopped") {
		t.Fatalf("err = %v", err)
	}
	fx.f.AssertNotCalled(fakerunner.Glob("docker run * demo-hpds-marker-*"))
	fx.f.AssertNotCalled(fakerunner.Glob("docker compose * up *"))
}

func TestLoadPhenotypeUnhealthyHPDSFails(t *testing.T) {
	fx := newLoaderFixture(t)
	fx.health = "unhealthy"
	_, err := fx.load(ops.PhenotypeLoadOptions{})
	if err == nil || !strings.Contains(err.Error(), `health "unhealthy"`) || !strings.Contains(err.Error(), "The data is loaded") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadPhenotypeRefusesBeforeTouchingHPDS(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*loaderFixture)
		heap  int
		want  string
		code  int
	}{
		{"shared data", func(fx *loaderFixture) { fx.cfg.HPDS.Data = stack.HPDSShared; fx.cfg.HPDS.SharedName = "set1" }, 0, "read-only", 1},
		{"no image recorded", func(fx *loaderFixture) { fx.state.Images = nil }, 0, "run `pic-sure up`", exitcode.CodePrecondition},
		{"image missing", func(fx *loaderFixture) { fx.imageMissing = true }, 0, "run `pic-sure build`", exitcode.CodePrecondition},
		{"bad heap", func(*loaderFixture) {}, -1, "--heap", exitcode.CodeUsage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newLoaderFixture(t)
			tc.setup(fx)
			_, err := fx.load(ops.PhenotypeLoadOptions{HeapMB: tc.heap})
			if err == nil || !strings.Contains(err.Error(), tc.want) || exitcode.FromError(err) != tc.code {
				t.Fatalf("err = %v (exit %d), want %q and exit %d", err, exitcode.FromError(err), tc.want, tc.code)
			}
			fx.f.AssertNotCalled(fakerunner.Glob("docker compose * stop hpds"))
		})
	}
}

func TestLoadPhenotypeCopiesACSVTheDaemonCantSee(t *testing.T) {
	for _, denied := range []bool{false, true} {
		t.Run(fmt.Sprintf("denied=%v", denied), func(t *testing.T) { testLoadCopies(t, denied) })
	}
}

func testLoadCopies(t *testing.T, denied bool) {
	fx := newLoaderFixture(t)
	fx.hidden, fx.denied = filepath.Dir(fx.csv), denied
	cacheTmp := t.TempDir()
	var copyDir string
	_, err := fx.load(ops.PhenotypeLoadOptions{MkdirTemp: func(pattern string) (string, error) {
		var err error
		copyDir, err = os.MkdirTemp(cacheTmp, pattern)
		return copyDir, err
	}})
	if err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(copyDir, "allConcepts.csv")
	if !slices.Equal(fx.probed, []string{fx.csv, copied}) {
		t.Errorf("probed %q", fx.probed)
	}
	fx.f.AssertCalled(fakerunner.Glob("docker run * demo-hpds-etl-* -v " + copied + ":/opt/local/hpds/allConcepts.csv:ro *"))
	if _, err := os.Stat(copyDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the copy's directory is still there: %v", err)
	}
}

func TestLoadPhenotypeFailsWhenTheDaemonSeesNoCopy(t *testing.T) {
	cacheTmp := t.TempDir()
	for name, mkdir := range map[string]func(string) (string, error){
		"no temp dir":     nil,
		"hidden copy too": func(p string) (string, error) { return os.MkdirTemp(cacheTmp, p) },
	} {
		t.Run(name, func(t *testing.T) {
			fx := newLoaderFixture(t)
			fx.hidden = filepath.Dir(fx.csv)
			if mkdir != nil {
				fx.hidden = filepath.Dir(cacheTmp)
				fx.csv = filepath.Join(cacheTmp, "pheno.csv")
				if err := os.WriteFile(fx.csv, []byte(loaderCSV), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			_, err := fx.load(ops.PhenotypeLoadOptions{MkdirTemp: mkdir})
			if err == nil || exitcode.FromError(err) != exitcode.CodePrecondition || !strings.Contains(err.Error(), "can't read") {
				t.Fatalf("err = %v", err)
			}
			fx.f.AssertNotCalled(fakerunner.Glob("docker compose * stop hpds"))
		})
	}
}

func TestLoadPhenotypeProbeFailureIsAnError(t *testing.T) {
	fx := newLoaderFixture(t)
	fx.probeFails = true
	_, err := fx.load(ops.PhenotypeLoadOptions{MkdirTemp: func(string) (string, error) {
		t.Fatal("copied the CSV after a docker failure")
		return "", nil
	}})
	if err == nil || !strings.Contains(err.Error(), "pull access denied") {
		t.Fatalf("err = %v", err)
	}
	fx.f.AssertNotCalled(fakerunner.Glob("docker compose * stop hpds"))
}

func TestLoadPhenotypeMissingKeyFailsBeforeStopping(t *testing.T) {
	fx := newLoaderFixture(t)
	if err := os.Remove(fx.st.Path(stack.HPDSKeyFile)); err != nil {
		t.Fatal(err)
	}
	_, err := fx.load(ops.PhenotypeLoadOptions{})
	if err == nil || !strings.Contains(err.Error(), "HPDS key") {
		t.Fatalf("err = %v", err)
	}
	fx.f.AssertNotCalled(fakerunner.Glob("docker compose * stop hpds"))
}
