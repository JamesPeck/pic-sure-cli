package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

// pruneLockTimeout is how long prune waits for a cache lock before it
// skips that item as in use by a running build.
const pruneLockTimeout = 5 * time.Second

func newCacheCmd(a *App) *cobra.Command {
	prune := &cobra.Command{
		Use:   "prune",
		Short: "Remove cache entries no stack uses",
		Long: `Remove the commit-tagged and dev images, source trees, leftover build
contexts, downloads and temporary directories that nothing uses.

An item is in use, and kept, if any container (running or stopped)
references it, or if the state.json of a stack labelled on a container or
volume, or of the stack you run this in, names it. If a labelled stack's
directory can't be read (it was moved or deleted), prune keeps every shared
image and source tree that stack might use, unless --force is given. Items
made or changed in the last hour are always kept, as a running command may
not have recorded them yet. Images tagged by other tools, git clones and
lock files are never removed. prune waits up to 5 s for a running build to
finish with the cache, and otherwise fails, removing nothing.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			force, _ := cmd.Flags().GetBool("force")
			return a.cachePrune(cmd, ops.PruneOptions{DryRun: dryRun, Force: force})
		},
	}
	prune.Flags().Bool("dry-run", false, "show what would be removed, and remove nothing")
	prune.Flags().Bool("force", false, "also remove what a stack whose directory can't be read might use")
	return newGroup("cache", "Inspect and prune the host cache",
		&cobra.Command{
			Use:   "list",
			Short: "List cached sources, images and downloads, and what uses them",
			Args:  cobra.NoArgs,
			RunE:  func(cmd *cobra.Command, _ []string) error { return a.cacheList(cmd) },
		},
		prune,
	)
}

// openCache opens the default cache and finds the stack the command runs
// in, if any, for CacheOptions.
func (a *App) openCache(cmd *cobra.Command, lockTimeout time.Duration) (*cache.Cache, ops.CacheOptions, error) {
	var opts ops.CacheOptions
	cwd, err := os.Getwd()
	if err != nil {
		return nil, opts, err
	}
	dir, err := stack.Find(a.Global.Stack, cwd)
	switch {
	case err == nil:
		opts.Stacks = []string{dir}
	case !errors.Is(err, stack.ErrNotFound) || a.Global.Stack != "":
		return nil, opts, err
	}
	root, err := cache.DefaultRoot()
	if err != nil {
		return nil, opts, err
	}
	c, err := cache.Open(root, cache.Options{Holder: cmd.CommandPath(), LockTimeout: lockTimeout})
	return c, opts, err
}

func (a *App) cacheList(cmd *cobra.Command) error {
	c, opts, err := a.openCache(cmd, 0)
	if err != nil {
		return err
	}
	report, err := ops.CacheInventory(cmd.Context(), a.newDeps(), c, opts)
	if err != nil {
		return err
	}
	return a.printReport(report, func(w io.Writer) error { return writeCacheList(w, report) })
}

func (a *App) cachePrune(cmd *cobra.Command, opts ops.PruneOptions) error {
	c, cacheOpts, err := a.openCache(cmd, pruneLockTimeout)
	if err != nil {
		return err
	}
	opts.CacheOptions = cacheOpts
	report, err := ops.PruneCache(cmd.Context(), a.newDeps(), c, opts)
	if err != nil {
		return err
	}
	return a.finish(report, func(w io.Writer) error { return writePruneSummary(w, report) })
}

func writeCacheList(w io.Writer, r *ops.CacheReport) error {
	var b bytes.Buffer
	fmt.Fprintf(&b, "Cache: %s\n", r.Root)
	if len(r.Stacks) > 0 {
		b.WriteString("\nStacks:\n")
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		for _, s := range r.Stacks {
			note := ""
			if !s.Readable {
				note = "UNREADABLE: " + s.Error
			}
			_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\n", orDash(s.Name), s.Dir, note)
		}
		_ = tw.Flush()
	}
	b.WriteString("\n")
	if len(r.Items) == 0 {
		b.WriteString("The cache is empty.\n")
	} else {
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "KIND\tNAME\tSIZE\tSTATUS\tUSED BY")
		for _, it := range r.Items {
			users := it.UsedBy
			if len(users) == 0 && len(it.MayBeUsedBy) > 0 {
				users = []string{"maybe " + strings.Join(it.MayBeUsedBy, ", ")}
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", it.Kind, it.Name, ops.FormatBytes(it.Size), it.Status, orDash(strings.Join(users, ", ")))
		}
		_ = tw.Flush()
	}
	_, err := w.Write(b.Bytes())
	return err
}

func writePruneSummary(w io.Writer, r *ops.PruneReport) error {
	verb, freed := "Removed", "freed"
	if r.DryRun {
		verb, freed = "Would remove", "freeing"
	}
	var b bytes.Buffer
	for _, it := range r.Removed {
		fmt.Fprintf(&b, "%s %s %s (%s)\n", strings.ToLower(verb), it.Kind, it.Name, ops.FormatBytes(it.Size))
	}
	gone := map[string]bool{}
	for _, it := range r.Removed {
		gone[it.Name] = true
	}
	for _, s := range r.Skipped {
		gone[s.Name] = true
	}
	kept := map[string]int{}
	for _, it := range r.Items {
		if !gone[it.Name] {
			kept[it.Status]++
		}
	}
	var notes []string
	for _, s := range []string{ops.CacheInUse, ops.CacheUnknownStack, ops.CacheRecent} {
		if n := kept[s]; n > 0 {
			notes = append(notes, fmt.Sprintf("%d %s", n, s))
		}
	}
	items := "items"
	if len(r.Removed) == 1 {
		items = "item"
	}
	fmt.Fprintf(&b, "%s %d %s, %s %s.", verb, len(r.Removed), items, freed, ops.FormatBytes(r.Freed))
	if len(notes) > 0 {
		b.WriteString(" Kept " + strings.Join(notes, ", ") + ".")
	}
	b.WriteString("\n")
	_, err := w.Write(b.Bytes())
	return err
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
