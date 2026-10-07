package cache

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
)

// EntryKind is what an Entry on disk is.
type EntryKind string

// Entry kinds. Bare clones, the release-control clone and lock files aren't
// entries: prune never removes them.
const (
	// EntrySource is a source tree, src/<repo>/<sha>/.
	EntrySource EntryKind = "source"
	// EntryBuild is a build context left in build/ by a build that died.
	EntryBuild EntryKind = "build"
	// EntryDownload is a downloaded dataset in downloads/.
	EntryDownload EntryKind = "download"
	// EntryTemp is a per-run directory in tmp/, or a temporary sibling of
	// a clone or source tree left by a run that died.
	EntryTemp EntryKind = "temp"
)

// Entry is one item in the cache that prune may remove.
type Entry struct {
	Kind EntryKind
	// Name is the path relative to the cache root, slash-separated.
	Name string
	// Path is the absolute path.
	Path string
	// Repo is the repository a source tree or a clone's temporary sibling
	// belongs to, whose fetch lock covers it.
	Repo string
	// SHA is a source tree's commit.
	SHA string
	// Size is the total size of its regular files, in bytes.
	Size    int64
	ModTime time.Time
}

// Entries lists the source trees, build contexts, downloads and temporary
// directories in the cache, sorted by name.
func (c *Cache) Entries() ([]Entry, error) {
	var out []Entry
	add := func(kind EntryKind, rel, repo, sha string) error {
		e, err := c.entry(kind, rel, repo, sha)
		if errors.Is(err, fs.ErrNotExist) {
			return nil // removed while listing
		}
		if err != nil {
			return err
		}
		out = append(out, e)
		return nil
	}

	repos, err := readDir(filepath.Join(c.root, srcDir))
	if err != nil {
		return nil, err
	}
	for _, repo := range repos {
		if !repo.IsDir() || checkName("repository", repo.Name()) != nil {
			continue
		}
		trees, err := readDir(filepath.Join(c.root, srcDir, repo.Name()))
		if err != nil {
			return nil, err
		}
		for _, t := range trees {
			rel := srcDir + "/" + repo.Name() + "/" + t.Name()
			switch {
			case strings.Contains(t.Name(), ".tmp-"):
				err = add(EntryTemp, rel, repo.Name(), "")
			case t.IsDir() && checkSHA(t.Name()) == nil:
				err = add(EntrySource, rel, repo.Name(), t.Name())
			}
			if err != nil {
				return nil, err
			}
		}
	}

	clones, err := readDir(filepath.Join(c.root, gitDir))
	if err != nil {
		return nil, err
	}
	for _, e := range clones {
		repo, _, ok := strings.Cut(e.Name(), ".git.tmp-")
		if ok && checkName("repository", repo) == nil {
			if err := add(EntryTemp, gitDir+"/"+e.Name(), repo, ""); err != nil {
				return nil, err
			}
		}
	}
	top, err := readDir(c.root)
	if err != nil {
		return nil, err
	}
	for _, e := range top {
		if strings.HasPrefix(e.Name(), releaseControlDir+".tmp-") {
			if err := add(EntryTemp, e.Name(), releaseControlDir, ""); err != nil {
				return nil, err
			}
		}
	}

	for _, d := range []struct {
		dir  string
		kind EntryKind
	}{{buildDir, EntryBuild}, {downloadsDir, EntryDownload}, {tmpDir, EntryTemp}} {
		entries, err := readDir(filepath.Join(c.root, d.dir))
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if err := add(d.kind, d.dir+"/"+e.Name(), "", ""); err != nil {
				return nil, err
			}
		}
	}
	slices.SortFunc(out, func(a, b Entry) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func readDir(dir string) ([]fs.DirEntry, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return entries, err
}

func (c *Cache) entry(kind EntryKind, rel, repo, sha string) (Entry, error) {
	path := filepath.Join(c.root, filepath.FromSlash(rel))
	info, err := os.Lstat(path)
	if err != nil {
		return Entry{}, err
	}
	e := Entry{Kind: kind, Name: rel, Path: path, Repo: repo, SHA: sha, ModTime: info.ModTime()}
	err = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable parts only make the size an underestimate
		}
		if d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				e.Size += fi.Size()
			}
		}
		return nil
	})
	return e, err
}

// RemoveEntry removes an entry Entries returned, holding the lock of
// whatever might be writing or reading it: the repository's fetch lock for
// a source tree or a clone's temporary sibling, the reactor lock for a
// reactor build context, and the image's lock for a frontend build
// context. A source tree is first renamed to a temporary sibling, so a
// removal cut short never leaves a partial tree that EnsureSource would
// take as complete. When a lock is busy past the cache's lock timeout the
// error wraps ErrLockTimeout and nothing is removed.
func (c *Cache) RemoveEntry(ctx context.Context, e Entry) error {
	rel := filepath.FromSlash(e.Name)
	if e.Name == "" || !filepath.IsLocal(rel) || e.Path != filepath.Join(c.root, rel) {
		return fmt.Errorf("cache: %q is not an entry of %s", e.Name, c.root)
	}
	var (
		lock *Lock
		err  error
	)
	switch {
	case e.Repo != "":
		lock, err = c.LockRepo(ctx, e.Repo)
	case e.Kind == EntryBuild:
		name := filepath.Base(e.Path)
		if tag, ok := strings.CutPrefix(name, "frontend-"); ok && checkName("image tag", tag) == nil {
			lock, err = c.LockImage(ctx, catalog.ImagesBuiltFrom(catalog.Frontend)[0].Repository()+":"+tag)
		} else {
			lock, err = c.LockReactor(ctx)
		}
	}
	if err != nil {
		return err
	}
	if lock != nil {
		defer func() { _ = lock.Unlock() }()
	}

	path := e.Path
	if e.Kind == EntrySource {
		gone := path + ".tmp-prune"
		if err := os.RemoveAll(gone); err != nil {
			return err
		}
		if err := os.Rename(path, gone); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		path = gone
	}
	return os.RemoveAll(path)
}
