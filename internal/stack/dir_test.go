package stack

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
)

// tempDir returns a fresh directory with symlinks resolved, so it compares
// equal to Stack.Dir (t.TempDir is under a symlink on macOS).
func tempDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// makeStack turns dir into a stack the way init leaves one: pic-sure.yaml
// plus .pic-sure/.
func makeStack(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, CLIDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ConfigFile), []byte("schema: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// openStack opens the stack in dir and closes it when the test ends.
func openStack(t *testing.T, dir string) *Stack {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func wantNotFound(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrNotFound) || exitcode.FromError(err) != exitcode.CodePrecondition {
		t.Errorf("err = %v (exit %d), want ErrNotFound with exit 3", err, exitcode.FromError(err))
	}
}

func TestFind(t *testing.T) {
	base := tempDir(t)
	outer := filepath.Join(base, "outer")
	inner := filepath.Join(outer, "certs", "inner")
	deep := filepath.Join(inner, "a", "b")
	other := filepath.Join(base, "other")
	halfOuter := filepath.Join(base, "half")
	halfInner := filepath.Join(halfOuter, "sub")
	mkdirs(t, deep, other, halfInner)
	makeStack(t, outer)
	makeStack(t, inner)
	makeStack(t, other)
	makeStack(t, halfOuter)
	// A directory with only pic-sure.yaml is not a stack.
	if err := os.WriteFile(filepath.Join(halfInner, ConfigFile), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name, flag, cwd, want string
	}{
		{name: "cwd is the stack", cwd: outer, want: outer},
		{name: "walks up to the nearest stack", cwd: deep, want: inner},
		{name: "walks up out of a non-stack subdirectory", cwd: filepath.Join(outer, "certs"), want: outer},
		{name: "needs .pic-sure/ as well as pic-sure.yaml", cwd: halfInner, want: halfOuter},
		{name: "--stack wins over cwd", flag: other, cwd: deep, want: other},
		{name: "--stack is relative to cwd", flag: "../other", cwd: outer, want: other},
		{name: "--stack doesn't walk up", flag: inner, cwd: base, want: inner},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Find(tt.flag, tt.cwd)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("Find(%q, %q) = %q, want %q", tt.flag, tt.cwd, got, tt.want)
			}
		})
	}

	t.Run("not found", func(t *testing.T) {
		_, err := Find("", base)
		wantNotFound(t, err)
	})
	t.Run("--stack names a non-stack", func(t *testing.T) {
		_, err := Find(filepath.Join(outer, "certs"), base)
		wantNotFound(t, err)
	})
	t.Run("--stack names a directory with only pic-sure.yaml", func(t *testing.T) {
		_, err := Find(halfInner, base)
		wantNotFound(t, err)
		if err == nil || !strings.Contains(err.Error(), ".pic-sure/") {
			t.Errorf("err = %v, want it to name the missing .pic-sure/", err)
		}
	})
	t.Run("--stack names a missing directory", func(t *testing.T) {
		_, err := Find(filepath.Join(base, "nowhere"), base)
		wantNotFound(t, err)
	})
	t.Run("--stack names a file", func(t *testing.T) {
		_, err := Find(filepath.Join(outer, ConfigFile), base)
		wantNotFound(t, err)
	})
}

func TestInitDir(t *testing.T) {
	base := tempDir(t)
	mkdirs(t, filepath.Join(base, "real"))
	if err := os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name, arg, flag, want string
	}{
		{name: "neither: cwd", want: base},
		{name: "DIR", arg: "new", want: filepath.Join(base, "new")},
		{name: "--stack", flag: "new", want: filepath.Join(base, "new")},
		{name: "DIR and the same --stack", arg: "new", flag: filepath.Join(base, "new"), want: filepath.Join(base, "new")},
		{name: "same directory spelled differently", arg: "./new/../new/", flag: "new", want: filepath.Join(base, "new")},
		{name: "same directory through a symlinked parent", arg: "link/x", flag: "real/x", want: filepath.Join(base, "link", "x")},
		{name: "absolute DIR", arg: "/srv/stack", want: "/srv/stack"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := InitDir(tt.arg, tt.flag, base)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("InitDir(%q, %q) = %q, want %q", tt.arg, tt.flag, got, tt.want)
			}
		})
	}

	t.Run("DIR and a different --stack is a usage error", func(t *testing.T) {
		_, err := InitDir("a", "b", base)
		if exitcode.FromError(err) != exitcode.CodeUsage {
			t.Errorf("err = %v (exit %d), want exit 2", err, exitcode.FromError(err))
		}
	})
}

func TestOpen(t *testing.T) {
	dir := tempDir(t)
	makeStack(t, dir)
	link := filepath.Join(tempDir(t), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}

	s := openStack(t, link)
	if s.Dir != dir {
		t.Errorf("Dir = %q, want %q (symlinks resolved)", s.Dir, dir)
	}
	if got, want := s.Path(".pic-sure/render/compose.yaml"), filepath.Join(dir, ".pic-sure", "render", "compose.yaml"); got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}

	_, err := Open(t.TempDir())
	wantNotFound(t, err)
}

func TestCreate(t *testing.T) {
	t.Run("new directory", func(t *testing.T) {
		dir := filepath.Join(tempDir(t), "parent", "stack")
		s, err := Create(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Close() }()
		if s.Dir != dir {
			t.Errorf("Dir = %q, want %q", s.Dir, dir)
		}
		wantMode(t, dir, os.ModeDir|0o755)
		wantMode(t, filepath.Join(dir, CLIDir), os.ModeDir|0o755)
		// The stack dir is the CLI's to remove; the parent it had to make
		// isn't, because it's outside the stack.
		wantEntries(t, s, []Entry{
			{Path: CLIDir, Type: EntryDir},
			{Path: ".", Type: EntryDir},
			{Path: ManifestFile, Type: EntryFile},
		})
	})

	t.Run("existing directory", func(t *testing.T) {
		dir := tempDir(t)
		s, err := Create(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Close() }()
		wantEntries(t, s, []Entry{
			{Path: CLIDir, Type: EntryDir},
			{Path: ManifestFile, Type: EntryFile},
		})
	})

	t.Run("again on a created stack", func(t *testing.T) {
		dir := filepath.Join(tempDir(t), "stack")
		s, err := Create(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.WriteFile(ConfigFile, []byte("schema: 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		before, _ := s.Manifest()
		_ = s.Close()

		s, err = Create(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Close() }()
		wantEntries(t, s, before.Entries)
	})

	t.Run("a file in the way", func(t *testing.T) {
		file := filepath.Join(tempDir(t), "file")
		if err := os.WriteFile(file, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Create(file); err == nil {
			t.Error("Create on a file succeeded")
		}
	})
}

func TestLabels(t *testing.T) {
	dir := tempDir(t)
	makeStack(t, dir)
	s := openStack(t, dir)
	got := s.Labels("mystack")
	if len(got) != 2 || got["org.hms-dbmi.picsure.stack"] != "mystack" || got["org.hms-dbmi.picsure.stack-dir"] != dir {
		t.Errorf("Labels = %v", got)
	}
}

func TestFindNotOwned(t *testing.T) {
	base := tempDir(t)
	planted := filepath.Join(base, "planted")
	deep := filepath.Join(planted, "a")
	mine := filepath.Join(base, "mine")
	mkdirs(t, deep, mine)
	makeStack(t, planted)
	makeStack(t, mine)

	// owned maps a path to a uid other than ours; the rest are ours.
	me := os.Geteuid()
	owned := map[string]int{}
	oldOwner, oldEUID, oldSudo := fileOwner, euid, sudoUID
	t.Cleanup(func() { fileOwner, euid, sudoUID = oldOwner, oldEUID, oldSudo })
	fileOwner = func(p string, _ fs.FileInfo) int {
		if uid, ok := owned[p]; ok {
			return uid
		}
		return me
	}

	for _, p := range []string{planted, filepath.Join(planted, ConfigFile)} {
		t.Run("someone else owns "+filepath.Base(p), func(t *testing.T) {
			clear(owned)
			owned[p] = me + 1
			for _, cwd := range []string{planted, deep} {
				_, err := Find("", cwd)
				if !errors.Is(err, ErrNotOwned) || exitcode.FromError(err) != exitcode.CodePrecondition {
					t.Fatalf("Find from %s: err = %v, want ErrNotOwned with exit 3", cwd, err)
				}
				for _, want := range []string{p, "uid " + strconv.Itoa(me+1), "--stack " + planted} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("err = %q, want it to contain %q", err, want)
					}
				}
			}
			if got, err := Find(planted, deep); err != nil || got != planted {
				t.Errorf("Find with --stack = %q, %v; want %q: --stack is never restricted", got, err, planted)
			}
			if got, err := Find("", mine); err != nil || got != mine {
				t.Errorf("Find in my own stack = %q, %v", got, err)
			}
		})
	}

	t.Run("root under sudo trusts the invoking user", func(t *testing.T) {
		clear(owned)
		owned[planted] = 1234
		owned[filepath.Join(planted, ConfigFile)] = 1234
		owned[mine] = 0
		owned[filepath.Join(mine, ConfigFile)] = 0
		euid = func() int { return 0 }
		sudoUID = func() string { return "1234" }
		if _, err := Find("", planted); err != nil {
			t.Errorf("sudo user's stack: %v", err)
		}
		if _, err := Find("", mine); err != nil {
			t.Errorf("root's own stack: %v", err)
		}
		sudoUID = func() string { return "" }
		if _, err := Find("", planted); !errors.Is(err, ErrNotOwned) {
			t.Errorf("root without SUDO_UID: err = %v, want ErrNotOwned", err)
		}
	})
}
