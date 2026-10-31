package stack

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// RemoveReport is what RemoveCreated did, or would do in dry mode. Paths
// are slash-separated and relative to the stack directory.
type RemoveReport struct {
	// Removed lists the temp files of interrupted writes, then the
	// recorded paths removed, deepest first.
	Removed []string `json:"removed"`
	// Kept lists recorded paths left in place: a directory that still holds
	// files pic-sure didn't create, or a path that is no longer what
	// pic-sure created there (replaced by an operator, or under a symlink).
	Kept []string `json:"kept,omitempty"`
	// Remaining lists what is left at the top of the stack directory, with
	// a trailing slash on directories. Empty when DirRemoved.
	Remaining []string `json:"remaining,omitempty"`
	// DirRemoved says the stack directory itself was removed: init created
	// it and nothing else was left in it.
	DirRemoved bool `json:"dir_removed"`
}

// RemoveCreated removes everything the manifest lists, deepest first, for
// destroy (§9.8). It never follows a symlink, keeps a recorded directory
// that still holds anything else, and keeps a path that is no longer the
// kind pic-sure created. Temp files that a crashed write left beside a
// recorded path go too. The manifest goes last, with .pic-sure/, and only
// when .pic-sure/ would then be empty; until then a re-run can carry on
// where a failed one stopped. The stack directory goes only if init created
// it and it is empty. Any other failure doesn't stop the rest, and is
// returned at the end, with the manifest kept.
//
// For a real removal, the caller holds the stack lock, whose file is removed
// with the rest; a command waiting for the lock then fails (see Lock). The
// Stack is then unusable except for Close. With dryRun, no files or manifest
// entries are changed: removals are simulated in order, including directory
// emptiness. Errors that occur only when writing cannot be predicted.
func (s *Stack) RemoveCreated(dryRun bool) (RemoveReport, error) {
	return (&removal{Stack: s, dryRun: dryRun, removed: make(map[string]bool)}).run()
}

type removal struct {
	*Stack
	dryRun   bool
	removed  map[string]bool
	manifest Manifest
}

func (s *removal) run() (RemoveReport, error) {
	var r RemoveReport
	m, err := s.Manifest()
	if err != nil {
		return r, err
	}
	s.manifest = Manifest{Version: m.Version, Entries: slices.Clone(m.Entries)}
	var failed []string
	fail := func(p string, err error) { failed = append(failed, fmt.Sprintf("%s: %v", p, err)) }

	if err := s.removeTemps(m, &r); err != nil {
		fail("temp files", err)
	}
	// pic-sure.yaml goes after the rest, and not after a failure, so a
	// failed run leaves a stack that destroy can open again.
	entries := slices.Clone(m.Entries)
	slices.SortFunc(entries, func(a, b Entry) int {
		return cmp.Or(
			cmp.Compare(isConfig(a), isConfig(b)),
			cmp.Compare(depth(b.Path), depth(a.Path)),
			strings.Compare(b.Path, a.Path))
	})
	for _, e := range entries {
		switch {
		case e.Path == ManifestFile || e.Path == LockFile || e.Path == CLIDir || e.Path == ".":
			continue
		case e.Path == ConfigFile && len(failed) > 0:
			continue
		}
		p := filepath.FromSlash(e.Path)
		if s.noSymlinks(filepath.Dir(p)) != nil {
			r.Kept = append(r.Kept, e.Path)
			continue
		}
		_, lerr := s.lstat(p)
		existed := lerr == nil
		switch err := s.removeRecorded(e.Path); {
		case err == nil:
			if existed {
				r.Removed = append(r.Removed, e.Path)
			}
		case errors.Is(err, ErrNotCreated) || notEmpty(err):
			r.Kept = append(r.Kept, e.Path)
		default:
			fail(e.Path, err)
		}
	}
	if len(failed) > 0 {
		return r, fmt.Errorf("couldn't remove %s", strings.Join(failed, "; "))
	}

	cliDirGone, err := s.removeCLIDir(m, &r)
	if err != nil {
		return r, err
	}
	if cliDirGone && m.Has(".") {
		switch err := s.removeStackDir(); {
		case err == nil:
			r.DirRemoved = true
			r.Removed = append(r.Removed, ".")
			return r, nil
		case notEmpty(err):
			r.Kept = append(r.Kept, ".")
		default:
			return r, err
		}
	}
	r.Remaining, err = s.topLevel()
	return r, err
}

// removeTemps removes WriteFile's temp files whose target is a recorded
// path, from every directory holding a recorded path.
func (s *removal) removeTemps(m Manifest, r *RemoveReport) error {
	dirs := map[string]bool{}
	for _, e := range m.Entries {
		if e.Path != "." {
			dirs[path.Dir(e.Path)] = true
		}
	}
	for _, dir := range slices.Sorted(maps.Keys(dirs)) {
		d := filepath.FromSlash(dir)
		if s.noSymlinks(d) != nil {
			continue
		}
		names, err := s.readDir(d)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			continue
		}
		if err != nil {
			return err
		}
		for _, de := range names {
			target, ok := tempTarget(de.Name())
			if !ok || !de.Type().IsRegular() || !m.Has(path.Join(dir, target)) {
				continue
			}
			rel := path.Join(dir, de.Name())
			if err := s.remove(filepath.FromSlash(rel)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			r.Removed = append(r.Removed, rel)
		}
	}
	return nil
}

// removeCLIDir removes the lock, then the manifest and .pic-sure/ when
// nothing else is left in .pic-sure/, and reports whether .pic-sure/ went.
// The lock goes this late so no other command can take a new one while
// destroy runs, and goes even when .pic-sure/ stays, so a command waiting
// for it fails (see Lock) instead of running on a stack without its config.
func (s *removal) removeCLIDir(m Manifest, r *RemoveReport) (bool, error) {
	if s.noSymlinks(CLIDir) != nil {
		r.Kept = append(r.Kept, CLIDir)
		return false, nil
	}
	if m.Has(LockFile) {
		_, lerr := s.lstat(filepath.FromSlash(LockFile))
		switch err := s.removeRecorded(LockFile); {
		case err == nil:
			if lerr == nil {
				r.Removed = append(r.Removed, LockFile)
			}
		case errors.Is(err, ErrNotCreated):
			r.Kept = append(r.Kept, LockFile)
		default:
			return false, err
		}
	}
	names, err := s.readDir(CLIDir)
	if err != nil {
		return false, err
	}
	for _, de := range names {
		if de.Name() != path.Base(ManifestFile) {
			r.Kept = append(r.Kept, CLIDir)
			return false, nil
		}
	}
	if err := s.remove(filepath.FromSlash(ManifestFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if err := s.remove(CLIDir); err != nil {
		return false, err
	}
	r.Removed = append(r.Removed, ManifestFile, CLIDir)
	return true, nil
}

// removeStackDir removes the stack directory if it is empty. rmdir never
// removes a file or a non-empty directory, and s.Dir has its symlinks
// resolved.
func (s *removal) removeStackDir() error {
	if s.dryRun {
		return s.remove(".")
	}
	return syscall.Rmdir(s.Dir)
}

// topLevel lists the stack directory's entries, directories with a
// trailing slash.
func (s *removal) topLevel() ([]string, error) {
	names, err := s.readDir(".")
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(names))
	for _, de := range names {
		n := de.Name()
		if de.IsDir() {
			n += "/"
		}
		out = append(out, n)
	}
	return out, nil
}

// removeRecorded applies Remove's ownership checks in both modes. Real
// removals keep its per-path manifest updates: a failed update still leaves
// that removal out of the report.
func (s *removal) removeRecorded(rel string) error {
	p, err := local(rel)
	if err != nil {
		return err
	}
	path := filepath.ToSlash(p)
	m := s.manifest
	if !s.dryRun {
		m, err = s.Manifest()
		if err != nil {
			return err
		}
	}
	e, ok := m.entry(path)
	if !ok {
		if _, err := s.lstat(p); err != nil {
			return err
		}
		return fmt.Errorf("removing %s: %w", s.Path(p), ErrNotCreated)
	}
	if err := s.noSymlinks(filepath.Dir(p)); err != nil {
		return err
	}
	fi, err := s.lstat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR):
		return s.forget(path)
	case err != nil:
		return err
	case fi.IsDir() != (e.Type == EntryDir):
		return fmt.Errorf("removing %s: it is no longer the %s pic-sure created: %w", s.Path(p), e.Type, ErrNotCreated)
	}
	if err := s.remove(p); err != nil {
		return err
	}
	return s.forget(path)
}

func (s *removal) forget(path string) error {
	if !s.dryRun {
		return s.Stack.forget(path)
	}
	if err := s.noSymlinks(CLIDir); err != nil {
		return err
	}
	s.manifest.Entries = slices.DeleteFunc(s.manifest.Entries, func(e Entry) bool { return e.Path == path })
	return nil
}

func (s *removal) lstat(p string) (fs.FileInfo, error) {
	if s.dryRun {
		for parent := p; ; parent = filepath.Dir(parent) {
			if s.removed[parent] {
				return nil, &fs.PathError{Op: "lstat", Path: p, Err: fs.ErrNotExist}
			}
			if parent == filepath.Dir(parent) || parent == "." {
				break
			}
		}
	}
	return s.root.Lstat(p)
}

func (s *removal) remove(p string) error {
	if !s.dryRun {
		return s.root.Remove(p)
	}
	fi, err := s.lstat(p)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		names, err := s.readDir(p)
		if err != nil {
			return err
		}
		if len(names) > 0 {
			return &fs.PathError{Op: "remove", Path: p, Err: syscall.ENOTEMPTY}
		}
	}
	s.removed[p] = true
	return nil
}

func (s *removal) readDir(dir string) ([]fs.DirEntry, error) {
	names, err := s.Stack.readDir(dir)
	if s.dryRun {
		names = slices.DeleteFunc(names, func(de fs.DirEntry) bool {
			return s.removed[filepath.Join(dir, de.Name())]
		})
	}
	return names, err
}

func (s *Stack) readDir(dir string) ([]fs.DirEntry, error) {
	f, err := s.root.Open(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	names, err := f.ReadDir(-1)
	slices.SortFunc(names, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return names, err
}

// tempTarget returns the base name a WriteFile temp file was written for.
func tempTarget(name string) (string, bool) {
	if !IsTempName(name) {
		return "", false
	}
	return name[1:strings.LastIndex(name, tempMarker)], true
}

// depth is a manifest path's depth: 0 for ".", 1 for a top-level path.
func depth(p string) int {
	if p == "." {
		return 0
	}
	return strings.Count(p, "/") + 1
}

func isConfig(e Entry) int {
	if e.Path == ConfigFile {
		return 1
	}
	return 0
}

func notEmpty(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST)
}
