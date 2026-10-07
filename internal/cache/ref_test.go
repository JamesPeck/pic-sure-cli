package cache_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/git"
)

func TestResolveRefFetchesUnlessTheShaIsKnown(t *testing.T) {
	up := newUpstream(t)
	first := up.commit(map[string]string{"a.txt": "one\n"})
	r := &countingRunner{}
	c := open(t, cache.Options{Git: git.New(r)})
	ctx := context.Background()

	if got, err := c.ResolveRef(ctx, "pic-sure", "main"); err != nil || got != first {
		t.Fatalf("ResolveRef(main) = %s, %v; want %s", got, err, first)
	}
	second := up.commit(map[string]string{"a.txt": "two\n"})
	if got, err := c.ResolveRef(ctx, "pic-sure", "main"); err != nil || got != second {
		t.Fatalf("after a push, ResolveRef(main) = %s, %v; want %s", got, err, second)
	}
	calls := r.calls.Load()
	if got, err := c.ResolveRef(ctx, "pic-sure", first); err != nil || got != first {
		t.Fatalf("ResolveRef(sha) = %s, %v", got, err)
	}
	if n := r.calls.Load() - calls; n != 1 {
		t.Errorf("a known sha ran git %d times, want 1 (no fetch)", n)
	}
	if _, err := c.ResolveRef(ctx, "pic-sure", "no-such-branch"); !errors.Is(err, git.ErrUnknownRef) {
		t.Errorf("unknown ref: err = %v, want ErrUnknownRef", err)
	}
}

func TestResolveRefRefuses(t *testing.T) {
	ctx := context.Background()
	c := open(t, cache.Options{})
	if _, err := c.ResolveRef(ctx, "nope", "main"); err == nil || !strings.Contains(err.Error(), "unknown component") {
		t.Errorf("unknown component: err = %v", err)
	}
	if _, err := c.ResolveRef(ctx, "pic-sure", "main"); err == nil || !strings.Contains(err.Error(), "needs Options.Git") {
		t.Errorf("no Git: err = %v", err)
	}
}
