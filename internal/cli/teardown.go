package cli

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"

	"github.com/spf13/cobra"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/render"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

func newResetCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "reset",
		Short: "Remove the stack's containers and data volumes; keep its config",
		Long: `Stop the stack and remove its containers and its data volumes: HPDS,
dictionary, staging, certs, truststore, and the database unless --keep-db.
The config, secrets, logs and TLS sources are kept, so the next ` + "`pic-sure up`" + `
sets the stack up again, empty.

On a terminal you confirm by typing the stack name; otherwise pass --yes.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			keepDB, _ := cmd.Flags().GetBool("keep-db")
			return a.teardown(cmd, ops.TeardownOptions{KeepDB: keepDB})
		},
	}
	c.Flags().Bool("keep-db", false, "keep the database volume")
	return c
}

func newDestroyCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "destroy",
		Short: "Remove everything the CLI created for this stack",
		Long: `Remove the stack's containers, every volume labelled for it, its dev
images, and the files and directories pic-sure created in the stack
directory (those manifest.json lists). Files pic-sure didn't create are
left, and listed. Shared data sets and other stacks are never touched.

Commit-tagged images are shared between stacks and left for ` + "`cache prune`" + `;
--prune-images removes those no other stack uses, by cache prune's rules.

On a terminal you confirm by typing the stack name; otherwise pass --yes.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			prune, _ := cmd.Flags().GetBool("prune-images")
			return a.teardown(cmd, ops.TeardownOptions{PruneImages: prune})
		},
	}
	c.Flags().Bool("prune-images", false, "also remove shared images no other stack uses")
	return c
}

// teardown runs reset or destroy, after the confirmation, under the stack
// lock.
func (a *App) teardown(cmd *cobra.Command, opts ops.TeardownOptions) error {
	destroy := cmd.Name() == "destroy"
	st, err := a.openStack(cmd)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	cfg, err := st.LoadConfig()
	if err != nil {
		return configError(err)
	}
	opts.Name = cfg.Name

	what := fmt.Sprintf("This stops stack %s and deletes its data, TLS and database volumes. Its config and secrets are kept.", cfg.Name)
	if opts.KeepDB {
		what = fmt.Sprintf("This stops stack %s and deletes its data and TLS volumes. Its database, config and secrets are kept.", cfg.Name)
	}
	if destroy {
		what = fmt.Sprintf("This removes stack %s in %s: its containers, volumes and dev images, and the files pic-sure created there.", cfg.Name, st.Dir)
	}
	if err := a.confirmName(cmd, cfg.Name, what); err != nil {
		return err
	}

	d := a.newDeps()
	lock, err := a.lockStack(cmd.Context(), cmd, st, d.Sink)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()
	if d.Compose, err = a.teardownCompose(d.Runner, st, cfg); err != nil {
		return err
	}
	if opts.PruneImages {
		root, err := cache.DefaultRoot()
		if err != nil {
			return err
		}
		if opts.Cache, err = cache.Open(root, cache.Options{Holder: cmd.CommandPath(), LockTimeout: pruneLockTimeout}); err != nil {
			return err
		}
	}

	if !destroy {
		report, err := ops.Reset(cmd.Context(), d, st, opts)
		if err != nil {
			return err
		}
		return a.finish(report, func(w io.Writer) error { return writeResetSummary(w, report) })
	}
	report, err := ops.Destroy(cmd.Context(), d, st, opts)
	if err != nil {
		return err
	}
	return a.finish(report, func(w io.Writer) error { return writeDestroySummary(w, st.Dir, report) })
}

// confirmName asks the user to type the stack name (§9.8). --yes answers
// for them. Without a terminal to ask on, it is exit 4, before anything
// has changed.
func (a *App) confirmName(cmd *cobra.Command, name, what string) error {
	if a.Global.Yes {
		return nil
	}
	if !a.canPrompt() {
		return exitcode.ConfirmRequired("%s needs confirmation: pass --yes, or run it on a terminal and type the stack name. Nothing was changed.", cmd.CommandPath())
	}
	_, _ = fmt.Fprintf(a.stderr(), "%s\nType the stack name (%s) to confirm: ", what, name)
	line, err := bufio.NewReader(a.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if strings.TrimSpace(line) != name {
		return exitcode.ConfirmRequired("that isn't %s; nothing was changed", name)
	}
	return nil
}

// teardownCompose is the compose adapter for reset and destroy. Unlike
// stackCompose, a stack that was never rendered gets none, and one without
// secrets.yaml gets empty secrets: compose down needs the variables set,
// not their values, and a half-made stack must still be removable.
func (a *App) teardownCompose(r docker.Runner, st *stack.Stack, cfg *stack.Config) (*docker.Compose, error) {
	c, err := docker.NewCompose(r, st.Dir, nil)
	if errors.Is(err, docker.ErrNotRendered) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sec, err := st.LoadSecrets()
	if errors.Is(err, fs.ErrNotExist) {
		sec, err = &stack.Secrets{}, nil
	}
	if err != nil {
		return nil, err
	}
	env, err := render.ComposeEnv(cfg, sec)
	if err != nil {
		return nil, err
	}
	c.Env = func() []string { return env }
	if a.output().mode == modeJSON {
		c.Progress = docker.ProgressJSON
	}
	return c, nil
}

func writeResetSummary(w io.Writer, r *ops.TeardownReport) error {
	var b bytes.Buffer
	fmt.Fprintf(&b, "Reset stack %s: removed %s.\n", r.Stack, countOf(len(r.Volumes), "volume"))
	if len(r.KeptVolumes) > 0 {
		fmt.Fprintf(&b, "Kept %s.\n", strings.Join(r.KeptVolumes, ", "))
	}
	b.WriteString("Run `pic-sure up` to set it up again.\n")
	_, err := w.Write(b.Bytes())
	return err
}

func writeDestroySummary(w io.Writer, dir string, r *ops.TeardownReport) error {
	var b bytes.Buffer
	fmt.Fprintf(&b, "Destroyed stack %s: removed %s and %s", r.Stack, countOf(len(r.Volumes), "volume"), countOf(len(r.Images), "dev image"))
	if r.Files != nil {
		fmt.Fprintf(&b, ", and %s", countOf(len(r.Files.Removed), "path"))
	}
	b.WriteString(".\n")
	if r.Pruned != nil {
		fmt.Fprintf(&b, "Pruned %s, freeing %s.\n", countOf(len(r.Pruned.Removed), "shared image"), ops.FormatBytes(r.Pruned.Freed))
	}
	switch {
	case r.Files == nil:
	case r.Files.DirRemoved:
		fmt.Fprintf(&b, "Removed %s.\n", dir)
	case len(r.Files.Remaining) > 0:
		fmt.Fprintf(&b, "Left in %s:\n", dir)
		for _, p := range r.Files.Remaining {
			fmt.Fprintf(&b, "  %s\n", p)
		}
	}
	_, err := w.Write(b.Bytes())
	return err
}

func countOf(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
