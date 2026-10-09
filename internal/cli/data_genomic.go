package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
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

// genomicLoadFlags are load-genomic's flags for a load, which --recover
// takes none of.
var genomicLoadFlags = []string{"partition", "vcf-index", "vcf-dir", "heap", "promote", "all-partitions", "backup", "enable-profile"}

func (a *App) loadGenomic(cmd *cobra.Command, _ []string) error {
	f := cmd.Flags()
	if rec, _ := f.GetBool("recover"); rec {
		for _, name := range genomicLoadFlags {
			if f.Changed(name) {
				return withUsageHint(exitcode.Usage("--recover loads nothing, so it can't be combined with --%s", name))
			}
		}
		return a.recoverGenomic(cmd)
	}
	var missing []string
	for _, name := range []string{"partition", "vcf-index"} {
		if !f.Changed(name) {
			missing = append(missing, strconv.Quote(name))
		}
	}
	if len(missing) > 0 {
		return withUsageHint(exitcode.Usage("required flag(s) %s not set", strings.Join(missing, ", ")))
	}
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
	opts.LockUse = c.WithEvents(d.Sink, ops.GenomicInputStepID).LockUse
	opts.Converge = ops.ConvergeOptions{Cache: c, CLIVersion: a.Info.Version, Compose: a.upCompose(d, st, cfg, sec)}

	if err := checkOwned(cmd, d, st, cfg); err != nil {
		return err
	}
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

// genomicRecoverReport is load-genomic --recover's --json data.
type genomicRecoverReport struct {
	// Leftovers are the .promote-* and .old-* directories found.
	Leftovers []string `json:"leftovers"`
	// Partitions says what became of each partition they were left for.
	Partitions []recoveredPartition `json:"partitions"`
	// HPDSStarted reports that HPDS was running, so it was started again.
	HPDSStarted bool `json:"hpds_started"`
}

type recoveredPartition struct {
	Partition string `json:"partition"`
	// Result is completed, restored or discarded (ops.RecoveredPartition).
	Result string `json:"result"`
}

func (a *App) recoverGenomic(cmd *cobra.Command) error {
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
	if err := checkOwned(cmd, d, st, cfg); err != nil {
		return err
	}
	// Before the compose project is needed, which a stack on a shared
	// data set may not have rendered.
	if err := ops.RefuseSharedGenomicRecover(ctx, d, st, cfg); err != nil {
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
	state.StartOperation("data load-genomic --recover", d.Clock.Now())
	if err := st.SaveState(state); err != nil {
		return err
	}
	r, err := ops.RecoverGenomic(ctx, d, st, cfg)
	if ferr := finishUp(d, st, err); err == nil {
		err = ferr
	}
	if err != nil {
		return err
	}
	report := genomicRecoverReport{Leftovers: r.Leftovers, Partitions: []recoveredPartition{}, HPDSStarted: r.HPDSStarted}
	if report.Leftovers == nil {
		report.Leftovers = []string{}
	}
	for _, p := range r.Partitions {
		report.Partitions = append(report.Partitions, recoveredPartition(p))
	}
	return a.finish(report, func(w io.Writer) error {
		if len(r.Leftovers) == 0 {
			_, err := fmt.Fprintf(w, "The genomic data holds nothing an interrupted promote left; nothing to recover.\n")
			return err
		}
		msg := fmt.Sprintf("Recovered what an interrupted promote left (%s):\n", strings.Join(r.Leftovers, ", "))
		for _, p := range r.Partitions {
			msg += fmt.Sprintf("  %s %s\n", p.Result, p.Partition)
		}
		if r.HPDSStarted {
			msg += "HPDS is healthy again.\n"
		} else {
			msg += "HPDS wasn't running; start the stack with `pic-sure up`.\n"
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
