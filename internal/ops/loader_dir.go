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
	"github.com/JamesPeck/pic-sure-cli/internal/phenoinput"
)

const (
	dirLoaderName = "SequentialLoader"
	// dirLoaderInput is the directory the sequential loader reads.
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
// and SQL loading was dropped (D26). macOS metadata (phenoinput.MacMetadata)
// is skipped silently. It also returns the other top-level entries, which
// the loader never sees.
func dirInputs(dir string) (files, ignored []string, err error) {
	if dir == "" {
		return nil, nil, exitcode.Usage("--input-dir needs a directory")
	}
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
		if phenoinput.MacMetadata(name) {
			continue
		}
		lower := strings.ToLower(name)
		if strings.HasSuffix(lower, ".sql") || lower == "sql.properties" {
			return nil, nil, exitcode.Usage("--input-dir: %s is an SQL input, which pic-sure doesn't load; export the data as CSV", filepath.Join(dir, name))
		}
		isCSV := strings.HasSuffix(lower, ".csv")
		if !isCSV && name != dirLoaderConfig {
			ignored = append(ignored, name)
			continue
		}
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			return nil, nil, exitcode.Usage("--input-dir: %w", err)
		}
		if !fi.Mode().IsRegular() {
			ignored = append(ignored, name)
			continue
		}
		if isCSV {
			csvs++
		}
		files = append(files, name)
	}
	if csvs == 0 {
		return nil, nil, exitcode.Usage("--input-dir: %s holds no .csv file", dir)
	}
	slices.Sort(files)
	seen := map[string]string{}
	for _, f := range files {
		n := loaderInputName(f)
		if prev, ok := seen[n]; ok {
			return nil, nil, exitcode.Usage("--input-dir: %s and %s would both load as %s; rename one", prev, f, n)
		}
		seen[n] = f
	}
	return files, ignored, nil
}

// loaderInputName is what the loader sees name as: with a lowercase .csv.
// Upstream SequentialLoader uses LowRAMMultiCSVLoader only when every input
// ends in a lowercase .csv, and its own, different CSV parser otherwise.
func loaderInputName(name string) string {
	if ext := filepath.Ext(name); strings.EqualFold(ext, ".csv") {
		return strings.TrimSuffix(name, ext) + ".csv"
	}
	return name
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
	if err := l.preflight(ctx); err != nil {
		return err
	}
	files, ignored, err := dirInputs(l.opts.InputDir)
	if err != nil {
		return err
	}
	if len(ignored) > 0 {
		sink.Emit(events.Warning{ID: LoaderInputStepID, Text: "not loading these entries of " + l.opts.InputDir + ": " + strings.Join(ignored, ", ")})
	}
	if l.opts.Dataset == "" {
		sink.Emit(events.Progress{ID: LoaderInputStepID, Text: "hashing the files in " + l.opts.InputDir})
		sum, err := dirManifest(ctx, l.opts.InputDir, files)
		if err != nil {
			return err
		}
		l.opts.Dataset = "phenotype:" + sum
	}
	srcs := make([]string, len(files))
	l.inputNames = make([]string, len(files))
	for i, f := range files {
		srcs[i] = filepath.Join(l.opts.InputDir, f)
		l.inputNames[i] = loaderInputName(f)
	}
	l.inputs, err = l.ensureVisible(ctx, sink, l.opts.InputDir, srcs, l.inputNames, "phenotype-dir-")
	return err
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
	// Each input is mounted on its own: SequentialLoader reads any other
	// file in its input directory with a different CSV parser.
	mounts := []docker.Mount{{Source: name, Target: hpdsDir}}
	for i, in := range l.inputs {
		mounts = append(mounts, docker.Mount{Source: in, Target: path.Join(dirLoaderInput, l.inputNames[i]), ReadOnly: true})
	}
	if err := l.runETL(ctx, sink, dirLoaderName, mounts); err != nil {
		return err
	}
	check := []string{"sh", "-c", "test -s /data/" + dirLoaderOutput[0] + " && test -s /data/" + dirLoaderOutput[1]}
	code, err := l.container(ctx, "hpds-check", []docker.Mount{{Source: name, Target: "/data"}}, check, nil, nil, nil)
	if err != nil {
		return fmt.Errorf("checking the loader's output in volume %s: %w", name, err)
	}
	if code != 0 {
		return fmt.Errorf("the HPDS loader wrote no %s or %s, or an empty one; its output is above and in the run log", dirLoaderOutput[0], dirLoaderOutput[1])
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
