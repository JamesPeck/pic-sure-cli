package stack

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
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

	r, err := s.RemoveCreated(false)
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

	r, err := s.RemoveCreated(false)
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

	r, err := s.RemoveCreated(false)
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

	r, err := s.RemoveCreated(false)
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

	r, err := s.RemoveCreated(false)
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
	// The lock goes anyway, so nothing waiting for it carries on here.
	if want := []Entry{{Path: CLIDir, Type: EntryDir}, {Path: ManifestFile, Type: EntryFile}}; !slices.Equal(m.Entries, want) {
		t.Errorf("manifest = %v, want %v", m.Entries, want)
	}
}

func TestLockFailsWhenTheStackIsDestroyedWhileWaiting(t *testing.T) {
	for _, keepCLIDir := range []bool{false, true} {
		s := newStack(t)
		populate(t, s)
		if keepCLIDir {
			// Something not pic-sure's keeps .pic-sure/ and its manifest.
			writeOperatorFile(t, s, ".pic-sure/mine")
		}
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
		if _, err := s.RemoveCreated(false); err != nil {
			t.Fatal(err)
		}
		_ = held.Unlock()
		select {
		case err := <-got:
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("keepCLIDir=%v: waiter: err = %v, want ErrNotFound", keepCLIDir, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("keepCLIDir=%v: the waiter never returned", keepCLIDir)
		}
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

	if _, err := s.RemoveCreated(false); err == nil || !strings.Contains(err.Error(), "data/readme.txt") {
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

// These complete reports are pinned against ba5b055 before adding dry mode.
func TestRemoveCreatedBaselineReports(t *testing.T) {
	for _, tt := range []struct {
		name        string
		createdRoot bool
		setup       func(*testing.T, *Stack)
		want        string
		wantError   string
	}{
		{
			name: "existing root",
			want: `{"removed":[".pic-sure/render/files/httpd.conf",".pic-sure/render/files",".pic-sure/render/compose.yaml","data/readme.txt",".pic-sure/state.json",".pic-sure/render",".pic-sure/logs","data","pic-sure.yaml",".pic-sure/lock",".pic-sure/manifest.json",".pic-sure"],"dir_removed":false}`,
		},
		{
			name:        "created root and interrupted writes",
			createdRoot: true,
			setup: func(t *testing.T, s *Stack) {
				for _, p := range []string{".pic-sure.yaml.tmp-123-4", ".pic-sure/.manifest.json.tmp-123-4", ".pic-sure/render/files/.httpd.conf.tmp-123-4", "data/.readme.txt.tmp-123-4"} {
					writeOperatorFile(t, s, p)
				}
			},
			want: `{"removed":[".pic-sure.yaml.tmp-123-4",".pic-sure/.manifest.json.tmp-123-4",".pic-sure/render/files/.httpd.conf.tmp-123-4","data/.readme.txt.tmp-123-4",".pic-sure/render/files/httpd.conf",".pic-sure/render/files",".pic-sure/render/compose.yaml","data/readme.txt",".pic-sure/state.json",".pic-sure/render",".pic-sure/logs","data","pic-sure.yaml",".pic-sure/lock",".pic-sure/manifest.json",".pic-sure","."],"dir_removed":true}`,
		},
		{
			name:        "operator files keep nested directories and root",
			createdRoot: true,
			setup: func(t *testing.T, s *Stack) {
				writeOperatorFile(t, s, "data/nested/mine")
				writeOperatorFile(t, s, ".notes.tmp-1-2")
			},
			want: `{"removed":[".pic-sure/render/files/httpd.conf",".pic-sure/render/files",".pic-sure/render/compose.yaml","data/readme.txt",".pic-sure/state.json",".pic-sure/render",".pic-sure/logs","pic-sure.yaml",".pic-sure/lock",".pic-sure/manifest.json",".pic-sure"],"kept":["data","."],"remaining":[".notes.tmp-1-2","data/"],"dir_removed":false}`,
		},
		{
			name:        "operator file keeps CLI directory and manifest",
			createdRoot: true,
			setup:       func(t *testing.T, s *Stack) { writeOperatorFile(t, s, ".pic-sure/mine") },
			want:        `{"removed":[".pic-sure/render/files/httpd.conf",".pic-sure/render/files",".pic-sure/render/compose.yaml","data/readme.txt",".pic-sure/state.json",".pic-sure/render",".pic-sure/logs","data","pic-sure.yaml",".pic-sure/lock"],"kept":[".pic-sure"],"remaining":[".pic-sure/"],"dir_removed":false}`,
		},
		{
			name: "missing recorded file",
			setup: func(t *testing.T, s *Stack) {
				if err := os.Remove(s.Path("data/readme.txt")); err != nil {
					t.Fatal(err)
				}
			},
			want: `{"removed":[".pic-sure/render/files/httpd.conf",".pic-sure/render/files",".pic-sure/render/compose.yaml",".pic-sure/state.json",".pic-sure/render",".pic-sure/logs","data","pic-sure.yaml",".pic-sure/lock",".pic-sure/manifest.json",".pic-sure"],"dir_removed":false}`,
		},
		{
			name: "operator replaces directory with symlink and config with directory",
			setup: func(t *testing.T, s *Stack) {
				if err := os.RemoveAll(s.Path("data")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(".pic-sure/render", s.Path("data")); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(s.Path(ConfigFile)); err != nil {
					t.Fatal(err)
				}
				writeOperatorFile(t, s, ConfigFile+"/inside")
			},
			want: `{"removed":[".pic-sure/render/files/httpd.conf",".pic-sure/render/files",".pic-sure/render/compose.yaml",".pic-sure/state.json",".pic-sure/render",".pic-sure/logs",".pic-sure/lock",".pic-sure/manifest.json",".pic-sure"],"kept":["data/readme.txt","data","pic-sure.yaml"],"remaining":["data","pic-sure.yaml/"],"dir_removed":false}`,
		},
		{
			name: "operator replaces lock with directory",
			setup: func(t *testing.T, s *Stack) {
				if err := os.Remove(s.Path(LockFile)); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(s.Path(LockFile), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: `{"removed":[".pic-sure/render/files/httpd.conf",".pic-sure/render/files",".pic-sure/render/compose.yaml","data/readme.txt",".pic-sure/state.json",".pic-sure/render",".pic-sure/logs","data","pic-sure.yaml"],"kept":[".pic-sure/lock",".pic-sure"],"remaining":[".pic-sure/"],"dir_removed":false}`,
		},
		{
			name:      "file removal failure preserves config lock and manifest",
			setup:     func(t *testing.T, s *Stack) { removeTestReadOnly(t, s, "data") },
			want:      `{"removed":[".pic-sure/render/files/httpd.conf",".pic-sure/render/files",".pic-sure/render/compose.yaml",".pic-sure/state.json",".pic-sure/render",".pic-sure/logs"],"kept":["data"],"dir_removed":false}`,
			wantError: "data/readme.txt:",
		},
		{
			name: "temp failure stops temp loop but continues recorded removals",
			setup: func(t *testing.T, s *Stack) {
				writeOperatorFile(t, s, "data/.readme.txt.tmp-123-4")
				removeTestReadOnly(t, s, "data")
			},
			want:      `{"removed":[".pic-sure/render/files/httpd.conf",".pic-sure/render/files",".pic-sure/render/compose.yaml",".pic-sure/state.json",".pic-sure/render",".pic-sure/logs"],"kept":["data"],"dir_removed":false}`,
			wantError: "temp files:",
		},
		{
			name:      "manifest update failure omits already unlinked files from report",
			setup:     func(t *testing.T, s *Stack) { removeTestReadOnly(t, s, CLIDir) },
			want:      `{"removed":null,"dir_removed":false}`,
			wantError: "couldn't remove .pic-sure/render/files/httpd.conf:",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := tempDir(t)
			if tt.createdRoot {
				dir = filepath.Join(dir, "stack")
			}
			s, err := Create(dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			populate(t, s)
			if tt.setup != nil {
				tt.setup(t, s)
			}
			var preview RemoveReport
			if tt.wantError == "" {
				before := snapshotRemovalTree(t, s.Dir)
				preview, err = s.RemoveCreated(true)
				if err != nil {
					t.Fatal(err)
				}
				wantRemovalTreeUnchanged(t, before, snapshotRemovalTree(t, s.Dir))
			}
			r, err := s.RemoveCreated(false)
			if tt.wantError == "" && !reflect.DeepEqual(preview, r) {
				t.Errorf("dry report = %+v, real report = %+v", preview, r)
			}
			if tt.wantError == "" && err != nil || tt.wantError != "" && (err == nil || !strings.Contains(err.Error(), tt.wantError)) {
				t.Fatalf("error = %v, want %q", err, tt.wantError)
			}
			got, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Errorf("report = %s\nwant   = %s", got, tt.want)
			}
		})
	}
}

func removeTestReadOnly(t *testing.T, s *Stack, rel string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	if err := os.Chmod(s.Path(rel), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(s.Path(rel), 0o755) })
}

type removalTreeEntry struct {
	info    fs.FileInfo
	content string
}

// Snapshot identities and timestamps as well as bytes: a preview must not
// rewrite the manifest with identical contents or recreate a removed file.
func snapshotRemovalTree(t *testing.T, dir string) map[string]removalTreeEntry {
	t.Helper()
	out := make(map[string]removalTreeEntry)
	err := filepath.WalkDir(dir, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := de.Info()
		if err != nil {
			return err
		}
		entry := removalTreeEntry{info: info}
		switch {
		case info.Mode().IsRegular():
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			entry.content = string(data)
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			entry.content = target
		}
		out[p] = entry
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func wantRemovalTreeUnchanged(t *testing.T, before, after map[string]removalTreeEntry) {
	t.Helper()
	if len(before) != len(after) {
		t.Errorf("preview changed entry count: before %d, after %d", len(before), len(after))
	}
	for p, a := range before {
		b, ok := after[p]
		if !ok || !os.SameFile(a.info, b.info) || a.info.Mode() != b.info.Mode() || a.info.Size() != b.info.Size() || !a.info.ModTime().Equal(b.info.ModTime()) || a.content != b.content {
			t.Errorf("preview changed %s", p)
		}
	}
}

func TestRemoveCreatedDryRunSymlinksAndOperatorTemps(t *testing.T) {
	for _, keepTemps := range []bool{false, true} {
		name := "file symlinks removed without following targets"
		if keepTemps {
			name = "operator temps keep their parent directory"
		}
		t.Run(name, func(t *testing.T) {
			s := newStack(t)
			populate(t, s)
			outside := tempDir(t)
			target := filepath.Join(outside, "operator")
			if err := os.WriteFile(target, []byte("outside"), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, p := range []string{ConfigFile, LockFile, "data/readme.txt"} {
				if err := os.Remove(s.Path(p)); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, s.Path(p)); err != nil {
					t.Fatal(err)
				}
			}
			if keepTemps {
				if err := os.Symlink(target, s.Path("data/.readme.txt.tmp-1-2")); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(s.Path("data/.readme.txt.tmp-1-3"), 0o755); err != nil {
					t.Fatal(err)
				}
				writeOperatorFile(t, s, "data/.unknown.tmp-1-4")
			}
			before := snapshotRemovalTree(t, s.Dir)
			outsideBefore := snapshotRemovalTree(t, outside)
			preview, err := s.RemoveCreated(true)
			if err != nil {
				t.Fatal(err)
			}
			wantRemovalTreeUnchanged(t, before, snapshotRemovalTree(t, s.Dir))
			wantRemovalTreeUnchanged(t, outsideBefore, snapshotRemovalTree(t, outside))
			again, err := s.RemoveCreated(true)
			if err != nil || !reflect.DeepEqual(preview, again) {
				t.Fatalf("second preview = %+v, %v; first = %+v", again, err, preview)
			}
			wantRemovalTreeUnchanged(t, before, snapshotRemovalTree(t, s.Dir))
			real, err := s.RemoveCreated(false)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(preview, real) {
				t.Errorf("dry report = %+v, real report = %+v", preview, real)
			}
			for _, p := range []string{ConfigFile, LockFile, "data/readme.txt"} {
				if !slices.Contains(real.Removed, p) {
					t.Errorf("missing removal of %s: %+v", p, real)
				}
			}
			if keepTemps {
				if !slices.Equal(real.Kept, []string{"data"}) || !slices.Equal(real.Remaining, []string{"data/"}) {
					t.Errorf("operator temps not kept: %+v", real)
				}
				for _, p := range []string{"data/.readme.txt.tmp-1-2", "data/.readme.txt.tmp-1-3", "data/.unknown.tmp-1-4"} {
					if _, err := os.Lstat(s.Path(p)); err != nil {
						t.Errorf("operator temp %s: %v", p, err)
					}
				}
			}
			wantRemovalTreeUnchanged(t, outsideBefore, snapshotRemovalTree(t, outside))
		})
	}
}
