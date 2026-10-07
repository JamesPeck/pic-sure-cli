package cache

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
)

// ResolveRef returns the full commit sha that ref (a tag, branch or sha)
// names in the catalog component's repository, under the repository's fetch
// lock. A missing bare clone is cloned; an existing one is fetched first,
// so a branch resolves to its current head, unless ref is a full sha the
// clone already has. EnsureSource then finds the commit without fetching.
func (c *Cache) ResolveRef(ctx context.Context, component, ref string) (string, error) {
	comp, ok := catalog.LookupComponent(component)
	if !ok {
		return "", fmt.Errorf("cache: unknown component %q", component)
	}
	if c.git == nil {
		return "", errors.New("cache: ResolveRef needs Options.Git")
	}
	repo := comp.RepoName()
	lock, err := c.LockRepo(ctx, repo)
	if err != nil {
		return "", err
	}
	defer func() { _ = lock.Unlock() }()

	bare := filepath.Join(c.root, gitDir, repo+".git")
	removeStale(filepath.Dir(bare), func(e fs.DirEntry) bool {
		return strings.HasPrefix(e.Name(), repo+".git.tmp-")
	})
	cloned, err := isDir(bare)
	if err != nil {
		return "", err
	}
	if cloned && checkSHA(ref) == nil {
		if got, err := c.git.ResolveRef(ctx, bare, ref); err == nil && got == ref {
			return got, nil
		}
	}
	if cloned {
		c.progress("fetching %s", comp.Repo)
		if err := c.git.Fetch(ctx, bare, nil, true); err != nil {
			return "", fmt.Errorf("fetching %s: %w", comp.Repo, err)
		}
	} else {
		c.progress("cloning %s", comp.Repo)
		if err := c.git.EnsureBare(ctx, comp.CloneURL(), bare); err != nil {
			return "", fmt.Errorf("cloning %s: %w", comp.Repo, err)
		}
	}
	sha, err := c.git.ResolveRef(ctx, bare, ref)
	if err != nil {
		return "", fmt.Errorf("resolving %s in %s: %w", ref, comp.Repo, err)
	}
	return sha, nil
}
