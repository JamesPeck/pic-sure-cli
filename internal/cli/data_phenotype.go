package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/phenoinput"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

// phenotypeReport is load-phenotype's --json data.
type phenotypeReport struct {
	// Dataset is the provenance marker the load wrote:
	// phenotype:<sha256 of the CSV loaded, or of --input-dir's manifest>.
	Dataset string `json:"dataset"`
	// Dictionary is the dictionary source: auto or custom.
	Dictionary string `json:"dictionary"`
	// Weights reports whether the search weights were recomputed.
	Weights bool `json:"weights"`
}

// phenotypeArgs are load-phenotype's flags, checked; the paths are
// absolute. One of file and inputDir is set.
type phenotypeArgs struct {
	file, entry        string
	inputDir           string
	heap               int
	dictionary         string
	datasets, concepts string
	facets             ops.FacetOptions
	skipWeights        bool
}

// phenotypeFlags checks load-phenotype's flags before anything is touched,
// as AIO's etl.sh load_phenotype does.
func phenotypeFlags(f *pflag.FlagSet) (phenotypeArgs, error) {
	var p phenotypeArgs
	file, _ := f.GetString("file")
	p.entry, _ = f.GetString("entry")
	p.heap, _ = f.GetInt("heap")
	p.dictionary, _ = f.GetString("dictionary")
	p.skipWeights, _ = f.GetBool("skip-weights")
	if p.heap < 0 || f.Changed("heap") && p.heap == 0 {
		return p, exitcode.Usage("--heap must be a positive number of MB, not %d", p.heap)
	}
	var err error
	if f.Changed("input-dir") {
		if f.Changed("entry") {
			return p, exitcode.Usage("--entry is for --file")
		}
		dir, _ := f.GetString("input-dir")
		if p.inputDir, err = filepath.Abs(dir); err != nil {
			return p, exitcode.Usage("--input-dir: %w", err)
		}
		if err := ops.CheckPhenotypeDir(p.inputDir); err != nil {
			return p, err
		}
	} else if p.file, err = filepath.Abs(file); err != nil {
		return p, exitcode.Usage("--file: %w", err)
	}
	if p.dictionary != ops.DictionaryAuto && p.dictionary != ops.DictionaryCustom {
		return p, exitcode.Usage("--dictionary must be %s or %s, not %q", ops.DictionaryAuto, ops.DictionaryCustom, p.dictionary)
	}
	csvs := []struct {
		flag string
		path *string
	}{{"datasets", &p.datasets}, {"concepts", &p.concepts}}
	facets := []struct {
		flag string
		path *string
	}{{"facets-categories", &p.facets.Categories}, {"facets", &p.facets.Facets}, {"facet-concepts", &p.facets.Concepts}}
	if p.dictionary == ops.DictionaryAuto {
		for _, c := range append(csvs, facets...) {
			if f.Changed(c.flag) {
				return p, exitcode.Usage("--%s is for --dictionary custom", c.flag)
			}
		}
		return p, nil
	}
	if !f.Changed("datasets") || !f.Changed("concepts") {
		return p, exitcode.Usage("--dictionary custom requires --datasets and --concepts")
	}
	given := 0
	for _, c := range facets {
		if f.Changed(c.flag) {
			given++
		}
	}
	switch given {
	case 0:
	case len(facets):
		csvs = append(csvs, facets...)
	default:
		return p, exitcode.Usage("--facets-categories, --facets and --facet-concepts go together: give all three or none")
	}
	for _, c := range csvs {
		v, _ := f.GetString(c.flag)
		if *c.path, err = inputFile("--"+c.flag, v); err != nil {
			return p, err
		}
	}
	return p, nil
}

func (a *App) loadPhenotype(cmd *cobra.Command, args []string) error {
	p, err := phenotypeFlags(cmd.Flags())
	if err != nil {
		return err
	}
	if len(a.Global.SkipSteps) > 0 {
		return exitcode.Usage("--skip-step: load-phenotype's steps depend on each other, so none can be skipped")
	}

	ctx := cmd.Context()
	st, err := a.openStack(cmd)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	d := a.newDeps()
	lock, err := a.lockStack(ctx, cmd, st, d.Sink)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()

	cfg, err := st.LoadConfig()
	if err != nil {
		return configError(err)
	}
	if err := ops.RefuseSharedHPDS(cfg); err != nil {
		return err
	}
	state, err := st.LoadState()
	if errors.Is(err, fs.ErrNotExist) || err == nil && state.InitializedAt.IsZero() {
		return exitcode.Precondition("the stack in %s isn't initialised; run `pic-sure init %s` to finish it", st.Dir, st.Dir)
	}
	if err != nil {
		return err
	}
	var sec *stack.Secrets
	if d.Compose, cfg, sec, err = a.stackComposeConfig(cmd, d.Runner, st); err != nil {
		return err
	}
	root, err := cache.DefaultRoot()
	if err != nil {
		return err
	}
	c, err := cache.Open(root, cache.Options{Git: d.Git, Holder: cmd.CommandPath()})
	if err != nil {
		return err
	}

	load := ops.PhenotypeLoadOptions{InputDir: p.inputDir, HeapMB: p.heap, MkdirTemp: c.TempDir,
		LockUse: c.WithEvents(d.Sink, ops.LoaderInputStepID).LockUse}
	if p.file != "" {
		in, cleanup, err := phenoinput.Resolve(ctx, p.file, phenoinput.Options{Entry: p.entry, MkdirTemp: c.TempDir})
		var entryErr *phenoinput.EntryError
		switch {
		case errors.As(err, &entryErr), errors.Is(err, fs.ErrNotExist):
			return exitcode.Usage("%w", err)
		case err != nil:
			return err
		}
		defer func() { _ = cleanup() }()
		for _, w := range in.Warnings {
			d.Sink.Emit(events.Warning{Text: w})
		}
		load.CSV = in.CSV
	}

	state.StartOperation("data load-phenotype", d.Clock.Now())
	if err := st.SaveState(state); err != nil {
		return err
	}
	dataset, err := ops.DataLoadPhenotype(ctx, d, st, cfg, sec, state, ops.PhenotypeOptions{
		Load:        load,
		Dictionary:  p.dictionary,
		Datasets:    p.datasets,
		Concepts:    p.concepts,
		Facets:      p.facets,
		SkipWeights: p.skipWeights,
		Cache:       c,
	})
	if err != nil {
		err = p.rerunHint(st.Dir, dataset, err)
	}
	if ferr := finishUp(d, st, err); err == nil {
		err = ferr
	}
	if err != nil {
		return err
	}
	weights := "the search weights recomputed"
	if p.skipWeights {
		weights = "the search weights not recomputed (--skip-weights)"
	}
	return a.finish(phenotypeReport{Dataset: dataset, Dictionary: p.dictionary, Weights: !p.skipWeights}, func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "Loaded %s into HPDS (%s) and the %s dictionary, with %s; HPDS and dictionary-api are healthy.\n",
			p.input(), dataset, p.dictionary, weights)
		return err
	})
}

// rerunHint adds to a failed load's error the commands that retry it: the
// dictionary commands from the failed step on, when HPDS already has the
// new data, or else the whole load. Usage errors get none.
func (p phenotypeArgs) rerunHint(dir, dataset string, err error) error {
	if exitcode.FromError(err) == exitcode.CodeUsage {
		return err
	}
	pic := "pic-sure --stack " + shellQuote(dir) + " "
	var de *ops.PhenotypeDictionaryError
	if !errors.As(err, &de) {
		return fmt.Errorf("%w; to retry the load, run: %s", err, pic+p.command())
	}
	type group struct {
		cmd   string
		steps []string
	}
	var groups []group
	if p.dictionary == ops.DictionaryAuto {
		hydrate := "dictionary hydrate --clear"
		if p.heap != 0 {
			hydrate += " --heap " + strconv.Itoa(p.heap)
		}
		groups = append(groups, group{hydrate, []string{ops.StepColumnMeta, ops.StepHydrate}})
	} else {
		groups = append(groups, group{"dictionary load-csv --datasets " + shellQuote(p.datasets) + " --concepts " + shellQuote(p.concepts) + " --clear",
			[]string{ops.StepDictionaryClear, ops.StepDatasets, ops.StepConcepts}})
		if p.facets != (ops.FacetOptions{}) {
			groups = append(groups, group{"dictionary load-facets --categories " + shellQuote(p.facets.Categories) +
				" --facets " + shellQuote(p.facets.Facets) + " --concepts " + shellQuote(p.facets.Concepts), []string{ops.StepFacets}})
		}
	}
	if !p.skipWeights {
		groups = append(groups, group{"dictionary weights", []string{ops.StepWeights}})
	}
	// Every dictionary command ends with the refresh, so re-running the
	// last one retries a failed refresh. A step no command lists reruns
	// them all.
	from := 0
	for i, g := range groups {
		if slices.Contains(g.steps, de.Step) || de.Step == ops.StepDictionaryRefresh && i == len(groups)-1 {
			from = i
		}
	}
	var cmds []string
	for _, g := range groups[from:] {
		cmds = append(cmds, pic+g.cmd)
	}
	return fmt.Errorf("%w. HPDS has the new data (%s); to finish the load, run: %s", err, dataset, strings.Join(cmds, " && "))
}

// input is the file or directory loaded.
func (p phenotypeArgs) input() string {
	if p.inputDir != "" {
		return p.inputDir
	}
	return p.file
}

// command is the load-phenotype command line for p.
func (p phenotypeArgs) command() string {
	parts := []string{"data", "load-phenotype", "--file", shellQuote(p.file)}
	if p.inputDir != "" {
		parts = []string{"data", "load-phenotype", "--input-dir", shellQuote(p.inputDir)}
	}
	if p.entry != "" {
		parts = append(parts, "--entry", shellQuote(p.entry))
	}
	if p.heap != 0 {
		parts = append(parts, "--heap", strconv.Itoa(p.heap))
	}
	if p.dictionary == ops.DictionaryCustom {
		parts = append(parts, "--dictionary", "custom", "--datasets", shellQuote(p.datasets), "--concepts", shellQuote(p.concepts))
		if p.facets != (ops.FacetOptions{}) {
			parts = append(parts, "--facets-categories", shellQuote(p.facets.Categories), "--facets", shellQuote(p.facets.Facets),
				"--facet-concepts", shellQuote(p.facets.Concepts))
		}
	}
	if p.skipWeights {
		parts = append(parts, "--skip-weights")
	}
	return strings.Join(parts, " ")
}
