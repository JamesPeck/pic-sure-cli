package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
)

// Client is the git operations the CLI uses. Repositories are bare clones
// that this package manages; dir is always the bare repository itself,
// except in WorkTree.
type Client interface {
	// EnsureBare makes dir a bare clone of url. A missing dir is cloned,
	// with every branch and tag, into a temporary sibling that is renamed
	// into place, so an interrupted clone never leaves a half-made dir. An
	// existing dir only has its origin URL set to url; call Fetch to update
	// it.
	EnsureBare(ctx context.Context, url, dir string) error
	// Fetch updates dir from its origin. Nil refspecs means every branch
	// (+refs/heads/*:refs/heads/*). With tags, every tag is fetched too,
	// and a tag that moved upstream is moved here. Branches and tags
	// deleted upstream are pruned.
	Fetch(ctx context.Context, dir string, refspecs []string, tags bool) error
	// ResolveRef returns the full commit sha that ref names in dir: a tag
	// (annotated tags are peeled), a branch, or a full or abbreviated sha.
	// A tag wins over a branch of the same name. If dir has no such commit,
	// the error wraps ErrUnknownRef.
	ResolveRef(ctx context.Context, dir, ref string) (string, error)
	// WorkTree reports the checked-out commit of the working tree at dir,
	// a user's checkout rather than a managed clone, and whether it has
	// changes: modified, staged or untracked (not ignored) files.
	WorkTree(ctx context.Context, dir string) (WorkTree, error)
	// LsRemote lists the branches and tags at url without cloning it.
	LsRemote(ctx context.Context, url string) ([]Ref, error)
	// Archive streams the tree at sha in dir as a tar archive (`git
	// archive`), with files at mode 0644 or 0755. Read it to the end to see
	// git's exit status: a failure surfaces as the error from Read. Close
	// stops git if it is still running. Unpack writes the archive to disk.
	Archive(ctx context.Context, dir, sha string) (io.ReadCloser, error)
	// WithEnv returns a Client that adds env (NAME=value entries) to every
	// git it runs, on top of this Client's own. Callers use it for the
	// proxy variables (§9.10).
	WithEnv(env ...string) Client
}

// Ref is one ref that LsRemote found.
type Ref struct {
	// Name is the full ref name, such as refs/heads/main or refs/tags/v1.0.
	Name string
	// SHA is the commit the ref points to. For an annotated tag it is the
	// tagged commit, not the tag object.
	SHA string
}

// ErrUnknownRef is wrapped by ResolveRef's error when the repository has no
// commit by that name, which a Fetch may fix.
var ErrUnknownRef = errors.New("unknown git ref")

// New returns a Client that runs the user's git through r, so their
// credential helpers, SSH setup and URL rewrites apply.
func New(r docker.Runner) Client {
	// Neither git nor ssh may wait for typed input: the TUI owns the
	// terminal, and a child outside the foreground process group is stopped
	// when it reads from it. GIT_TERMINAL_PROMPT=0 makes git fail instead.
	// SSH_ASKPASS_REQUIRE=force sends ssh's passphrase and host-key prompts
	// to the askpass program, which fails unless the user has their own.
	// Credential helpers and ssh-agent keys still work. The runner passes
	// on only a few variables, so the user's askpass, and the display a
	// graphical one needs, go through Cmd.Env.
	env := []string{"GIT_TERMINAL_PROMPT=0", "SSH_ASKPASS_REQUIRE=force"}
	if askpass := os.Getenv("SSH_ASKPASS"); askpass != "" {
		env = append(env, "SSH_ASKPASS="+askpass)
		if display := os.Getenv("DISPLAY"); display != "" {
			env = append(env, "DISPLAY="+display)
		}
	} else {
		env = append(env, "SSH_ASKPASS=false")
	}
	return &client{runner: r, env: env}
}

type client struct {
	runner docker.Runner
	env    []string
}

func (c *client) WithEnv(env ...string) Client {
	return &client{runner: c.runner, env: append(append([]string(nil), c.env...), env...)}
}

func (c *client) cmd(args ...string) docker.Cmd {
	return docker.Cmd{Argv: append([]string{"git"}, args...), Env: c.env}
}

func (c *client) run(ctx context.Context, args ...string) ([]byte, error) {
	res, err := docker.RunChecked(ctx, c.runner, c.cmd(args...))
	return res.Stdout, err
}

func (c *client) EnsureBare(ctx context.Context, url, dir string) error {
	if err := checkArgs(url, dir); err != nil {
		return err
	}
	switch _, err := os.Stat(dir); {
	case err == nil:
		_, err := c.run(ctx, "--git-dir="+dir, "config", "remote.origin.url", url)
		return err
	case !errors.Is(err, os.ErrNotExist):
		return err
	}

	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, filepath.Base(dir)+".tmp-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }() // a no-op once renamed
	if _, err := c.run(ctx, "clone", "--bare", "--quiet", "--origin=origin", url, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, dir); err != nil {
		// Someone else cloned it first; theirs is as good as ours.
		if _, statErr := os.Stat(dir); statErr == nil {
			return nil
		}
		return err
	}
	return nil
}

func (c *client) Fetch(ctx context.Context, dir string, refspecs []string, tags bool) error {
	if len(refspecs) == 0 {
		refspecs = []string{"+refs/heads/*:refs/heads/*"}
	}
	if tags {
		// An explicit forced refspec, unlike --tags, also moves a tag that
		// was moved upstream.
		refspecs = append(refspecs[:len(refspecs):len(refspecs)], "+refs/tags/*:refs/tags/*")
	}
	if err := checkArgs(append([]string{dir}, refspecs...)...); err != nil {
		return err
	}
	args := append([]string{"--git-dir=" + dir, "fetch", "--quiet", "--prune", "--no-tags", "origin"}, refspecs...)
	_, err := c.run(ctx, args...)
	return err
}

func (c *client) ResolveRef(ctx context.Context, dir, ref string) (string, error) {
	if err := checkArgs(dir, ref); err != nil {
		return "", err
	}
	cmd := c.cmd("--git-dir="+dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	res, err := c.runner.Run(ctx, cmd)
	if err != nil {
		return "", err
	}
	switch {
	case res.ExitCode == 0:
		return strings.TrimSpace(string(res.Stdout)), nil
	case res.ExitCode == 1 && len(bytes.TrimSpace(res.Stderr)) == 0:
		// With --quiet, exit 1 and nothing on stderr means no such commit.
		return "", fmt.Errorf("%w %q in %s", ErrUnknownRef, ref, dir)
	default:
		return "", &docker.ExitError{Argv: cmd.Argv, ExitCode: res.ExitCode, Stderr: res.Stderr}
	}
}

// WorkTree is the state of a user's checkout.
type WorkTree struct {
	Head  string // the full commit sha HEAD names
	Dirty bool   // there are uncommitted or untracked changes
}

func (c *client) WorkTree(ctx context.Context, dir string) (WorkTree, error) {
	if err := checkArgs(dir); err != nil {
		return WorkTree{}, err
	}
	// --no-optional-locks keeps status from taking index.lock to refresh
	// the index, which would collide with the user's own git.
	head, err := c.run(ctx, "--no-optional-locks", "-C", dir, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return WorkTree{}, err
	}
	// --untracked-files and --ignore-submodules override the user's
	// status.showUntrackedFiles and diff.ignoreSubmodules.
	status, err := c.run(ctx, "--no-optional-locks", "-C", dir, "status", "--porcelain", "-z", "--untracked-files=normal", "--ignore-submodules=none")
	if err != nil {
		return WorkTree{}, err
	}
	return WorkTree{Head: strings.TrimSpace(string(head)), Dirty: len(status) > 0}, nil
}

func (c *client) LsRemote(ctx context.Context, url string) ([]Ref, error) {
	if err := checkArgs(url); err != nil {
		return nil, err
	}
	out, err := c.run(ctx, "ls-remote", "--heads", "--tags", url)
	if err != nil {
		return nil, err
	}
	return parseLsRemote(out)
}

// parseLsRemote parses `<sha>\t<ref>` lines. An annotated tag is listed
// twice, the second time as `<ref>^{}` with the commit it tags.
func parseLsRemote(out []byte) ([]Ref, error) {
	var refs []Ref
	index := map[string]int{}
	for line := range strings.Lines(string(out)) {
		line = strings.TrimRight(line, "\n")
		if line == "" {
			continue
		}
		sha, name, ok := strings.Cut(line, "\t")
		if !ok {
			return nil, fmt.Errorf("git ls-remote: unexpected line %q", line)
		}
		if tag, peeled := strings.CutSuffix(name, "^{}"); peeled {
			if i, ok := index[tag]; ok {
				refs[i].SHA = sha
				continue
			}
			name = tag
		}
		index[name] = len(refs)
		refs = append(refs, Ref{Name: name, SHA: sha})
	}
	return refs, nil
}

func (c *client) Archive(ctx context.Context, dir, sha string) (io.ReadCloser, error) {
	if err := checkArgs(dir, sha); err != nil {
		return nil, err
	}
	// The tree must not depend on the user's git config: tar.umask would
	// change the modes (git's default gives 0664 and 0775), and core.autocrlf
	// or core.eol would give text files CRLF line endings.
	cmd := c.cmd("-c", "tar.umask=022", "-c", "core.autocrlf=false", "-c", "core.eol=lf",
		"--git-dir="+dir, "archive", "--format=tar", sha)
	ctx, cancel := context.WithCancel(ctx)
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		var stderr bytes.Buffer
		code, err := c.runner.Stream(ctx, cmd, pw, &stderr)
		if err == nil && code != 0 {
			err = &docker.ExitError{Argv: cmd.Argv, ExitCode: code, Stderr: stderr.Bytes()}
		}
		pw.CloseWithError(err) // nil gives the reader io.EOF
	}()
	return &archive{PipeReader: pr, cancel: cancel, done: done}, nil
}

type archive struct {
	*io.PipeReader
	cancel context.CancelFunc
	done   chan struct{}
}

// Close stops git if it is still running and waits for it to exit.
func (a *archive) Close() error {
	a.cancel()
	_ = a.PipeReader.Close()
	<-a.done
	return nil
}

// checkArgs refuses arguments that git would parse as options.
func checkArgs(args ...string) error {
	for _, a := range args {
		if a == "" || strings.HasPrefix(a, "-") {
			return fmt.Errorf("git: invalid argument %q", a)
		}
	}
	return nil
}
