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

// RemoveReport is what RemoveCreated did. Paths are slash-separated and
// relative to the stack directory.
type RemoveReport struct {
	// Removed lists the recorded paths removed, deepest first, then the
	// temp files of interrupted writes.
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
// The caller holds the stack lock, whose file is removed with the rest; a
// command waiting for the lock then fails (see Lock). The Stack is unusable
// afterwards except for Close.
func (s *Stack) RemoveCreated() (RemoveReport, error) {
	var r RemoveReport
	m, err := s.Manifest()
	if err != nil {
		return r, err
	}
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
		case e.Path == ManifestFile || e.Path == CLIDir || e.Path == ".":
			continue
		case e.Path == ConfigFile && len(failed) > 0:
			continue
		}
		p := filepath.FromSlash(e.Path)
		if s.noSymlinks(filepath.Dir(p)) != nil {
			r.Kept = append(r.Kept, e.Path)
			continue
		}
		_, lerr := s.root.Lstat(p)
		existed := lerr == nil
		switch err := s.Remove(e.Path); {
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

	cliDirGone, err := s.removeCLIDir(&r)
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
func (s *Stack) removeTemps(m Manifest, r *RemoveReport) error {
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
			if err := s.root.Remove(filepath.FromSlash(rel)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			r.Removed = append(r.Removed, rel)
		}
	}
	return nil
}

// removeCLIDir removes the manifest and .pic-sure/ when nothing else is
// left in .pic-sure/, and reports whether it did.
func (s *Stack) removeCLIDir(r *RemoveReport) (bool, error) {
	if s.noSymlinks(CLIDir) != nil {
		r.Kept = append(r.Kept, CLIDir)
		return false, nil
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
	if err := s.root.Remove(filepath.FromSlash(ManifestFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if err := s.root.Remove(CLIDir); err != nil {
		return false, err
	}
	r.Removed = append(r.Removed, ManifestFile, CLIDir)
	return true, nil
}

// removeStackDir removes the stack directory if it is empty. rmdir never
// removes a file or a non-empty directory, and s.Dir has its symlinks
// resolved.
func (s *Stack) removeStackDir() error {
	return syscall.Rmdir(s.Dir)
}

// topLevel lists the stack directory's entries, directories with a
// trailing slash.
func (s *Stack) topLevel() ([]string, error) {
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
