package cache

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/git"
)

// The layout under the cache root (§7.1).
const (
	gitDir            = "git"       // git/<repo>.git: bare clones
	srcDir            = "src"       // src/<repo>/<sha>/: source trees
	downloadsDir      = "downloads" // demo datasets
	buildDir          = "build"     // build/<sha12>/: image build contexts
	tmpDir            = "tmp"       // per-run temporary directories
	locksDir          = "locks"     // lock files, which are never removed
	releaseControlDir = "release-control"
)

// Cache is the host cache at one root. It is safe for concurrent use, and
// its locks also exclude other processes.
type Cache struct {
	root        string
	git         git.Client
	holder      string
	lockTimeout time.Duration
	sink        events.Sink
	step        string
}

// Options configures Open.
type Options struct {
	// Git fetches and archives source trees. Only EnsureSource uses it.
	Git git.Client
	// Holder describes this command in the cache's lock files, for the
	// message a command waiting on one of its locks shows, e.g.
	// "pic-sure build". The default is "pic-sure".
	Holder string
	// LockTimeout, when positive, replaces every lock's default timeout.
	LockTimeout time.Duration
}

// DefaultRoot returns the cache root: pic-sure under $XDG_CACHE_HOME, or
// under ~/.cache when XDG_CACHE_HOME is unset or relative. It refuses a root
// inside the temporary directory ($TMPDIR), because Docker VMs such as
// Colima and Lima share only the home directory, and builds bind-mount
// source trees from the cache.
func DefaultRoot() (string, error) {
	base := os.Getenv("XDG_CACHE_HOME")
	if !filepath.IsAbs(base) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("finding the cache directory: %w", err)
		}
		if !filepath.IsAbs(home) {
			return "", fmt.Errorf("finding the cache directory: $HOME (%q) is not an absolute path", home)
		}
		base = filepath.Join(home, ".cache")
	}
	root := filepath.Join(base, "pic-sure")
	if tmp := os.TempDir(); within(root, tmp) {
		return "", fmt.Errorf("the host cache %s is inside the temporary directory %s, which Docker can't always mount; "+
			"set XDG_CACHE_HOME to a directory under your home directory", root, tmp)
	}
	return root, nil
}

// within reports whether path is dir or inside it, comparing their real
// locations so that a symlink (macOS's /var is /private/var) can't hide it.
func within(path, dir string) bool {
	rel, err := filepath.Rel(realPath(dir), realPath(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// realPath resolves the symlinks in the longest existing prefix of p, an
// absolute path, and appends the rest unresolved.
func realPath(p string) string {
	p = filepath.Clean(p)
	var rest []string
	for {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(append([]string{real}, rest...)...)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(append([]string{p}, rest...)...)
		}
		rest = append([]string{filepath.Base(p)}, rest...)
		p = parent
	}
}

// Open opens the cache at root, an absolute path such as DefaultRoot's,
// creating its directories as needed. Open does not apply DefaultRoot's
// temporary-directory check.
func Open(root string, opts Options) (*Cache, error) {
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("cache root %q is not an absolute path", root)
	}
	root = filepath.Clean(root)
	// XDG asks for 0700 on a base directory it has to create.
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		return nil, err
	}
	for _, dir := range []string{gitDir, srcDir, downloadsDir, buildDir, tmpDir, locksDir} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			return nil, err
		}
	}
	holder := opts.Holder
	if holder == "" {
		holder = "pic-sure"
	}
	return &Cache{root: root, git: opts.Git, holder: holder, lockTimeout: opts.LockTimeout}, nil
}

// WithEvents returns a Cache that reports to sink as step: its progress
// while fetching and unpacking, and its waits for locks other commands
// hold.
func (c *Cache) WithEvents(sink events.Sink, step string) *Cache {
	cc := *c
	cc.sink, cc.step = sink, step
	return &cc
}

func (c *Cache) progress(format string, args ...any) {
	if c.sink != nil {
		c.sink.Emit(events.Progress{ID: c.step, Text: fmt.Sprintf(format, args...)})
	}
}

// Root is the cache's root directory.
func (c *Cache) Root() string { return c.root }

// ReleaseControlDir is where the release-control clone lives. It doesn't
// exist until the clone is made; take LockRepo("release-control") around
// cloning or fetching it.
func (c *Cache) ReleaseControlDir() string { return filepath.Join(c.root, releaseControlDir) }

// DownloadsDir is the directory for downloaded datasets.
func (c *Cache) DownloadsDir() string { return filepath.Join(c.root, downloadsDir) }

// BuildDir is build/<sha12>, the transient directory for the image build
// contexts of the reactor build at commit sha. It isn't created: the build
// creates and removes it while holding the reactor lock.
func (c *Cache) BuildDir(sha string) (string, error) {
	if err := checkSHA(sha); err != nil {
		return "", err
	}
	return filepath.Join(c.root, buildDir, sha[:12]), nil
}

// staleTempAge is how old a directory in tmp/ must be for TempDir to treat
// it as left behind by a run that died. No run lasts this long.
const staleTempAge = 7 * 24 * time.Hour

// TempDir creates a new directory under the cache's tmp/ for one run's
// temporary files, named pattern as in os.MkdirTemp, at mode 0700. Unlike
// $TMPDIR it can be bind-mounted into containers. The caller removes it
// when done. TempDir first removes any directory there older than a week,
// which a run that was killed left behind.
func (c *Cache) TempDir(pattern string) (string, error) {
	dir := filepath.Join(c.root, tmpDir)
	cutoff := time.Now().Add(-staleTempAge)
	removeStale(dir, func(e fs.DirEntry) bool {
		info, err := e.Info()
		return err == nil && info.ModTime().Before(cutoff)
	})
	return os.MkdirTemp(dir, pattern)
}

// validName is what a repository or other single path element in the cache
// may be called.
var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func checkName(kind, s string) error {
	if !validName.MatchString(s) {
		return fmt.Errorf("cache: invalid %s name %q", kind, s)
	}
	return nil
}

var fullSHA = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

func checkSHA(sha string) error {
	if !fullSHA.MatchString(sha) {
		return fmt.Errorf("cache: %q is not a full lowercase commit sha", sha)
	}
	return nil
}
