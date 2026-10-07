package cache_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
)

const treeSHA = "0123456789abcdef0123456789abcdef01234567"

func makeFiles(t *testing.T, root string, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("1234"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEntriesListsWhatPruneMayRemove(t *testing.T) {
	_, c := twoCaches(t)
	makeFiles(t, c.Root(),
		"src/pic-sure/"+treeSHA+"/a", "src/pic-sure/"+treeSHA+"/b/c",
		"src/pic-sure/"+treeSHA+".tmp-123/a",
		"src/pic-sure/not-a-sha/a",
		"git/pic-sure.git/HEAD", "git/pic-sure.git.tmp-9/HEAD",
		"release-control/x", "release-control.tmp-7/x",
		"build/0123456789ab/x", "build/frontend-0123456789ab-01234567/x",
		"downloads/nhanes.tgz", "tmp/run-1/x",
	)
	entries, err := c.Entries()
	if err != nil {
		t.Fatal(err)
	}
	type want struct {
		kind cache.EntryKind
		repo string
		size int64
	}
	wants := map[string]want{
		"src/pic-sure/" + treeSHA:              {cache.EntrySource, "pic-sure", 8},
		"src/pic-sure/" + treeSHA + ".tmp-123": {cache.EntryTemp, "pic-sure", 4},
		"git/pic-sure.git.tmp-9":               {cache.EntryTemp, "pic-sure", 4},
		"release-control.tmp-7":                {cache.EntryTemp, "release-control", 4},
		"build/0123456789ab":                   {cache.EntryBuild, "", 4},
		"build/frontend-0123456789ab-01234567": {cache.EntryBuild, "", 4},
		"downloads/nhanes.tgz":                 {cache.EntryDownload, "", 4},
		"tmp/run-1":                            {cache.EntryTemp, "", 4},
	}
	if len(entries) != len(wants) {
		t.Errorf("got %d entries, want %d: %+v", len(entries), len(wants), entries)
	}
	for _, e := range entries {
		w, ok := wants[e.Name]
		if !ok {
			t.Errorf("unexpected entry %s", e.Name)
			continue
		}
		if e.Kind != w.kind || e.Repo != w.repo || e.Size != w.size || e.Path != filepath.Join(c.Root(), filepath.FromSlash(e.Name)) {
			t.Errorf("%s: %+v, want %+v", e.Name, e, w)
		}
		if e.Kind == cache.EntrySource && e.SHA != treeSHA {
			t.Errorf("%s: sha %q", e.Name, e.SHA)
		}
	}
}

func entryNamed(t *testing.T, c *cache.Cache, name string) cache.Entry {
	t.Helper()
	entries, err := c.Entries()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("no entry %s", name)
	return cache.Entry{}
}

func TestRemoveEntryRemovesASourceTreeWholeUnderItsLock(t *testing.T) {
	first, second := twoCaches(t)
	ctx := context.Background()
	name := "src/pic-sure/" + treeSHA
	makeFiles(t, second.Root(), name+"/a", name+".tmp-prune/stale")
	e := entryNamed(t, second, name)

	held, err := first.LockRepo(ctx, "pic-sure")
	if err != nil {
		t.Fatal(err)
	}
	if err := second.RemoveEntry(ctx, e); !errors.Is(err, cache.ErrLockTimeout) {
		t.Fatalf("with the fetch lock held: %v, want ErrLockTimeout", err)
	}
	if _, err := os.Stat(e.Path); err != nil {
		t.Fatalf("tree touched while the lock was held: %v", err)
	}
	_ = held.Unlock()

	if err := second.RemoveEntry(ctx, e); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{e.Path, e.Path + ".tmp-prune"} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still there: %v", p, err)
		}
	}
	if err := second.RemoveEntry(ctx, e); err != nil {
		t.Errorf("removing it again: %v", err)
	}
}

func TestRemoveEntryTakesTheBuildsLocks(t *testing.T) {
	first, second := twoCaches(t)
	ctx := context.Background()
	makeFiles(t, second.Root(), "build/0123456789ab/x", "build/frontend-0123456789ab-01234567/x")

	held, err := first.LockReactor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.RemoveEntry(ctx, entryNamed(t, second, "build/0123456789ab")); !errors.Is(err, cache.ErrLockTimeout) {
		t.Errorf("reactor context with the reactor lock held: %v", err)
	}
	_ = held.Unlock()

	held, err = first.LockImage(ctx, "hms-dbmi/pic-sure-httpd:0123456789ab-01234567")
	if err != nil {
		t.Fatal(err)
	}
	front := entryNamed(t, second, "build/frontend-0123456789ab-01234567")
	if err := second.RemoveEntry(ctx, front); !errors.Is(err, cache.ErrLockTimeout) {
		t.Errorf("frontend context with its image lock held: %v", err)
	}
	_ = held.Unlock()
	if err := second.RemoveEntry(ctx, front); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(front.Path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("frontend context still there: %v", err)
	}
}

func TestRemoveEntryRefusesAPathOutsideTheCache(t *testing.T) {
	_, c := twoCaches(t)
	outside := t.TempDir()
	for _, e := range []cache.Entry{
		{Kind: cache.EntryTemp, Name: "../x", Path: filepath.Join(c.Root(), "../x")},
		{Kind: cache.EntryTemp, Name: "tmp/x", Path: outside},
		{Kind: cache.EntryTemp},
	} {
		if err := c.RemoveEntry(context.Background(), e); err == nil {
			t.Errorf("%+v: removed", e)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("outside directory removed: %v", err)
	}
}
