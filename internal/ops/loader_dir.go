package ops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

const (
	dirLoaderName = "SequentialLoader"
	// dirLoaderInput is where the sequential loader reads its input files.
	dirLoaderInput = "/opt/local/hpds_input"
	// dirLoaderConfig is the loader's optional per-file CSV settings.
	dirLoaderConfig = "config.json"
)

// dirLoaderOutput is what the sequential loader writes that HPDS reads. Only
// these are copied into hpds-data; the first two must exist.
var dirLoaderOutput = []string{
	"allObservationsStore.javabin",
	"columnMeta.javabin",
	"columnMeta.csv",
	"columnMetaErrors.csv",
}

// dirInputs returns the top-level files of dir the sequential loader reads,
// sorted: every *.csv and config.json, following symlinks. It refuses a
// directory with no CSV, and SQL inputs: the loader runs with no network,
// and SQL loading was dropped (D26). It also returns the other top-level
// entries, which the loader ignores.
func dirInputs(dir string) (files, ignored []string, err error) {
	fi, err := os.Stat(dir)
	if err != nil {
		return nil, nil, exitcode.Usage("--input-dir: %w", err)
	}
	if !fi.IsDir() {
		return nil, nil, exitcode.Usage("--input-dir: %s isn't a directory", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, exitcode.Usage("--input-dir: %w", err)
	}
	csvs := 0
	for _, e := range entries {
		name := e.Name()
		lower := strings.ToLower(name)
		if strings.HasSuffix(lower, ".sql") || lower == "sql.properties" {
			return nil, nil, exitcode.Usage("--input-dir: %s is an SQL input, which pic-sure doesn't load; export the data as CSV", filepath.Join(dir, name))
		}
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			return nil, nil, exitcode.Usage("--input-dir: %w", err)
		}
		switch {
		case !fi.Mode().IsRegular() || !strings.HasSuffix(lower, ".csv") && name != dirLoaderConfig:
			ignored = append(ignored, name)
		default:
			if strings.HasSuffix(lower, ".csv") {
				csvs++
			}
			files = append(files, name)
		}
	}
	if csvs == 0 {
		return nil, nil, exitcode.Usage("--input-dir: %s holds no .csv file", dir)
	}
	slices.Sort(files)
	return files, ignored, nil
}

// CheckPhenotypeDir checks an --input-dir as the load will, before
// anything is touched.
func CheckPhenotypeDir(dir string) error {
	_, _, err := dirInputs(dir)
	return err
}

// dirManifest is the provenance of an input directory's load: the sha256
// of one "<sha256>  <name>" line per input file, in name order.
func dirManifest(ctx context.Context, dir string, files []string) (string, error) {
	h := sha256.New()
	for _, f := range files {
		sum, err := fileSHA256(ctx, filepath.Join(dir, f))
		if err != nil {
			return "", err
		}
		_, _ = fmt.Fprintf(h, "%s  %s\n", sum, f)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// inputDir is input for InputDir: it checks the directory's files, works
// out the provenance, and makes sure the daemon sees them, copying them
// into the cache if it doesn't. Nothing has changed yet if it fails.
func (l *loader) inputDir(ctx context.Context, sink events.Sink) error {
	if _, err := l.st.LoadHPDSKey(); err != nil {
		return fmt.Errorf("the stack's HPDS key (%s): %w", l.st.Path(stack.HPDSKeyFile), err)
	}
	if err := l.findImage(ctx); err != nil {
		return err
	}
	files, ignored, err := dirInputs(l.opts.InputDir)
	if err != nil {
		return err
	}
	if len(ignored) > 0 {
		sink.Emit(events.Warning{ID: LoaderInputStepID, Text: "the loader ignores these entries of " + l.opts.InputDir + ": " + strings.Join(ignored, ", ")})
	}
	if l.opts.Dataset == "" {
		sink.Emit(events.Progress{ID: LoaderInputStepID, Text: "hashing the files in " + l.opts.InputDir})
		sum, err := dirManifest(ctx, l.opts.InputDir, files)
		if err != nil {
			return err
		}
		l.opts.Dataset = "phenotype:" + sum
	}

	paths := make([]string, len(files))
	sizes := make([]int64, len(files))
	for i, f := range files {
		paths[i] = path.Join(dirLoaderInput, f)
		if sizes[i], err = fileSize(filepath.Join(l.opts.InputDir, f)); err != nil {
			return err
		}
	}
	visible := func(dir string) (bool, error) {
		return l.daemonSees(ctx, "hpds-input", docker.Mount{Source: dir, Target: dirLoaderInput, ReadOnly: true}, paths, sizes)
	}
	if ok, err := visible(l.dir); ok || err != nil {
		return err
	}
	// As for a CSV: a daemon in a VM may not see the directory, or a
	// symlink in it may point outside it.
	if l.opts.MkdirTemp == nil {
		return exitcode.Precondition("the Docker daemon can't read the files in %s; move it under your home directory", l.opts.InputDir)
	}
	sink.Emit(events.Progress{ID: LoaderInputStepID, Text: "the Docker daemon can't read the files in " + l.opts.InputDir + "; copying them into the cache"})
	if err := l.makeCopyDir(ctx, "phenotype-dir-"); err != nil {
		return err
	}
	// The copy's directory is private, so the loader mounts one inside it
	// that a container can list.
	l.dir = filepath.Join(l.copyDir, "input")
	if err := os.Mkdir(l.dir, 0o755); err != nil {
		return err
	}
	for _, f := range files {
		if err := copyInput(ctx, filepath.Join(l.opts.InputDir, f), filepath.Join(l.dir, f)); err != nil {
			return err
		}
	}
	if ok, err := visible(l.dir); err != nil || !ok {
		if err != nil {
			return err
		}
		return exitcode.Precondition("the Docker daemon can't read the files in %s or their copy in the cache (%s); "+
			"move it under a directory the daemon shares", l.opts.InputDir, l.dir)
	}
	return nil
}

// loadDir runs the sequential loader over the input directory into a new
// temporary volume holding a copy of the HPDS key. hpds keeps running on
// its data meanwhile. It fails if the loader wrote no store.
func (l *loader) loadDir(ctx context.Context, sink events.Sink) error {
	name, err := docker.UniqueName(l.cfg.Name+"-hpds-load", l.d.Rand)
	if err != nil {
		return err
	}
	if err := l.d.Docker.VolumeCreate(ctx, name, l.st.Labels(l.cfg.Name)); err != nil {
		return fmt.Errorf("creating the temporary volume %s: %w", name, err)
	}
	l.tempVolume = name
	key, err := l.st.LoadHPDSKey()
	if err != nil {
		return err
	}
	if err := l.helper(ctx, name, "hpds-load-key", "umask 077; cat > /data/encryption_key", strings.NewReader(string(key)+"\n")); err != nil {
		return fmt.Errorf("copying the HPDS key into volume %s: %w", name, err)
	}
	if err := l.runETL(ctx, sink, dirLoaderName, []docker.Mount{
		{Source: name, Target: hpdsDir},
		{Source: l.dir, Target: dirLoaderInput, ReadOnly: true},
	}); err != nil {
		return err
	}
	if err := l.helper(ctx, name, "hpds-check", "test -s /data/"+dirLoaderOutput[0]+" && test -s /data/"+dirLoaderOutput[1], nil); err != nil {
		return fmt.Errorf("the HPDS loader wrote no %s or %s; its output is above and in the run log", dirLoaderOutput[0], dirLoaderOutput[1])
	}
	return nil
}

// copyLoaded copies the loader's output from the temporary volume into
// hpds-data, which the wipe emptied of the previous load's files, then
// writes the provenance marker.
func (l *loader) copyLoaded(ctx context.Context, _ events.Sink) error {
	mounts := []docker.Mount{{Source: l.volume(), Target: "/data"}, {Source: l.tempVolume, Target: "/new", ReadOnly: true}}
	script := `cd /new; for f; do if test -e "$f"; then cp -a "$f" /data/; fi; done; cat > /data/` + datasetMarker
	if err := l.script(ctx, "hpds-copy", mounts, script, dirLoaderOutput, strings.NewReader(l.opts.Dataset+"\n"), nil); err != nil {
		return fmt.Errorf("copying the loaded data from volume %s into %s: %w", l.tempVolume, l.volume(), err)
	}
	return nil
}

// removeTempVolume removes loadDir's volume, even after an interrupt, and
// warns if it can't.
func (l *loader) removeTempVolume(ctx context.Context) {
	if l.tempVolume == "" {
		return
	}
	if err := l.d.Docker.VolumeRemove(context.WithoutCancel(ctx), l.tempVolume); err != nil {
		l.d.Sink.Emit(events.Warning{Text: fmt.Sprintf("removing the temporary volume %s: %v; remove it with `docker volume rm %s`", l.tempVolume, err, l.tempVolume)})
	}
}
