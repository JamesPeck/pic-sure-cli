package ops

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

// The phenotype loader's step IDs. Its steps depend on each other, so none
// can be skipped.
const (
	LoaderInputStepID = "hpds-input"
	LoaderStopStepID  = "hpds-stop"
	LoaderWipeStepID  = "hpds-wipe"
	LoaderRunStepID   = "hpds-load"
	LoaderCopyStepID  = "hpds-copy"
	LoaderStartStepID = "hpds-start"
)

// DefaultLoaderHeapMB is the loader's JVM heap when --heap isn't given.
const DefaultLoaderHeapMB = 4096

// DefaultDirLoaderHeapMB is the sequential loader's heap for an input
// directory when --heap isn't given, as in AIO's etl.sh load_multiple.
const DefaultDirLoaderHeapMB = 8000

// HPDSStartTimeout bounds the wait for hpds to turn healthy after a load.
// HPDS reads the whole phenotype store at startup, so it is generous.
const HPDSStartTimeout = 15 * time.Minute

// DemoLoaderArgs are the loader arguments for demo loads, as in AIO's
// load-demo-data.sh. Custom loads pass none, as AIO's etl.sh load_csv does.
// CSVLoaderNewSearch ignores ROLLUP today (only NO_ROLLUP is parsed, and
// variable-name rollup stays off either way), so both load the same data.
const DemoLoaderArgs = "ROLLUP"

const (
	hpdsService     = "hpds"
	hpdsETLImage    = "pic-sure-hpds-etl"
	hpdsDir         = "/opt/local/hpds"
	loaderCSVTarget = hpdsDir + "/allConcepts.csv"
	loaderName      = "CSVLoaderNewSearch"
	// datasetMarker is the provenance marker a load writes in hpds-data.
	datasetMarker = ".picsure-dataset"
)

// loaderStaleFiles are what a previous load left in hpds-data that this one
// replaces. A failed load can leave large temp files behind, and HPDS must
// never start on a store from one load and a columnMeta from another. The
// genomic data under all/ and the key are kept.
var loaderStaleFiles = []string{
	"allObservationsStore.javabin",
	"allObservationsTemp.javabin",
	"columnMeta.javabin",
	"columnMeta.csv",
	"columnMetaErrors.csv",
	datasetMarker,
}

// PhenotypeLoadOptions configures LoadPhenotype.
type PhenotypeLoadOptions struct {
	// CSV is the absolute host path of the allConcepts CSV to load.
	CSV string
	// InputDir, instead of CSV, is the absolute host path of a directory of
	// phenotype CSVs for the sequential loader.
	InputDir string
	// Dataset is the provenance marker's content: demo:<name> for a demo
	// load. Empty means phenotype:<sha256 of CSV>, or for InputDir the
	// sha256 of its manifest (see dirManifest).
	Dataset string
	// HeapMB is the loader's heap; zero means DefaultLoaderHeapMB, or
	// DefaultDirLoaderHeapMB for InputDir.
	HeapMB int
	// LoaderArgs is the loader's LOADER_ARGS: DemoLoaderArgs for a demo
	// load, empty for a custom one.
	LoaderArgs string
	// MkdirTemp makes a directory the Docker daemon can see, for a copy of a
	// CSV it can't (a file outside $HOME under Colima or Lima): the cache's
	// TempDir.
	MkdirTemp func(pattern string) (string, error)
	// LockUse, if set, takes the cache's use lock, held while a copy made
	// in a MkdirTemp directory exists, so `cache prune` leaves it alone.
	LockUse func(context.Context) (*cache.Lock, error)
}

// RefuseSharedHPDS returns the error a loader that writes HPDS data gives on
// a stack that uses a shared data set (§9.7): such a set is read-only.
func RefuseSharedHPDS(cfg *stack.Config) error {
	if cfg.HPDS.Data != stack.HPDSShared {
		return nil
	}
	return fmt.Errorf("this stack's HPDS uses the shared data set %q (hpds.data: shared), which is read-only; "+
		"set hpds.data to local to load data into the stack's own volume", cfg.HPDS.SharedName)
}

// LoadPhenotype is §9.6's loader, shared by `data demo` and `data
// load-phenotype`. The caller holds the stack lock and sets d.Compose. It
// checks the daemon can read the CSV, stops hpds, removes the previous
// load's files from hpds-data, installs the HPDS key, runs the hpds-etl
// loader, writes the provenance marker, then starts hpds and waits until it
// is healthy. If a step after the stop fails, hpds stays stopped and the
// error says how to recover. It returns the provenance marker's content.
//
// With InputDir, the sequential loader runs before hpds stops, into a
// temporary volume that is removed afterwards, and its output replaces the
// previous load's files once hpds is stopped (see loadDir).
func LoadPhenotype(ctx context.Context, d *Deps, st *stack.Stack, cfg *stack.Config, state *stack.State, opts PhenotypeLoadOptions) (string, error) {
	if err := RefuseSharedHPDS(cfg); err != nil {
		return "", err
	}
	if opts.HeapMB < 0 {
		return "", exitcode.Usage("--heap must be a positive number of MB, not %d", opts.HeapMB)
	}
	if (opts.CSV == "") == (opts.InputDir == "") {
		return "", exitcode.Usage("give either a phenotype CSV or an input directory")
	}
	if opts.HeapMB == 0 {
		opts.HeapMB = DefaultLoaderHeapMB
		if opts.InputDir != "" {
			opts.HeapMB = DefaultDirLoaderHeapMB
		}
	}
	l := &loader{d: d, st: st, cfg: cfg, state: state, opts: opts, csv: opts.CSV}
	defer l.removeCopy()
	var plan []steps.Step
	if opts.InputDir == "" {
		plan = []steps.Step{
			{ID: LoaderInputStepID, Title: "Check the phenotype input", Apply: l.input},
			{ID: LoaderStopStepID, Title: "Stop HPDS", Apply: l.stop},
			{ID: LoaderWipeStepID, Title: "Remove the previous HPDS data", Apply: l.wipe},
			HPDSKeyStep(d, st, cfg),
			{ID: LoaderRunStepID, Title: "Load the phenotype data", Apply: l.load},
			{ID: LoaderStartStepID, Title: "Start HPDS", Apply: l.start},
		}
	} else {
		defer l.removeTempVolume(ctx)
		plan = []steps.Step{
			{ID: LoaderInputStepID, Title: "Check the phenotype input directory", Apply: l.inputDir},
			{ID: LoaderRunStepID, Title: "Load the phenotype data into a temporary volume", Apply: l.loadDir},
			{ID: LoaderStopStepID, Title: "Stop HPDS", Apply: l.stop},
			{ID: LoaderWipeStepID, Title: "Remove the previous HPDS data", Apply: l.wipe},
			HPDSKeyStep(d, st, cfg),
			{ID: LoaderCopyStepID, Title: "Copy the loaded data into HPDS", Apply: l.copyLoaded},
			{ID: LoaderStartStepID, Title: "Start HPDS", Apply: l.start},
		}
	}
	err := steps.Run(ctx, d.Sink, plan, steps.Options{})
	if err == nil {
		return l.opts.Dataset, nil
	}
	// steps.Error's own advice, that a re-run skips the steps already done,
	// doesn't hold here: a re-run loads again from the start.
	var se *steps.Error
	if !errors.As(err, &se) || se.Interrupted {
		return "", err
	}
	failed := fmt.Errorf("step %s failed: %w", se.Step, se.Err)
	switch se.Step {
	case LoaderInputStepID, LoaderStopStepID:
		return "", failed
	case LoaderRunStepID:
		if opts.InputDir != "" {
			return "", fmt.Errorf("%w. HPDS and its data are unchanged", failed)
		}
	case LoaderWipeStepID:
		return "", fmt.Errorf("%w. HPDS is stopped: fix the problem and run the load again, or `pic-sure up` to start HPDS", failed)
	case LoaderStartStepID:
		return l.opts.Dataset, fmt.Errorf("%w. The data is loaded; see `pic-sure logs hpds`, then start HPDS with `pic-sure up`", failed)
	}
	return "", fmt.Errorf("%w. HPDS is stopped and its phenotype data was removed: fix the problem and run the load again, "+
		"or `pic-sure up` to start HPDS with no data", failed)
}

type loader struct {
	d     *Deps
	st    *stack.Stack
	cfg   *stack.Config
	state *stack.State
	opts  PhenotypeLoadOptions
	// csv is what the loader mounts: opts.CSV, or a copy the daemon can
	// see, in copyDir. inputs are the same for opts.InputDir's files.
	csv     string
	inputs  []string
	copyDir string
	useLock *cache.Lock
	image   string
	// tempVolume is loadDir's volume, once created.
	tempVolume string
}

func (l *loader) volume() string {
	v, _ := catalog.LookupVolume(hpdsDataVolume)
	return v.DockerName(l.cfg.Name)
}

// input finds the loader image, works out the provenance, and makes sure the
// daemon sees the CSV. Nothing has changed yet if it fails.
func (l *loader) input(ctx context.Context, sink events.Sink) error {
	// The hpds-key step would find a missing key only after the wipe.
	if _, err := l.st.LoadHPDSKey(); err != nil {
		return fmt.Errorf("the stack's HPDS key (%s): %w", l.st.Path(stack.HPDSKeyFile), err)
	}
	if err := l.findImage(ctx); err != nil {
		return err
	}

	if l.opts.Dataset == "" {
		sink.Emit(events.Progress{ID: LoaderInputStepID, Text: "hashing " + l.opts.CSV})
		sum, err := fileSHA256(ctx, l.opts.CSV)
		if err != nil {
			return err
		}
		l.opts.Dataset = "phenotype:" + sum
	}

	csv, err := l.ensureVisible(ctx, sink, l.opts.CSV, []string{l.opts.CSV}, []string{"allConcepts.csv"}, "phenotype-")
	if err != nil {
		return err
	}
	l.csv = csv[0]
	return nil
}

// ensureVisible returns where the loader can bind-mount srcs, the host
// files it reads: srcs, or if the daemon can't see them, their copies,
// named names, in a MkdirTemp directory. what names them in messages.
func (l *loader) ensureVisible(ctx context.Context, sink events.Sink, what string, srcs, names []string, pattern string) ([]string, error) {
	sizes := make([]int64, len(srcs))
	for i, src := range srcs {
		var err error
		if sizes[i], err = fileSize(src); err != nil {
			return nil, err
		}
	}
	if ok, err := l.visible(ctx, srcs, names, sizes); ok || err != nil {
		return srcs, err
	}
	// A daemon in a VM sees only the host directories shared with it
	// (Colima and Lima share $HOME), and a bind mount of any other file
	// silently shows an empty directory. The cache is one it must see.
	if l.opts.MkdirTemp == nil {
		return nil, exitcode.Precondition("the Docker daemon can't read %s; move it under your home directory", what)
	}
	sink.Emit(events.Progress{ID: LoaderInputStepID, Text: "the Docker daemon can't read " + what + "; copying into the cache"})
	if err := l.makeCopyDir(ctx, pattern); err != nil {
		return nil, err
	}
	dsts := make([]string, len(srcs))
	for i, src := range srcs {
		dsts[i] = filepath.Join(l.copyDir, names[i])
		if err := copyInput(ctx, src, dsts[i]); err != nil {
			return nil, err
		}
	}
	if ok, err := l.visible(ctx, dsts, names, sizes); err != nil || !ok {
		if err != nil {
			return nil, err
		}
		return nil, exitcode.Precondition("the Docker daemon can't read %s or its copy in the cache (%s); "+
			"move it under a directory the daemon shares", what, l.copyDir)
	}
	return dsts, nil
}

// findImage sets l.image to the stack's hpds-etl image, which must exist.
func (l *loader) findImage(ctx context.Context) error {
	img, _ := catalog.LookupImage(hpdsETLImage)
	tag := l.state.Images[img.Name]
	if tag == "" {
		return exitcode.Precondition("state.json records no %s image; run `pic-sure up` to build the stack's images", img.Name)
	}
	l.image = img.Repository() + ":" + tag
	ok, err := l.d.Docker.ImageExists(ctx, l.image)
	if err != nil {
		return err
	}
	if !ok {
		return exitcode.Precondition("the loader image %s is missing; run `pic-sure build` to rebuild it", l.image)
	}
	return nil
}

// visible reports whether a container that bind-mounts each of paths, as
// /input/<name>, sees a regular file of the matching size there.
func (l *loader) visible(ctx context.Context, paths, names []string, sizes []int64) (bool, error) {
	mounts := make([]docker.Mount, len(paths))
	files := make([]string, len(paths))
	for i, p := range paths {
		files[i] = "/input/" + names[i]
		mounts[i] = docker.Mount{Source: p, Target: files[i], ReadOnly: true}
	}
	return l.daemonSees(ctx, "hpds-input", mounts, files, sizes)
}

// daemonSees reports whether a container with mounts sees each of files, a
// path in the container, as a regular file of the matching size. A daemon
// that refuses the mount (Docker Desktop's "Mounts denied: The path … is
// not shared from the host") counts as not seeing them; any other docker
// failure is an error.
func (l *loader) daemonSees(ctx context.Context, prefix string, mounts []docker.Mount, files []string, sizes []int64) (bool, error) {
	var out, stderr bytes.Buffer
	script := `for f; do if test -f "$f"; then stat -c %s "$f"; else echo -1; fi; done`
	code, err := l.container(ctx, prefix, mounts, append([]string{"sh", "-c", script, "sh"}, files...), nil, &out, &stderr)
	if err != nil {
		// docker's error quotes only the last line of its stderr, which on
		// Docker Desktop is a link to the file-sharing docs.
		msg := strings.ToLower(err.Error() + "\n" + stderr.String())
		if strings.Contains(msg, "mounts denied") || strings.Contains(msg, "not shared from the host") {
			return false, nil
		}
		return false, fmt.Errorf("checking that the Docker daemon can read %s: %w", mounts[0].Source, err)
	}
	got := strings.Fields(out.String())
	if code != 0 || len(got) != len(sizes) {
		return false, nil
	}
	for i, s := range got {
		if n, err := strconv.ParseInt(s, 10, 64); err != nil || n != sizes[i] {
			return false, nil
		}
	}
	return true, nil
}

// makeCopyDir makes copyDir, holding the cache's use lock first.
func (l *loader) makeCopyDir(ctx context.Context, pattern string) error {
	if l.opts.LockUse != nil {
		lock, err := l.opts.LockUse(ctx)
		if err != nil {
			return err
		}
		l.useLock = lock
	}
	var err error
	l.copyDir, err = l.opts.MkdirTemp(pattern)
	return err
}

func (l *loader) removeCopy() {
	if l.copyDir != "" {
		_ = os.RemoveAll(l.copyDir)
	}
	if l.useLock != nil {
		_ = l.useLock.Unlock()
	}
}

func (l *loader) stop(ctx context.Context, sink events.Sink) error {
	out := events.NewLogWriter(sink, LoaderStopStepID, events.StreamStderr)
	defer func() { _ = out.Close() }()
	if err := l.d.Compose.Stop(ctx, out, hpdsService); err != nil {
		return fmt.Errorf("stopping hpds: %w", err)
	}
	return nil
}

func (l *loader) wipe(ctx context.Context, _ events.Sink) error {
	vol, err := l.st.EnsureVolume(ctx, l.d.Docker, l.cfg.Name, l.volume(), hpdsDataVolume)
	if err != nil {
		return err
	}
	script := "cd /data && rm -f " + strings.Join(loaderStaleFiles, " ")
	if err := l.helper(ctx, vol.Name, "hpds-wipe", script, nil); err != nil {
		return fmt.Errorf("removing the previous HPDS data from volume %s: %w", vol.Name, err)
	}
	return nil
}

func (l *loader) load(ctx context.Context, sink events.Sink) error {
	if err := l.runETL(ctx, sink, loaderName, []docker.Mount{
		{Source: l.volume(), Target: hpdsDir},
		{Source: l.csv, Target: loaderCSVTarget, ReadOnly: true},
	}); err != nil {
		return err
	}

	// Docker leaves an empty file in the volume where it mounted the CSV;
	// it isn't HPDS data. The marker comes last, so it always describes a
	// complete load.
	marker := strings.NewReader(l.opts.Dataset + "\n")
	script := "rm -f /data/allConcepts.csv; cat > /data/" + datasetMarker
	if err := l.helper(ctx, l.volume(), "hpds-marker", script, marker); err != nil {
		return fmt.Errorf("writing %s in volume %s: %w", datasetMarker, l.volume(), err)
	}
	return nil
}

func (l *loader) runETL(ctx context.Context, sink events.Sink, loader string, mounts []docker.Mount) error {
	name, err := docker.UniqueName(l.cfg.Name+"-hpds-etl", l.d.Rand)
	if err != nil {
		return err
	}
	stdout := events.NewLogWriter(sink, LoaderRunStepID, events.StreamStdout)
	stderr := events.NewLogWriter(sink, LoaderRunStepID, events.StreamStderr)
	code, err := l.d.Docker.Run(ctx, docker.RunOpts{
		Image: l.image,
		Name:  name,
		// The volume's files are root's, whatever USER the image sets.
		User:    "0:0",
		Remove:  true,
		Network: "none",
		Labels:  l.st.Labels(l.cfg.Name),
		Mounts:  mounts,
		Env: []string{
			"HEAPSIZE=" + strconv.Itoa(l.opts.HeapMB),
			"LOADER_NAME=" + loader,
			"LOADER_ARGS=" + l.opts.LoaderArgs,
		},
		Stdout: stdout,
		Stderr: stderr,
	})
	_ = stdout.Close()
	_ = stderr.Close()
	if err != nil {
		_ = l.d.Docker.Rm(context.WithoutCancel(ctx), name, true)
		return fmt.Errorf("running the HPDS loader: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("the HPDS loader exited %d; its output is above and in the run log", code)
	}
	return nil
}

func (l *loader) start(ctx context.Context, sink events.Sink) error {
	out := events.NewLogWriter(sink, LoaderStartStepID, events.StreamStderr)
	err := l.d.Compose.Up(ctx, docker.ComposeUpOpts{
		Services:    []string{hpdsService},
		Wait:        true,
		WaitTimeout: HPDSStartTimeout,
		Out:         out,
	})
	_ = out.Close()
	if err != nil {
		return fmt.Errorf("starting hpds: %w", err)
	}
	svc, err := composeService(ctx, l.d, hpdsService)
	if err != nil {
		return err
	}
	if svc == nil || svc.Health != "healthy" {
		state, health := "missing", ""
		if svc != nil {
			state, health = svc.State, svc.Health
		}
		return fmt.Errorf("hpds isn't healthy after starting (state %s, health %q)", state, health)
	}
	return nil
}

// helper runs script in an alpine container with volume at /data.
func (l *loader) helper(ctx context.Context, volume, prefix, script string, stdin io.Reader) error {
	return l.script(ctx, prefix, []docker.Mount{{Source: volume, Target: "/data"}}, script, nil, stdin, nil)
}

// script runs `sh -c "set -eu; SCRIPT" sh ARGS...` in an alpine container
// with mounts. A non-zero exit is an error ending with the last line of
// its stderr.
func (l *loader) script(ctx context.Context, prefix string, mounts []docker.Mount, script string, args []string, stdin io.Reader, stdout io.Writer) error {
	argv := []string{"sh", "-c", "set -eu; " + script}
	if len(args) > 0 {
		argv = append(append(argv, "sh"), args...)
	}
	var stderr bytes.Buffer
	code, err := l.container(ctx, prefix, mounts, argv, stdin, stdout, &stderr)
	if err != nil {
		return err
	}
	if code != 0 {
		err = fmt.Errorf("the helper container exited %d", code)
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			err = fmt.Errorf("%w: %s", err, msg[strings.LastIndexByte(msg, '\n')+1:])
		}
	}
	return err
}

// container runs a uniquely named, self-removing alpine container with no
// network, removing it if docker fails, and returns its exit code.
func (l *loader) container(ctx context.Context, prefix string, mounts []docker.Mount, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	name, err := docker.UniqueName(l.cfg.Name+"-"+prefix, l.d.Rand)
	if err != nil {
		return 0, err
	}
	alpine, _ := catalog.LookupImage("alpine")
	opts := docker.RunOpts{
		Image:   alpine.Ref,
		Name:    name,
		Remove:  true,
		Network: "none",
		Labels:  l.st.Labels(l.cfg.Name),
		Mounts:  mounts,
		Args:    args,
		Stdin:   stdin,
		Stdout:  stdout,
		Stderr:  stderr,
	}
	code, err := l.d.Docker.Run(ctx, opts)
	if err != nil {
		_ = l.d.Docker.Rm(context.WithoutCancel(ctx), name, true)
	}
	return code, err
}

func fileSize(path string) (int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

func fileSHA256(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, ctxReader{ctx, f}); err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyInput(ctx context.Context, src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	if _, err := io.Copy(out, ctxReader{ctx, in}); err != nil {
		return fmt.Errorf("copying %s: %w", src, err)
	}
	return nil
}

// ctxReader stops a long copy when ctx ends.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := context.Cause(c.ctx); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
