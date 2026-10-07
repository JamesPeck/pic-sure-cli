package cache_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
)

func TestStackRegistry(t *testing.T) {
	ctx := context.Background()
	_, c := twoCaches(t)
	dir := filepath.Join(t.TempDir(), "stack")
	if err := c.RegisterStack(ctx, "relative", "x"); err == nil {
		t.Error("registered a relative directory")
	}
	if err := c.RegisterStack(ctx, dir+"/.", "demo"); err != nil {
		t.Fatal(err)
	}
	reg, err := c.RegisteredStacks()
	if err != nil || len(reg) != 1 {
		t.Fatalf("registry %+v, %v", reg, err)
	}
	key := reg[0].Key
	path := filepath.Join(c.Root(), "stacks", key)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Unchanged: not rewritten.
	if err := c.RegisterStack(ctx, dir, "demo"); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.Stat(path); !os.SameFile(info, again) {
		t.Error("an unchanged entry was rewritten")
	}
	// Renamed: rewritten in place.
	if err := c.RegisterStack(ctx, dir, "renamed"); err != nil {
		t.Fatal(err)
	}

	// A broken entry and a dead write's temporary file.
	if err := os.WriteFile(filepath.Join(c.Root(), "stacks", "broken"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	tmp := path + ".tmp-123"
	if err := os.WriteFile(tmp, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := c.RegisteredStacks()
	if err != nil {
		t.Fatal(err)
	}
	want := []cache.RegisteredStack{{Dir: dir, Name: "renamed", Key: key}, {Key: "broken"}}
	slices.SortFunc(want, func(a, b cache.RegisteredStack) int { return strings.Compare(a.Key, b.Key) })
	if !slices.Equal(got, want) {
		t.Errorf("registry %+v, want %+v", got, want)
	}
	entries, err := c.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(entries, func(e cache.Entry) bool { return e.Kind == cache.EntryTemp && e.Path == tmp }) {
		t.Errorf("entries %+v lack the temporary file", entries)
	}

	if err := c.UnregisterStack(dir); err != nil {
		t.Fatal(err)
	}
	if err := c.UnregisterStack(dir); err != nil {
		t.Errorf("unregistering twice: %v", err)
	}
	for _, key := range []string{"", "..", "a/b", "x.tmp-1"} {
		if err := c.ForgetStack(key); err == nil {
			t.Errorf("ForgetStack(%q) succeeded", key)
		}
	}
	if err := c.ForgetStack("broken"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.RegisteredStacks(); len(got) != 0 {
		t.Errorf("registry %+v, want empty", got)
	}
}
