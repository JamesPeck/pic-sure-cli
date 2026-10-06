package cache_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
)

// twoCaches opens the same root twice, as two commands would: first holds
// locks, second waits for them, giving up after a short timeout.
func twoCaches(t *testing.T) (first, second *cache.Cache) {
	t.Helper()
	root := t.TempDir()
	first, err := cache.Open(root, cache.Options{Holder: "pic-sure build"})
	if err != nil {
		t.Fatal(err)
	}
	second, err = cache.Open(root, cache.Options{LockTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return first, second
}

func TestALockTimesOutNamingItsHolder(t *testing.T) {
	first, second := twoCaches(t)
	ctx := context.Background()
	held, err := first.LockReactor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var rec events.Recorder

	_, err = second.WithEvents(&rec, "reactor").LockReactor(ctx)
	holder := fmt.Sprintf("pic-sure build (pid %d)", os.Getpid())
	if !errors.Is(err, cache.ErrLockTimeout) || !strings.Contains(err.Error(), holder) {
		t.Fatalf("err = %v, want a timeout naming %s", err, holder)
	}
	want := []string{"waiting for the reactor build lock held by " + holder}
	if got := progressTexts(rec.Events(), "reactor"); !slices.Equal(got, want) {
		t.Errorf("progress = %q, want %q", got, want)
	}

	if err := held.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := held.Unlock(); err != nil {
		t.Errorf("second Unlock: %v", err)
	}
	lock, err := second.LockReactor(ctx)
	if err != nil {
		t.Fatalf("after Unlock: %v", err)
	}
	_ = lock.Unlock()
}

func TestAWaitingLockIsTakenWhenReleased(t *testing.T) {
	first := open(t, cache.Options{})
	second, err := cache.Open(first.Root(), cache.Options{LockTimeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tag := "hms-dbmi/psama:0123456789ab"
	held, err := first.LockImage(ctx, tag)
	if err != nil {
		t.Fatal(err)
	}

	got := make(chan error, 1)
	go func() {
		lock, err := second.LockImage(ctx, tag)
		if err == nil {
			err = lock.Unlock()
		}
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("took a held lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	_ = held.Unlock()
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("still waiting after the holder released the lock")
	}
}

func TestAWaitingLockStopsWhenTheContextEnds(t *testing.T) {
	first := open(t, cache.Options{})
	second, err := cache.Open(first.Root(), cache.Options{LockTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	held, err := first.LockRepo(context.Background(), "pic-sure")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Unlock() }()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := second.LockRepo(ctx, "pic-sure"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context's", err)
	}
}

func TestLocksOnDifferentThingsDontContend(t *testing.T) {
	first, second := twoCaches(t)
	ctx := context.Background()
	takes := []func(*cache.Cache) (*cache.Lock, error){
		func(c *cache.Cache) (*cache.Lock, error) { return c.LockRepo(ctx, "pic-sure") },
		func(c *cache.Cache) (*cache.Lock, error) { return c.LockRepo(ctx, "PIC-SURE-Frontend") },
		func(c *cache.Cache) (*cache.Lock, error) { return c.LockReactor(ctx) },
		func(c *cache.Cache) (*cache.Lock, error) { return c.LockImage(ctx, "hms-dbmi/psama:0123456789ab") },
		func(c *cache.Cache) (*cache.Lock, error) { return c.LockImage(ctx, "hms-dbmi/psama:ba9876543210") },
	}
	// Each lock is taken alongside every other: first holds one while
	// second takes another.
	for i, take := range takes {
		held, err := take(first)
		if err != nil {
			t.Fatal(err)
		}
		for j, other := range takes {
			if j == i {
				continue
			}
			lock, err := other(second)
			if err != nil {
				t.Fatalf("lock %d while %d is held: %v", j, i, err)
			}
			_ = lock.Unlock()
		}
		_ = held.Unlock()
	}
}

func TestALockHeldByAnotherProcess(t *testing.T) {
	_, second := twoCaches(t)
	holder, stdin := startHolder(t, second.Root())

	_, err := second.LockReactor(context.Background())
	want := fmt.Sprintf("helper (pid %d)", holder.Process.Pid)
	if !errors.Is(err, cache.ErrLockTimeout) || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want a timeout naming %s", err, want)
	}

	_ = stdin.Close()
	if err := holder.Wait(); err != nil {
		t.Fatal(err)
	}
	lock, err := second.LockReactor(context.Background())
	if err != nil {
		t.Fatalf("after the holder exited: %v", err)
	}
	_ = lock.Unlock()
}

func TestLockNamesAreChecked(t *testing.T) {
	c := open(t, cache.Options{})
	ctx := context.Background()
	for _, repo := range []string{"", ".", "..", "../x", "a/b", "-x"} {
		if _, err := c.LockRepo(ctx, repo); err == nil {
			t.Errorf("LockRepo(%q) succeeded", repo)
		}
	}
	if _, err := c.LockImage(ctx, ""); err == nil {
		t.Error(`LockImage("") succeeded`)
	}
	// A tag's slashes are escaped, so its lock file is one name in locks/.
	lock, err := c.LockImage(ctx, "../../escape:tag")
	if err != nil {
		t.Fatal(err)
	}
	_ = lock.Unlock()
	if got := entries(t, filepath.Join(c.Root(), "locks")); len(got) != 1 {
		t.Errorf("locks/ holds %q", got)
	}
}
