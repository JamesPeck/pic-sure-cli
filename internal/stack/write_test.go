package stack

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func wantMode(t *testing.T, path string, want fs.FileMode) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode() & (fs.ModeType | fs.ModePerm); got != want {
		t.Errorf("%s: mode %v, want %v", path, got, want)
	}
}

func wantContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("%s = %q, want %q", path, got, want)
	}
}

// wantNoTempFiles fails if a temp file from an atomic write is left in dir.
func wantNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && strings.Contains(d.Name(), ".tmp-") {
			t.Errorf("temp file left behind: %s", p)
		}
		return nil
	})
}

// newStack returns a created stack in a fresh directory.
func newStack(t *testing.T) *Stack {
	t.Helper()
	s, err := Create(tempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestWriteFile(t *testing.T) {
	s := newStack(t)
	if err := s.WriteFile("pic-sure.yaml", []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantContent(t, s.Path("pic-sure.yaml"), "one")
	wantMode(t, s.Path("pic-sure.yaml"), 0o644)

	// Overwriting replaces the content and sets the new mode.
	if err := s.WriteFile("pic-sure.yaml", []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	wantContent(t, s.Path("pic-sure.yaml"), "two")
	wantMode(t, s.Path("pic-sure.yaml"), 0o600)
	wantNoTempFiles(t, s.Dir)
}

func TestWriteFileModeIgnoresUmask(t *testing.T) {
	s := newStack(t)
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)

	if err := s.WriteFile("open.txt", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	wantMode(t, s.Path("open.txt"), 0o644)
	if err := s.MkdirAll("a/b", 0o755); err != nil {
		t.Fatal(err)
	}
	wantMode(t, s.Path("a"), fs.ModeDir|0o755)
	wantMode(t, s.Path("a/b"), fs.ModeDir|0o755)
	f, err := s.CreateFile("stream.log", 0o640)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	wantMode(t, s.Path("stream.log"), 0o640)
}

func TestWriteFileNeedsItsParent(t *testing.T) {
	s := newStack(t)
	if err := s.WriteFile("missing/file", nil, 0o644); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want ErrNotExist", err)
	}
}

func TestWriteFileFailureKeepsTheOldFile(t *testing.T) {
	s := newStack(t)
	if err := s.MkdirAll("dir", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteFile("dir/keep", []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Renaming a file over a non-empty directory fails, after the temp file
	// is written.
	if err := s.WriteFile("dir", []byte("new"), 0o644); err == nil {
		t.Fatal("writing over a directory succeeded")
	}
	wantContent(t, s.Path("dir/keep"), "old")
	wantNoTempFiles(t, s.Dir)
}

// Readers racing an atomic writer see one complete version or the other.
func TestWriteFileIsAtomicForReaders(t *testing.T) {
	s := newStack(t)
	versions := [][]byte{bytes.Repeat([]byte("a"), 1<<20), bytes.Repeat([]byte("b"), 1<<20)}
	if err := s.WriteFile("big", versions[0], 0o644); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			got, err := os.ReadFile(s.Path("big"))
			if err != nil {
				t.Error(err)
				return
			}
			if !bytes.Equal(got, versions[0]) && !bytes.Equal(got, versions[1]) {
				t.Errorf("read a partial file: %d bytes", len(got))
				return
			}
		}
	}()
	for i := range 50 {
		if err := s.WriteFile("big", versions[i%2], 0o644); err != nil {
			t.Error(err)
			break
		}
	}
	close(done)
	wg.Wait()
}

func TestWritesStayInsideTheStack(t *testing.T) {
	s := newStack(t)
	outside := tempDir(t)
	if err := os.WriteFile(filepath.Join(outside, "target"), []byte("operator"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, s.Path("out")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "target"), s.Path("link")); err != nil {
		t.Fatal(err)
	}

	for _, rel := range []string{"../escape", "/etc/escape", "", "out/new", "out/target"} {
		if err := s.WriteFile(rel, []byte("x"), 0o644); err == nil {
			t.Errorf("WriteFile(%q) succeeded", rel)
		}
		if err := s.MkdirAll(rel+"/d", 0o755); err == nil {
			t.Errorf("MkdirAll(%q) succeeded", rel+"/d")
		}
		if f, err := s.CreateFile(rel, 0o644); err == nil {
			_ = f.Close()
			t.Errorf("CreateFile(%q) succeeded", rel)
		}
	}
	if _, err := s.ReadFile("out/target"); err == nil {
		t.Error("ReadFile followed a symlink out of the stack")
	}

	// Writing to a symlink replaces the link; its target is untouched.
	if err := s.WriteFile("link", []byte("cli"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantMode(t, s.Path("link"), 0o644)
	wantContent(t, s.Path("link"), "cli")
	wantContent(t, filepath.Join(outside, "target"), "operator")
	if entries, _ := os.ReadDir(outside); len(entries) != 1 {
		t.Errorf("files appeared outside the stack: %v", entries)
	}
}

func TestMkdirAll(t *testing.T) {
	s := newStack(t)
	if err := os.Mkdir(s.Path("certs"), 0o755); err != nil { // the operator's
		t.Fatal(err)
	}
	if err := s.MkdirAll("certs/trust/extra", 0o700); err != nil {
		t.Fatal(err)
	}
	wantMode(t, s.Path("certs/trust"), fs.ModeDir|0o700)
	m, _ := s.Manifest()
	if m.Has("certs") || !m.Has("certs/trust") || !m.Has("certs/trust/extra") {
		t.Errorf("manifest = %+v, want only the directories MkdirAll created", m.Entries)
	}

	if err := s.WriteFile("file", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.MkdirAll("file/sub", 0o755); err == nil {
		t.Error("MkdirAll through a file succeeded")
	}
}
