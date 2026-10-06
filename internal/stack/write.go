package stack

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
)

// ErrNotCreated is returned by Remove for a path the manifest doesn't list.
var ErrNotCreated = errors.New("not created by pic-sure")

// WriteFile atomically replaces the file rel (a slash-separated path inside
// the stack) with data and mode perm, whatever the umask: it writes a temp
// file in the same directory, fsyncs it, renames it over rel and fsyncs the
// directory. Readers see the old content or the new, never a mix. If rel is
// a symlink, the link is replaced, never written through. The parent
// directory must exist (see MkdirAll). A file that didn't exist before is
// recorded in the manifest; overwriting an operator's file doesn't make it
// the CLI's.
func (s *Stack) WriteFile(rel string, data []byte, perm fs.FileMode) error {
	p, err := local(rel)
	if err != nil {
		return err
	}
	created, err := s.writeAtomic(p, data, perm)
	if err != nil || !created {
		return err
	}
	return s.record(Entry{Path: filepath.ToSlash(p), Type: EntryFile})
}

// MkdirAll creates the directory rel and any missing parents inside the
// stack with mode perm, whatever the umask, and records each one it created.
// Directories that already exist are left as they are.
func (s *Stack) MkdirAll(rel string, perm fs.FileMode) (err error) {
	p, err := local(rel)
	if err != nil {
		return err
	}
	var created []Entry
	defer func() {
		// Record what was created even if a later component failed.
		if len(created) > 0 {
			if rerr := s.record(created...); err == nil {
				err = rerr
			}
		}
	}()
	prefix := ""
	for _, part := range strings.Split(p, string(filepath.Separator)) {
		prefix = filepath.Join(prefix, part)
		err = s.root.Mkdir(prefix, perm)
		if errors.Is(err, fs.ErrExist) {
			var fi fs.FileInfo
			if fi, err = s.root.Stat(prefix); err != nil {
				return err
			}
			if !fi.IsDir() {
				return fmt.Errorf("%s exists and is not a directory", s.Path(prefix))
			}
			continue
		}
		if err != nil {
			return err
		}
		created = append(created, Entry{Path: filepath.ToSlash(prefix), Type: EntryDir})
		if err = s.root.Chmod(prefix, perm); err != nil {
			return err
		}
	}
	return nil
}

// CreateFile creates the new file rel with mode perm, whatever the umask,
// records it, and returns it open for writing. It is for files written as a
// stream, such as run logs; it fails if rel exists.
func (s *Stack) CreateFile(rel string, perm fs.FileMode) (*os.File, error) {
	p, err := local(rel)
	if err != nil {
		return nil, err
	}
	f, err := s.root.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := s.record(Entry{Path: filepath.ToSlash(p), Type: EntryFile}); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// Remove deletes rel, a file or empty directory the CLI created, and drops
// it from the manifest. A recorded path that is already gone is just
// dropped. A path the manifest doesn't list is refused with ErrNotCreated.
// A symlink is removed, not its target.
func (s *Stack) Remove(rel string) error {
	p, err := local(rel)
	if err != nil {
		return err
	}
	m, err := s.Manifest()
	if err != nil {
		return err
	}
	if !m.Has(filepath.ToSlash(p)) {
		if _, err := s.root.Lstat(p); err != nil {
			return err
		}
		return fmt.Errorf("removing %s: %w", s.Path(p), ErrNotCreated)
	}
	if err := s.root.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return s.forget(filepath.ToSlash(p))
}

// ReadFile reads rel, confined to the stack like the writes.
func (s *Stack) ReadFile(rel string) ([]byte, error) {
	p, err := local(rel)
	if err != nil {
		return nil, err
	}
	return s.root.ReadFile(p)
}

// FS returns the stack directory as a read-only fs.FS, confined like the
// writes.
func (s *Stack) FS() fs.FS { return s.root.FS() }

// tempSeq makes temp file names unique within the process; the pid makes
// them unique across processes, and O_EXCL catches what's left (a crashed
// run's file with a reused pid).
var tempSeq atomic.Uint64

// writeAtomic is WriteFile without the manifest. It reports whether p
// didn't exist before.
func (s *Stack) writeAtomic(p string, data []byte, perm fs.FileMode) (created bool, err error) {
	_, err = s.root.Lstat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		created = true
	case err != nil:
		return false, err
	}

	dir, base := filepath.Split(p)
	var tmp string
	var f *os.File
	for range 10 {
		tmp = filepath.Join(dir, "."+base+".tmp-"+strconv.Itoa(os.Getpid())+"-"+strconv.FormatUint(tempSeq.Add(1), 10))
		// 0600 until the Chmod below, so a secret is never readable by
		// others, even briefly.
		f, err = s.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if !errors.Is(err, fs.ErrExist) {
			break
		}
	}
	if err != nil {
		return false, err
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = s.root.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return false, err
	}
	if err = f.Chmod(perm); err != nil {
		return false, err
	}
	if err = f.Sync(); err != nil {
		return false, err
	}
	if err = f.Close(); err != nil {
		return false, err
	}
	if err = s.root.Rename(tmp, p); err != nil {
		return false, err
	}
	return created, s.syncDir(dir)
}

// syncDir fsyncs dir so a rename in it survives a crash.
func (s *Stack) syncDir(dir string) error {
	if dir == "" {
		dir = "."
	}
	d, err := s.root.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// local checks that rel is a relative path inside the stack and returns it
// cleaned, in OS form. os.Root enforces confinement on every call anyway;
// this rejects bad paths early, and gives the manifest one spelling per path.
func local(rel string) (string, error) {
	p := filepath.FromSlash(rel)
	if !filepath.IsLocal(p) {
		return "", fmt.Errorf("%q is not a path inside the stack directory", rel)
	}
	return filepath.Clean(p), nil
}
