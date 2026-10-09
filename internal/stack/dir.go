package stack

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
)

// The files that make a directory a stack (§6.1), relative to it.
const (
	ConfigFile = "pic-sure.yaml"
	CLIDir     = ".pic-sure"
)

// Files the CLI keeps in CLIDir.
const (
	ManifestFile = CLIDir + "/manifest.json"
	StateFile    = CLIDir + "/state.json"
	LockFile     = CLIDir + "/lock"
)

// ErrNotFound is wrapped by the error Find and Open return when there is no
// stack where they looked.
var ErrNotFound = errors.New("no pic-sure stack found")

// Stack is an open stack directory. Every write goes through an os.Root
// rooted at Dir, so nothing the CLI writes can land outside it, even through
// a symlink. Close it when done.
type Stack struct {
	// Dir is the stack directory: absolute, with symlinks resolved.
	Dir string

	root *os.Root
	// mu serializes this process's manifest updates; the flock in
	// updateManifest serializes them across processes.
	mu sync.Mutex
}

// Find returns the directory of the stack a command acts on (§6.1, D14):
// dir when it is set (--stack DIR, relative to cwd), otherwise the nearest
// directory at or above cwd that holds both pic-sure.yaml and .pic-sure/.
// Finding no stack is an exit-3 error wrapping ErrNotFound, and finding one
// without --stack that the user doesn't own is an exit-3 error wrapping
// ErrNotOwned.
func Find(dir, cwd string) (string, error) {
	if dir != "" {
		dir = absFrom(cwd, dir)
		if err := checkStack(dir); err != nil {
			return "", err
		}
		return dir, nil
	}
	for d := filepath.Clean(cwd); ; d = filepath.Dir(d) {
		ok, _, err := isStack(d)
		if err != nil {
			return "", err
		}
		if ok {
			if err := checkOwner(d); err != nil {
				return "", err
			}
			return d, nil
		}
		if filepath.Dir(d) == d {
			return "", exitcode.Precondition("%w in %s or any directory above it; pass --stack DIR, or create one with pic-sure init", ErrNotFound, cwd)
		}
	}
}

// InitDir returns the directory `init [DIR]` creates its stack in (D14): the
// DIR argument when given, else --stack, else cwd. Relative paths are taken
// relative to cwd. A DIR and a --stack that name different directories are
// an exit-2 usage error. When neither is given, a cwd holding stack files
// that another user owns is an exit-3 error wrapping ErrNotOwned, as in
// Find (checkInitOwner).
func InitDir(arg, stackFlag, cwd string) (string, error) {
	switch {
	case arg != "" && stackFlag != "":
		a, s := absFrom(cwd, arg), absFrom(cwd, stackFlag)
		if canonical(a) != canonical(s) {
			return "", exitcode.Usage("init %s and --stack %s name different directories; give just one", arg, stackFlag)
		}
		return a, nil
	case arg != "":
		return absFrom(cwd, arg), nil
	case stackFlag != "":
		return absFrom(cwd, stackFlag), nil
	default:
		dir := filepath.Clean(cwd)
		if err := checkInitOwner(dir); err != nil {
			return "", err
		}
		return dir, nil
	}
}

// Open opens the existing stack in dir. A dir that isn't a stack is an
// exit-3 error wrapping ErrNotFound.
func Open(dir string) (*Stack, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := checkStack(dir); err != nil {
		return nil, err
	}
	return openRoot(dir)
}

// Create makes dir a stack directory for init: it creates dir if it doesn't
// exist (and any missing parents), then .pic-sure/, and records what it
// created in the manifest. It doesn't write pic-sure.yaml. Running it again
// on a partly or fully created stack is safe.
func Create(dir string) (*Stack, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	createdDir, err := mkdirStackDir(dir)
	if err != nil {
		return nil, err
	}
	s, err := openRoot(dir)
	if err != nil {
		return nil, err
	}
	if err := s.create(createdDir); err != nil {
		_ = s.Close()
		if createdDir {
			// Undo, so a retry still finds dir missing and records it.
			// os.Remove removes only empty directories.
			_ = os.Remove(filepath.Join(dir, CLIDir))
			_ = os.Remove(dir)
		}
		return nil, err
	}
	return s, nil
}

func (s *Stack) create(createdDir bool) error {
	switch err := s.root.Mkdir(CLIDir, 0o755); {
	case err == nil:
		if err := s.root.Chmod(CLIDir, 0o755); err != nil {
			return err
		}
	case !errors.Is(err, fs.ErrExist):
		return err
	}
	// .pic-sure/ is the CLI's by definition (§6.1), so it is recorded even
	// when an interrupted earlier run created it.
	entries := []Entry{{Path: CLIDir, Type: EntryDir}}
	if createdDir {
		entries = append(entries, Entry{Path: ".", Type: EntryDir})
	}
	return s.record(entries...)
}

// mkdirStackDir creates dir with mode 0755, after any missing parents, and
// reports whether it created dir itself. Only dir is the CLI's to remove
// later; parents it had to create are not recorded.
func mkdirStackDir(dir string) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return false, err
	}
	err := os.Mkdir(dir, 0o755)
	switch {
	case err == nil:
		return true, os.Chmod(dir, 0o755)
	case errors.Is(err, fs.ErrExist):
		fi, err := os.Stat(dir)
		if err != nil {
			return false, err
		}
		if !fi.IsDir() {
			return false, fmt.Errorf("%s exists and is not a directory", dir)
		}
		return false, nil
	default:
		return false, err
	}
}

func openRoot(dir string) (*Stack, error) {
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &Stack{Dir: dir, root: root}, nil
}

// Close releases the stack directory. It doesn't release a Lock.
func (s *Stack) Close() error { return s.root.Close() }

// Path returns the absolute host path of rel, a slash-separated path inside
// the stack, for places that need one, such as bind mounts and compose -f.
func (s *Stack) Path(rel string) string { return filepath.Join(s.Dir, filepath.FromSlash(rel)) }

// checkStack returns nil if dir is a stack, and an exit-3 error saying what
// is missing otherwise.
func checkStack(dir string) error {
	ok, missing, err := isStack(dir)
	if err != nil {
		return err
	}
	if !ok {
		return exitcode.Precondition("%w in %s: it has no %s; create one with pic-sure init", ErrNotFound, dir, missing)
	}
	return nil
}

// isStack reports whether dir holds pic-sure.yaml and .pic-sure/, and if
// not, which of them is missing.
func isStack(dir string) (ok bool, missing string, err error) {
	for _, m := range []struct {
		name string
		dir  bool
	}{{ConfigFile, false}, {CLIDir, true}} {
		fi, err := os.Stat(filepath.Join(dir, m.name))
		// ENOTDIR: dir is a file.
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || err == nil && fi.IsDir() != m.dir {
			name := m.name
			if m.dir {
				name += "/"
			}
			return false, name, nil
		}
		if err != nil {
			return false, "", err
		}
	}
	return true, "", nil
}

func absFrom(cwd, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(cwd, p)
}

// canonical resolves the symlinks in the longest existing prefix of the
// absolute path p, so two spellings of one directory compare equal even
// before it exists (/tmp/x and /private/tmp/x on macOS).
func canonical(p string) string {
	rest := ""
	for d := p; ; d = filepath.Dir(d) {
		if r, err := filepath.EvalSymlinks(d); err == nil {
			return filepath.Join(r, rest)
		}
		if filepath.Dir(d) == d {
			return p
		}
		rest = filepath.Join(filepath.Base(d), rest)
	}
}
