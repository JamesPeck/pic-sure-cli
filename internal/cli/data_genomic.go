package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
)

// genomicReport is load-genomic's --json data.
type genomicReport struct {
	Partition string `json:"partition"`
	// Promoted lists the partitions copied into the live genomic data.
	Promoted []string `json:"promoted"`
	// Profile is hpds.profile after the load.
	Profile string `json:"profile"`
}

func (a *App) loadGenomic(cmd *cobra.Command, _ []string) error {
	f := cmd.Flags()
	var opts ops.GenomicLoadOptions
	opts.Partition, _ = f.GetString("partition")
	index, _ := f.GetString("vcf-index")
	dir, _ := f.GetString("vcf-dir")
	opts.HeapMB, _ = f.GetInt("heap")
	opts.Promote, _ = f.GetBool("promote")
	opts.AllPartitions, _ = f.GetBool("all-partitions")
	opts.Backup, _ = f.GetBool("backup")
	opts.EnableProfile, _ = f.GetBool("enable-profile")
	if f.Changed("heap") && opts.HeapMB == 0 {
		return exitcode.Usage("--heap must be a positive number of MB, not 0")
	}
	var err error
	if opts.VCFIndex, err = inputPath("--vcf-index", index, false); err != nil {
		return err
	}
	if dir == "" {
		dir = filepath.Dir(opts.VCFIndex)
	}
	if opts.VCFDir, err = inputPath("--vcf-dir", dir, true); err != nil {
		return err
	}
	if err := opts.Check(); err != nil {
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
	if err := ops.RefuseSharedHPDS(cfg); err != nil {
		return err
	}
	if opts.EnableProfile {
		// The render reads the files the config names.
		if err := cfg.CheckFiles(st.Dir); err != nil {
			return configError(err)
		}
	}
	state, err := st.LoadState()
	if errors.Is(err, fs.ErrNotExist) || err == nil && state.InitializedAt.IsZero() {
		return exitcode.Precondition("the stack in %s isn't initialised; run `pic-sure init %s` to finish it", st.Dir, st.Dir)
	}
	if err != nil {
		return err
	}
	compose, cfg, sec, err := a.stackComposeConfig(cmd, d.Runner, st)
	if err != nil {
		return err
	}
	d.Compose = compose
	root, err := cache.DefaultRoot()
	if err != nil {
		return err
	}
	c, err := cache.Open(root, cache.Options{Git: d.Git, Holder: cmd.CommandPath()})
	if err != nil {
		return err
	}
	opts.MkdirTemp = c.TempDir
	opts.Converge = ops.ConvergeOptions{Cache: c, CLIVersion: a.Info.Version, Compose: a.upCompose(d, st, cfg, sec)}

	state.StartOperation("data load-genomic", d.Clock.Now())
	if err := st.SaveState(state); err != nil {
		return err
	}
	promoted, err := ops.LoadGenomic(ctx, d, st, cfg, state, opts)
	if ferr := finishUp(d, st, err); err == nil {
		err = ferr
	}
	if err != nil {
		return err
	}
	report := genomicReport{Partition: opts.Partition, Promoted: promoted, Profile: cfg.HPDS.Profile}
	if report.Promoted == nil {
		report.Promoted = []string{}
	}
	return a.finish(report, func(w io.Writer) error {
		msg := fmt.Sprintf("Loaded partition %s into the staging volume.\n", opts.Partition)
		if len(promoted) > 0 {
			msg += fmt.Sprintf("Promoted %s into HPDS's genomic data.\n", strings.Join(promoted, ", "))
		}
		if opts.Promote || opts.EnableProfile {
			msg += fmt.Sprintf("HPDS is healthy, with profile %q.\n", cfg.HPDS.Profile)
		}
		_, err := io.WriteString(w, msg)
		return err
	})
}

// inputPath returns path made absolute, which must be an existing
// directory if dir, or else a regular file. Anything else is exit 2.
func inputPath(flag, path string, dir bool) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", exitcode.Usage("%s %s: %v", flag, path, err)
	}
	fi, err := os.Stat(abs)
	switch {
	case err != nil:
		return "", exitcode.Usage("%s: %v", flag, err)
	case dir && !fi.IsDir():
		return "", exitcode.Usage("%s %s isn't a directory", flag, path)
	case !dir && !fi.Mode().IsRegular():
		return "", exitcode.Usage("%s %s isn't a regular file", flag, path)
	}
	return abs, nil
}
