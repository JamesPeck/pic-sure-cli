package cache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// RegisteredStack is an entry of the cache's stack registry, stacks/<key>:
// a stack that may use the cache's images and source trees even when no
// container, volume or network of it exists.
type RegisteredStack struct {
	Dir  string `json:"dir"`
	Name string `json:"name"`
	// Key is the entry's file name under stacks/. An entry whose file
	// can't be parsed has only its Key.
	Key string `json:"-"`
}

// stackKey is the registry's file name for the stack at dir, an absolute
// clean path.
func stackKey(dir string) string {
	sum := sha256.Sum256([]byte(dir))
	return hex.EncodeToString(sum[:16])
}

// RegisterStack records the stack at dir, named name, in the registry, so
// prune counts it while it has no labelled Docker resource. It holds the
// use lock, so a prune that has found the entry gone (its directory not a
// stack) can't remove it once it is current again. Rewriting an unchanged
// entry is a no-op, and the write is atomic.
func (c *Cache) RegisterStack(ctx context.Context, dir, name string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("cache: stack directory %q is not absolute", dir)
	}
	dir = filepath.Clean(dir)
	data, err := json.Marshal(RegisteredStack{Dir: dir, Name: name})
	if err != nil {
		return err
	}
	data = append(data, '\n')
	key := stackKey(dir)
	path := filepath.Join(c.root, stacksDir, key)
	lock, err := c.LockUse(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return nil
	}
	f, err := os.CreateTemp(filepath.Join(c.root, stacksDir), key+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("cache: registering the stack at %s: %w", dir, err)
	}
	return nil
}

// UnregisterStack removes the registry entry of the stack at dir, if there
// is one, for destroy.
func (c *Cache) UnregisterStack(dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("cache: stack directory %q is not absolute", dir)
	}
	return c.ForgetStack(stackKey(filepath.Clean(dir)))
}

// ForgetStack removes the registry entry key, a RegisteredStack's Key. A
// missing entry is not an error.
func (c *Cache) ForgetStack(key string) error {
	if key == "" || strings.ContainsAny(key, `/\`) || strings.Contains(key, ".tmp-") || key == "." || key == ".." {
		return fmt.Errorf("cache: invalid stack registry key %q", key)
	}
	err := os.Remove(filepath.Join(c.root, stacksDir, key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// RegisteredStacks lists the registry, sorted by key. An entry that can't
// be parsed, or names a relative directory, is returned with only its Key.
// Temporary files of a write in progress, or of one that died, are
// skipped; Entries lists them.
func (c *Cache) RegisteredStacks() ([]RegisteredStack, error) {
	files, err := readDir(filepath.Join(c.root, stacksDir))
	if err != nil {
		return nil, err
	}
	var out []RegisteredStack
	for _, f := range files {
		if !f.Type().IsRegular() || strings.Contains(f.Name(), ".tmp-") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(c.root, stacksDir, f.Name()))
		if errors.Is(err, fs.ErrNotExist) {
			continue // forgotten while listing
		}
		if err != nil {
			return nil, err
		}
		var s RegisteredStack
		if json.Unmarshal(data, &s) != nil || !filepath.IsAbs(s.Dir) {
			s = RegisteredStack{}
		}
		s.Key = f.Name()
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b RegisteredStack) int { return strings.Compare(a.Key, b.Key) })
	return out, nil
}
