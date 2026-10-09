package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

func newDictionaryCmd(a *App) *cobra.Command {
	return newGroup("dictionary", "Rebuild and load the search dictionary",
		newDictionaryHydrateCmd(a),
		newDictionaryLoadCSVCmd(a),
		newDictionaryLoadFacetsCmd(a),
		newDictionaryWeightsCmd(a),
	)
}

const dictionaryLong = `
The stack must be up: the operation runs a dictionary-etl container on the
stack's data network, sends it the data from short-lived curl containers,
then removes it, touches dict.update_info and restarts dictionary-api.`

func newDictionaryHydrateCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "hydrate",
		Short: "Build the dictionary from the loaded HPDS data",
		Long: `Build the dictionary from the HPDS data. With local data, CreateColumnmetaCSV
first writes columnMeta.csv into the hpds-data volume; a shared data set is
read-only and must already hold one.` + dictionaryLong,
		Args: cobra.NoArgs,
	}
	facets := c.Flags().Bool("include-dataset-facets", false, "also create the default facets")
	clearFirst := c.Flags().Bool("clear", false, "empty the dictionary first")
	heap := c.Flags().Int("heap", ops.DefaultColumnMetaHeap, "CreateColumnmetaCSV's heap in `MB`")
	c.RunE = func(cmd *cobra.Command, _ []string) error {
		if *heap <= 0 {
			return exitcode.Usage("--heap must be a positive number of MB")
		}
		opts := ops.HydrateOptions{IncludeDatasetFacets: *facets, Clear: *clearFirst, Heap: *heap}
		return a.dictionary(cmd, []string{ops.StepColumnMeta, ops.StepHydrate}, "Dictionary hydrated.", func(ctx context.Context, x *dictionaryRun) error {
			return ops.DictionaryHydrate(ctx, x.d, x.st, x.cfg, x.sec, x.state, opts, a.Global.SkipSteps)
		})
	}
	return skippable(c)
}

func newDictionaryLoadCSVCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "load-csv --datasets FILE --concepts ZIP",
		Short: "Load a custom dictionary from CSV",
		Long: `Load a custom dictionary: the datasets CSV (with a ref column), then each
dataset's concepts from the concepts_*.csv files in the zip, matched on their
dataset_ref column.` + dictionaryLong,
		Args: cobra.NoArgs,
	}
	datasets := c.Flags().String("datasets", "", "the datasets CSV")
	concepts := c.Flags().String("concepts", "", "a zip of concepts_*.csv files")
	clearFirst := c.Flags().Bool("clear", false, "empty the dictionary first")
	_ = c.MarkFlagRequired("datasets")
	_ = c.MarkFlagRequired("concepts")
	c.RunE = func(cmd *cobra.Command, _ []string) error {
		opts := ops.LoadCSVOptions{Clear: *clearFirst}
		var err error
		if opts.Datasets, err = inputFile("--datasets", *datasets); err != nil {
			return err
		}
		if opts.Concepts, err = inputFile("--concepts", *concepts); err != nil {
			return err
		}
		ids := []string{ops.StepDatasets, ops.StepConcepts}
		if opts.Clear {
			ids = append([]string{ops.StepDictionaryClear}, ids...)
		}
		return a.dictionary(cmd, ids, "Dictionary loaded.", func(ctx context.Context, x *dictionaryRun) error {
			c, err := openCache(cmd)
			if err != nil {
				return err
			}
			opts.TempDir = c.TempDir
			return ops.DictionaryLoadCSV(ctx, x.d, x.st, x.cfg, x.sec, x.state, opts, a.Global.SkipSteps)
		})
	}
	return skippable(c)
}

func newDictionaryLoadFacetsCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "load-facets --categories FILE --facets FILE --concepts FILE",
		Short: "Load facet categories, facets and facet concepts",
		Long:  `Load facet categories, facets and facet concepts from CSV, in that order.` + dictionaryLong,
		Args:  cobra.NoArgs,
	}
	categories := c.Flags().String("categories", "", "the facet categories CSV")
	facets := c.Flags().String("facets", "", "the facets CSV")
	concepts := c.Flags().String("concepts", "", "the facet concepts CSV")
	for _, f := range []string{"categories", "facets", "concepts"} {
		_ = c.MarkFlagRequired(f)
	}
	c.RunE = func(cmd *cobra.Command, _ []string) error {
		var opts ops.FacetOptions
		var err error
		if opts.Categories, err = inputFile("--categories", *categories); err != nil {
			return err
		}
		if opts.Facets, err = inputFile("--facets", *facets); err != nil {
			return err
		}
		if opts.Concepts, err = inputFile("--concepts", *concepts); err != nil {
			return err
		}
		return a.dictionary(cmd, []string{ops.StepFacets}, "Facets loaded.", func(ctx context.Context, x *dictionaryRun) error {
			return ops.DictionaryLoadFacets(ctx, x.d, x.st, x.cfg, x.sec, x.state, opts, a.Global.SkipSteps)
		})
	}
	return skippable(c)
}

func newDictionaryWeightsCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "weights",
		Short: "Recompute the search weights",
		Long: `Recompute the dictionary's search weights with the dictionary-weights image.
The default weights file is the one in the stack's pic-sure source.`,
		Args: cobra.NoArgs,
	}
	weights := c.Flags().String("weights", "", "the weights CSV (default: the pic-sure source's)")
	c.RunE = func(cmd *cobra.Command, _ []string) error {
		var opts ops.WeightsOptions
		if *weights != "" {
			var err error
			if opts.Weights, err = inputFile("--weights", *weights); err != nil {
				return err
			}
		}
		return a.dictionary(cmd, []string{ops.StepWeights}, "Search weights recomputed.", func(ctx context.Context, x *dictionaryRun) error {
			if opts.Weights == "" {
				var err error
				if opts.Cache, err = openCache(cmd); err != nil {
					return err
				}
			}
			return ops.DictionaryWeights(ctx, x.d, x.st, x.cfg, x.sec, x.state, opts, a.Global.SkipSteps)
		})
	}
	return skippable(c)
}

func openCache(cmd *cobra.Command) (*cache.Cache, error) {
	root, err := cache.DefaultRoot()
	if err != nil {
		return nil, err
	}
	return cache.Open(root, cache.Options{Holder: cmd.CommandPath()})
}

// inputFile returns path made absolute, which must be a regular file we
// can read.
func inputFile(flag, path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", exitcode.Usage("%s: %w", flag, err)
	}
	f, err := os.Open(abs)
	if err != nil {
		return "", exitcode.Usage("%s: %w", flag, err)
	}
	fi, err := f.Stat()
	_ = f.Close()
	if err != nil {
		return "", exitcode.Usage("%s: %w", flag, err)
	}
	if !fi.Mode().IsRegular() {
		return "", exitcode.Usage("%s: %s is not a file", flag, abs)
	}
	return abs, nil
}

// dictionaryRun is what a dictionary command's operation works on.
type dictionaryRun struct {
	d     *ops.Deps
	st    *stack.Stack
	cfg   *stack.Config
	sec   *stack.Secrets
	state *stack.State
}

// dictionary runs a dictionary operation under the stack lock on an
// initialised, rendered stack, recording it in state.json. The
// ids are the operation's step IDs before dictionary-refresh, which every
// one ends with; a --skip-step naming none of them is exit 2 before the lock.
func (a *App) dictionary(cmd *cobra.Command, ids []string, done string, op func(context.Context, *dictionaryRun) error) error {
	ids = append(ids, ops.StepDictionaryRefresh)
	if err := checkSkipSteps(cmd, ids, a.Global.SkipSteps); err != nil {
		return err
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
	c, cfg, sec, err := a.stackComposeConfig(cmd, d.Runner, st)
	if err != nil {
		return err
	}
	d.Compose = c
	state, err := st.LoadState()
	if errors.Is(err, fs.ErrNotExist) || err == nil && state.InitializedAt.IsZero() {
		return exitcode.Precondition("the stack in %s isn't initialised; run `pic-sure init %s` to finish it", st.Dir, st.Dir)
	}
	if err != nil {
		return err
	}
	name := cmd.Parent().Name() + " " + cmd.Name()
	state.StartOperation(name, d.Clock.Now())
	if err := st.SaveState(state); err != nil {
		return err
	}
	err = op(ctx, &dictionaryRun{d: d, st: st, cfg: cfg, sec: sec, state: state})
	if ferr := finishUp(d, st, err); err == nil {
		err = ferr
	}
	if err != nil {
		return err
	}
	return a.finish(map[string]string{"operation": name}, func(w io.Writer) error {
		_, err := fmt.Fprintln(w, done)
		return err
	})
}
