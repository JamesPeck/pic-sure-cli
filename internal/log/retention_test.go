package log

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// makeLogs creates run logs in dir's Dir, oldest first, of the given sizes,
// and returns their names. Sizes are made with Truncate, so large ones are
// sparse.
func makeLogs(t *testing.T, dir string, sizes ...int64) []string {
	t.Helper()
	logs := filepath.Join(dir, Dir)
	if err := os.MkdirAll(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	var names []string
	for i, size := range sizes {
		name := fmt.Sprintf("cli-20260101T0000%02d.000Z.log", i)
		f, err := os.Create(filepath.Join(logs, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(size); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
		names = append(names, name)
	}
	return names
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, Dir))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestPruneKeepsTheNewestThatFit(t *testing.T) {
	tests := []struct {
		name     string
		sizes    []int64 // oldest first; the last is the current log
		maxFiles int
		maxBytes int64
		keep     []int // indexes into sizes
	}{
		{name: "under both limits", sizes: []int64{1, 1, 1}, maxFiles: 5, maxBytes: 100, keep: []int{0, 1, 2}},
		{name: "file count", sizes: []int64{1, 1, 1, 1, 1}, maxFiles: 3, maxBytes: 100, keep: []int{2, 3, 4}},
		{name: "byte total", sizes: []int64{10, 10, 10, 10}, maxFiles: 10, maxBytes: 25, keep: []int{2, 3}},
		{name: "older small log goes after a big one", sizes: []int64{1, 50, 1, 1}, maxFiles: 10, maxBytes: 10, keep: []int{2, 3}},
		{name: "current counts toward bytes", sizes: []int64{5, 5, 20}, maxFiles: 10, maxBytes: 25, keep: []int{1, 2}},
		{name: "current kept when too big", sizes: []int64{1, 100}, maxFiles: 10, maxBytes: 10, keep: []int{1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			names := makeLogs(t, dir, tt.sizes...)
			current := names[len(names)-1]
			if err := prune(dirStore(dir), current, tt.maxFiles, tt.maxBytes); err != nil {
				t.Fatal(err)
			}
			var want []string
			for _, i := range tt.keep {
				want = append(want, names[i])
			}
			if got := listDir(t, dir); !slices.Equal(got, want) {
				t.Errorf("kept %q, want %q", got, want)
			}
		})
	}
}

func TestPruneKeepsCurrentEvenIfOlderByName(t *testing.T) {
	dir := t.TempDir()
	names := makeLogs(t, dir, 1, 1, 1)
	if err := prune(dirStore(dir), names[0], 2, 100); err != nil {
		t.Fatal(err)
	}
	if got, want := listDir(t, dir), []string{names[0], names[2]}; !slices.Equal(got, want) {
		t.Errorf("kept %q, want %q", got, want)
	}
}

func TestPruneLeavesOtherFilesAlone(t *testing.T) {
	dir := t.TempDir()
	names := makeLogs(t, dir, 1, 1, 1)
	logs := filepath.Join(dir, Dir)
	for _, name := range []string{"notes.txt", "cli-x.txt", "debug.log"} {
		if err := os.WriteFile(filepath.Join(logs, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(logs, "cli-dir.log"), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.log")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(logs, "cli-00000000T000000.000Z.log")); err != nil {
		t.Fatal(err)
	}

	if err := prune(dirStore(dir), names[2], 1, 100); err != nil {
		t.Fatal(err)
	}
	want := []string{"cli-00000000T000000.000Z.log", "cli-dir.log", "cli-x.txt", "debug.log", names[2], "notes.txt"}
	slices.Sort(want)
	if got := listDir(t, dir); !slices.Equal(got, want) {
		t.Errorf("left %q, want %q", got, want)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("symlink target: %v", err)
	}
}

// TestOpenFilePrunesToTheSpecLimits checks the limits OpenFile uses: 50
// files, or 50 MiB.
func TestOpenFilePrunesToTheSpecLimits(t *testing.T) {
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)

	dir := t.TempDir()
	sizes := make([]int64, 60)
	for i := range sizes {
		sizes[i] = 1
	}
	names := makeLogs(t, dir, sizes...)
	run := New(Options{File: true, Redactor: &Redactor{}})
	path, err := run.OpenFile(dirStore(dir), now)
	if err != nil {
		t.Fatal(err)
	}
	_ = run.Close()
	if got, want := listDir(t, dir), append(names[11:], filepath.Base(path)); !slices.Equal(got, want) {
		t.Errorf("by count: kept %d logs from %q, want the newest 49 and the new one", len(got), got[0])
	}

	dir = t.TempDir()
	names = makeLogs(t, dir, 20<<20, 20<<20, 20<<20)
	run = New(Options{File: true, Redactor: &Redactor{}})
	path, err = run.OpenFile(dirStore(dir), now)
	if err != nil {
		t.Fatal(err)
	}
	_ = run.Close()
	if got, want := listDir(t, dir), []string{names[1], names[2], filepath.Base(path)}; !slices.Equal(got, want) {
		t.Errorf("by size: kept %q, want %q", got, want)
	}
}

func TestPruneOrdersACollisionAfterItsTwin(t *testing.T) {
	dir := t.TempDir()
	makeLogs(t, dir)
	for _, name := range []string{"cli-20260101T000000.000Z.log", "cli-20260101T000000.000Z-1.log", "cli-20260101T000001.000Z.log"} {
		if err := os.WriteFile(filepath.Join(dir, Dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := prune(dirStore(dir), "cli-20260101T000001.000Z.log", 2, 100); err != nil {
		t.Fatal(err)
	}
	if got, want := listDir(t, dir), []string{"cli-20260101T000000.000Z-1.log", "cli-20260101T000001.000Z.log"}; !slices.Equal(got, want) {
		t.Errorf("left %q, want %q", got, want)
	}
}
