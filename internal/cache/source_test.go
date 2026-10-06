package cache_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/git"
)

func TestEnsureSourceUnpacksTheCommitOnce(t *testing.T) {
	up := newUpstream(t)
	sha := up.commit(map[string]string{"pom.xml": "<project/>\n", "hpds/Dockerfile": "FROM scratch\n"})
	r := &countingRunner{}
	c := open(t, cache.Options{Git: git.New(r)})
	var rec events.Recorder
	ctx := context.Background()

	dir, err := c.WithEvents(&rec, "sources").EnsureSource(ctx, "pic-sure", sha)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(c.Root(), "src", "pic-sure", sha); dir != want {
		t.Errorf("dir = %s, want %s", dir, want)
	}
	if got := readFile(t, filepath.Join(dir, "hpds", "Dockerfile")); got != "FROM scratch\n" {
		t.Errorf("hpds/Dockerfile = %q", got)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("tree root mode = %v, %v; want 0755", info.Mode(), err)
	}
	if _, err := os.Stat(filepath.Join(c.Root(), "git", "pic-sure.git", "HEAD")); err != nil {
		t.Errorf("no bare clone: %v", err)
	}
	wantProgress := []string{"cloning hms-dbmi/pic-sure", "unpacking hms-dbmi/pic-sure at " + sha[:12]}
	if got := progressTexts(rec.Events(), "sources"); !slices.Equal(got, wantProgress) {
		t.Errorf("progress = %q, want %q", got, wantProgress)
	}

	calls := r.calls.Load()
	again, err := c.EnsureSource(ctx, "pic-sure", sha)
	if err != nil || again != dir {
		t.Fatalf("second EnsureSource = %s, %v", again, err)
	}
	if n := r.calls.Load() - calls; n != 0 {
		t.Errorf("an existing tree ran git %d times", n)
	}
}

func TestEnsureSourceFetchesACommitTheCloneLacks(t *testing.T) {
	up := newUpstream(t)
	first := up.commit(map[string]string{"a.txt": "one\n"})
	c := open(t, cache.Options{Git: git.New(&countingRunner{})})
	ctx := context.Background()
	if _, err := c.EnsureSource(ctx, "pic-sure", first); err != nil {
		t.Fatal(err)
	}

	second := up.commit(map[string]string{"a.txt": "two\n"})
	var rec events.Recorder
	dir, err := c.WithEvents(&rec, "sources").EnsureSource(ctx, "pic-sure", second)
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, "a.txt")); got != "two\n" {
		t.Errorf("a.txt = %q", got)
	}
	if got := progressTexts(rec.Events(), "sources"); !slices.Contains(got, "fetching hms-dbmi/pic-sure") {
		t.Errorf("progress = %q, want a fetch", got)
	}
}

func TestEnsureSourceOfAnUnknownCommitLeavesNothing(t *testing.T) {
	up := newUpstream(t)
	up.commit(map[string]string{"a.txt": "one\n"})
	c := open(t, cache.Options{Git: git.New(&countingRunner{})})

	_, err := c.EnsureSource(context.Background(), "pic-sure", strings.Repeat("ab", 20))
	if !errors.Is(err, git.ErrUnknownRef) {
		t.Fatalf("err = %v, want ErrUnknownRef", err)
	}
	if got := entries(t, filepath.Join(c.Root(), "src", "pic-sure")); len(got) != 0 {
		t.Errorf("src/pic-sure holds %q", got)
	}
}

func TestEnsureSourceRefusesATagObject(t *testing.T) {
	up := newUpstream(t)
	commit := up.commit(map[string]string{"a.txt": "one\n"})
	up.git("tag", "--annotate", "--message", "v1", "v1")
	tag := up.git("rev-parse", "v1")
	c := open(t, cache.Options{Git: git.New(&countingRunner{})})

	_, err := c.EnsureSource(context.Background(), "pic-sure", tag)
	if err == nil || !strings.Contains(err.Error(), "names commit "+commit) {
		t.Fatalf("err = %v, want a not-a-commit error", err)
	}
}

func TestEnsureSourceRemovesTheLeftoversOfADeadRun(t *testing.T) {
	up := newUpstream(t)
	sha := up.commit(map[string]string{"a.txt": "one\n"})
	c := open(t, cache.Options{Git: git.New(&countingRunner{})})
	leftovers := []string{
		filepath.Join("src", "pic-sure", sha+".tmp-123"),
		filepath.Join("git", "pic-sure.git.tmp-456"),
	}
	other := filepath.Join("git", "PIC-SURE-Frontend.git.tmp-789") // another repo's lock covers it
	for _, p := range append(leftovers, other) {
		if err := os.MkdirAll(filepath.Join(c.Root(), p, "x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := c.EnsureSource(context.Background(), "pic-sure", sha); err != nil {
		t.Fatal(err)
	}
	for _, p := range leftovers {
		if _, err := os.Stat(filepath.Join(c.Root(), p)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is still there (%v)", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(c.Root(), other)); err != nil {
		t.Errorf("removed another repository's temporary clone: %v", err)
	}
}

func TestConcurrentEnsureSourceMakesOneTree(t *testing.T) {
	up := newUpstream(t)
	sha := up.commit(map[string]string{"a.txt": "one\n"})
	r := &countingRunner{}
	c := open(t, cache.Options{Git: git.New(r)})

	const n = 8
	dirs := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { dirs[i], errs[i] = c.EnsureSource(context.Background(), "pic-sure", sha) })
	}
	wg.Wait()

	for i := range n {
		if errs[i] != nil || dirs[i] != dirs[0] {
			t.Errorf("goroutine %d: %s, %v", i, dirs[i], errs[i])
		}
	}
	if got := r.archives.Load(); got != 1 {
		t.Errorf("git archive ran %d times, want 1", got)
	}
	if got := entries(t, filepath.Join(c.Root(), "src", "pic-sure")); !slices.Equal(got, []string{sha}) {
		t.Errorf("src/pic-sure holds %q", got)
	}
}

func TestConcurrentEnsureSourceAcrossProcessesMakesOneTree(t *testing.T) {
	up := newUpstream(t)
	sha := up.commit(map[string]string{"a.txt": "one\n"})
	root := t.TempDir()
	log := filepath.Join(t.TempDir(), "archives")

	type process struct {
		cmd            *exec.Cmd
		stdout, stderr bytes.Buffer
	}
	var cmds []*process
	for range 2 {
		p := &process{cmd: helper(t, "ensure-source", root, helperSHA+"="+sha, archiveLogEnv+"="+log)}
		p.cmd.Stdout, p.cmd.Stderr = &p.stdout, &p.stderr
		if err := p.cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, p)
	}
	want := filepath.Join(root, "src", "pic-sure", sha) + "\n"
	for i, out := range cmds {
		if err := out.cmd.Wait(); err != nil {
			t.Fatalf("process %d: %v\n%s", i, err, out.stderr.String())
		}
		if got := out.stdout.String(); got != want {
			t.Errorf("process %d printed %q, want %q", i, got, want)
		}
	}
	if got := strings.Count(readFile(t, log), "\n"); got != 1 {
		t.Errorf("git archive ran %d times, want 1", got)
	}
	if got := entries(t, filepath.Join(root, "src", "pic-sure")); !slices.Equal(got, []string{sha}) {
		t.Errorf("src/pic-sure holds %q", got)
	}
}

func TestEnsureSourceRefusesBadArguments(t *testing.T) {
	sha := strings.Repeat("ab", 20)
	for _, tc := range []struct{ component, sha string }{
		{"no-such-component", sha},
		{"pic-sure", sha[:12]},
		{"pic-sure", strings.ToUpper(sha)},
		{"pic-sure", "../" + sha[3:]},
		{"pic-sure", "main"},
	} {
		c := open(t, cache.Options{Git: git.New(fakerunner.New(t))}) // any git call fails the test
		if _, err := c.EnsureSource(context.Background(), tc.component, tc.sha); err == nil {
			t.Errorf("EnsureSource(%q, %q) succeeded", tc.component, tc.sha)
		}
	}
}

func progressTexts(evs []events.Event, step string) []string {
	var texts []string
	for _, ev := range evs {
		if p, ok := ev.(events.Progress); ok && p.ID == step {
			texts = append(texts, p.Text)
		}
	}
	return texts
}
