package cache

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/git"
)

// EnsureSource returns src/<repo>/<sha>, the source tree of the catalog
// component at commit sha, a full lowercase commit sha. A missing tree is
// made under the repository's fetch lock: the bare clone in git/<repo>.git
// is cloned or, if it lacks sha, fetched, and then `git archive` is
// unpacked into a temporary sibling that is renamed into place. So a tree
// that exists is complete, and it is never changed afterwards. Don't call
// EnsureSource while holding the same repository's LockRepo.
func (c *Cache) EnsureSource(ctx context.Context, component, sha string) (string, error) {
	dest, err := c.SourceDir(component, sha)
	if err != nil {
		return "", err
	}
	comp, _ := catalog.LookupComponent(component)
	repo := comp.RepoName()
	if done, err := isDir(dest); done || err != nil {
		return dest, err
	}
	if c.git == nil {
		return "", errors.New("cache: EnsureSource needs Options.Git")
	}

	lock, err := c.LockRepo(ctx, repo)
	if err != nil {
		return "", err
	}
	defer func() { _ = lock.Unlock() }()
	if done, err := isDir(dest); done || err != nil {
		return dest, err
	}
	// Only a lock holder makes temporary directories here, so any left
	// over belong to a run that died.
	bare := filepath.Join(c.root, gitDir, repo+".git")
	removeStale(filepath.Dir(bare), func(e fs.DirEntry) bool {
		return strings.HasPrefix(e.Name(), repo+".git.tmp-")
	})
	removeStale(filepath.Dir(dest), func(e fs.DirEntry) bool {
		return strings.Contains(e.Name(), ".tmp-")
	})

	if err := c.ensureCommit(ctx, comp, bare, sha); err != nil {
		return "", err
	}
	c.progress("unpacking %s at %s", comp.Repo, sha[:12])
	if err := c.unpack(ctx, bare, sha, dest); err != nil {
		return "", fmt.Errorf("unpacking %s at %s: %w", comp.Repo, sha, err)
	}
	return dest, nil
}

// SourceDir returns where EnsureSource keeps the source tree of the
// catalog component at commit sha, without making it. The tree is complete
// if the directory exists.
func (c *Cache) SourceDir(component, sha string) (string, error) {
	comp, ok := catalog.LookupComponent(component)
	if !ok {
		return "", fmt.Errorf("cache: unknown component %q", component)
	}
	if err := checkSHA(sha); err != nil {
		return "", err
	}
	return filepath.Join(c.root, srcDir, comp.RepoName(), sha), nil
}

// ensureCommit makes bare a clone of comp's repository that has commit sha,
// fetching every branch and tag if it doesn't have it yet.
func (c *Cache) ensureCommit(ctx context.Context, comp catalog.Component, bare, sha string) error {
	cloned, err := isDir(bare)
	if err != nil {
		return err
	}
	if !cloned {
		c.progress("cloning %s", comp.Repo)
	}
	if err := c.git.EnsureBare(ctx, comp.CloneURL(), bare); err != nil {
		return fmt.Errorf("cloning %s: %w", comp.Repo, err)
	}
	got, err := c.git.ResolveRef(ctx, bare, sha)
	if cloned && errors.Is(err, git.ErrUnknownRef) {
		c.progress("fetching %s", comp.Repo)
		if err := c.git.Fetch(ctx, bare, nil, true); err != nil {
			return fmt.Errorf("fetching %s: %w", comp.Repo, err)
		}
		got, err = c.git.ResolveRef(ctx, bare, sha)
	}
	if err != nil {
		return fmt.Errorf("finding commit %s in %s: %w", sha, comp.Repo, err)
	}
	if got != sha {
		// An annotated tag's own sha peels to the commit it tags.
		return fmt.Errorf("%s in %s is not a commit (it names commit %s)", sha, comp.Repo, got)
	}
	return nil
}

// unpack writes the tree at sha into dest through a temporary sibling.
func (c *Cache) unpack(ctx context.Context, bare, sha, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dest), sha+".tmp-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }() // a no-op once renamed
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	archive, err := c.git.Archive(ctx, bare, sha)
	if err != nil {
		return err
	}
	err = git.Unpack(archive, tmp)
	_ = archive.Close()
	if err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

// isDir reports whether path is an existing directory. Anything else
// there is an error.
func isDir(path string) (bool, error) {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	case !info.IsDir():
		return false, fmt.Errorf("%s is not a directory", path)
	}
	return true, nil
}

// removeStale removes the entries in dir that stale picks, ignoring errors:
// a leftover that can't be removed only wastes space.
func removeStale(dir string, stale func(fs.DirEntry) bool) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if stale(e) {
			_ = os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
}
