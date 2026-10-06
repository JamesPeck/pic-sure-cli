package stack

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestLockHolderProcess is not a test: it is the second process for the
// contention tests, run from this test binary. It takes the lock on the
// stack in $PICSURE_TEST_LOCK_HOLDER, says "locked", and holds the lock
// until its stdin closes.
func TestLockHolderProcess(t *testing.T) {
	dir := os.Getenv("PICSURE_TEST_LOCK_HOLDER")
	if dir == "" {
		t.Skip("helper process for the lock tests")
	}
	s, err := Open(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	l, err := s.Lock(context.Background(), LockOptions{Command: "pic-sure up"})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	fmt.Println("locked")
	_, _ = io.Copy(io.Discard, os.Stdin)
	_ = l.Unlock()
	os.Exit(0)
}

// lockHolderProc is another process holding a stack's lock.
type lockHolderProc struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
}

func startHolder(t *testing.T, dir string) *lockHolderProc {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockHolderProcess$")
	cmd.Env = append(os.Environ(), "PICSURE_TEST_LOCK_HOLDER="+dir)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h := &lockHolderProc{cmd: cmd, stdin: stdin}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	// A holder that never says "locked" is killed, ending the read.
	timer := time.AfterFunc(30*time.Second, func() { _ = cmd.Process.Kill() })
	defer timer.Stop()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if strings.TrimSpace(line) != "locked" {
		t.Fatalf("holder said %q, %v", line, err)
	}
	return h
}

// release makes the holder unlock and exit.
func (h *lockHolderProc) release(t *testing.T) {
	t.Helper()
	_ = h.stdin.Close()
	if err := h.cmd.Wait(); err != nil {
		t.Fatalf("holder: %v", err)
	}
}

func TestLockContentionBetweenProcesses(t *testing.T) {
	s := newStack(t)
	if err := s.WriteFile(ConfigFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	h := startHolder(t, s.Dir)

	// Without Wait, a held lock fails at once, naming the holder.
	_, err := s.Lock(context.Background(), LockOptions{})
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("err = %v, want ErrLocked", err)
	}
	if want := fmt.Sprintf("pic-sure up (pid %d)", h.cmd.Process.Pid); !strings.Contains(err.Error(), want) {
		t.Errorf("err = %q, want it to name %q", err, want)
	}

	// With Wait, Lock waits until the holder lets go.
	holders := make(chan string, 1)
	got := make(chan error, 1)
	go func() {
		l, err := s.Lock(context.Background(), LockOptions{Wait: true, OnWait: func(h string) { holders <- h }})
		if err == nil {
			defer func() { _ = l.Unlock() }()
		}
		got <- err
	}()
	select {
	case holder := <-holders:
		if !strings.Contains(holder, "pic-sure up") {
			t.Errorf("OnWait(%q), want the holder", holder)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("OnWait wasn't called")
	}
	select {
	case err := <-got:
		t.Fatalf("Lock returned (%v) while the other process held the lock", err)
	case <-time.After(3 * lockPoll):
	}
	h.release(t)
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Lock still waiting after the holder released it")
	}
}

func TestLockReleasedWhenTheHolderDies(t *testing.T) {
	s := newStack(t)
	if err := s.WriteFile(ConfigFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	h := startHolder(t, s.Dir)
	if err := h.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = h.cmd.Wait()

	l, err := s.Lock(context.Background(), LockOptions{})
	if err != nil {
		t.Fatalf("lock of a killed holder is still held: %v", err)
	}
	_ = l.Unlock()
}

func TestLockWaitEndsWithTheContext(t *testing.T) {
	s := newStack(t)
	if err := s.WriteFile(ConfigFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	startHolder(t, s.Dir)

	cause := errors.New("interrupted")
	ctx, cancel := context.WithCancelCause(context.Background())
	time.AfterFunc(2*lockPoll, func() { cancel(cause) })
	if _, err := s.Lock(ctx, LockOptions{Wait: true}); !errors.Is(err, cause) {
		t.Errorf("err = %v, want the context's cause", err)
	}
}

func TestLockInProcess(t *testing.T) {
	s := newStack(t)
	l, err := s.Lock(context.Background(), LockOptions{Command: "pic-sure update"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lock(context.Background(), LockOptions{}); !errors.Is(err, ErrLocked) {
		t.Errorf("second Lock: err = %v, want ErrLocked", err)
	}
	if err := l.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := l.Unlock(); err != nil {
		t.Errorf("second Unlock: %v", err)
	}
	wantContent(t, s.Path(LockFile), "") // no stale holder

	l, err = s.Lock(context.Background(), LockOptions{})
	if err != nil {
		t.Fatalf("Lock after Unlock: %v", err)
	}
	_ = l.Unlock()
	if m, _ := s.Manifest(); !m.Has(LockFile) {
		t.Error("the lock file isn't recorded in the manifest")
	}
}

func TestLockRefusesASymlinkedLockFile(t *testing.T) {
	s := newStack(t)
	if err := s.WriteFile(ConfigFile, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../"+ConfigFile, s.Path(LockFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lock(context.Background(), LockOptions{}); err == nil {
		t.Error("Lock through a symlink succeeded")
	}
	wantContent(t, s.Path(ConfigFile), "keep")
}
