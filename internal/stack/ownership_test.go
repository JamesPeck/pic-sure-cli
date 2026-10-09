package stack

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

const testID = "0123456789abcdef0123456789abcdef"

// stateDir makes a directory holding .pic-sure/state.json with stack ID id.
func stateDir(t *testing.T, id string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".pic-sure"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".pic-sure", "state.json"), []byte(`{"stack_id": "`+id+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestOwner(t *testing.T) {
	here := "/stacks/b"
	original := stateDir(t, testID)
	unrelated := stateDir(t, "ffffffffffffffffffffffffffffffff")
	unreadable := stateDir(t, testID)
	if err := os.Chmod(filepath.Join(unreadable, ".pic-sure"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(unreadable, ".pic-sure"), 0o755) })
	notDir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notDir, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	labels := func(id, dir string) map[string]string {
		l := map[string]string{LabelStack: "demo"}
		if id != "" {
			l[LabelStackID] = id
		}
		if dir != "" {
			l[LabelStackDir] = dir
		}
		return l
	}
	for _, tc := range []struct {
		name   string
		labels map[string]string
		want   Claim
	}{
		{"this stack's", labels(testID, here), Own},
		{"another ID", labels("ffffffffffffffffffffffffffffffff", here), Foreign},
		{"copy: the original still has the ID", labels(testID, original), Foreign},
		{"move: the old directory is gone", labels(testID, "/stacks/gone"), Moved},
		{"move: the old directory is a file now", labels(testID, notDir), Moved},
		{"move: a new stack took the old directory", labels(testID, unrelated), Moved},
		{"the old directory can't be read", labels(testID, unreadable), Foreign},
		{"the ID with no directory", labels(testID, ""), Foreign},
		{"no ID, this directory", labels("", here), Own},
		{"no ID, another directory", labels("", "/stacks/gone"), Foreign},
		{"no stack labels", map[string]string{}, Foreign},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Owner(testID, here, tc.labels); got != tc.want {
				t.Errorf("Owner = %d, want %d", got, tc.want)
			}
		})
	}
	// A stack with no ID yet owns only what its directory label names.
	if got := Owner("", here, labels(testID, here)); got != Foreign {
		t.Errorf("Owner with no ID, of a resource with one = %d, want Foreign", got)
	}
}

func TestEnsureID(t *testing.T) {
	s := newStack(t)
	if id, err := s.EnsureID(bytes.NewReader(make([]byte, 16))); err != nil || id != "" {
		t.Fatalf("EnsureID without state.json = %q, %v; want no ID", id, err)
	}
	old := &State{CLIVersion: "dev"}
	if err := s.SaveState(old); err != nil {
		t.Fatal(err)
	}
	id, err := s.EnsureID(bytes.NewReader(bytes.Repeat([]byte{0xab}, 16)))
	if want := "abababababababababababababababab"; err != nil || id != want {
		t.Fatalf("EnsureID = %q, %v; want %q", id, err, want)
	}
	if again, _ := s.EnsureID(bytes.NewReader(nil)); again != id {
		t.Errorf("EnsureID again = %q, want the same %q", again, id)
	}
	// A State loaded before the ID was given keeps it when saved.
	if err := s.SaveState(old); err != nil {
		t.Fatal(err)
	}
	if s.ID() != id || s.Labels("demo")[LabelStackID] != id {
		t.Errorf("after saving a State without the ID: ID = %q, labels %v; want %q", s.ID(), s.Labels("demo"), id)
	}
}

func TestOwnerThroughASymlinkAtTheOldPath(t *testing.T) {
	// mv a c && ln -s c a: the labels still say a.
	c, err := filepath.EvalSymlinks(stateDir(t, testID))
	if err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(t.TempDir(), "a")
	if err := os.Symlink(c, a); err != nil {
		t.Fatal(err)
	}
	for _, labels := range []map[string]string{
		{LabelStackDir: a, LabelStackID: testID},
		{LabelStackDir: a},
	} {
		if got := Owner(testID, c, labels); got != Own {
			t.Errorf("Owner(%v) = %d, want Own", labels, got)
		}
	}
}
