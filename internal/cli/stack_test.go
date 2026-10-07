package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

func newTestStack(t *testing.T) string {
	t.Helper()
	s, err := stack.Create(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.WriteFile(stack.ConfigFile, []byte("schema: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return s.Dir
}

func TestOpenStack(t *testing.T) {
	dir := newTestStack(t)
	sub := filepath.Join(dir, "certs")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("from a subdirectory", func(t *testing.T) {
		t.Chdir(sub)
		a, _, _ := testApp(t)
		st, err := a.openStack(findCmd(t, "status"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = st.Close() }()
		if st.Dir != dir {
			t.Errorf("Dir = %q, want %q", st.Dir, dir)
		}
	})
	t.Run("--stack", func(t *testing.T) {
		t.Chdir(t.TempDir())
		a, _, _ := testApp(t)
		a.Global.Stack = dir
		st, err := a.openStack(findCmd(t, "status"))
		if err != nil {
			t.Fatal(err)
		}
		_ = st.Close()
	})
	t.Run("no stack is exit 3", func(t *testing.T) {
		t.Chdir(t.TempDir())
		a, _, _ := testApp(t)
		if _, err := a.openStack(findCmd(t, "status")); exitcode.FromError(err) != exitcode.CodePrecondition {
			t.Errorf("err = %v, want exit 3", err)
		}
	})
}

func TestInitDirConflict(t *testing.T) {
	t.Chdir(t.TempDir())
	a, _, _ := testApp(t)
	a.Global.Stack = "a"
	if _, err := a.initDir([]string{"b"}); exitcode.FromError(err) != exitcode.CodeUsage {
		t.Errorf("err = %v, want exit 2", err)
	}
	dir, err := a.initDir([]string{"a"})
	if err != nil || filepath.Base(dir) != "a" {
		t.Errorf("initDir = %q, %v", dir, err)
	}
}

func TestLockStack(t *testing.T) {
	st, err := stack.Open(newTestStack(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	held, err := st.Lock(context.Background(), stack.LockOptions{Command: "pic-sure up"})
	if err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{Use: "update"}

	a, _, _ := testApp(t)
	_, err = a.lockStack(context.Background(), cmd, st, events.Discard)
	if exitcode.FromError(err) != exitcode.CodeFailed || !errors.Is(err, stack.ErrLocked) ||
		!strings.Contains(err.Error(), "pic-sure up") || !strings.Contains(err.Error(), "--wait-lock") {
		t.Errorf("err = %v, want exit 1 naming the holder and --wait-lock", err)
	}

	// --wait-lock waits, and says so.
	a.Global.WaitLock = true
	var rec events.Recorder
	time.AfterFunc(300*time.Millisecond, func() { _ = held.Unlock() })
	l, err := a.lockStack(context.Background(), cmd, st, &rec)
	if err != nil {
		t.Fatal(err)
	}
	_ = l.Unlock()
	if evs := rec.Events(); len(evs) != 1 || !strings.Contains(evs[0].(events.Warning).Text, "waiting for pic-sure up") {
		t.Errorf("events = %+v, want one waiting warning", evs)
	}
}

func TestWaitLockFlag(t *testing.T) {
	a, _, _ := testApp(t)
	if code := a.Run(context.Background(), []string{"version", "--wait-lock"}); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !a.Global.WaitLock {
		t.Error("--wait-lock not set")
	}
}
