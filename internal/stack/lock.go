package stack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
)

// ErrLocked is wrapped by the error Lock returns when another command holds
// the stack lock and LockOptions.Wait is false.
var ErrLocked = errors.New("locked")

// lockPoll is how often a waiting Lock retries. flock can't wait on a
// context, so Lock polls with LOCK_NB instead of blocking.
var lockPoll = 100 * time.Millisecond

// LockOptions configures Stack.Lock.
type LockOptions struct {
	// Wait makes Lock wait until the holder finishes (--wait-lock) instead
	// of failing at once.
	Wait bool
	// Command names this command in the lock file, for the message another
	// command shows while it holds the lock, e.g. "pic-sure up".
	Command string
	// OnWait, if set, is called once, with the holder's description, when
	// Lock starts waiting.
	OnWait func(holder string)
}

// Lock is a held stack lock.
type Lock struct {
	f *os.File
}

// holder is what the lock file says about the command holding it.
type holder struct {
	PID     int    `json:"pid"`
	Command string `json:"command,omitempty"`
}

// Lock takes the stack's exclusive lock, an flock on .pic-sure/lock (§10.5),
// which every mutating command holds for its whole run. If another process
// holds it, Lock fails with an error wrapping ErrLocked that names the
// holder, or with opts.Wait, waits until the holder releases it or ctx ends.
// The kernel releases the lock when its holder exits, however it exits, so a
// crash never leaves a stale lock. Each command takes the lock once: a
// second Lock in the same process waits for the first like any other.
func (s *Stack) Lock(ctx context.Context, opts LockOptions) (*Lock, error) {
	f, err := s.openLockFile()
	if err != nil {
		return nil, err
	}
	waiting := false
	for {
		err := flock(f, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			if err := s.stillLockFile(f); err != nil {
				_ = f.Close()
				return nil, err
			}
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("locking %s: %w", s.Path(LockFile), err)
		}
		if !opts.Wait {
			_ = f.Close()
			return nil, fmt.Errorf("the stack in %s is %w by %s", s.Dir, ErrLocked, s.lockHolder())
		}
		if !waiting && opts.OnWait != nil {
			opts.OnWait(s.lockHolder())
		}
		waiting = true
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, fmt.Errorf("waiting for the stack lock: %w", context.Cause(ctx))
		case <-time.After(lockPoll):
		}
	}

	data, _ := json.Marshal(holder{PID: os.Getpid(), Command: opts.Command})
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt(data, 0)
	}
	return &Lock{f: f}, nil
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

// stillLockFile fails if f is no longer .pic-sure/lock: destroy removed the
// stack while this command waited, and the flock it got is on the deleted
// file.
func (s *Stack) stillLockFile(f *os.File) error {
	held, err := f.Stat()
	if err != nil {
		return err
	}
	cur, err := s.root.Lstat(LockFile)
	if err == nil && os.SameFile(held, cur) {
		return nil
	}
	return exitcode.Precondition("%w: the stack in %s was destroyed while this command waited for its lock", ErrNotFound, s.Dir)
}

// openLockFile opens .pic-sure/lock, creating and recording it the first
// time. A symlink there is refused: Lock truncates the file.
func (s *Stack) openLockFile() (*os.File, error) {
	if err := s.noSymlinks(LockFile); err != nil {
		return nil, err
	}
	f, err := s.root.OpenFile(LockFile, os.O_RDWR, 0)
	if !errors.Is(err, fs.ErrNotExist) {
		return f, err
	}
	err = s.recordThenCreate(Entry{Path: LockFile, Type: EntryFile}, func() error {
		var err error
		if f, err = s.root.OpenFile(LockFile, os.O_RDWR|os.O_CREATE, 0o644); err != nil {
			return err
		}
		if err = f.Chmod(0o644); err != nil {
			_ = f.Close()
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return f, nil
}

// lockHolder describes the command holding the lock, from the lock file.
func (s *Stack) lockHolder() string {
	data, err := s.root.ReadFile(LockFile)
	var h holder
	if err != nil || json.Unmarshal(data, &h) != nil || h.PID == 0 {
		return "another pic-sure command"
	}
	if h.Command == "" {
		return fmt.Sprintf("pid %d", h.PID)
	}
	return fmt.Sprintf("%s (pid %d)", h.Command, h.PID)
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
