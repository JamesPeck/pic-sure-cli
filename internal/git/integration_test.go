package git_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/git"
)

// isolateGit skips the test without git, and keeps the user's git config
// and any enclosing repository out of it. In their place it puts a global
// config with settings that would change the client's results if it didn't
// override them.
func isolateGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	config := filepath.Join(t.TempDir(), "gitconfig")
	hostile := "[core]\n\tautocrlf = true\n[tar]\n\tumask = 0\n[clone]\n\tdefaultRemoteName = upstream\n"
	if err := os.WriteFile(config, []byte(hostile), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", config)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
	for _, k := range []string{"GIT_AUTHOR", "GIT_COMMITTER"} {
		t.Setenv(k+"_NAME", "Test")
		t.Setenv(k+"_EMAIL", "test@example.com")
	}
}

// upstream is a non-bare repository standing in for a remote.
type upstream struct {
	t   *testing.T
	dir string
}

func newUpstream(t *testing.T) *upstream {
	u := &upstream{t: t, dir: t.TempDir()}
	u.git("init", "--quiet", "--initial-branch=main")
	return u
}

func (u *upstream) git(args ...string) string {
	u.t.Helper()
	out, err := exec.Command("git", append([]string{"-C", u.dir}, args...)...).CombinedOutput()
	if err != nil {
		u.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (u *upstream) write(name, content string, mode os.FileMode) {
	u.t.Helper()
	p := filepath.Join(u.dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		u.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		u.t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		u.t.Fatal(err)
	}
}

func (u *upstream) symlink(name, target string) {
	u.t.Helper()
	if err := os.Symlink(target, filepath.Join(u.dir, name)); err != nil {
		u.t.Fatal(err)
	}
}

// commit commits everything and returns the new commit's sha.
func (u *upstream) commit(msg string) string {
	u.git("add", "--all")
	u.git("commit", "--quiet", "--message", msg)
	return u.git("rev-parse", "HEAD")
}

func TestBareCloneResolveAndFetch(t *testing.T) {
	isolateGit(t)
	ctx := context.Background()
	up := newUpstream(t)
	up.write("README", "one\n", 0o644)
	first := up.commit("first")
	up.git("tag", "--annotate", "--message", "release", "v1.0")
	up.git("tag", "light")
	up.git("switch", "--quiet", "--create", "feature")
	up.write("feature.txt", "f\n", 0o644)
	featureHead := up.commit("feature")
	up.git("switch", "--quiet", "main")
	up.git("branch", "light", featureHead) // same name as a tag

	c := git.New(&docker.ExecRunner{})
	cache := t.TempDir()
	bare := filepath.Join(cache, "git", "upstream.git")
	if err := c.EnsureBare(ctx, up.dir, bare); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(bare)); len(entries) != 1 {
		t.Errorf("clone left extra entries next to the bare repo: %v", entries)
	}

	resolve := func(ref, want string) {
		t.Helper()
		got, err := c.ResolveRef(ctx, bare, ref)
		if err != nil {
			t.Errorf("ResolveRef(%q): %v", ref, err)
		} else if got != want {
			t.Errorf("ResolveRef(%q) = %s, want %s", ref, got, want)
		}
	}
	resolve("v1.0", first)  // annotated tag, peeled to its commit
	resolve("light", first) // the tag, not the branch
	resolve("main", first)
	resolve("feature", featureHead)
	resolve(first, first)
	resolve(featureHead[:10], featureHead)
	if _, err := c.ResolveRef(ctx, bare, "no-such-branch"); !errors.Is(err, git.ErrUnknownRef) {
		t.Errorf("ResolveRef(no-such-branch): err = %v, want ErrUnknownRef", err)
	}

	// Upstream moves on: a new commit on main, v1.0 moved to it, feature
	// deleted.
	up.write("README", "two\n", 0o644)
	second := up.commit("second")
	up.git("tag", "--force", "--annotate", "--message", "re-release", "v1.0")
	up.git("branch", "--quiet", "--delete", "--force", "feature")

	if err := c.Fetch(ctx, bare, nil, true); err != nil {
		t.Fatal(err)
	}
	resolve("main", second)
	resolve("v1.0", second)
	if _, err := c.ResolveRef(ctx, bare, "feature"); !errors.Is(err, git.ErrUnknownRef) {
		t.Errorf("feature was deleted upstream but still resolves (err = %v)", err)
	}

	// EnsureBare on an existing clone repoints it at a new URL.
	moved := newUpstream(t)
	moved.write("other", "x\n", 0o644)
	movedHead := moved.commit("moved")
	if err := c.EnsureBare(ctx, moved.dir, bare); err != nil {
		t.Fatal(err)
	}
	if err := c.Fetch(ctx, bare, nil, false); err != nil {
		t.Fatal(err)
	}
	resolve("main", movedHead)
}

func TestLsRemote(t *testing.T) {
	isolateGit(t)
	up := newUpstream(t)
	up.write("README", "one\n", 0o644)
	head := up.commit("first")
	up.git("tag", "--annotate", "--message", "release", "v1.0")
	up.git("tag", "light")

	refs, err := git.New(&docker.ExecRunner{}).LsRemote(context.Background(), up.dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []git.Ref{
		{Name: "refs/heads/main", SHA: head},
		{Name: "refs/tags/light", SHA: head},
		{Name: "refs/tags/v1.0", SHA: head},
	}
	if !slices.Equal(refs, want) {
		t.Errorf("refs = %v\nwant %v", refs, want)
	}
}

// cloneAt makes a bare clone of up and returns it with the sha of HEAD.
func cloneAt(t *testing.T, up *upstream) (bare, sha string) {
	t.Helper()
	sha = up.commit("snapshot")
	bare = filepath.Join(t.TempDir(), "r.git")
	if err := git.New(&docker.ExecRunner{}).EnsureBare(context.Background(), up.dir, bare); err != nil {
		t.Fatal(err)
	}
	return bare, sha
}

func archiveTo(t *testing.T, bare, sha string) (string, error) {
	t.Helper()
	rc, err := git.New(&docker.ExecRunner{}).Archive(context.Background(), bare, sha)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	dest := t.TempDir()
	return dest, git.Unpack(rc, dest)
}

func TestArchiveUnpacksTheTree(t *testing.T) {
	isolateGit(t)
	up := newUpstream(t)
	up.write("README", "hello\n", 0o644)
	up.write("bin/run.sh", "#!/bin/sh\n", 0o755)
	up.write("deep/er/data.txt", "data\n", 0o600) // git records 0644
	up.symlink("latest", "deep/er/data.txt")
	up.symlink("deep/er/up", "../../README")
	bare, sha := cloneAt(t, up)

	dest, err := archiveTo(t, bare, sha)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]struct {
		content string
		mode    os.FileMode
	}{
		"README":           {"hello\n", 0o644},
		"bin/run.sh":       {"#!/bin/sh\n", 0o755},
		"deep/er/data.txt": {"data\n", 0o644},
	} {
		p := filepath.Join(dest, name)
		got, err := os.ReadFile(p)
		if err != nil || string(got) != want.content {
			t.Errorf("%s = %q, %v; want %q", name, got, err, want.content)
			continue
		}
		if fi, _ := os.Stat(p); fi.Mode().Perm() != want.mode {
			t.Errorf("%s mode = %v, want %v", name, fi.Mode().Perm(), want.mode)
		}
	}
	if fi, err := os.Stat(filepath.Join(dest, "deep")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("deep: %v, %v; want a 0755 directory", fi, err)
	}
	for link, want := range map[string]string{"latest": "deep/er/data.txt", "deep/er/up": "../../README"} {
		if got, err := os.Readlink(filepath.Join(dest, link)); err != nil || got != want {
			t.Errorf("%s -> %q, %v; want a symlink to %q", link, got, err, want)
		}
	}
}

func TestArchiveRefusesASymlinkOutOfTheTree(t *testing.T) {
	isolateGit(t)
	up := newUpstream(t)
	up.write("README", "hello\n", 0o644)
	up.symlink("escape", "../../outside")
	bare, sha := cloneAt(t, up)

	_, err := archiveTo(t, bare, sha)
	if err == nil || !strings.Contains(err.Error(), "points outside") {
		t.Errorf("err = %v, want a refusal of the escaping symlink", err)
	}
}

func TestArchiveOfUnknownCommitFails(t *testing.T) {
	isolateGit(t)
	up := newUpstream(t)
	up.write("README", "hello\n", 0o644)
	bare, _ := cloneAt(t, up)

	_, err := archiveTo(t, bare, strings.Repeat("0", 40))
	var exitErr *docker.ExitError
	if !errors.As(err, &exitErr) {
		t.Errorf("err = %v, want git's exit error", err)
	}
}
