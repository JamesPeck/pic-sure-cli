package ops_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
)

const loaderCSV2 = "PATIENT_NUM,CONCEPT_PATH,NUMERIC_VALUE,TEXT_VALUE\n2,\\demo\\sex\\,,female\n"

// dirFixture is a loaderFixture for an --input-dir load.
type dirFixture struct {
	*loaderFixture
	dir       string
	copyExit  int
	checkExit int
	copyStdin []byte
	copyArgs  []string
	keyStdin  []byte
	volumes   []string // created
	removed   []string
}

func newDirFixture(t *testing.T) *dirFixture {
	fx := &dirFixture{loaderFixture: newLoaderFixture(t), dir: t.TempDir()}
	for name, data := range map[string]string{"a.csv": loaderCSV, "b.csv": loaderCSV2, "config.json": "{}\n", "notes.txt": "x\n"} {
		if err := os.WriteFile(filepath.Join(fx.dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(fx.dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := fx.f
	f.On(fakerunner.Glob("docker volume create * demo-hpds-load-*")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		fx.volumes = append(fx.volumes, c.Argv[len(c.Argv)-1])
		return docker.Result{}, nil
	})
	f.On(fakerunner.Glob("docker volume rm demo-hpds-load-*")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		fx.removed = append(fx.removed, c.Argv[len(c.Argv)-1])
		return docker.Result{}, nil
	})
	f.On(fakerunner.Glob("docker rm -v -f demo-hpds-etl-*"))
	f.On(fakerunner.Glob("docker run -i --rm --name demo-hpds-load-key-* --network none * -v demo-hpds-load-*:/data alpine:* sh -c *")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		fx.keyStdin = c.Stdin
		return docker.Result{}, nil
	})
	f.On(fakerunner.Glob("docker run --rm --name demo-hpds-check-* --network none * -v demo-hpds-load-*:/data alpine:* sh -c *")).Do(func(context.Context, fakerunner.Call) (docker.Result, error) {
		return docker.Result{ExitCode: fx.checkExit}, nil
	})
	f.On(fakerunner.Glob("docker run -i --rm --name demo-hpds-copy-* --network none * alpine:* sh -c *")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		fx.copyStdin = c.Stdin
		fx.copyArgs = c.Argv[slices.Index(c.Argv, "-c")+3:]
		return docker.Result{ExitCode: fx.copyExit}, nil
	})
	return fx
}

func (fx *dirFixture) loadDir(ctx context.Context, opts ops.PhenotypeLoadOptions) (string, error) {
	opts.InputDir = fx.dir
	return ops.LoadPhenotype(ctx, fx.d, fx.st, fx.cfg, fx.state, opts)
}

// assertVolumeRemoved checks the one temporary volume created was removed.
func (fx *dirFixture) assertVolumeRemoved(t *testing.T) {
	t.Helper()
	if len(fx.volumes) != 1 || !slices.Equal(fx.volumes, fx.removed) {
		t.Errorf("created volumes %q, removed %q", fx.volumes, fx.removed)
	}
}

func manifestDataset(files ...string) string {
	h := sha256.New()
	for _, f := range files {
		_, _ = fmt.Fprintf(h, "%s  %s\n", f[strings.IndexByte(f, ' ')+1:], f[:strings.IndexByte(f, ' ')])
	}
	return "phenotype:" + hex.EncodeToString(h.Sum(nil))
}

func sum(s string) string {
	b := sha256.Sum256([]byte(s))
	return hex.EncodeToString(b[:])
}

func TestLoadPhenotypeDirLoadsIntoATempVolumeBeforeStopping(t *testing.T) {
	fx := newDirFixture(t)
	dataset, err := fx.loadDir(context.Background(), ops.PhenotypeLoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	vol := fx.volumes[0]
	fx.f.AssertOrder(
		fakerunner.Glob("docker run * demo-hpds-input-* -v "+fx.dir+":/opt/local/hpds_input:ro alpine:* sh -c * sh /opt/local/hpds_input/a.csv /opt/local/hpds_input/b.csv /opt/local/hpds_input/config.json"),
		fakerunner.Glob("docker volume create --label * "+vol),
		fakerunner.Glob("docker run -i * demo-hpds-load-key-* -v "+vol+":/data alpine:* sh -c set -eu; umask 077; cat > /data/encryption_key"),
		fakerunner.Glob("docker run --rm --name demo-hpds-etl-* --user 0:0 --network none * -v "+vol+":/opt/local/hpds -v "+fx.dir+":/opt/local/hpds_input:ro hms-dbmi/pic-sure-hpds-etl:abc123abc123"),
		fakerunner.Glob("docker run * demo-hpds-check-* sh -c set -eu; test -s /data/allObservationsStore.javabin && test -s /data/columnMeta.javabin"),
		fakerunner.Glob("docker compose * stop hpds"),
		fakerunner.Glob("docker run * demo-hpds-wipe-*"),
		fakerunner.Glob("docker run * demo-hpds-key-*"),
		fakerunner.Glob("docker run -i * demo-hpds-copy-* -v demo_hpds-data:/data -v "+vol+":/new:ro alpine:* sh -c *"),
		fakerunner.Glob("docker compose * up -d --wait --wait-timeout 900 hpds"),
		fakerunner.Glob("docker volume rm "+vol),
	)
	want := manifestDataset("a.csv "+sum(loaderCSV), "b.csv "+sum(loaderCSV2), "config.json "+sum("{}\n"))
	if dataset != want || string(fx.copyStdin) != want+"\n" {
		t.Errorf("dataset %q, marker %q; want %q", dataset, fx.copyStdin, want)
	}
	if !slices.Equal(fx.copyArgs, []string{"allObservationsStore.javabin", "columnMeta.javabin", "columnMeta.csv", "columnMetaErrors.csv"}) {
		t.Errorf("copied %q", fx.copyArgs)
	}
	key, _ := fx.st.LoadHPDSKey()
	if string(fx.keyStdin) != string(key)+"\n" {
		t.Errorf("temp volume key %q", fx.keyStdin)
	}
	env := fx.spy.env("demo-hpds-etl-")
	for _, e := range []string{"HEAPSIZE=8000", "LOADER_NAME=SequentialLoader", "LOADER_ARGS="} {
		if !slices.Contains(env, e) {
			t.Errorf("loader env %q lacks %s", env, e)
		}
	}
	var warned bool
	for _, e := range fx.rec.Events() {
		if w, ok := e.(events.Warning); ok && strings.Contains(w.Text, "ignores") && strings.HasSuffix(w.Text, ": notes.txt, sub") {
			warned = true
		}
	}
	if !warned {
		t.Error("no warning about the ignored entries")
	}
	fx.assertVolumeRemoved(t)
}

func TestLoadPhenotypeDirLoaderFailureLeavesHPDSRunning(t *testing.T) {
	for name, setup := range map[string]func(*dirFixture){
		"loader exits 1": func(fx *dirFixture) { fx.loaderExit = 1 },
		"no store":       func(fx *dirFixture) { fx.checkExit = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			fx := newDirFixture(t)
			setup(fx)
			_, err := fx.loadDir(context.Background(), ops.PhenotypeLoadOptions{})
			if err == nil || !strings.Contains(err.Error(), "step hpds-load failed") || !strings.Contains(err.Error(), "HPDS and its data are unchanged") {
				t.Fatalf("err = %v", err)
			}
			fx.f.AssertNotCalled(fakerunner.Glob("docker compose * stop hpds"))
			fx.assertVolumeRemoved(t)
		})
	}
}

func TestLoadPhenotypeDirCopyFailureLeavesHPDSStopped(t *testing.T) {
	fx := newDirFixture(t)
	fx.copyExit = 1
	_, err := fx.loadDir(context.Background(), ops.PhenotypeLoadOptions{})
	if err == nil || !strings.Contains(err.Error(), "step hpds-copy failed") || !strings.Contains(err.Error(), "HPDS is stopped and its phenotype data was removed") {
		t.Fatalf("err = %v", err)
	}
	fx.f.AssertNotCalled(fakerunner.Glob("docker compose * up *"))
	fx.assertVolumeRemoved(t)
}

func TestLoadPhenotypeDirInterruptedRemovesTheVolume(t *testing.T) {
	fx := newDirFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fx.onLoader = func() error {
		cancel()
		return context.Canceled
	}
	if _, err := fx.loadDir(ctx, ops.PhenotypeLoadOptions{}); err == nil {
		t.Fatal("no error")
	}
	fx.f.AssertCalled(fakerunner.Glob("docker rm -v -f demo-hpds-etl-*"))
	fx.assertVolumeRemoved(t)
}

func TestLoadPhenotypeDirRefusals(t *testing.T) {
	for name, files := range map[string][]string{
		"no csv":         {"notes.txt"},
		"sql":            {"a.csv", "load.sql"},
		"sql.properties": {"a.csv", "sql.properties"},
	} {
		t.Run(name, func(t *testing.T) {
			fx := newDirFixture(t)
			fx.dir = t.TempDir()
			for _, f := range files {
				if err := os.WriteFile(filepath.Join(fx.dir, f), []byte(loaderCSV), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			_, err := fx.loadDir(context.Background(), ops.PhenotypeLoadOptions{})
			if exitcode.FromError(err) != exitcode.CodeUsage {
				t.Fatalf("err = %v (exit %d)", err, exitcode.FromError(err))
			}
			fx.f.AssertNotCalled(fakerunner.Glob("docker volume create *"))
			fx.f.AssertNotCalled(fakerunner.Glob("docker compose * stop hpds"))
		})
	}
	fx := newDirFixture(t)
	_, err := ops.LoadPhenotype(context.Background(), fx.d, fx.st, fx.cfg, fx.state, ops.PhenotypeLoadOptions{CSV: fx.csv, InputDir: fx.dir})
	if exitcode.FromError(err) != exitcode.CodeUsage {
		t.Fatalf("CSV and InputDir: err = %v", err)
	}
}

func TestLoadPhenotypeDirCopiesFilesTheDaemonCantSee(t *testing.T) {
	fx := newDirFixture(t)
	fx.hidden = fx.dir
	fx.dir = filepath.Join(fx.dir, "in")
	if err := os.Mkdir(fx.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fx.dir, "a.csv"), []byte(loaderCSV), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := cache.Open(filepath.Join(t.TempDir(), "cache"), cache.Options{LockTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	var copyDir string
	var lockedWhileCopying bool
	_, err = fx.loadDir(context.Background(), ops.PhenotypeLoadOptions{
		MkdirTemp: func(pattern string) (string, error) {
			var err error
			copyDir, err = c.TempDir(pattern)
			_, perr := c.LockPrune(context.Background())
			lockedWhileCopying = perr != nil
			return copyDir, err
		},
		LockUse: c.LockUse,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !lockedWhileCopying {
		t.Error("the use lock wasn't held while the copy existed")
	}
	copied := filepath.Join(copyDir, "input")
	if !slices.Equal(fx.probed, []string{fx.dir, copied}) {
		t.Errorf("probed %q", fx.probed)
	}
	fx.f.AssertCalled(fakerunner.Glob("docker run * demo-hpds-etl-* -v " + copied + ":/opt/local/hpds_input:ro *"))
	if _, err := os.Stat(copyDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the copy's directory is still there: %v", err)
	}
	lock, err := c.LockPrune(context.Background())
	if err != nil {
		t.Fatalf("the use lock is still held: %v", err)
	}
	_ = lock.Unlock()
}

func TestLoadPhenotypeDirProvenanceIgnoresOtherFiles(t *testing.T) {
	a, b := newDirFixture(t), newDirFixture(t)
	if err := os.WriteFile(filepath.Join(b.dir, "README"), []byte("more\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	da, err := a.loadDir(context.Background(), ops.PhenotypeLoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	db, err := b.loadDir(context.Background(), ops.PhenotypeLoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if da != db {
		t.Errorf("datasets differ: %q, %q", da, db)
	}
}
