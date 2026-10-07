package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"

	"github.com/spf13/cobra"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/phenoinput"
)

// phenotypeReport is load-phenotype's --json data.
type phenotypeReport struct {
	// Dataset is the provenance marker the load wrote:
	// phenotype:<sha256 of the CSV loaded>.
	Dataset string `json:"dataset"`
}

func (a *App) loadPhenotype(cmd *cobra.Command, args []string) error {
	f := cmd.Flags()
	if f.Changed("input-dir") {
		return notImplemented("043")(cmd, args)
	}
	file, _ := f.GetString("file")
	entry, _ := f.GetString("entry")
	heap, _ := f.GetInt("heap")
	if heap < 0 || f.Changed("heap") && heap == 0 {
		return exitcode.Usage("--heap must be a positive number of MB, not %d", heap)
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
	if d.Compose, cfg, _, err = a.stackComposeConfig(cmd, d.Runner, st); err != nil {
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

	in, cleanup, err := phenoinput.Resolve(ctx, file, phenoinput.Options{Entry: entry, MkdirTemp: c.TempDir})
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

	state.StartOperation("data load-phenotype", d.Clock.Now())
	if err := st.SaveState(state); err != nil {
		return err
	}
	dataset, err := ops.LoadPhenotype(ctx, d, st, cfg, state, ops.PhenotypeLoadOptions{
		CSV:       in.CSV,
		HeapMB:    heap,
		MkdirTemp: c.TempDir,
	})
	if ferr := finishUp(d, st, err); err == nil {
		err = ferr
	}
	if err != nil {
		return err
	}
	return a.finish(phenotypeReport{Dataset: dataset}, func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "Loaded %s into HPDS (%s); HPDS is healthy.\n", file, dataset)
		return err
	})
}
