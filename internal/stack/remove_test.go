package stack

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// populate writes what init and up leave in a stack: the config, the lock,
// files under .pic-sure/, and a CLI-made directory outside it.
func populate(t *testing.T, s *Stack) {
	t.Helper()
	for _, dir := range []string{".pic-sure/render/files", ".pic-sure/logs", "data"} {
		if err := s.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{ConfigFile, ".pic-sure/state.json", ".pic-sure/render/compose.yaml", ".pic-sure/render/files/httpd.conf", "data/readme.txt"} {
		if err := s.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	l, err := s.Lock(context.Background(), LockOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = l.Unlock()
}

func writeOperatorFile(t *testing.T, s *Stack, rel string) {
	t.Helper()
	p := s.Path(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("operator"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveCreatedKeepsOperatorFiles(t *testing.T) {
	s := newStack(t)
	// There before init, so the stack dir isn't the CLI's.
	writeOperatorFile(t, s, "certs/server.crt")
	writeOperatorFile(t, s, "notes.txt")
	populate(t, s)
	// Dropped into a directory the CLI made.
	writeOperatorFile(t, s, "data/mine.csv")

	r, err := s.RemoveCreated()
	if err != nil {
		t.Fatal(err)
	}
	if r.DirRemoved {
		t.Error("removed a stack dir init didn't create")
	}
	if !slices.Equal(r.Kept, []string{"data"}) {
		t.Errorf("Kept = %v, want [data]", r.Kept)
	}
	if want := []string{"certs/", "data/", "notes.txt"}; !slices.Equal(r.Remaining, want) {
		t.Errorf("Remaining = %v, want %v", r.Remaining, want)
	}
	for _, p := range []string{"certs/server.crt", "notes.txt", "data/mine.csv"} {
		wantContent(t, s.Path(p), "operator")
	}
	for _, p := range []string{CLIDir, ConfigFile, "data/readme.txt"} {
		if _, err := os.Lstat(s.Path(p)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: err = %v, want it gone", p, err)
		}
	}
	// Deepest first, the config after the rest, then the lock, the
	// manifest and .pic-sure/.
	if n := len(r.Removed); n < 4 || !slices.Equal(r.Removed[n-4:], []string{ConfigFile, LockFile, ManifestFile, CLIDir}) {
		t.Errorf("Removed = %v", r.Removed)
	}
}

func TestRemoveCreatedRemovesTheDirInitCreated(t *testing.T) {
	dir := filepath.Join(tempDir(t), "stack")
	s, err := Create(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	populate(t, s)
	// A crashed write's temp file beside the config.
	writeOperatorFile(t, s, ".pic-sure.yaml.tmp-123-4")

	r, err := s.RemoveCreated()
	if err != nil {
		t.Fatal(err)
	}
	if !r.DirRemoved || len(r.Remaining) > 0 || len(r.Kept) > 0 {
		t.Errorf("report = %+v, want the dir removed", r)
	}
	if !slices.Contains(r.Removed, ".pic-sure.yaml.tmp-123-4") {
		t.Errorf("Removed = %v, want the temp file", r.Removed)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stack dir: err = %v, want it gone", err)
	}
}

func TestRemoveCreatedKeepsTheDirInitCreatedWhenNotEmpty(t *testing.T) {
	dir := filepath.Join(tempDir(t), "stack")
	s, err := Create(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	populate(t, s)
	writeOperatorFile(t, s, "overrides/10-local.yaml")
	// Temp-named, but not beside a recorded path: the operator's.
	writeOperatorFile(t, s, ".notes.tmp-1-2")

	r, err := s.RemoveCreated()
	if err != nil {
		t.Fatal(err)
	}
	if r.DirRemoved || !slices.Equal(r.Kept, []string{"."}) {
		t.Errorf("report = %+v, want the dir kept", r)
	}
	if want := []string{".notes.tmp-1-2", "overrides/"}; !slices.Equal(r.Remaining, want) {
		t.Errorf("Remaining = %v, want %v", r.Remaining, want)
	}
}

func TestRemoveCreatedNeverFollowsSymlinks(t *testing.T) {
	s := newStack(t)
	populate(t, s)
	outside := tempDir(t)
	if err := os.WriteFile(filepath.Join(outside, "readme.txt"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	// An operator replaced the CLI's data/ with a symlink, and its config
	// with a directory.
	if err := os.RemoveAll(s.Path("data")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, s.Path("data")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(s.Path(ConfigFile)); err != nil {
		t.Fatal(err)
	}
	writeOperatorFile(t, s, ConfigFile+"/inside")

	r, err := s.RemoveCreated()
	if err != nil {
		t.Fatal(err)
	}
	wantContent(t, filepath.Join(outside, "readme.txt"), "outside")
	wantContent(t, s.Path(ConfigFile+"/inside"), "operator")
	if want := []string{"data/readme.txt", "data", ConfigFile}; !slices.Equal(r.Kept, want) {
		t.Errorf("Kept = %v, want %v", r.Kept, want)
	}
}

func TestRemoveCreatedKeepsTheManifestWhileCLIDirHoldsOtherFiles(t *testing.T) {
	s := newStack(t)
	populate(t, s)
	writeOperatorFile(t, s, ".pic-sure/mine")

	r, err := s.RemoveCreated()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.Kept, []string{CLIDir}) {
		t.Errorf("Kept = %v, want [.pic-sure]", r.Kept)
	}
	m, err := s.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if want := []Entry{{Path: CLIDir, Type: EntryDir}, {Path: ManifestFile, Type: EntryFile}, {Path: LockFile, Type: EntryFile}}; !slices.Equal(m.Entries, want) {
		t.Errorf("manifest = %v, want %v", m.Entries, want)
	}
}

func TestLockFailsWhenTheStackIsDestroyedWhileWaiting(t *testing.T) {
	s := newStack(t)
	populate(t, s)
	held, err := s.Lock(context.Background(), LockOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	waiting := make(chan struct{})
	go func() {
		_, err := s.Lock(context.Background(), LockOptions{Wait: true, OnWait: func(string) { close(waiting) }})
		got <- err
	}()
	<-waiting // it has the old lock file open
	if _, err := s.RemoveCreated(); err != nil {
		t.Fatal(err)
	}
	_ = held.Unlock()
	select {
	case err := <-got:
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("waiter: err = %v, want ErrNotFound", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter never returned")
	}
}

func TestRemoveCreatedKeepsTheStackOpenableAfterAFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	s := newStack(t)
	populate(t, s)
	// data/readme.txt can't be unlinked.
	if err := os.Chmod(s.Path("data"), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(s.Path("data"), 0o755) })

	if _, err := s.RemoveCreated(); err == nil || !strings.Contains(err.Error(), "data/readme.txt") {
		t.Fatalf("err = %v, want a failure naming data/readme.txt", err)
	}
	for _, p := range []string{ConfigFile, ManifestFile, LockFile} {
		if _, err := os.Lstat(s.Path(p)); err != nil {
			t.Errorf("%s: %v, want it kept", p, err)
		}
	}
	if _, err := Open(s.Dir); err != nil {
		t.Errorf("the stack no longer opens: %v", err)
	}
	if m, _ := s.Manifest(); !m.Has("data/readme.txt") || !m.Has(ConfigFile) {
		t.Errorf("manifest lost what is left: %v", m.Entries)
	}
}
