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
	"syscall"
)

// ErrNotCreated is returned by Remove for a path the manifest doesn't list.
var ErrNotCreated = errors.New("not created by pic-sure")

// WriteFile atomically replaces the file rel (a slash-separated path inside
// the stack) with data and mode perm, whatever the umask: it writes a temp
// file in the same directory, fsyncs it, renames it over rel and fsyncs the
// directory. Readers see the old content or the new, never a mix. If rel is
// a symlink, the link is replaced, never written through; a symlinked
// directory on the way to rel is refused. The parent directory must exist
// (see MkdirAll). A file that didn't exist before is recorded in the
// manifest; overwriting an operator's file doesn't make it the CLI's.
func (s *Stack) WriteFile(rel string, data []byte, perm fs.FileMode) error {
	p, err := local(rel)
	if err != nil {
		return err
	}
	if err := s.noSymlinks(filepath.Dir(p)); err != nil {
		return err
	}
	_, err = s.root.Lstat(p)
	switch {
	case err == nil:
		return s.writeAtomic(p, data, perm)
	case errors.Is(err, fs.ErrNotExist):
		return s.recordThenCreate(Entry{Path: filepath.ToSlash(p), Type: EntryFile}, func() error {
			return s.writeAtomic(p, data, perm)
		})
	default:
		return err
	}
}

// MkdirAll creates the directory rel and any missing parents inside the
// stack with mode perm, whatever the umask, and records each one it creates.
// Directories that already exist are left as they are; a symlink on the way
// is refused.
func (s *Stack) MkdirAll(rel string, perm fs.FileMode) error {
	p, err := local(rel)
	if err != nil {
		return err
	}
	prefix := ""
	for _, part := range strings.Split(p, string(filepath.Separator)) {
		prefix = filepath.Join(prefix, part)
		fi, err := s.root.Lstat(prefix)
		switch {
		case err == nil && fi.Mode()&fs.ModeSymlink != 0:
			return s.symlinkError(prefix)
		case err == nil && !fi.IsDir():
			return fmt.Errorf("%s exists and is not a directory", s.Path(prefix))
		case err == nil:
			continue
		case !errors.Is(err, fs.ErrNotExist):
			return err
		}
		dir := prefix
		err = s.recordThenCreate(Entry{Path: filepath.ToSlash(dir), Type: EntryDir}, func() error {
			err := s.root.Mkdir(dir, perm)
			if errors.Is(err, fs.ErrExist) {
				// Another pic-sure run, such as a read-only command
				// starting its log, made it since the Lstat and recorded
				// it too. Failing would drop that run's manifest entry.
				if fi, lerr := s.root.Lstat(dir); lerr == nil && fi.IsDir() {
					return nil
				}
			}
			if err != nil {
				return err
			}
			return s.root.Chmod(dir, perm)
		})
		if err != nil {
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
	if err := s.noSymlinks(filepath.Dir(p)); err != nil {
		return nil, err
	}
	if _, err := s.root.Lstat(p); err == nil {
		return nil, fmt.Errorf("creating %s: %w", s.Path(p), fs.ErrExist)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	var f *os.File
	err = s.recordThenCreate(Entry{Path: filepath.ToSlash(p), Type: EntryFile}, func() error {
		var err error
		if f, err = s.root.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm); err != nil {
			return err
		}
		if err = f.Chmod(perm); err != nil {
			_ = f.Close()
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Remove deletes rel, a file or empty directory the CLI created, and drops
// it from the manifest. A recorded path that is already gone is just
// dropped. A path the manifest doesn't list is refused with ErrNotCreated,
// and so is one that is no longer the kind of thing the CLI created there,
// or that lies under a symlinked directory. A symlink is removed, not its
// target.
func (s *Stack) Remove(rel string) error {
	p, err := local(rel)
	if err != nil {
		return err
	}
	path := filepath.ToSlash(p)
	m, err := s.Manifest()
	if err != nil {
		return err
	}
	e, ok := m.entry(path)
	if !ok {
		if _, err := s.root.Lstat(p); err != nil {
			return err
		}
		return fmt.Errorf("removing %s: %w", s.Path(p), ErrNotCreated)
	}
	if err := s.noSymlinks(filepath.Dir(p)); err != nil {
		return err
	}
	fi, err := s.root.Lstat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR):
		return s.forget(path)
	case err != nil:
		return err
	case fi.IsDir() != (e.Type == EntryDir):
		return fmt.Errorf("removing %s: it is no longer the %s pic-sure created: %w", s.Path(p), e.Type, ErrNotCreated)
	}
	if err := s.root.Remove(p); err != nil {
		return err
	}
	return s.forget(path)
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

// tempMarker separates the target's name from the pid and sequence number
// in a temp file's name: .<name>.tmp-<pid>-<seq>.
const tempMarker = ".tmp-"

// IsTempName reports whether name, a base name, is one of WriteFile's temp
// files. One outlives the write only if pic-sure died mid-write, and it is
// never in the manifest, so destroy should remove those it finds beside
// recorded paths.
func IsTempName(name string) bool {
	i := strings.LastIndex(name, tempMarker)
	if i < 2 || name[0] != '.' {
		return false
	}
	pid, seq, ok := strings.Cut(name[i+len(tempMarker):], "-")
	return ok && isDigits(pid) && isDigits(seq)
}

// tempName returns a new temp file name for the target base.
func tempName(base string) string {
	return "." + base + tempMarker + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatUint(tempSeq.Add(1), 10)
}

func isDigits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

// tempSeq makes temp file names unique within the process; the pid makes
// them unique across processes, and O_EXCL catches what's left (a crashed
// run's file with a reused pid).
var tempSeq atomic.Uint64

// writeAtomic is WriteFile without the checks and the manifest.
func (s *Stack) writeAtomic(p string, data []byte, perm fs.FileMode) (err error) {
	dir, base := filepath.Split(p)
	var tmp string
	var f *os.File
	for range 10 {
		tmp = filepath.Join(dir, tempName(base))
		// 0600 until the Chmod below, so a secret is never readable by
		// others, even briefly.
		f, err = s.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if !errors.Is(err, fs.ErrExist) {
			break
		}
	}
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = s.root.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Chmod(perm); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = s.root.Rename(tmp, p); err != nil {
		return err
	}
	return s.syncDir(dir)
}

// recordThenCreate records e, then calls create to make it. Recording first
// means neither a failed manifest update nor a crash can leave a path the CLI
// made that the manifest doesn't list, which destroy would then leave
// behind. If create fails, the entry is dropped unless the path exists
// because create made it: an ErrExist failure means someone else did.
func (s *Stack) recordThenCreate(e Entry, create func() error) error {
	if err := s.record(e); err != nil {
		return err
	}
	err := create()
	if err != nil {
		_, lerr := s.root.Lstat(filepath.FromSlash(e.Path))
		if errors.Is(err, fs.ErrExist) || errors.Is(lerr, fs.ErrNotExist) {
			_ = s.forget(e.Path)
		}
	}
	return err
}

// noSymlinks fails if p or any directory on the way to it is a symlink.
// os.Root follows symlinks that stay inside the stack, so without this a
// write or removal under a directory an operator replaced with a symlink
// would reach the operator's files (§9.8). It stops at the first missing
// component.
func (s *Stack) noSymlinks(p string) error {
	if p == "." {
		return nil
	}
	prefix := ""
	for _, part := range strings.Split(p, string(filepath.Separator)) {
		prefix = filepath.Join(prefix, part)
		fi, err := s.root.Lstat(prefix)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return s.symlinkError(prefix)
		}
	}
	return nil
}

func (s *Stack) symlinkError(p string) error {
	return fmt.Errorf("%s is a symlink; pic-sure doesn't write or remove through symlinks in the stack", s.Path(p))
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
