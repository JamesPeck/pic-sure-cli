package ops

import (
	"bytes"
	"cmp"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

// shared-data publish's step IDs, besides LoaderStopStepID and
// LoaderStartStepID. Its steps depend on each other, so none can be skipped.
const (
	SharedCheckStepID = "shared-check"
	SharedCopyStepID  = "shared-copy"
)

// The labels on a published data set's volumes. They are AIO's, so `list`
// also shows sets AIO's publish-shared-hpds-data.sh made, except
// .cli-version and .source-stack, which replace AIO's .aio-commit and
// .source-project. A data set never
// carries the publishing stack's labels: destroying that stack would remove
// it.
const (
	// SharedDataLabel names the data set a volume belongs to.
	SharedDataLabel = "org.hms-dbmi.picsure.shared-hpds-data"
	// SharedDataKindLabel is hpds-data or hpds-genomic.
	SharedDataKindLabel = SharedDataLabel + ".kind"
	// SharedDataContentsLabel is "phenotype=<.picsure-dataset or unknown>
	// genomic=<partitions, comma-separated, or none>".
	SharedDataContentsLabel = SharedDataLabel + ".contents"
	// SharedDataProfileLabel is the HPDS profile a stack mounting the set
	// runs with when its hpds.profile is empty: GenomicProfile when the set
	// holds genomic data.
	SharedDataProfileLabel     = SharedDataLabel + ".hpds-profile"
	SharedDataCommitLabel      = SharedDataLabel + ".picsure-commit"
	SharedDataCLIVersionLabel  = SharedDataLabel + ".cli-version"
	SharedDataSourceStackLabel = SharedDataLabel + ".source-stack"
	SharedDataCreatedLabel     = SharedDataLabel + ".created"
	// sharedDataRunLabel tells this run's volumes from ones another publish
	// created under the same name at the same moment.
	sharedDataRunLabel = SharedDataLabel + ".publish-id"
	// aioSourceProjectLabel is .source-stack on a set AIO published.
	aioSourceProjectLabel = SharedDataLabel + ".source-project"
)

// PublishedMarker is the file publish writes at the top of both volumes:
// "name=<set> created=<RFC 3339>".
const PublishedMarker = ".picsure-published"

var sharedDataName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// sharedPhenotypeFiles must be in hpds-data, non-empty, for a set to be
// published: HPDS reads the first three, the dictionary the last.
var sharedPhenotypeFiles = []string{"encryption_key", "allObservationsStore.javabin", "columnMeta.javabin", "columnMeta.csv"}

// sharedDataFiles are what publish copies from hpds-data: the loader's
// output and its provenance, never its inputs or temp files.
var sharedDataFiles = append(slices.Clone(sharedPhenotypeFiles), "columnMetaErrors.csv", datasetMarker)

// sharedGenomicIndexes are what HPDS writes into each <partition>/<contig>/
// on its first start with GenomicProfile. A read-only mount can't, so they
// must be there before publishing.
var sharedGenomicIndexes = []string{"variantIndex_fbbis.javabin", "BucketIndexBySample.javabin"}

// SharedDataSet is a published data set, as its volumes' labels describe it.
type SharedDataSet struct {
	Name          string `json:"name"`
	Contents      string `json:"contents"`
	HPDSProfile   string `json:"hpds_profile"`
	PicsureCommit string `json:"picsure_commit"`
	CLIVersion    string `json:"cli_version"`
	SourceStack   string `json:"source_stack"`
	Created       string `json:"created"`
	// Volumes are the set's volumes that exist, sorted: both, unless one
	// was removed by hand.
	Volumes []string `json:"volumes"`
}

// CheckSharedDataName returns a usage error unless name can name a data
// set. It is a compose project name's rule, so <name>_hpds-data is a valid
// volume name.
func CheckSharedDataName(name string) error {
	if !sharedDataName.MatchString(name) {
		return exitcode.Usage("a data set name must match ^[a-z0-9][a-z0-9_-]*$, not %q", name)
	}
	return nil
}

func sharedVolumes(name string) (data, genomic string) {
	d, _ := catalog.LookupVolume("shared-hpds-data")
	g, _ := catalog.LookupVolume("shared-hpds-genomic")
	return d.DockerName(name), g.DockerName(name)
}

// RefusePublishFromShared returns the error publish gives on a stack that
// mounts a shared data set: it has no data of its own to publish.
func RefusePublishFromShared(cfg *stack.Config) error {
	if cfg.HPDS.Data != stack.HPDSShared {
		return nil
	}
	return fmt.Errorf("this stack's HPDS uses the shared data set %q (hpds.data: shared); "+
		"publish from a stack that loaded its own data", cfg.HPDS.SharedName)
}

// PublishOptions configures PublishSharedData.
type PublishOptions struct {
	Name string
	// CLIVersion is recorded on the volumes.
	CLIVersion string
}

// PublishSharedData is §9.7's publish. The caller holds the stack lock and
// sets d.Compose. It checks the stack's hpds-data holds a complete
// phenotype load and every genomic contig its indexes, stops hpds if it is
// running, creates <name>_hpds-data and <name>_hpds-genomic, copies the
// loader output and the genomic partitions into them, writes
// PublishedMarker, and starts hpds again. A name whose volumes exist is
// refused: data sets are immutable. On failure it removes the volumes this
// run created.
func PublishSharedData(ctx context.Context, d *Deps, st *stack.Stack, cfg *stack.Config, state *stack.State, opts PublishOptions) (SharedDataSet, error) {
	if err := CheckSharedDataName(opts.Name); err != nil {
		return SharedDataSet{}, err
	}
	if err := RefusePublishFromShared(cfg); err != nil {
		return SharedDataSet{}, err
	}
	p := &publish{loader: &loader{d: d, st: st, cfg: cfg, state: state}, opts: opts}
	p.dstData, p.dstGenomic = sharedVolumes(opts.Name)
	notRunning := func(context.Context) (bool, error) { return !p.wasRunning, nil }
	plan := []steps.Step{
		{ID: SharedCheckStepID, Title: "Check the data to publish", Apply: p.check},
		// Stopped even when it isn't running: one in a restart loop
		// could start in the middle of the copy.
		{ID: LoaderStopStepID, Title: "Stop HPDS", Apply: p.stop},
		{ID: SharedCopyStepID, Title: "Copy the data into data set " + opts.Name, Apply: p.copy},
		{ID: LoaderStartStepID, Title: "Start HPDS", Check: notRunning, Apply: p.start},
	}
	err := steps.Run(ctx, d.Sink, plan, steps.Options{})
	if err == nil {
		return p.set, nil
	}
	var se *steps.Error
	if !errors.As(err, &se) {
		return SharedDataSet{}, err
	}
	// steps.Error's own advice, that a re-run resumes or skips the steps
	// already done, doesn't hold here: a re-run publishes again from the
	// start, and is refused once the set exists.
	failed := fmt.Errorf("step %s failed: %w", se.Step, se.Err)
	if se.Interrupted {
		failed = fmt.Errorf("stopped at step %s: %w", se.Step, se.Err)
	}
	switch {
	case se.Step == LoaderStartStepID:
		return p.set, fmt.Errorf("%w. Data set %s is published; see `pic-sure logs hpds`, then start HPDS with `pic-sure up`", failed, opts.Name)
	case se.Step == SharedCheckStepID || !p.wasRunning:
		return SharedDataSet{}, failed
	case se.Interrupted || se.Step == LoaderStopStepID:
		return SharedDataSet{}, fmt.Errorf("%w. HPDS may be stopped; start it with `pic-sure up`", failed)
	}
	// The copy failed: put HPDS back as it was.
	if serr := p.start(ctx, d.Sink); serr != nil {
		return SharedDataSet{}, errors.Join(failed, fmt.Errorf("HPDS is stopped: %w; see `pic-sure logs hpds`, then start it with `pic-sure up`", serr))
	}
	return SharedDataSet{}, failed
}

type publish struct {
	*loader
	opts                PublishOptions
	srcData, srcGenomic string
	dstData, dstGenomic string
	partitions          []string
	wasRunning          bool
	labels              map[string]string
	set                 SharedDataSet
}

// check reads the source volumes and works out the labels. Nothing has
// changed yet if it fails.
func (p *publish) check(ctx context.Context, sink events.Sink) error {
	for _, vol := range []string{p.dstData, p.dstGenomic} {
		_, err := p.d.Docker.VolumeInspect(ctx, vol)
		if err == nil {
			return exitcode.Precondition("volume %s already exists. Data sets are immutable: publish under a new name", vol)
		}
		if !errors.Is(err, docker.ErrNotFound) {
			return err
		}
	}
	p.srcData = p.volume()
	g, _ := catalog.LookupVolume(hpdsGenomicVolume)
	p.srcGenomic = g.DockerName(p.cfg.Name)
	for _, vol := range []string{p.srcData, p.srcGenomic} {
		if _, err := p.d.Docker.VolumeInspect(ctx, vol); errors.Is(err, docker.ErrNotFound) {
			return exitcode.Precondition("volume %s doesn't exist; load data into this stack first", vol)
		} else if err != nil {
			return err
		}
	}

	probe, err := p.probe(ctx)
	if err != nil {
		return err
	}
	if len(probe.missing) > 0 {
		return exitcode.Precondition("volume %s is missing %s; load phenotype data into this stack first "+
			"(`pic-sure data load-phenotype` or `pic-sure data demo`)", p.srcData, strings.Join(probe.missing, ", "))
	}
	for _, part := range probe.partitions {
		if !slices.ContainsFunc(probe.contigs, func(c string) bool { return strings.HasPrefix(c, part+"/") }) {
			return exitcode.Precondition("genomic partition %s in volume %s holds no <partition>/<contig>/ directory; "+
				"load it again with `pic-sure data load-genomic --promote`", part, p.srcGenomic)
		}
	}
	if len(probe.unindexed) > 0 {
		return exitcode.Precondition("the genomic data in volume %s lacks the indexes HPDS writes on its first start: %s. "+
			"Start HPDS on it once with its genomic profile (`pic-sure config set hpds.profile %s`, then `pic-sure up`), then publish", p.srcGenomic, strings.Join(probe.unindexed, ", "), GenomicProfile)
	}
	if len(probe.partitions) > hpdsMaxPartitions {
		return exitcode.Precondition("volume %s holds %d genomic partitions (%s), and HPDS starts with at most %d",
			p.srcGenomic, len(probe.partitions), strings.Join(probe.partitions, ", "), hpdsMaxPartitions)
	}
	p.partitions = probe.partitions

	svc, err := composeService(ctx, p.d, hpdsService)
	if err != nil {
		return err
	}
	p.wasRunning = svc != nil && (svc.State == "running" || svc.State == "restarting")

	id := make([]byte, 8)
	if _, err := io.ReadFull(p.d.Rand, id); err != nil {
		return err
	}
	phenotype, genomic, profile := probe.dataset, "none", ""
	if phenotype == "" {
		phenotype = "unknown"
	}
	if len(p.partitions) > 0 {
		genomic, profile = strings.Join(p.partitions, ","), GenomicProfile
	}
	p.set = SharedDataSet{
		Name:          p.opts.Name,
		Contents:      "phenotype=" + phenotype + " genomic=" + genomic,
		HPDSProfile:   profile,
		PicsureCommit: p.picsureCommit(ctx),
		CLIVersion:    p.opts.CLIVersion,
		SourceStack:   p.cfg.Name,
		Created:       p.d.Clock.Now().UTC().Format(time.RFC3339),
		Volumes:       []string{p.dstData, p.dstGenomic},
	}
	p.labels = map[string]string{
		SharedDataLabel:            p.set.Name,
		SharedDataContentsLabel:    p.set.Contents,
		SharedDataProfileLabel:     p.set.HPDSProfile,
		SharedDataCommitLabel:      p.set.PicsureCommit,
		SharedDataCLIVersionLabel:  p.set.CLIVersion,
		SharedDataSourceStackLabel: p.set.SourceStack,
		SharedDataCreatedLabel:     p.set.Created,
		sharedDataRunLabel:         hex.EncodeToString(id),
	}
	sink.Emit(events.Progress{ID: SharedCheckStepID, Text: p.set.Contents})
	return nil
}

type publishProbe struct {
	missing    []string // phenotype files that are missing or empty
	dataset    string   // the .picsure-dataset marker
	partitions []string // the genomic partitions to publish
	contigs    []string // <partition>/<contig>
	unindexed  []string // <partition>/<contig>/<index> files that are missing
}

// probe reads both source volumes in one helper container.
func (p *publish) probe(ctx context.Context) (publishProbe, error) {
	// Every top-level directory of the genomic volume is a partition to
	// HPDS, hidden ones too, except a backup AIO left there and an
	// unfinished promote's copy.
	script := `cd "$1"; for f in ` + strings.Join(sharedPhenotypeFiles, " ") + `; do [ -s "$f" ] || printf 'missing %s\n' "$f"; done; ` +
		`if [ -f ` + datasetMarker + ` ]; then printf 'dataset %s\n' "$(head -n 1 ` + datasetMarker + `)"; fi; ` +
		`cd "$2"; for p in * .[!.]* ..?*; do [ -d "$p" ] || continue; case "$p" in ` + genomicBackup + `|` + promotePrefix + `*) continue;; esac; ` +
		`printf 'partition %s\n' "$p"; for c in "./$p"/*/; do [ -d "$c" ] || continue; c=${c#./}; printf 'contig %s\n' "${c%/}"; ` +
		`for f in ` + strings.Join(sharedGenomicIndexes, " ") + `; do [ -s "$c$f" ] || printf 'unindexed %s\n' "$c$f"; done; done; done`
	mounts := []docker.Mount{{Source: p.srcData, Target: "/d", ReadOnly: true}, {Source: p.srcGenomic, Target: "/g", ReadOnly: true}}
	var out bytes.Buffer
	if err := p.script(ctx, "shared-probe", mounts, script, []string{"/d", "/g"}, nil, &out); err != nil {
		return publishProbe{}, fmt.Errorf("reading volumes %s and %s: %w", p.srcData, p.srcGenomic, err)
	}
	var r publishProbe
	for line := range strings.Lines(out.String()) {
		kind, value, _ := strings.Cut(strings.TrimSuffix(line, "\n"), " ")
		switch kind {
		case "missing":
			r.missing = append(r.missing, value)
		case "dataset":
			r.dataset = value
		case "partition":
			r.partitions = append(r.partitions, value)
		case "contig":
			r.contigs = append(r.contigs, value)
		case "unindexed":
			r.unindexed = append(r.unindexed, value)
		}
	}
	return r, nil
}

// picsureCommit is the pic-sure commit the stack's HPDS loader was built
// from, else the one state.json records, else "unknown".
func (p *publish) picsureCommit(ctx context.Context) string {
	img, _ := catalog.LookupImage(hpdsETLImage)
	if tag := p.state.Images[img.Name]; tag != "" {
		if labels, err := p.d.Docker.ImageLabels(ctx, img.Repository()+":"+tag); err == nil && labels[ReactorSrcLabel] != "" {
			return labels[ReactorSrcLabel]
		}
	}
	if c := p.state.Components["pic-sure"].Commit; c != "" {
		return c
	}
	return "unknown"
}

// copy creates the set's volumes and copies the data into them. If it
// fails, even when interrupted, it removes the volumes it created.
func (p *publish) copy(ctx context.Context, sink events.Sink) (err error) {
	var tried []string
	defer func() {
		if err != nil {
			err = p.removeOwn(context.WithoutCancel(ctx), tried, err)
		}
	}()
	for _, v := range []struct{ name, kind string }{{p.dstData, hpdsDataVolume}, {p.dstGenomic, hpdsGenomicVolume}} {
		labels := maps.Clone(p.labels)
		labels[SharedDataKindLabel] = v.kind
		tried = append(tried, v.name)
		if err := p.d.Docker.VolumeCreate(ctx, v.name, labels); err != nil {
			return fmt.Errorf("creating volume %s: %w", v.name, err)
		}
		got, err := p.d.Docker.VolumeInspect(ctx, v.name)
		if err != nil {
			return err
		}
		if got.Labels[sharedDataRunLabel] != p.labels[sharedDataRunLabel] {
			return exitcode.Precondition("volume %s appeared while publishing; is another publish of %s running?", v.name, p.opts.Name)
		}
	}

	sink.Emit(events.Progress{ID: SharedCopyStepID, Text: "copying " + p.srcData + " and " + p.srcGenomic})
	// HPDS mounts the genomic volume at all/ inside the read-only data
	// volume, so the mount point must exist in the copy. Paths start with
	// ./ since a partition name may start with -.
	script := `name=$1; created=$2; shift 2; ` +
		`cd /sd; for f in ` + strings.Join(sharedDataFiles, " ") + `; do if [ -e "$f" ]; then cp -a "$f" /dd/; fi; done; mkdir -p /dd/all; ` +
		`cd /sg; for p; do cp -a "./$p" /dg/; done; ` +
		`for v in /dd /dg; do printf 'name=%s created=%s\n' "$name" "$created" > "$v/` + PublishedMarker + `"; done`
	mounts := []docker.Mount{
		{Source: p.srcData, Target: "/sd", ReadOnly: true},
		{Source: p.srcGenomic, Target: "/sg", ReadOnly: true},
		{Source: p.dstData, Target: "/dd"},
		{Source: p.dstGenomic, Target: "/dg"},
	}
	args := append([]string{p.set.Name, p.set.Created}, p.partitions...)
	if err := p.script(ctx, "shared-copy", mounts, script, args, nil, nil); err != nil {
		return fmt.Errorf("copying into data set %s: %w", p.opts.Name, err)
	}
	return nil
}

// removeOwn removes those of vols that carry this run's publish-id, adding
// any failure to err. Creating a volume that exists succeeds, so only the
// label tells which ones this run made.
func (p *publish) removeOwn(ctx context.Context, vols []string, err error) error {
	for _, vol := range vols {
		v, rerr := p.d.Docker.VolumeInspect(ctx, vol)
		if errors.Is(rerr, docker.ErrNotFound) || rerr == nil && v.Labels[sharedDataRunLabel] != p.labels[sharedDataRunLabel] {
			continue
		}
		if rerr == nil {
			rerr = p.d.Docker.VolumeRemove(ctx, vol)
		}
		if rerr != nil {
			err = errors.Join(err, fmt.Errorf("removing volume %s, which this run may have created: %w; "+
				"if it has no %s, remove it with `docker volume rm %s`", vol, rerr, PublishedMarker, vol))
		}
	}
	return err
}

// ListSharedData returns every published data set on the daemon, by name.
func ListSharedData(ctx context.Context, d *Deps) ([]SharedDataSet, error) {
	vols, err := d.Docker.VolumeList(ctx, SharedDataLabel)
	if err != nil {
		return nil, err
	}
	var sets []SharedDataSet
	for _, v := range vols {
		name := v.Labels[SharedDataLabel]
		i := slices.IndexFunc(sets, func(s SharedDataSet) bool { return s.Name == name })
		if i < 0 {
			sets = append(sets, SharedDataSet{
				Name:          name,
				Contents:      v.Labels[SharedDataContentsLabel],
				HPDSProfile:   v.Labels[SharedDataProfileLabel],
				PicsureCommit: v.Labels[SharedDataCommitLabel],
				CLIVersion:    v.Labels[SharedDataCLIVersionLabel],
				SourceStack:   cmp.Or(v.Labels[SharedDataSourceStackLabel], v.Labels[aioSourceProjectLabel]),
				Created:       v.Labels[SharedDataCreatedLabel],
			})
			i = len(sets) - 1
		}
		sets[i].Volumes = append(sets[i].Volumes, v.Name)
	}
	slices.SortFunc(sets, func(a, b SharedDataSet) int { return strings.Compare(a.Name, b.Name) })
	return sets, nil
}

// RemoveSharedData removes data set name's volumes and returns them. It
// refuses while any container, even a stopped one, uses either, and never
// removes a volume that isn't labelled as the set's.
func RemoveSharedData(ctx context.Context, d *Deps, name string) ([]string, error) {
	if err := CheckSharedDataName(name); err != nil {
		return nil, err
	}
	data, genomic := sharedVolumes(name)
	var vols, users []string
	for _, vol := range []string{data, genomic} {
		v, err := d.Docker.VolumeInspect(ctx, vol)
		if errors.Is(err, docker.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if v.Labels[SharedDataLabel] != name {
			return nil, exitcode.Precondition("volume %s isn't part of a published data set (no %s=%s label); not removing it",
				vol, SharedDataLabel, name)
		}
		vols = append(vols, vol)
		cs, err := d.Docker.ContainersUsingVolume(ctx, vol)
		if err != nil {
			return nil, err
		}
		for _, c := range cs {
			if u := c.Name + " (" + c.State + ")"; !slices.Contains(users, u) {
				users = append(users, u)
			}
		}
	}
	if len(vols) == 0 {
		return nil, exitcode.Precondition("there is no data set %s; `pic-sure shared-data list` lists them", name)
	}
	if len(users) > 0 {
		return nil, exitcode.Precondition("data set %s is in use by %s; switch those stacks off it "+
			"(hpds.data: local) or remove the containers first", name, strings.Join(users, ", "))
	}
	for i, vol := range vols {
		if err := d.Docker.VolumeRemove(ctx, vol); err != nil {
			return vols[:i], fmt.Errorf("removing volume %s: %w", vol, err)
		}
	}
	return vols, nil
}

// SharedDataProfile checks that data set name is published, both its
// volumes present, labelled as the set's and holding the same
// PublishedMarker, and returns the HPDS profile recorded on it: what a
// stack mounting the set runs with when its hpds.profile is empty. Publish
// labels the volumes before it copies, so only the marker, written last,
// tells a finished set from one being published or left by an interrupted
// publish; the genomic seed fails on a set without it.
func SharedDataProfile(ctx context.Context, d *Deps, name string) (string, error) {
	data, genomic := sharedVolumes(name)
	var profile string
	for _, vol := range []string{data, genomic} {
		v, err := d.Docker.VolumeInspect(ctx, vol)
		if errors.Is(err, docker.ErrNotFound) {
			return "", exitcode.Precondition("shared data set %s isn't on this Docker host (no volume %s); "+
				"`pic-sure shared-data list` lists the published sets", name, vol)
		}
		if err != nil {
			return "", err
		}
		if v.Labels[SharedDataLabel] != name {
			return "", exitcode.Precondition("volume %s isn't part of a published data set (no %s=%s label); "+
				"publish the set with `pic-sure shared-data publish`", vol, SharedDataLabel, name)
		}
		if vol == data {
			profile = v.Labels[SharedDataProfileLabel]
		}
	}
	helper, err := docker.UniqueName("pic-sure-shared-check", d.Rand)
	if err != nil {
		return "", err
	}
	alpine, _ := catalog.LookupImage("alpine")
	var stderr bytes.Buffer
	code, err := d.Docker.Run(ctx, docker.RunOpts{
		Image:   alpine.Ref,
		Name:    helper,
		Remove:  true,
		Network: "none",
		Mounts:  []docker.Mount{{Source: data, Target: "/d", ReadOnly: true}, {Source: genomic, Target: "/g", ReadOnly: true}},
		Args: []string{"sh", "-c", `[ -s "$1/` + PublishedMarker + `" ] && cmp -s "$1/` + PublishedMarker + `" "$2/` + PublishedMarker + `"` +
			` || exit ` + strconv.Itoa(unfinishedSetExit), "sh", "/d", "/g"},
		Stderr: &stderr,
	})
	if err != nil {
		_ = d.Docker.Rm(context.WithoutCancel(ctx), helper, true)
	}
	switch {
	case err != nil:
		return "", fmt.Errorf("checking shared data set %s: %w", name, err)
	case code == unfinishedSetExit:
		return "", exitcode.Precondition("shared data set %s is incomplete: its volumes lack a matching %s, so it is still being "+
			"published or its publish was interrupted; wait for the publish, or remove the set with `pic-sure shared-data remove %s` "+
			"and publish it again", name, PublishedMarker, name)
	case code != 0:
		msg := strings.TrimSpace(stderr.String())
		return "", fmt.Errorf("checking shared data set %s: the helper container exited %d: %s", name, code, msg[strings.LastIndexByte(msg, '\n')+1:])
	}
	return profile, nil
}

// unfinishedSetExit is SharedDataProfile's helper's exit code for a set
// without a matching PublishedMarker.
const unfinishedSetExit = 42
