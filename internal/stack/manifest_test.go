package stack

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"sync"
	"testing"
)

func wantEntries(t *testing.T, s *Stack, want []Entry) {
	t.Helper()
	m, err := s.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(m.Entries, want) {
		t.Errorf("manifest entries:\n got  %+v\n want %+v", m.Entries, want)
	}
}

func TestManifestRecordsWhatTheCLICreates(t *testing.T) {
	s := newStack(t)
	// An operator's file, there before the CLI wrote to it.
	if err := os.WriteFile(s.Path("operator.txt"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := s.WriteFile("pic-sure.yaml", []byte("schema: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteFile("pic-sure.yaml", []byte("schema: 1\nname: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteFile("operator.txt", []byte("overwritten"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.MkdirAll(".pic-sure/render/files", 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := s.CreateFile(".pic-sure/render/files/run.log", 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	want := []Entry{
		{Path: ".pic-sure", Type: EntryDir},
		{Path: ".pic-sure/manifest.json", Type: EntryFile},
		{Path: "pic-sure.yaml", Type: EntryFile},
		{Path: ".pic-sure/render", Type: EntryDir},
		{Path: ".pic-sure/render/files", Type: EntryDir},
		{Path: ".pic-sure/render/files/run.log", Type: EntryFile},
	}
	wantEntries(t, s, want)

	// The manifest is on disk, not just in memory.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	wantEntries(t, openStack(t, s.Dir), want)
	wantContent(t, s.Path(ManifestFile), `{
  "version": 1,
  "entries": [
    {
      "path": ".pic-sure",
      "type": "dir"
    },
    {
      "path": ".pic-sure/manifest.json",
      "type": "file"
    },
    {
      "path": "pic-sure.yaml",
      "type": "file"
    },
    {
      "path": ".pic-sure/render",
      "type": "dir"
    },
    {
      "path": ".pic-sure/render/files",
      "type": "dir"
    },
    {
      "path": ".pic-sure/render/files/run.log",
      "type": "file"
    }
  ]
}
`)
}

func TestManifestPathsAreCanonical(t *testing.T) {
	s := newStack(t)
	if err := s.WriteFile("./a/../b", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if m, _ := s.Manifest(); !m.Has("b") {
		t.Errorf("manifest = %+v, want b", m.Entries)
	}
}

// Two handles on one stack stand in for two processes: neither loses the
// other's entries, though only the flock keeps them apart.
func TestManifestConcurrentRecording(t *testing.T) {
	a := newStack(t)
	if err := a.WriteFile(ConfigFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	b := openStack(t, a.Dir)

	var wg sync.WaitGroup
	for i, s := range []*Stack{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 25 {
				if err := s.WriteFile(fmt.Sprintf("f%d-%d", i, j), nil, 0o644); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()

	m, err := a.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		for j := range 25 {
			if p := fmt.Sprintf("f%d-%d", i, j); !m.Has(p) {
				t.Errorf("manifest lost %s", p)
			}
		}
	}
}

func TestRemove(t *testing.T) {
	s := newStack(t)
	if err := s.MkdirAll(".pic-sure/logs", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteFile(".pic-sure/logs/old.log", nil, 0o644); err != nil {
		t.Fatal(err)
	}

	// A non-empty directory stays, and stays recorded.
	if err := s.Remove(".pic-sure/logs"); err == nil {
		t.Error("removed a non-empty directory")
	}
	if err := s.Remove(".pic-sure/logs/old.log"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(".pic-sure/logs"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(s.Path(".pic-sure/logs")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("logs still there: %v", err)
	}
	wantEntries(t, s, []Entry{
		{Path: ".pic-sure", Type: EntryDir},
		{Path: ".pic-sure/manifest.json", Type: EntryFile},
	})

	t.Run("refuses a path the CLI didn't create", func(t *testing.T) {
		if err := os.WriteFile(s.Path("operator.txt"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := s.Remove("operator.txt"); !errors.Is(err, ErrNotCreated) {
			t.Errorf("err = %v, want ErrNotCreated", err)
		}
		if _, err := os.Stat(s.Path("operator.txt")); err != nil {
			t.Errorf("operator file removed: %v", err)
		}
		if err := s.Remove("never-existed"); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("err = %v, want ErrNotExist", err)
		}
	})

	t.Run("forgets a recorded path that is already gone", func(t *testing.T) {
		if err := s.WriteFile("gone", nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(s.Path("gone")); err != nil {
			t.Fatal(err)
		}
		if err := s.Remove("gone"); err != nil {
			t.Fatal(err)
		}
		if m, _ := s.Manifest(); m.Has("gone") {
			t.Error("gone is still recorded")
		}
	})

	t.Run("refuses a path under a symlinked directory", func(t *testing.T) {
		if err := s.MkdirAll("gen", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := s.WriteFile("gen/config.yaml", nil, 0o644); err != nil {
			t.Fatal(err)
		}
		// The operator replaced the CLI's directory with a link to theirs.
		if err := os.MkdirAll(s.Path("overrides"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(s.Path("overrides/config.yaml"), []byte("mine"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(s.Path("gen")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("overrides", s.Path("gen")); err != nil {
			t.Fatal(err)
		}
		if err := s.Remove("gen/config.yaml"); err == nil {
			t.Error("Remove through a symlinked directory succeeded")
		}
		wantContent(t, s.Path("overrides/config.yaml"), "mine")
	})

	t.Run("refuses a path that changed kind", func(t *testing.T) {
		if err := s.MkdirAll("was-dir", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(s.Path("was-dir")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(s.Path("was-dir"), []byte("mine"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := s.Remove("was-dir"); !errors.Is(err, ErrNotCreated) {
			t.Errorf("err = %v, want ErrNotCreated", err)
		}
		wantContent(t, s.Path("was-dir"), "mine")
	})

	t.Run("removes a symlink, not its target", func(t *testing.T) {
		if err := s.WriteFile("target", []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := s.WriteFile("link", nil, 0o644); err != nil {
			t.Fatal(err)
		}
		// The operator replaced a file the CLI created with a symlink.
		if err := os.Remove(s.Path("link")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("target", s.Path("link")); err != nil {
			t.Fatal(err)
		}
		if err := s.Remove("link"); err != nil {
			t.Fatal(err)
		}
		wantContent(t, s.Path("target"), "keep")
	})
}

func TestManifestReadErrors(t *testing.T) {
	for name, content := range map[string]string{
		"corrupt": "{not json",
		"newer":   `{"version": 2, "entries": []}`,
	} {
		t.Run(name, func(t *testing.T) {
			s := newStack(t)
			if err := os.WriteFile(s.Path(ManifestFile), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Manifest(); err == nil {
				t.Error("Manifest succeeded")
			}
			// Recording must not overwrite what it can't read, and the
			// path isn't created unrecorded.
			if err := s.WriteFile("new", nil, 0o644); err == nil {
				t.Error("WriteFile recorded into an unreadable manifest")
			}
			wantContent(t, s.Path(ManifestFile), content)
			if _, err := os.Lstat(s.Path("new")); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("new was created without being recorded: %v", err)
			}

			// Once the manifest is readable, a retry records the path.
			if err := os.Remove(s.Path(ManifestFile)); err != nil {
				t.Fatal(err)
			}
			if err := s.WriteFile("new", nil, 0o644); err != nil {
				t.Fatal(err)
			}
			if m, _ := s.Manifest(); !m.Has("new") {
				t.Error("the retried write isn't recorded")
			}
		})
	}
}
