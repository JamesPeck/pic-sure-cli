package git_test

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/git"
)

func TestEveryCallDisablesPromptsAndCarriesCallerEnv(t *testing.T) {
	t.Setenv("SSH_ASKPASS", "")
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("git ls-remote *"))
	base := git.New(f)
	proxied := base.WithEnv("HTTPS_PROXY=http://proxy:3128", "NO_PROXY=localhost")

	ctx := context.Background()
	if _, err := proxied.LsRemote(ctx, "https://example.com/r.git"); err != nil {
		t.Fatal(err)
	}
	if _, err := base.LsRemote(ctx, "https://example.com/r.git"); err != nil {
		t.Fatal(err)
	}

	calls := f.Calls()
	noPrompts := []string{"GIT_TERMINAL_PROMPT", "SSH_ASKPASS_REQUIRE", "SSH_ASKPASS"}
	if want := append(noPrompts, "HTTPS_PROXY", "NO_PROXY"); !slices.Equal(calls[0].Env, want) {
		t.Errorf("proxied client env = %v, want %v", calls[0].Env, want)
	}
	if !slices.Equal(calls[1].Env, noPrompts) {
		t.Errorf("WithEnv changed the original client: env = %v, want %v", calls[1].Env, noPrompts)
	}
}

func TestTheUsersOwnAskpassIsKept(t *testing.T) {
	t.Setenv("SSH_ASKPASS", "/usr/local/bin/my-askpass")
	t.Setenv("DISPLAY", ":0")
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("git ls-remote *"))
	if _, err := git.New(f).LsRemote(context.Background(), "https://example.com/r.git"); err != nil {
		t.Fatal(err)
	}
	// The runner doesn't pass them on, so the client must.
	if want := []string{"GIT_TERMINAL_PROMPT", "SSH_ASKPASS_REQUIRE", "SSH_ASKPASS", "DISPLAY"}; !slices.Equal(f.Calls()[0].Env, want) {
		t.Errorf("env = %v, want %v", f.Calls()[0].Env, want)
	}
}

func TestFetchArgv(t *testing.T) {
	for _, tc := range []struct {
		name     string
		refspecs []string
		tags     bool
		want     string
	}{
		{"every branch", nil, false,
			"git --git-dir=/c/r.git fetch --quiet --prune --no-tags origin +refs/heads/*:refs/heads/*"},
		{"branches and tags", nil, true,
			"git --git-dir=/c/r.git fetch --quiet --prune --no-tags origin +refs/heads/*:refs/heads/* +refs/tags/*:refs/tags/*"},
		{"one commit", []string{"0123abcd"}, false,
			"git --git-dir=/c/r.git fetch --quiet --prune --no-tags origin 0123abcd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fakerunner.New(t)
			f.On(fakerunner.Glob("git *"))
			if err := git.New(f).Fetch(context.Background(), "/c/r.git", tc.refspecs, tc.tags); err != nil {
				t.Fatal(err)
			}
			if got := f.Calls()[0].String(); got != tc.want {
				t.Errorf("argv = %s\nwant   %s", got, tc.want)
			}
		})
	}
}

func TestFetchDoesNotAppendIntoTheCallersRefspecs(t *testing.T) {
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("git *"))
	backing := make([]string, 1, 2)
	backing[0] = "+refs/heads/main:refs/heads/main"
	if err := git.New(f).Fetch(context.Background(), "/c/r.git", backing, true); err != nil {
		t.Fatal(err)
	}
	if extra := backing[:2][1]; extra != "" {
		t.Errorf("Fetch wrote %q into the caller's spare capacity", extra)
	}
}

func TestResolveRefErrors(t *testing.T) {
	ctx := context.Background()

	f := fakerunner.New(t)
	f.On(fakerunner.Glob("git * rev-parse *")).Exit(1)
	_, err := git.New(f).ResolveRef(ctx, "/c/r.git", "v9")
	if !errors.Is(err, git.ErrUnknownRef) {
		t.Errorf("exit 1, empty stderr: err = %v, want ErrUnknownRef", err)
	}

	f = fakerunner.New(t)
	f.On(fakerunner.Glob("git * rev-parse *")).Exit(128).Stderr("fatal: not a git repository: '/c/r.git'\n")
	_, err = git.New(f).ResolveRef(ctx, "/c/r.git", "main")
	var exitErr *docker.ExitError
	if errors.Is(err, git.ErrUnknownRef) || !errors.As(err, &exitErr) || !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("broken repo: err = %v, want an ExitError carrying git's message", err)
	}
}

func TestLsRemotePeelsAnnotatedTags(t *testing.T) {
	f := fakerunner.New(t)
	f.On(fakerunner.Exact("git", "ls-remote", "--heads", "--tags", "https://example.com/r.git")).Stdout(
		"aaaa\trefs/heads/main\n" +
			"bbbb\trefs/tags/light\n" +
			"cccc\trefs/tags/v1.0\n" +
			"dddd\trefs/tags/v1.0^{}\n")
	refs, err := git.New(f).LsRemote(context.Background(), "https://example.com/r.git")
	if err != nil {
		t.Fatal(err)
	}
	want := []git.Ref{
		{Name: "refs/heads/main", SHA: "aaaa"},
		{Name: "refs/tags/light", SHA: "bbbb"},
		{Name: "refs/tags/v1.0", SHA: "dddd"},
	}
	if !slices.Equal(refs, want) {
		t.Errorf("refs = %v, want %v", refs, want)
	}
}

func TestArgumentsThatLookLikeOptionsAreRefused(t *testing.T) {
	f := fakerunner.New(t) // no rules: any git call fails the test
	c := git.New(f)
	ctx := context.Background()
	if _, err := c.LsRemote(ctx, "--upload-pack=touch /tmp/pwned"); err == nil {
		t.Error("LsRemote accepted an option as the URL")
	}
	if _, err := c.ResolveRef(ctx, "/c/r.git", "--all"); err == nil {
		t.Error("ResolveRef accepted an option as the ref")
	}
	if err := c.Fetch(ctx, "/c/r.git", []string{"--upload-pack=x"}, false); err == nil {
		t.Error("Fetch accepted an option as a refspec")
	}
	if _, err := c.Archive(ctx, "/c/r.git", "--output=/tmp/x"); err == nil {
		t.Error("Archive accepted an option as the sha")
	}
	if err := c.EnsureBare(ctx, "-u", t.TempDir()+"/r.git"); err == nil {
		t.Error("EnsureBare accepted an option as the URL")
	}
}

func TestArchiveSurfacesGitFailureFromRead(t *testing.T) {
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("git * archive *")).Exit(128).Stderr("fatal: not a valid object name: deadbeef\n")
	rc, err := git.New(f).Archive(context.Background(), "/c/r.git", "deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	_, err = io.ReadAll(rc)
	if err == nil || !strings.Contains(err.Error(), "not a valid object name") {
		t.Errorf("read err = %v, want git's failure", err)
	}
}

func TestArchiveCloseStopsGit(t *testing.T) {
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("git * archive *")).Do(func(ctx context.Context, _ fakerunner.Call) (docker.Result, error) {
		<-ctx.Done() // git still writing when the reader gives up
		return docker.Result{}, ctx.Err()
	})
	rc, err := git.New(f).Archive(context.Background(), "/c/r.git", "deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() { _ = rc.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not stop git")
	}
}
