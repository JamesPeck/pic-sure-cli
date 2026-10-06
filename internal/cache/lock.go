package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// How long each lock waits for another command to release it, unless
// Options.LockTimeout says otherwise. They cover a first clone of the
// largest repository, a cold Maven build, and the slowest image build.
const (
	RepoLockTimeout    = 15 * time.Minute
	ReactorLockTimeout = time.Hour
	ImageLockTimeout   = 30 * time.Minute
)

// ErrLockTimeout is wrapped by the error a lock returns when its timeout
// passes while another command still holds it.
var ErrLockTimeout = errors.New("timed out")

// lockPoll is how often a waiting lock retries. flock can't wait on a
// context, so locking polls with LOCK_NB instead of blocking.
const lockPoll = 100 * time.Millisecond

// Lock is a held cache lock.
type Lock struct {
	f *os.File
}

// holder is what a lock file says about the command holding it.
type holder struct {
	PID    int    `json:"pid"`
	Holder string `json:"holder,omitempty"`
}

// LockRepo takes the lock for fetching repo (a component's RepoName, or
// "release-control") into the cache. EnsureSource takes it itself.
func (c *Cache) LockRepo(ctx context.Context, repo string) (*Lock, error) {
	if err := checkName("repository", repo); err != nil {
		return nil, err
	}
	return c.lock(ctx, "repo-"+repo+".lock", "fetch lock for "+repo, RepoLockTimeout)
}

// LockReactor takes the global reactor lock, which every Maven run holds so
// that two builds never use the pic-sure-m2 volume at once. It is global to
// this cache, not to the Docker daemon: a command using another cache root,
// such as another user's on the same daemon, doesn't see it.
func (c *Cache) LockReactor(ctx context.Context) (*Lock, error) {
	return c.lock(ctx, "reactor.lock", "reactor build lock", ReactorLockTimeout)
}

// LockImage takes the lock for building the image tag, such as
// hms-dbmi/psama:0123456789ab.
func (c *Cache) LockImage(ctx context.Context, tag string) (*Lock, error) {
	if tag == "" || strings.ContainsRune(tag, 0) {
		return nil, fmt.Errorf("cache: invalid image tag %q", tag)
	}
	return c.lock(ctx, "image-"+url.PathEscape(tag)+".lock", "build lock for "+tag, ImageLockTimeout)
}

// lock takes an exclusive flock on locks/<file>, waiting up to timeout for
// another holder to release it. Flocks belong to the open file, so a second
// lock in the same process waits like any other. The kernel releases a lock
// when its holder exits, however it exits, so a crash never leaves a stale
// lock.
func (c *Cache) lock(ctx context.Context, file, what string, timeout time.Duration) (*Lock, error) {
	if c.lockTimeout > 0 {
		timeout = c.lockTimeout
	}
	path := filepath.Join(c.root, locksDir, file)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := c.wait(ctx, f, path, what, timeout); err != nil {
		_ = f.Close()
		return nil, err
	}
	data, _ := json.Marshal(holder{PID: os.Getpid(), Holder: c.holder})
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt(data, 0)
	}
	return &Lock{f: f}, nil
}

// wait takes the flock on f, polling until timeout if another holder has
// it.
func (c *Cache) wait(ctx context.Context, f *os.File, path, what string, timeout time.Duration) error {
	locked, err := tryLock(f, what)
	if locked || err != nil {
		return err
	}
	c.progress("waiting for the %s held by %s", what, lockHolder(path))
	expired := time.NewTimer(timeout)
	defer expired.Stop()
	poll := time.NewTicker(lockPoll)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for the %s: %w", what, context.Cause(ctx))
		case <-expired.C:
			return fmt.Errorf("%w after %s waiting for the %s held by %s", ErrLockTimeout, timeout, what, lockHolder(path))
		case <-poll.C:
		}
		if locked, err := tryLock(f, what); locked || err != nil {
			return err
		}
	}
}

// tryLock takes the flock on f if nobody else holds it.
func tryLock(f *os.File, what string) (bool, error) {
	err := flock(f, syscall.LOCK_EX|syscall.LOCK_NB)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.EWOULDBLOCK):
		return false, nil
	default:
		return false, fmt.Errorf("taking the %s: %w", what, err)
	}
}

// Unlock releases the lock. It is safe to call more than once.
func (l *Lock) Unlock() error {
	if l.f == nil {
		return nil
	}
	_ = l.f.Truncate(0) // so nobody reads a stale holder
	err := l.f.Close()  // closing releases the flock
	l.f = nil
	return err
}

// lockHolder describes the command holding the lock at path, from the lock
// file.
func lockHolder(path string) string {
	data, err := os.ReadFile(path)
	var h holder
	if err != nil || json.Unmarshal(data, &h) != nil || h.PID == 0 {
		return "another pic-sure command"
	}
	if h.Holder == "" {
		return fmt.Sprintf("pid %d", h.PID)
	}
	return fmt.Sprintf("%s (pid %d)", h.Holder, h.PID)
}

// flock is syscall.Flock, retried when a signal interrupts it.
func flock(f *os.File, how int) error {
	for {
		err := syscall.Flock(int(f.Fd()), how)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}
