package release

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/git"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

// lockName is release-control's fetch lock in the cache.
const lockName = "release-control"

// maxBuildSpec bounds how much of build-spec.json is read.
const maxBuildSpec = 1 << 20

var commitish = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)

// Options says which release-control commit to read.
type Options struct {
	// Repo is release-control's URL (release.repo).
	Repo string
	// Branch is the branch whose head is used (release.branch).
	Branch string
	// Commit, when set, pins a commit instead (--release-commit): a full or
	// abbreviated sha.
	Commit string
}

// A Release is one release-control commit and its build-spec.
type Release struct {
	Repo   string
	Branch string
	Commit string // full commit sha
	Spec   *BuildSpec
}

// Fetch clones or fetches release-control into the cache, picks the
// branch head or the pinned commit, and reads build-spec.json there. g runs
// git; c reports progress to the step its WithEvents names. A malformed
// pin is exit 2, and a pin release-control doesn't have is exit 3.
func Fetch(ctx context.Context, c *cache.Cache, g git.Client, sink events.Sink, step string, opts Options) (*Release, error) {
	if opts.Commit != "" && !commitish.MatchString(opts.Commit) {
		return nil, exitcode.Usage("--release-commit %q is not a commit sha", opts.Commit)
	}
	if opts.Commit == "" && opts.Branch == "" {
		return nil, exitcode.Usage("release.branch is empty")
	}
	lock, err := c.LockRepo(ctx, lockName)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Unlock() }()

	dir := c.ReleaseControlDir()
	sha, err := fetchCommit(ctx, g, sink, step, dir, opts)
	if err != nil {
		return nil, err
	}
	data, err := readFile(ctx, g, dir, sha, BuildSpecFile)
	if err != nil {
		return nil, fmt.Errorf("reading %s at release-control %s: %w", BuildSpecFile, sha[:12], err)
	}
	spec, err := ParseBuildSpec(data)
	if err != nil {
		return nil, fmt.Errorf("release-control %s: %w", sha[:12], err)
	}
	return &Release{Repo: opts.Repo, Branch: opts.Branch, Commit: sha, Spec: spec}, nil
}

// fetchCommit brings dir up to date with opts.Repo and returns the commit
// to read. A pinned commit that dir already has is used without fetching.
func fetchCommit(ctx context.Context, g git.Client, sink events.Sink, step, dir string, opts Options) (string, error) {
	removeStale(dir)
	existed, err := exists(dir)
	if err != nil {
		return "", err
	}
	progress(sink, step, "fetching release-control from %s", opts.Repo)
	if err := g.EnsureBare(ctx, opts.Repo, dir); err != nil {
		return "", fmt.Errorf("cloning release-control %s: %w", opts.Repo, err)
	}
	if opts.Commit != "" {
		if sha, err := g.ResolveRef(ctx, dir, opts.Commit); err == nil {
			return sha, nil
		}
	}
	if existed {
		if err := g.Fetch(ctx, dir, nil, false); err != nil {
			return "", fmt.Errorf("fetching release-control %s: %w", opts.Repo, err)
		}
	}
	if opts.Commit != "" {
		sha, err := g.ResolveRef(ctx, dir, opts.Commit)
		if errors.Is(err, git.ErrUnknownRef) {
			return "", exitcode.Precondition("release-control %s has no commit %s", opts.Repo, opts.Commit)
		}
		return sha, err
	}
	sha, err := g.ResolveRef(ctx, dir, "refs/heads/"+opts.Branch)
	if errors.Is(err, git.ErrUnknownRef) {
		return "", exitcode.Precondition("release-control %s has no branch %s", opts.Repo, opts.Branch)
	}
	return sha, err
}

// removeStale removes the temporary siblings of dir that a clone killed
// before its rename left behind. Only the lock holder clones, so any there
// are a dead run's.
func removeStale(dir string) {
	entries, _ := os.ReadDir(filepath.Dir(dir))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), filepath.Base(dir)+".tmp-") {
			_ = os.RemoveAll(filepath.Join(filepath.Dir(dir), e.Name()))
		}
	}
}

func exists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// readFile returns one file from the tree at sha, read from `git archive`.
func readFile(ctx context.Context, g git.Client, dir, sha, name string) ([]byte, error) {
	archive, err := g.Archive(ctx, dir, sha)
	if err != nil {
		return nil, err
	}
	defer func() { _ = archive.Close() }()
	tr := tar.NewReader(archive)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("no %s", name)
		}
		if err != nil {
			return nil, err
		}
		if hdr.Name != name {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("%s is not a regular file", name)
		}
		data, err := io.ReadAll(io.LimitReader(tr, maxBuildSpec+1))
		if err != nil {
			return nil, err
		}
		if len(data) > maxBuildSpec {
			return nil, fmt.Errorf("%s is larger than %d bytes", name, maxBuildSpec)
		}
		return data, nil
	}
}

// ResolveComponents resolves each component's ref to a commit in its cached
// clone: the ref pic-sure.yaml sets (components.<name>.ref) if any, else
// the build-spec's. A component the build-spec doesn't name falls back to
// main, with a warning. The result is keyed by component name, as
// state.json's components are.
func (r *Release) ResolveComponents(ctx context.Context, c *cache.Cache, sink events.Sink, step string, cfg stack.Components) (map[string]stack.Component, error) {
	overrides := map[string]string{
		catalog.PicSure:       cfg.PicSure.Ref,
		catalog.Frontend:      cfg.Frontend.Ref,
		catalog.Migrations:    cfg.Migrations.Ref,
		catalog.DictionaryETL: cfg.DictionaryETL.Ref,
	}
	out := map[string]stack.Component{}
	for _, comp := range catalog.Components() {
		ref := overrides[comp.Name]
		if ref == "" {
			var ok bool
			if ref, ok = r.Spec.Ref(comp.SpecKey); !ok {
				ref = "main"
				warn(sink, step, "the build-spec at release-control %s has no %s entry; using %s's main branch",
					r.Commit[:12], comp.SpecKey, comp.Repo)
			}
		}
		progress(sink, step, "resolving %s %s", comp.Name, ref)
		sha, err := c.ResolveRef(ctx, comp.Name, ref)
		if errors.Is(err, git.ErrUnknownRef) {
			return nil, exitcode.Precondition("%s has no tag, branch or commit %q", comp.Repo, ref)
		}
		if err != nil {
			return nil, err
		}
		out[comp.Name] = stack.Component{Ref: ref, Commit: sha}
	}
	return out, nil
}

// Record stores the release commit and the resolved component commits in
// state, replacing the ones recorded before. The caller saves state.
func (r *Release) Record(state *stack.State, components map[string]stack.Component) {
	state.Release = stack.Release{Repo: r.Repo, Branch: r.Branch, Commit: r.Commit}
	state.Components = components
}

func progress(sink events.Sink, step, format string, args ...any) {
	if sink != nil {
		sink.Emit(events.Progress{ID: step, Text: fmt.Sprintf(format, args...)})
	}
}

func warn(sink events.Sink, step, format string, args ...any) {
	if sink != nil {
		sink.Emit(events.Warning{ID: step, Text: fmt.Sprintf(format, args...)})
	}
}
