package ops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

// The genomic loader's step IDs, besides LoaderStopStepID,
// LoaderStartStepID and RenderStepID. Its steps depend on each other, so
// none can be skipped.
const (
	GenomicInputStepID    = "genomic-input"
	GenomicStageStepID    = "genomic-stage"
	GenomicSplitStepID    = "genomic-split"
	GenomicMetadataStepID = "genomic-metadata"
	GenomicFinalizeStepID = "genomic-finalize"
	GenomicPromoteStepID  = "genomic-promote"
	GenomicProfileStepID  = "hpds-profile"
)

// DefaultGenomicHeapMB is the genomic loaders' JVM heap when --heap isn't
// given, AIO's load-vcf default.
const DefaultGenomicHeapMB = 16000

// GenomicProfile is the HPDS Spring profile that reads the genomic store.
const GenomicProfile = "bch-dev"

const (
	genomicStagingVolume = "genomic-staging"
	hpdsGenomicVolume    = "hpds-genomic"
	// genomicBackup is the directory in genomic-staging that --backup
	// copies the live store into. Not in hpds-genomic, as AIO has it: HPDS
	// would load it as one more partition. It can't be a partition name,
	// since shared-data publish skips it.
	genomicBackup = "all-bak"
	// promotePrefix names a partition's copy in hpds-genomic until it is
	// complete, and oldPrefix the live partition it replaces, moved aside
	// until the copy is renamed into place.
	promotePrefix = ".promote-"
	oldPrefix     = ".old-"
	// hpdsMaxPartitions is the most partitions HPDS's
	// localPatientDistributed genomic processor starts with: it reads every
	// top-level directory of its genomic dir as one.
	hpdsMaxPartitions = 10
)

var partitionName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// GenomicLoadOptions configures LoadGenomic.
type GenomicLoadOptions struct {
	// Partition names the genomic partition the VCFs are loaded into.
	Partition string
	// VCFIndex is the absolute host path of the vcfIndex.tsv.
	VCFIndex string
	// VCFDir is the absolute host directory holding every VCF the index
	// names. Containers see it at the same path, since the index names the
	// VCFs by host path. Empty means VCFIndex's directory.
	VCFDir string
	// HeapMB is each loader's heap; zero means DefaultGenomicHeapMB.
	HeapMB int
	// Promote copies the partition into hpds-genomic, with HPDS stopped.
	Promote bool
	// AllPartitions makes Promote copy every staged partition, not only
	// this run's.
	AllPartitions bool
	// Backup makes Promote first copy the live store into all-bak in the
	// staging volume.
	Backup bool
	// EnableProfile sets hpds.profile to GenomicProfile, renders, and
	// starts HPDS on it.
	EnableProfile bool
	// Converge holds the render step's cache, CLI version and Composer, for
	// EnableProfile.
	Converge ConvergeOptions
	// MkdirTemp makes a directory the Docker daemon can see, for a copy of
	// VCFs it can't: the cache's TempDir.
	MkdirTemp func(pattern string) (string, error)
	// LockUse, if set, takes the cache's use lock, held while the copy in
	// the MkdirTemp directory exists, so `cache prune` leaves it alone.
	LockUse func(context.Context) (*cache.Lock, error)
}

// Check returns a usage error for options no load can run with.
func (o GenomicLoadOptions) Check() error {
	switch {
	case !partitionName.MatchString(o.Partition):
		return exitcode.Usage("--partition must match ^[A-Za-z0-9_-]+$, not %q", o.Partition)
	case o.Partition == genomicBackup:
		return exitcode.Usage("--partition %s is reserved for --backup's copy", genomicBackup)
	case o.HeapMB < 0:
		return exitcode.Usage("--heap must be a positive number of MB, not %d", o.HeapMB)
	case o.AllPartitions && !o.Promote:
		return exitcode.Usage("--all-partitions needs --promote")
	case o.Backup && !o.Promote:
		return exitcode.Usage("--backup needs --promote")
	case strings.ContainsRune(o.VCFDir, ':'):
		// Docker's -v syntax can't take it.
		return exitcode.Usage("--vcf-dir %s contains ':', which Docker can't mount", o.VCFDir)
	}
	return nil
}

// LoadGenomic is §9.6's genomic load. The caller holds the stack lock and
// sets d.Compose. It runs SplitChromosomeVcfLoader, VariantMetadataLoader
// and GenomicDatasetFinalizer over the VCFs into the stack's staging
// volume, where the partition replaces any earlier load of it, with HPDS
// still running on its live data. With Promote or EnableProfile it then
// stops hpds, copies the partition (or every staged one) into hpds-genomic,
// switches the profile and re-renders, and starts hpds again. It returns
// the partitions it promoted.
func LoadGenomic(ctx context.Context, d *Deps, st *stack.Stack, cfg *stack.Config, state *stack.State, opts GenomicLoadOptions) ([]string, error) {
	if err := RefuseSharedHPDS(cfg); err != nil {
		return nil, err
	}
	if opts.VCFDir == "" {
		opts.VCFDir = filepath.Dir(opts.VCFIndex)
	}
	if err := opts.Check(); err != nil {
		return nil, err
	}
	if opts.HeapMB == 0 {
		opts.HeapMB = DefaultGenomicHeapMB
	}
	l := &loader{d: d, st: st, cfg: cfg, state: state, opts: PhenotypeLoadOptions{MkdirTemp: opts.MkdirTemp, LockUse: opts.LockUse}}
	g := &genomicLoad{loader: l, opts: opts, vcfMount: opts.VCFDir}
	defer g.removeCopy()
	plan := []steps.Step{
		{ID: GenomicInputStepID, Title: "Check the VCF input", Apply: g.input},
		{ID: GenomicStageStepID, Title: "Stage the VCF index", Apply: g.stage},
		{ID: GenomicSplitStepID, Title: "Load the VCFs by chromosome", Apply: g.runLoader(GenomicSplitStepID, "SplitChromosomeVcfLoader", true)},
		{ID: GenomicMetadataStepID, Title: "Load the variant metadata", Apply: g.runLoader(GenomicMetadataStepID, "VariantMetadataLoader", true)},
		{ID: GenomicFinalizeStepID, Title: "Finalize partition " + opts.Partition, Apply: g.finalize},
	}
	restart := opts.Promote || opts.EnableProfile
	if restart {
		plan = append(plan, steps.Step{ID: LoaderStopStepID, Title: "Stop HPDS", Apply: g.stop})
	}
	if opts.Promote {
		plan = append(plan, steps.Step{ID: GenomicPromoteStepID, Title: "Promote the genomic data", Apply: g.promote})
	}
	if opts.EnableProfile {
		r := &upRestarts{d: d, st: st, cfg: cfg, opts: opts.Converge}
		plan = append(plan,
			steps.Step{ID: GenomicProfileStepID, Title: "Set hpds.profile to " + GenomicProfile, Apply: g.setProfile},
			r.watchRender(RenderStep(d, st, cfg, state, opts.Converge)))
	}
	if restart {
		plan = append(plan, withCompose(d, opts.Converge, steps.Step{ID: LoaderStartStepID, Title: "Start HPDS", Apply: g.start}))
	}

	err := steps.Run(ctx, d.Sink, plan, steps.Options{})
	if err == nil {
		return g.promoted, nil
	}
	// steps.Error's own advice, that a re-run resumes or skips the steps
	// already done, doesn't hold here: a re-run loads again from the start.
	var se *steps.Error
	if !errors.As(err, &se) {
		return nil, err
	}
	failed := fmt.Errorf("step %s failed: %w", se.Step, se.Err)
	again := "fix the problem and run the load again"
	if se.Interrupted {
		again = "run the load again"
	}
	switch {
	case se.Interrupted && se.Step == GenomicPromoteStepID && g.promoteErr != nil:
		failed = fmt.Errorf("stopped at step %s: %w: %v", se.Step, se.Err, g.promoteErr)
	case se.Interrupted:
		failed = fmt.Errorf("stopped at step %s: %w", se.Step, se.Err)
	}
	switch se.Step {
	case GenomicInputStepID:
		return nil, failed
	case LoaderStopStepID:
		if se.Interrupted {
			return nil, fmt.Errorf("%w. HPDS may be stopped: run the load again, or `pic-sure up` to start HPDS", failed)
		}
		return nil, failed
	case GenomicStageStepID, GenomicSplitStepID, GenomicMetadataStepID, GenomicFinalizeStepID:
		return nil, fmt.Errorf("%w. HPDS and its data are unchanged; %s", failed, again)
	case GenomicPromoteStepID:
		if g.unrecovered {
			return nil, fmt.Errorf("%w. HPDS is stopped; don't start it until a load with --promote has recovered %s", failed, g.vol(hpdsGenomicVolume))
		}
		return nil, fmt.Errorf("%w. HPDS is stopped: %s, or `pic-sure up` to start HPDS on the partitions it has", failed, again)
	case LoaderStartStepID:
		return g.promoted, fmt.Errorf("%w. See `pic-sure logs hpds`, then start HPDS with `pic-sure up`", failed)
	default: // the profile and render steps
		return g.promoted, fmt.Errorf("%w. HPDS is stopped: fix the problem, then `pic-sure up` renders the stack and starts HPDS "+
			"(hpds.profile is %q)", failed, cfg.HPDS.Profile)
	}
}

type genomicLoad struct {
	*loader
	opts  GenomicLoadOptions
	index []byte
	vcfs  []string
	sizes []int64
	// vcfMount is what the loaders mount at opts.VCFDir: the directory
	// itself, or a copy the daemon can see, in copyDir.
	vcfMount string
	// toPromote is what Promote copies, and promoted what it has copied.
	toPromote []string
	promoted  []string
	// leftovers are the .promote- and .old- directories an interrupted
	// promote left in hpds-genomic, which promote recovers first.
	leftovers []string
	// wasLive are the partitions hpds-genomic held before this load.
	wasLive []string
	// promoteErr is what promote returned, which steps.Run replaces with
	// the context's cause when interrupted. unrecovered reports that
	// settling hpds-genomic after it failed also failed.
	promoteErr  error
	unrecovered bool
}

func (g *genomicLoad) vol(name string) string {
	v, _ := catalog.LookupVolume(name)
	return v.DockerName(g.cfg.Name)
}

// input finds the loader image, reads the index, and makes sure the daemon
// sees every VCF it names. If it fails, nothing has changed but perhaps
// the creation of an empty hpds-genomic.
func (g *genomicLoad) input(ctx context.Context, sink events.Sink) error {
	if err := g.findImage(ctx); err != nil {
		return err
	}
	var err error
	if g.index, err = os.ReadFile(g.opts.VCFIndex); err != nil {
		return err
	}
	if g.vcfs, g.sizes, err = vcfIndexFiles(g.opts.VCFIndex, g.index, g.opts.VCFDir); err != nil {
		return err
	}
	if err := g.checkVisible(ctx, sink); err != nil {
		return err
	}
	if !g.opts.Promote && !g.opts.EnableProfile {
		return nil
	}
	dirs, err := g.livePartitions(ctx)
	if err != nil {
		return err
	}
	var live []string
	for _, d := range dirs {
		switch p, old := strings.CutPrefix(d, oldPrefix); {
		case old || strings.HasPrefix(d, promotePrefix):
			g.leftovers = append(g.leftovers, d)
			if old && !slices.Contains(live, p) {
				live = append(live, p)
			}
		case !slices.Contains(live, d):
			live = append(live, d)
		}
	}
	g.wasLive = live
	if len(g.leftovers) > 0 {
		what := g.vol(hpdsGenomicVolume) + " holds what an interrupted promote left (" + strings.Join(g.leftovers, ", ") + ")"
		if g.opts.Promote {
			sink.Emit(events.Progress{ID: GenomicInputStepID, Text: what + "; it is recovered once HPDS stops"})
		} else {
			sink.Emit(events.Warning{ID: GenomicInputStepID, Text: what + ", which HPDS loads as partitions; a load with --promote recovers it"})
		}
	}
	if !g.opts.Promote {
		if len(live) == 0 {
			sink.Emit(events.Warning{ID: GenomicInputStepID, Text: "--enable-profile without --promote, and " + g.vol(hpdsGenomicVolume) +
				" holds no genomic partition: HPDS's " + GenomicProfile + " profile won't start without one"})
		}
		return nil
	}
	g.toPromote = []string{g.opts.Partition}
	if g.opts.AllPartitions {
		dirs, err := g.dirs(ctx, g.vol(genomicStagingVolume), "/data/genomic")
		if err != nil {
			return err
		}
		staged := slices.DeleteFunc(dirs, func(p string) bool { return !partitionName.MatchString(p) || p == genomicBackup })
		if !slices.Contains(staged, g.opts.Partition) {
			staged = append(staged, g.opts.Partition)
		}
		g.toPromote = staged
	}
	after := live
	for _, p := range g.toPromote {
		if !slices.Contains(after, p) {
			after = append(after, p)
		}
	}
	if len(after) > hpdsMaxPartitions {
		return exitcode.Precondition("promoting %s would leave %d genomic partitions (%s), and HPDS starts with at most %d",
			strings.Join(g.toPromote, ", "), len(after), strings.Join(after, ", "), hpdsMaxPartitions)
	}
	return nil
}

// livePartitions lists every directory in hpds-genomic, hidden ones too,
// since HPDS loads each as a partition. It creates a missing volume, as
// compose would.
func (g *genomicLoad) livePartitions(ctx context.Context) ([]string, error) {
	vol, err := g.st.EnsureVolume(ctx, g.d.Docker, g.cfg.Name, g.vol(hpdsGenomicVolume), hpdsGenomicVolume)
	if err != nil {
		return nil, err
	}
	return g.dirs(ctx, vol.Name, "/data")
}

// vcfIndexFiles returns the VCFs a vcfIndex.tsv names, and their sizes. The
// loaders skip its first line, a header, and read the file name from the
// first tab-separated column. Each must be an existing file under vcfDir.
func vcfIndexFiles(index string, data []byte, vcfDir string) ([]string, []int64, error) {
	var files []string
	var sizes []int64
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if i == 0 || strings.TrimSpace(line) == "" {
			continue
		}
		name, _, _ := strings.Cut(line, "\t")
		bad := func(format string, args ...any) error {
			return exitcode.Usage("%s line %d: %s", index, i+1, fmt.Sprintf(format, args...))
		}
		switch {
		case !filepath.IsAbs(name):
			return nil, nil, bad("%q isn't an absolute path; the loaders open each VCF by the path in the index", name)
		case !within(filepath.Clean(name), vcfDir):
			return nil, nil, bad("%s isn't under --vcf-dir %s, the only directory the loaders see", name, vcfDir)
		}
		fi, err := os.Stat(name)
		if err != nil {
			return nil, nil, bad("%v", err)
		}
		if !fi.Mode().IsRegular() {
			return nil, nil, bad("%s isn't a regular file", name)
		}
		files = append(files, filepath.Clean(name))
		sizes = append(sizes, fi.Size())
	}
	if len(files) == 0 {
		return nil, nil, exitcode.Usage("%s names no VCF files", index)
	}
	return files, sizes, nil
}

// checkVisible makes sure a container that mounts the VCF directory at its
// own path sees every VCF. If the daemon doesn't share it (a directory
// outside $HOME under Colima or Lima), or a VCF is a symlink to a file
// outside it, the VCFs are copied into a MkdirTemp dir, which is mounted at
// the same path instead.
func (g *genomicLoad) checkVisible(ctx context.Context, sink events.Sink) error {
	mount := docker.Mount{Source: g.opts.VCFDir, Target: g.opts.VCFDir, ReadOnly: true}
	out := g.linksOut()
	if out == "" {
		if ok, err := g.daemonSees(ctx, "genomic-input", []docker.Mount{mount}, g.vcfs, g.sizes); ok || err != nil {
			return err
		}
	}
	if g.opts.MkdirTemp == nil {
		return exitcode.Precondition("the Docker daemon can't read the VCFs in %s; move them under your home directory", g.opts.VCFDir)
	}
	why := "the Docker daemon can't read " + g.opts.VCFDir
	if out != "" {
		why = out + " links to a file outside " + g.opts.VCFDir + ", which the loaders can't see"
	}
	sink.Emit(events.Progress{ID: GenomicInputStepID, Text: why + "; copying the VCFs into the cache"})
	if err := g.makeCopyDir(ctx, "genomic-"); err != nil {
		return err
	}
	for _, f := range g.vcfs {
		rel, err := filepath.Rel(g.opts.VCFDir, f)
		if err != nil {
			return err
		}
		dst := filepath.Join(g.copyDir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := copyInput(ctx, f, dst); err != nil {
			return err
		}
	}
	mount.Source = g.copyDir
	if ok, err := g.daemonSees(ctx, "genomic-input", []docker.Mount{mount}, g.vcfs, g.sizes); err != nil || !ok {
		if err != nil {
			return err
		}
		return exitcode.Precondition("the Docker daemon can't read the VCFs in %s or their copy in the cache (%s); "+
			"move them under a directory the daemon shares", g.opts.VCFDir, g.copyDir)
	}
	g.vcfMount = g.copyDir
	return nil
}

// linksOut returns the first VCF that is a symlink to a file outside
// VCFDir, where a container that mounts only VCFDir can't follow it, or "".
func (g *genomicLoad) linksOut() string {
	dir, err := filepath.EvalSymlinks(g.opts.VCFDir)
	if err != nil {
		return ""
	}
	for _, f := range g.vcfs {
		if real, err := filepath.EvalSymlinks(f); err == nil && !within(real, dir) {
			return f
		}
	}
	return ""
}

// stage clears the loaders' working directories in the staging volume,
// left by an earlier load that failed, and writes the index there.
func (g *genomicLoad) stage(ctx context.Context, _ events.Sink) error {
	vol, err := g.st.EnsureVolume(ctx, g.d.Docker, g.cfg.Name, g.vol(genomicStagingVolume), genomicStagingVolume)
	if err != nil {
		return err
	}
	script := "rm -rf /data/all /data/merged; mkdir -p /data/all /data/merged; cat > /data/vcfIndex.tsv"
	if err := g.helper(ctx, vol.Name, "genomic-stage", script, bytes.NewReader(g.index)); err != nil {
		return fmt.Errorf("staging the VCF index in volume %s: %w", vol.Name, err)
	}
	return nil
}

// runLoader returns a step that runs the hpds-etl loader name with the
// staging volume at /opt/local/hpds and, if withVCFs, the VCF directory at
// its own path. The loaders write the partition's contigs under all/.
func (g *genomicLoad) runLoader(id, name string, withVCFs bool) func(context.Context, events.Sink) error {
	return func(ctx context.Context, sink events.Sink) error {
		cname, err := docker.UniqueName(g.cfg.Name+"-"+id, g.d.Rand)
		if err != nil {
			return err
		}
		mounts := []docker.Mount{{Source: g.vol(genomicStagingVolume), Target: hpdsDir}}
		if withVCFs {
			mounts = append(mounts, docker.Mount{Source: g.vcfMount, Target: g.opts.VCFDir, ReadOnly: true})
		}
		stdout := events.NewLogWriter(sink, id, events.StreamStdout)
		stderr := events.NewLogWriter(sink, id, events.StreamStderr)
		code, err := g.d.Docker.Run(ctx, docker.RunOpts{
			Image:   g.image,
			Name:    cname,
			User:    "0:0",
			Remove:  true,
			Network: "none",
			Labels:  g.st.Labels(g.cfg.Name),
			Mounts:  mounts,
			Env:     []string{"HEAPSIZE=" + strconv.Itoa(g.opts.HeapMB), "LOADER_NAME=" + name},
			Stdout:  stdout,
			Stderr:  stderr,
		})
		_ = stdout.Close()
		_ = stderr.Close()
		if err != nil {
			_ = g.d.Docker.Rm(context.WithoutCancel(ctx), cname, true)
			return fmt.Errorf("running %s: %w", name, err)
		}
		if code != 0 {
			return fmt.Errorf("%s exited %d; its output is above and in the run log", name, code)
		}
		return nil
	}
}

// finalize runs GenomicDatasetFinalizer, then moves the loaded contigs to
// genomic/<partition> in the staging volume, replacing any earlier load
// of the partition.
func (g *genomicLoad) finalize(ctx context.Context, sink events.Sink) error {
	if err := g.runLoader(GenomicFinalizeStepID, "GenomicDatasetFinalizer", false)(ctx, sink); err != nil {
		return err
	}
	script := `cd "$1"; rm -rf "./genomic/$2"; mkdir -p genomic; mv all "./genomic/$2"; rm -rf merged`
	if err := g.script(ctx, "genomic-move", []docker.Mount{{Source: g.vol(genomicStagingVolume), Target: "/data"}},
		script, []string{"/data", g.opts.Partition}, nil, nil); err != nil {
		return fmt.Errorf("moving partition %s into place in volume %s: %w", g.opts.Partition, g.vol(genomicStagingVolume), err)
	}
	return nil
}

// settleScript defines `settle TARGET NEW OLD`, which finishes or undoes an
// interrupted replacement of TARGET by a complete copy, NEW: TARGET is
// moved aside to OLD, NEW renamed to TARGET, and OLD removed. OLD exists
// only once NEW is complete, so if TARGET is missing, NEW (or failing that
// OLD) is renamed to it. Then OLD and any partial NEW are removed. It
// prints "completed T" if TARGET is NEW, and "restored T" if it renamed OLD
// back. Paths start with ./ since a partition name may start with -.
const settleScript = `settle() { if [ -e "$3" ]; then if [ -e "$1" ]; then printf 'completed %s\n' "${1#./}"; ` +
	`elif [ -e "$2" ]; then mv "$2" "$1"; printf 'completed %s\n' "${1#./}"; else mv "$3" "$1"; printf 'restored %s\n' "${1#./}"; fi; ` +
	`rm -rf "$3"; fi; rm -rf "$2"; }; `

// recoverScript settles every partition of the hpds-genomic volume at $1
// that an interrupted promote left a .promote- or .old- directory for.
const recoverScript = `cd "$1"; for d in ./` + promotePrefix + `* ./` + oldPrefix + `*; do if [ -d "$d" ]; then ` +
	`p=${d#./` + promotePrefix + `}; p=${p#./` + oldPrefix + `}; settle "./$p" "./` + promotePrefix + `$p" "./` + oldPrefix + `$p"; fi; done`

// promote copies toPromote from the staging volume into hpds-genomic. Each
// partition is copied to .promote-<p>, then the live one is renamed to
// .old-<p>, the copy to <p>, and .old-<p> removed, so an interruption
// leaves at most one partition part way, which settle puts right. It
// prints "promoted <p>" before removing .old-<p>, so that a partition is
// reported as promoted by it or, after an interruption, by settle. After any failure, even an interrupted one, a second helper run
// without the cancelled context settles every partition, since HPDS would
// load a leftover as a partition. With Backup it first copies the live
// store into the staging volume's all-bak the same way.
func (g *genomicLoad) promote(ctx context.Context, sink events.Sink) (err error) {
	staging, live := g.vol(genomicStagingVolume), g.vol(hpdsGenomicVolume)
	var out bytes.Buffer
	defer func() {
		if err != nil {
			err = g.afterFailedPromote(context.WithoutCancel(ctx), err, out.String())
		}
		g.promoteErr = err
	}()
	if len(g.leftovers) > 0 {
		if err := g.recoverLive(ctx, sink); err != nil {
			return err
		}
	}
	if g.opts.Backup {
		if err := g.backup(ctx, sink); err != nil {
			return err
		}
	}
	sink.Emit(events.Progress{ID: GenomicPromoteStepID, Text: "promoting " + strings.Join(g.toPromote, ", ")})
	mounts := []docker.Mount{{Source: live, Target: "/live"}, {Source: staging, Target: "/staged", ReadOnly: true}}
	script := settleScript + `live=$1; staged=$2; shift 2; cd "$live"; for p; do ` +
		`settle "./$p" "./` + promotePrefix + `$p" "./` + oldPrefix + `$p"; cp -a "$staged/genomic/$p" "./` + promotePrefix + `$p"; ` +
		`if [ -e "./$p" ]; then mv "./$p" "./` + oldPrefix + `$p"; fi; mv "./` + promotePrefix + `$p" "./$p"; ` +
		`printf 'promoted %s\n' "$p"; rm -rf "./` + oldPrefix + `$p"; done`
	args := append([]string{"/live", "/staged"}, g.toPromote...)
	if err := g.script(ctx, "genomic-promote", mounts, script, args, nil, &out); err != nil {
		return fmt.Errorf("copying %s into volume %s: %w", strings.Join(g.toPromote, ", "), live, err)
	}
	g.promoted = g.toPromote
	return nil
}

// afterFailedPromote settles hpds-genomic after promote failed with err,
// and says what state that left each partition in. out is the promote
// script's output so far.
func (g *genomicLoad) afterFailedPromote(ctx context.Context, err error, out string) error {
	live := g.vol(hpdsGenomicVolume)
	settled, rerr := g.settleLive(ctx)
	if rerr != nil {
		g.unrecovered = true
		return errors.Join(err, fmt.Errorf("recovering volume %s: %w. HPDS would load its leftover %s* and %s* directories "+
			"as partitions; the next load with --promote recovers them", live, rerr, promotePrefix, oldPrefix))
	}
	// A partition is promoted if the script or settle said so, or if it is
	// there now and wasn't before.
	var said []string
	for line := range strings.Lines(out + settled) {
		line = strings.TrimSuffix(line, "\n")
		if p, ok := strings.CutPrefix(line, "promoted "); ok {
			said = append(said, p)
		} else if p, ok := strings.CutPrefix(line, "completed "); ok {
			said = append(said, p)
		}
	}
	now, lerr := g.dirs(ctx, live, "/data")
	if lerr != nil {
		return errors.Join(err, lerr)
	}
	var done, notDone []string
	for _, p := range g.toPromote {
		if slices.Contains(said, p) || slices.Contains(now, p) && !slices.Contains(g.wasLive, p) {
			done = append(done, p)
		} else {
			notDone = append(notDone, p)
		}
	}
	state := "every partition in " + live + " is whole"
	if len(done) > 0 {
		state += "; promoted: " + strings.Join(done, ", ")
	}
	if len(notDone) > 0 {
		state += "; not promoted: " + strings.Join(notDone, ", ")
	}
	return fmt.Errorf("%w (%s)", err, state)
}

// recoverLive settles the leftovers an interrupted promote left in
// hpds-genomic, before this load's backup and promote.
func (g *genomicLoad) recoverLive(ctx context.Context, sink events.Sink) error {
	live := g.vol(hpdsGenomicVolume)
	sink.Emit(events.Progress{ID: GenomicPromoteStepID, Text: "recovering what an interrupted promote left in " + live +
		" (" + strings.Join(g.leftovers, ", ") + ")"})
	out, err := g.settleLive(ctx)
	if err != nil {
		return fmt.Errorf("recovering volume %s: %w", live, err)
	}
	if out = strings.TrimSpace(out); out != "" {
		sink.Emit(events.Progress{ID: GenomicPromoteStepID, Text: strings.ReplaceAll(out, "\n", "; ")})
	}
	return nil
}

// settleLive runs recoverScript on hpds-genomic and returns its output.
func (g *genomicLoad) settleLive(ctx context.Context) (string, error) {
	var out bytes.Buffer
	mounts := []docker.Mount{{Source: g.vol(hpdsGenomicVolume), Target: "/live"}}
	err := g.script(ctx, "genomic-recover", mounts, settleScript+recoverScript, []string{"/live"}, nil, &out)
	return out.String(), err
}

// backup copies the live store into all-bak in the staging volume, via
// all-bak.new and all-bak.old as promote does a partition, so all-bak is
// always a whole backup. A failed copy is settled even if interrupted.
func (g *genomicLoad) backup(ctx context.Context, sink events.Sink) error {
	staging, live := g.vol(genomicStagingVolume), g.vol(hpdsGenomicVolume)
	sink.Emit(events.Progress{ID: GenomicPromoteStepID, Text: "backing up " + live + " into " + genomicBackup + " in " + staging})
	mounts := []docker.Mount{{Source: live, Target: "/live", ReadOnly: true}, {Source: staging, Target: "/staged"}}
	settle := `cd "$1"; settle ` + genomicBackup + ` ` + genomicBackup + `.new ` + genomicBackup + `.old`
	script := settleScript + settle + `; cp -a "$2/." ` + genomicBackup + `.new/; ` +
		`if [ -e ` + genomicBackup + ` ]; then mv ` + genomicBackup + ` ` + genomicBackup + `.old; fi; ` +
		`mv ` + genomicBackup + `.new ` + genomicBackup + `; rm -rf ` + genomicBackup + `.old`
	err := g.script(ctx, "genomic-backup", mounts, script, []string{"/staged", "/live"}, nil, nil)
	if err == nil {
		return nil
	}
	err = fmt.Errorf("backing up volume %s into %s in volume %s: %w", live, genomicBackup, staging, err)
	if serr := g.script(context.WithoutCancel(ctx), "genomic-backup", mounts[1:], settleScript+settle, []string{"/staged"}, nil, nil); serr != nil {
		return errors.Join(err, fmt.Errorf("removing the partial backup %s.new: %w", genomicBackup, serr))
	}
	return fmt.Errorf("%w (%s holds a whole backup, if it held one before)", err, genomicBackup)
}

// dirs lists the directories under dir in volume, hidden ones too.
func (g *genomicLoad) dirs(ctx context.Context, volume, dir string) ([]string, error) {
	var out bytes.Buffer
	script := `cd "$1" 2>/dev/null || exit 0; for e in * .[!.]* ..?*; do if [ -d "$e" ]; then printf '%s\n' "$e"; fi; done`
	mounts := []docker.Mount{{Source: volume, Target: "/data", ReadOnly: true}}
	if err := g.script(ctx, "genomic-list", mounts, script, []string{dir}, nil, &out); err != nil {
		return nil, fmt.Errorf("listing the partitions in volume %s: %w", volume, err)
	}
	return strings.FieldsFunc(out.String(), func(r rune) bool { return r == '\n' }), nil
}

// setProfile sets hpds.profile in pic-sure.yaml, and in cfg for the render.
func (g *genomicLoad) setProfile(context.Context, events.Sink) error {
	if g.cfg.HPDS.Profile == GenomicProfile {
		return nil
	}
	doc, err := g.st.ReadConfigDoc()
	if err != nil {
		return err
	}
	if err := doc.SetValue("hpds.profile", GenomicProfile); err != nil {
		return err
	}
	data, err := doc.Bytes()
	if err != nil {
		return err
	}
	if err := g.st.WriteConfig(data); err != nil {
		return err
	}
	g.cfg.HPDS.Profile = GenomicProfile
	return nil
}

// start starts hpds and waits until it is healthy. HPDS was stopped, so it
// has read any file the render changed: it comes off PendingRestarts. Any
// other service the render marked is left for `pic-sure up`.
func (g *genomicLoad) start(ctx context.Context, sink events.Sink) error {
	if err := g.loader.start(ctx, sink); err != nil {
		return err
	}
	state, err := g.st.LoadState()
	if err != nil {
		return err
	}
	if len(state.PendingRestarts) == 0 {
		return nil
	}
	state.PendingRestarts = slices.DeleteFunc(state.PendingRestarts, func(s string) bool { return s == hpdsService })
	if len(state.PendingRestarts) > 0 {
		sink.Emit(events.Warning{ID: LoaderStartStepID, Text: strings.Join(state.PendingRestarts, ", ") +
			" must restart to read changed files; run `pic-sure up` to restart them"})
	}
	return g.st.SaveState(state)
}
