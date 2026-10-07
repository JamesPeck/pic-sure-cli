package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"

	"github.com/spf13/cobra"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/netproxy"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

func newBuildCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "build [COMPONENT...]",
		Short: "Build the stack's images (default: every component)",
		Long: `Build, or in images.mode pull pull, the images of the components
(pic-sure, frontend, migrations, dictionary-etl) at the commits the stack
records, and record their tags. Images that are already up to date are
kept. A component with components.<name>.source set is built from that
checkout, tagged dev-<stack>-<sha12>, and rebuilt every time while the
checkout has uncommitted changes. Build logs go to .pic-sure/logs/build/.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			force, _ := cmd.Flags().GetBool("force")
			return a.build(cmd, args, force)
		},
	}
	c.Flags().Bool("force", false, "rebuild even if the images exist")
	return c
}

func (a *App) build(cmd *cobra.Command, components []string, force bool) (err error) {
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
	if err := cfg.CheckFiles(st.Dir); err != nil {
		return configError(err)
	}
	state, err := st.LoadState()
	if errors.Is(err, fs.ErrNotExist) {
		state = &stack.State{}
	} else if err != nil {
		return err
	}
	proxy, err := netproxy.New(netproxy.Config{HTTP: cfg.Proxy.HTTP, HTTPS: cfg.Proxy.HTTPS, NoProxy: cfg.Proxy.NoProxy}, netproxy.CatalogServices())
	if err != nil {
		return err
	}
	d.Git = d.Git.WithEnv(proxy.Env()...)
	root, err := cache.DefaultRoot()
	if err != nil {
		return err
	}
	c, err := cache.Open(root, cache.Options{Git: d.Git, Holder: cmd.CommandPath()})
	if err != nil {
		return err
	}

	state.StartOperation("build", d.Clock.Now())
	if err := st.SaveState(state); err != nil {
		return err
	}
	report, err := ops.Build(ctx, d, st, cfg, state, ops.BuildOptions{
		ImagesOptions: ops.ImagesOptions{Cache: c, Components: components, Force: force},
		SkipSteps:     a.Global.SkipSteps,
	})
	state.FinishOperation(err, d.Clock.Now())
	if serr := st.SaveState(state); err == nil && serr != nil {
		err = serr
	}
	if err != nil {
		return err
	}
	return a.finish(report, func(w io.Writer) error { return writeBuild(w, report) })
}

// writeBuild is the human summary: one line per image.
func writeBuild(w io.Writer, r *ops.BuildReport) error {
	for _, img := range r.Images {
		if _, err := fmt.Fprintf(w, "%-11s %s\n", img.Action, img.Ref); err != nil {
			return err
		}
	}
	return nil
}
