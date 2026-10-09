package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
)

func newSharedDataCmd(a *App) *cobra.Command {
	return newGroup("shared-data", "Publish HPDS data for other stacks to mount read-only",
		&cobra.Command{
			Use:   "publish NAME",
			Short: "Publish this stack's HPDS data as an immutable data set",
			Long: "Copy this stack's HPDS phenotype and genomic data into the volumes NAME_hpds-data and\n" +
				"NAME_hpds-genomic, which any stack on this Docker host can mount read-only\n" +
				"(hpds.data: shared, hpds.shared_name: NAME). HPDS is stopped during the copy.\n" +
				"Data sets are immutable: an existing NAME is refused, so publish a new name instead.",
			Args: cobra.ExactArgs(1),
			RunE: a.publishSharedData,
		},
		&cobra.Command{
			Use:   "list",
			Short: "List the published data sets",
			Args:  cobra.NoArgs,
			RunE:  a.listSharedData,
		},
		&cobra.Command{
			Use:   "remove NAME",
			Short: "Remove a data set no container uses",
			Args:  cobra.ExactArgs(1),
			RunE:  a.removeSharedData,
		},
	)
}

func (a *App) publishSharedData(cmd *cobra.Command, args []string) error {
	name := args[0]
	if err := ops.CheckSharedDataName(name); err != nil {
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

	cfg, err := st.LoadConfig()
	if err != nil {
		return configError(err)
	}
	if err := ops.RefusePublishFromShared(cfg); err != nil {
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

	if err := checkOwned(cmd, d, st, cfg); err != nil {
		return err
	}
	state.StartOperation("shared-data publish", d.Clock.Now())
	if err := st.SaveState(state); err != nil {
		return err
	}
	set, err := ops.PublishSharedData(ctx, d, st, cfg, state, ops.PublishOptions{Name: name, CLIVersion: a.Info.Version})
	if ferr := finishUp(d, st, err); err == nil {
		err = ferr
	}
	if err != nil {
		return err
	}
	return a.finish(set, func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "Published data set %s (%s) in volumes %s.\n"+
			"A new stack mounts it with `pic-sure init --hpds-data shared:%s`; an existing one with "+
			"`pic-sure config set hpds.shared_name %s`, then `pic-sure config set hpds.data shared` and `pic-sure up`.\n",
			set.Name, set.Contents, strings.Join(set.Volumes, " and "), set.Name, set.Name)
		return err
	})
}

// sharedDataList is shared-data list's --json data.
type sharedDataList struct {
	Sets []ops.SharedDataSet `json:"data_sets"`
}

func (a *App) listSharedData(cmd *cobra.Command, _ []string) error {
	sets, err := ops.ListSharedData(cmd.Context(), a.newDeps())
	if err != nil {
		return err
	}
	if sets == nil {
		sets = []ops.SharedDataSet{}
	}
	return a.printReport(sharedDataList{Sets: sets}, func(w io.Writer) error {
		if len(sets) == 0 {
			_, err := io.WriteString(w, "No published data sets.\n")
			return err
		}
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "NAME\tCONTENTS\tPROFILE\tCREATED\tFROM STACK\tVOLUMES")
		for _, s := range sets {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", s.Name, s.Contents, orDash(s.HPDSProfile), s.Created, orDash(s.SourceStack), strings.Join(s.Volumes, ","))
		}
		return tw.Flush()
	})
}

func (a *App) removeSharedData(cmd *cobra.Command, args []string) error {
	removed, err := ops.RemoveSharedData(cmd.Context(), a.newDeps(), args[0])
	if err != nil {
		return err
	}
	return a.finish(map[string]any{"name": args[0], "removed": removed}, func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "Removed data set %s (%s).\n", args[0], strings.Join(removed, ", "))
		return err
	})
}
