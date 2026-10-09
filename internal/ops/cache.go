package ops

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

// StepPrune is cache prune's one step.
const StepPrune = "prune"

// CacheImage is an image item's kind; the other items' kinds are
// cache.EntryKind values.
const CacheImage = "image"

// Cache item statuses (§7.1).
const (
	// CacheInUse: a container references it, or a readable state.json of a
	// stack names it. Never pruned.
	CacheInUse = "in_use"
	// CacheUnknownStack: nothing known uses it, but a labelled or
	// registered stack whose state.json can't be read might, or an
	// unparseable registry entry hides which stack. Pruned only with
	// --force.
	CacheUnknownStack = "unknown_stack"
	// CacheRecent: unused, but created or changed within RecentCacheAge,
	// so a build or load still running may be about to record or mount
	// it. Never pruned, even with --force.
	CacheRecent = "recent"
	// CacheUnused: pruned.
	CacheUnused = "unused"
)

// RecentCacheAge is how long after it was made or last changed a cache item
// counts as possibly in use by a command still running, such as a build
// that hasn't recorded its images in state.json yet.
const RecentCacheAge = time.Hour

// commitTag is an image tag the build gives a commit (§7.2): <sha12>, or
// <sha12>-<cfghash8> for the frontend.
var commitTag = regexp.MustCompile(`^[0-9a-f]{12}(-[0-9a-f]{8})?$`)

// devTag is a local source's tag (§7.3), dev-<stack>-<sha12>[-dirty].
var devTag = regexp.MustCompile(`^dev-(.+)-[0-9a-f]{12}(-dirty)?$`)

// CacheStack is a stack whose use of the cache prune respects.
type CacheStack struct {
	Name string `json:"name,omitempty"`
	Dir  string `json:"dir"`
	// Readable says its state.json was read; Error says why not.
	Readable bool   `json:"readable"`
	Error    string `json:"error,omitempty"`
	// Registered says the cache's stack registry has it.
	Registered bool `json:"registered,omitempty"`
	// Gone marks a registered stack whose directory is no longer a stack
	// and that has no labelled Docker resource: it uses nothing, and prune
	// drops its registry entry.
	Gone bool `json:"gone,omitempty"`

	state    *stack.State
	labelled bool
	key      string // its registry entry
	broken   bool   // its registry entry can't be read
}

// CacheItem is one image or cache entry.
type CacheItem struct {
	// Kind is CacheImage or a cache.EntryKind.
	Kind string `json:"kind"`
	// Name is the image reference, or the path under the cache root.
	Name    string `json:"name"`
	ImageID string `json:"image_id,omitempty"`
	// Size is in bytes. An image's size is shared by all its tags.
	Size   int64  `json:"size"`
	Status string `json:"status"`
	// UsedBy lists "stack NAME" and "container NAME" for an item in use.
	UsedBy []string `json:"used_by,omitempty"`
	// MayBeUsedBy lists the unreadable stacks that might use it.
	MayBeUsedBy []string `json:"may_be_used_by,omitempty"`

	modTime time.Time
	entry   *cache.Entry
	image   *docker.Image
}

// CacheReport is `cache list`'s report.
type CacheReport struct {
	Root   string       `json:"root"`
	Stacks []CacheStack `json:"stacks"`
	Items  []CacheItem  `json:"items"`
}

// CacheOptions configures CacheInventory.
type CacheOptions struct {
	// Stacks are stack directories to count besides the labelled ones,
	// such as the one the command runs in, which may have built images
	// before it has any containers or volumes.
	Stacks []string
}

// CacheInventory lists the cache's source trees, build contexts, downloads
// and temporary directories, and the commit-tagged and dev images, with
// what uses each (§7.1). A stack counts if a container, volume or network
// carries its stack-dir label, the cache's stack registry has it, or opts
// names it.
func CacheInventory(ctx context.Context, d *Deps, c *cache.Cache, opts CacheOptions) (*CacheReport, error) {
	containers, err := d.Docker.ContainerList(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}
	vols, err := d.Docker.VolumeList(ctx, stack.LabelStackDir)
	if err != nil {
		return nil, fmt.Errorf("listing volumes: %w", err)
	}
	nets, err := d.Docker.NetworkList(ctx, stack.LabelStackDir)
	if err != nil {
		return nil, fmt.Errorf("listing networks: %w", err)
	}
	labelled := make([]map[string]string, 0, len(containers)+len(vols)+len(nets))
	for _, ct := range containers {
		labelled = append(labelled, ct.Labels)
	}
	for _, v := range vols {
		labelled = append(labelled, v.Labels)
	}
	for _, n := range nets {
		labelled = append(labelled, n.Labels)
	}
	registry, err := c.RegisteredStacks()
	if err != nil {
		return nil, fmt.Errorf("reading the stack registry: %w", err)
	}
	stacks := readStacks(labelled, registry, opts.Stacks)

	imgs, err := d.Docker.ImageList(ctx, catalog.Namespace+"/*")
	if err != nil {
		return nil, fmt.Errorf("listing images: %w", err)
	}
	entries, err := c.Entries()
	if err != nil {
		return nil, fmt.Errorf("listing the cache: %w", err)
	}

	var items []CacheItem
	for i := range imgs {
		if _, ok := cachedImage(imgs[i].Ref); ok {
			items = append(items, CacheItem{Kind: CacheImage, Name: imgs[i].Ref, ImageID: imgs[i].ID,
				Size: imgs[i].Size, modTime: imgs[i].Created, image: &imgs[i]})
		}
	}
	for i := range entries {
		e := &entries[i]
		items = append(items, CacheItem{Kind: string(e.Kind), Name: e.Name, Size: e.Size, modTime: e.ModTime, entry: e})
	}
	classifyCache(items, containers, stacks, c.Root(), d.Clock.Now())
	return &CacheReport{Root: c.Root(), Stacks: stacks, Items: items}, nil
}

// cachedImage reports whether ref is an image prune manages: a built
// catalog image with a commit or dev tag. It returns the dev tag's stack,
// empty for a commit tag.
func cachedImage(ref string) (devStack string, ok bool) {
	repo, tag, _ := strings.Cut(ref, ":")
	name, found := strings.CutPrefix(repo, catalog.Namespace+"/")
	if !found {
		return "", false
	}
	img, known := catalog.LookupImage(name)
	if !known || !img.Built() {
		return "", false
	}
	if commitTag.MatchString(tag) {
		return "", true
	}
	if m := devTag.FindStringSubmatch(tag); m != nil {
		return m[1], true
	}
	return "", false
}

// readStacks finds the stacks the labels and the registry name, plus extra,
// and reads each one's state.json. A registered stack whose directory is no
// longer a stack is gone, unless labels name it: then it has moved, and
// counts as unreadable.
func readStacks(labels []map[string]string, registry []cache.RegisteredStack, extra []string) []CacheStack {
	var stacks []CacheStack
	seen := map[string]int{}
	add := func(name, dir string) *CacheStack {
		if i, ok := seen[dir]; ok {
			if stacks[i].Name == "" {
				stacks[i].Name = name
			}
			return &stacks[i]
		}
		seen[dir] = len(stacks)
		stacks = append(stacks, CacheStack{Name: name, Dir: dir})
		return &stacks[len(stacks)-1]
	}
	for _, l := range labels {
		if dir := l[stack.LabelStackDir]; dir != "" {
			add(l[stack.LabelStack], dir).labelled = true
		}
	}
	for _, dir := range extra {
		add("", dir)
	}
	for _, r := range registry {
		if r.Dir == "" {
			stacks = append(stacks, CacheStack{Registered: true, key: r.Key, broken: true,
				Error: "can't parse its " + registryEntry(r.Key)})
			continue
		}
		s := add(r.Name, r.Dir)
		s.Registered, s.key = true, r.Key
	}
	for i := range stacks {
		s := &stacks[i]
		if s.broken {
			continue
		}
		state, err := loadStackState(s.Dir)
		switch {
		case err == nil:
		case s.Registered && !s.labelled && errors.Is(err, stack.ErrNotFound) && !hasCLIDir(s.Dir):
			s.Gone, s.Error = true, "no longer a stack"
			continue
		default:
			s.Error = err.Error()
			continue
		}
		s.Readable, s.state = true, state
	}
	slices.SortFunc(stacks, func(a, b CacheStack) int { return strings.Compare(a.Dir, b.Dir) })
	return stacks
}

// hasCLIDir reports whether dir may still hold a stack's .pic-sure/, and
// with it a state.json naming what the stack uses.
func hasCLIDir(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, stack.CLIDir))
	return !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR)
}

func loadStackState(dir string) (*stack.State, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("stack directory %q is not absolute", dir)
	}
	st, err := stack.Open(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = st.Close() }()
	state, err := st.LoadState()
	if errors.Is(err, fs.ErrNotExist) {
		return &stack.State{}, nil // not built yet: it names nothing
	}
	return state, err
}

// classifyCache sets each item's status by the §7.1 rules.
func classifyCache(items []CacheItem, containers []docker.ContainerInfo, stacks []CacheStack, root string, now time.Time) {
	roots := []string{filepath.Clean(root)}
	if real, err := filepath.EvalSymlinks(root); err == nil && real != roots[0] {
		roots = append(roots, real)
	}
	for i := range items {
		it := &items[i]
		it.UsedBy = append(containerUses(it, containers, roots), stackUses(it, stacks)...)
		switch {
		case len(it.UsedBy) > 0:
			it.Status = CacheInUse
		case now.Sub(it.modTime) < RecentCacheAge:
			it.Status = CacheRecent
		case len(it.MayBeUsedBy) > 0:
			it.Status = CacheUnknownStack
		default:
			it.Status = CacheUnused
		}
	}
}

func containerUses(it *CacheItem, containers []docker.ContainerInfo, roots []string) []string {
	var uses []string
	for _, ct := range containers {
		used := false
		if it.image != nil {
			used = ct.ImageID == it.image.ID || ct.Image == it.Name || ct.Image == "docker.io/"+it.Name
		} else {
			used = slices.ContainsFunc(ct.Mounts, func(m string) bool { return mountsEntry(m, it.Name, roots) })
		}
		if used {
			uses = append(uses, "container "+ct.Name)
		}
	}
	return uses
}

// mountsEntry reports whether the host path mount reaches the cache entry
// rel: the entry itself, a path inside it, or a directory containing it,
// under any of the cache root's spellings.
func mountsEntry(mount, rel string, roots []string) bool {
	if !filepath.IsAbs(mount) {
		return false // a volume name
	}
	for _, root := range roots {
		dir := filepath.Join(root, filepath.FromSlash(rel))
		if withinLexically(mount, dir) || withinLexically(dir, mount) {
			return true
		}
	}
	return false
}

// withinLexically reports whether path is dir or inside it, comparing the
// paths as written without following symlinks.
func withinLexically(path, dir string) bool {
	r, err := filepath.Rel(dir, path)
	return err == nil && filepath.IsLocal(r)
}

// stackUses returns the readable stacks whose state names the item, and
// records in MayBeUsedBy the unreadable ones that might.
func stackUses(it *CacheItem, stacks []CacheStack) []string {
	var uses []string
	for _, s := range stacks {
		if s.Gone {
			continue
		}
		label := s.Name
		if label == "" {
			label = s.Dir
		}
		if !s.Readable {
			if mightUse(it, s) {
				it.MayBeUsedBy = append(it.MayBeUsedBy, label)
			}
			continue
		}
		if stateNames(it, s.state) {
			uses = append(uses, "stack "+label)
		}
	}
	return uses
}

// mightUse reports whether a stack whose state can't be read might use the
// item: any shared image or source tree, and its own dev images. Build
// contexts, downloads and temporary directories belong to no stack.
func mightUse(it *CacheItem, s CacheStack) bool {
	switch {
	case it.image != nil:
		dev, _ := cachedImage(it.Name)
		return dev == "" || dev == s.Name || s.Name == ""
	case it.entry != nil:
		return it.entry.Kind == cache.EntrySource
	}
	return false
}

func stateNames(it *CacheItem, state *stack.State) bool {
	switch {
	case it.image != nil:
		repo, tag, _ := strings.Cut(it.Name, ":")
		name := strings.TrimPrefix(repo, catalog.Namespace+"/")
		return state.Images[name] == tag || state.DevImages[name] == tag
	case it.entry != nil && it.entry.Kind == cache.EntrySource:
		for comp, c := range state.Components {
			if cc, ok := catalog.LookupComponent(comp); ok && cc.RepoName() == it.entry.Repo && c.Commit == it.entry.SHA {
				return true
			}
		}
	}
	return false
}

// PruneOptions configures PruneCache.
type PruneOptions struct {
	CacheOptions
	// DryRun reports what would go without removing anything.
	DryRun bool
	// Force also removes items an unreadable stack might use, and forgets
	// registry entries that can't be read. In-use and recent items are
	// never removed.
	Force bool
	// CommitImagesOnly limits the prune to commit-tagged images (destroy
	// --prune-images).
	CommitImagesOnly bool
}

// PruneReport is `cache prune`'s report.
type PruneReport struct {
	CacheReport
	DryRun bool `json:"dry_run"`
	// Removed lists what was removed, or with DryRun would be.
	Removed []CacheItem `json:"removed"`
	// Skipped lists what prune meant to remove but couldn't.
	Skipped []PruneSkip `json:"skipped,omitempty"`
	// Freed is the bytes reclaimed, or with DryRun that would be. An image
	// counts only once its last tag goes.
	Freed int64 `json:"freed"`
	// Forgotten lists the gone stacks whose registry entries were removed,
	// or with DryRun would be.
	Forgotten []CacheStack `json:"forgotten"`
}

// PruneSkip is an item prune couldn't remove.
type PruneSkip struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// PruneCache removes the cache items nothing uses (§7.1): unused images
// with `docker image rm`, which also refuses one a container uses, and
// cache entries with cache.RemoveEntry, under their locks. Unless DryRun,
// it holds the cache's prune lock throughout, so no image step runs
// between the inventory and the removals; when one is running past the
// cache's lock timeout, it fails without removing anything. An item whose
// own lock stays busy is skipped with a warning. Failures to remove one
// item don't stop the others; the error at the end names them.
func PruneCache(ctx context.Context, d *Deps, c *cache.Cache, opts PruneOptions) (*PruneReport, error) {
	report := &PruneReport{DryRun: opts.DryRun, Removed: []CacheItem{}, Forgotten: []CacheStack{}}
	step := steps.Step{
		ID:    StepPrune,
		Title: "Prune the cache",
		Apply: func(ctx context.Context, sink events.Sink) error {
			r, err := pruneCache(ctx, d, c, sink, opts)
			if r != nil {
				*report = *r
			}
			return err
		},
	}
	return report, steps.Run(ctx, d.Sink, []steps.Step{step}, steps.Options{})
}

// pruneCache is PruneCache's step, reporting on sink as the caller's step.
func pruneCache(ctx context.Context, d *Deps, c *cache.Cache, sink events.Sink, opts PruneOptions) (*PruneReport, error) {
	report := &PruneReport{DryRun: opts.DryRun, Removed: []CacheItem{}}
	if !opts.DryRun {
		lock, err := c.LockPrune(ctx)
		if errors.Is(err, cache.ErrLockTimeout) {
			return report, fmt.Errorf("a build is using the cache; prune again when it's done: %w", err)
		}
		if err != nil {
			return report, err
		}
		defer func() { _ = lock.Unlock() }()
	}
	inv, err := CacheInventory(ctx, d, c, opts.CacheOptions)
	if err != nil {
		return report, err
	}
	report.CacheReport = *inv
	return report, prune(ctx, d, c, sink, opts, report)
}

func prune(ctx context.Context, d *Deps, c *cache.Cache, sink events.Sink, opts PruneOptions, report *PruneReport) error {
	for _, s := range report.Stacks {
		if s.Readable || s.Gone {
			continue
		}
		label := s.Name
		if label == "" {
			label = "a stack"
		}
		action := "keeping every shared image and source tree it might use; --force removes them"
		if opts.Force {
			action = "--force: removing what it might use anyway"
		}
		text := fmt.Sprintf("can't read the state of %s at %s (%s): %s", label, s.Dir, s.Error, action)
		if s.broken {
			text = fmt.Sprintf("can't parse the %s, so it might name any stack: %s", registryEntry(s.key), action)
		}
		sink.Emit(events.Warning{ID: StepPrune, Text: text})
	}

	removed := map[string]bool{} // image refs
	var failed []string
	for _, it := range report.Items {
		switch it.Status {
		case CacheUnused:
		case CacheUnknownStack:
			if !opts.Force {
				continue
			}
		default:
			continue
		}
		if opts.CommitImagesOnly {
			if devStack, _ := cachedImage(it.Name); it.image == nil || devStack != "" {
				continue
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		verb := "removed"
		if opts.DryRun {
			verb = "would remove"
		} else if err := removeCacheItem(ctx, d, c, it); err != nil {
			reason := err.Error()
			if errors.Is(err, cache.ErrLockTimeout) {
				reason = "in use by a running build: " + reason
				sink.Emit(events.Warning{ID: StepPrune, Text: "skipped " + it.Name + ": " + reason})
			} else {
				failed = append(failed, it.Name)
				sink.Emit(events.Warning{ID: StepPrune, Text: "couldn't remove " + it.Name + ": " + reason})
			}
			report.Skipped = append(report.Skipped, PruneSkip{Name: it.Name, Reason: reason})
			continue
		}
		sink.Emit(events.Progress{ID: StepPrune, Text: fmt.Sprintf("%s %s (%s)", verb, it.Name, FormatBytes(it.Size))})
		report.Removed = append(report.Removed, it)
		if it.image != nil {
			removed[it.Name] = true
		} else {
			report.Freed += it.Size
		}
	}
	for _, s := range report.Stacks {
		if !s.Gone && (!s.broken || !opts.Force) {
			continue
		}
		verb := "forgot"
		if opts.DryRun {
			verb = "would forget"
		} else if err := c.ForgetStack(s.key); err != nil {
			failed = append(failed, registryEntry(s.key))
			sink.Emit(events.Warning{ID: StepPrune, Text: "couldn't remove " + registryEntry(s.key) + ": " + err.Error()})
			continue
		}
		sink.Emit(events.Progress{ID: StepPrune, Text: verb + " " + ForgottenStack(s)})
		report.Forgotten = append(report.Forgotten, s)
	}

	counted := map[string]bool{}
	for _, it := range report.Removed {
		if it.image == nil || counted[it.ImageID] {
			continue
		}
		if !slices.ContainsFunc(it.image.RepoTags, func(t string) bool { return !removed[t] }) {
			counted[it.ImageID] = true
			report.Freed += it.Size
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("couldn't remove %s", strings.Join(failed, ", "))
	}
	return nil
}

// ForgottenStack describes a stack prune forgets: a gone one, or with
// --force one whose registry entry can't be read.
func ForgottenStack(s CacheStack) string {
	if s.broken {
		return "the unreadable " + registryEntry(s.key)
	}
	if s.Name == "" {
		return "the gone stack at " + s.Dir
	}
	return fmt.Sprintf("the gone stack %s (%s)", s.Name, s.Dir)
}

func registryEntry(key string) string { return "registry entry stacks/" + key }

func removeCacheItem(ctx context.Context, d *Deps, c *cache.Cache, it CacheItem) error {
	if it.entry != nil {
		return c.RemoveEntry(ctx, *it.entry)
	}
	lock, err := c.LockImage(ctx, it.Name)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()
	return d.Docker.RemoveImage(ctx, it.Name)
}

// FormatBytes renders n bytes in decimal units, as docker does: 0 B,
// 512 B, 1.2 kB, 425 MB, 1.15 GB.
func FormatBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	v, exp := float64(n)/unit, 0
	for v >= 999.5 && exp < 5 { // 999.5 would print as 1000
		v /= unit
		exp++
	}
	prec := 0
	switch {
	case v < 9.995:
		prec = 2
	case v < 99.95:
		prec = 1
	}
	return strconv.FormatFloat(v, 'f', prec, 64) + " " + string("kMGTPE"[exp]) + "B"
}
