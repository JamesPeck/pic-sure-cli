package cache_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/git"
)

// The test binary doubles as a second pic-sure process: with helperEnv set
// it runs one cache call instead of the tests.
const (
	helperEnv     = "PICSURE_CACHE_TEST_HELPER"
	helperRoot    = "PICSURE_CACHE_TEST_ROOT"
	helperSHA     = "PICSURE_CACHE_TEST_SHA"
	archiveLogEnv = "PICSURE_CACHE_TEST_ARCHIVE_LOG"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperEnv); mode != "" {
		if err := runHelper(mode); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runHelper(mode string) error {
	r := &countingRunner{log: os.Getenv(archiveLogEnv)}
	c, err := cache.Open(os.Getenv(helperRoot), cache.Options{Git: git.New(r), Holder: "helper"})
	if err != nil {
		return err
	}
	ctx := context.Background()
	switch mode {
	case "ensure-source":
		dir, err := c.EnsureSource(ctx, "pic-sure", os.Getenv(helperSHA))
		if err != nil {
			return err
		}
		fmt.Println(dir)
	case "hold-reactor":
		lock, err := c.LockReactor(ctx)
		if err != nil {
			return err
		}
		fmt.Println("locked")
		_, _ = io.Copy(io.Discard, os.Stdin) // hold until the test closes stdin
		return lock.Unlock()
	default:
		return fmt.Errorf("unknown helper mode %q", mode)
	}
	return nil
}

// helper starts the test binary as a helper process.
func helper(t *testing.T, mode, root string, env ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), append([]string{helperEnv + "=" + mode, helperRoot + "=" + root}, env...)...)
	return cmd
}

// startHolder starts a helper process that holds root's reactor lock, and
// returns it once it has the lock. Closing its stdin releases the lock.
func startHolder(t *testing.T, root string) (*exec.Cmd, io.WriteCloser) {
	t.Helper()
	cmd := helper(t, "hold-reactor", root)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Wait() })
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "locked\n" {
		t.Fatalf("holder said %q, %v", line, err)
	}
	return cmd, stdin
}

// countingRunner runs commands for real and counts them, and the `git
// archive` runs among them. With log set, it also appends a line to that
// file for each archive, so that processes can be counted together.
type countingRunner struct {
	calls, archives atomic.Int32
	log             string
}

func (r *countingRunner) Run(ctx context.Context, c docker.Cmd) (docker.Result, error) {
	var stdout, stderr bytes.Buffer
	code, err := r.Stream(ctx, c, &stdout, &stderr)
	return docker.Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: code}, err
}

func (r *countingRunner) Stream(ctx context.Context, c docker.Cmd, stdout, stderr io.Writer) (int, error) {
	r.calls.Add(1)
	if slices.Contains(c.Argv, "archive") {
		r.archives.Add(1)
		if r.log != "" {
			f, err := os.OpenFile(r.log, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
			if err != nil {
				return -1, err
			}
			_, _ = fmt.Fprintln(f, os.Getpid())
			_ = f.Close()
		}
	}
	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...)
	cmd.Env = append(os.Environ(), c.Env...)
	cmd.Dir, cmd.Stdin, cmd.Stdout, cmd.Stderr = c.Dir, c.Stdin, stdout, stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return -1, ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return 0, err
}

// upstream is a non-bare repository standing in for github.com/hms-dbmi/pic-sure.
type upstream struct {
	t   *testing.T
	dir string
}

// newUpstream skips the test without git, keeps the user's git config out
// of it, and makes git fetch pic-sure's clone URL from a new local
// repository instead of the network.
func newUpstream(t *testing.T) *upstream {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	u := &upstream{t: t, dir: t.TempDir()}
	config := filepath.Join(t.TempDir(), "gitconfig")
	rewrite := fmt.Sprintf("[url %q]\n\tinsteadOf = https://github.com/hms-dbmi/pic-sure.git\n", "file://"+u.dir)
	if err := os.WriteFile(config, []byte(rewrite), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", config)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_ALLOW_PROTOCOL", "file") // never reach the network
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
	for _, k := range []string{"GIT_AUTHOR", "GIT_COMMITTER"} {
		t.Setenv(k+"_NAME", "Test")
		t.Setenv(k+"_EMAIL", "test@example.com")
	}
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

// commit writes files (name → content) and commits them, returning the
// commit's sha.
func (u *upstream) commit(files map[string]string) string {
	u.t.Helper()
	for name, content := range files {
		p := filepath.Join(u.dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			u.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			u.t.Fatal(err)
		}
	}
	u.git("add", "--all")
	u.git("commit", "--quiet", "--message", "test")
	return u.git("rev-parse", "HEAD")
}

// open opens a cache in a new temporary directory.
func open(t *testing.T, opts cache.Options) *cache.Cache {
	t.Helper()
	c, err := cache.Open(t.TempDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// entries lists the names in dir, which may not exist.
func entries(t *testing.T, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var names []string
	for _, de := range des {
		names = append(names, de.Name())
	}
	return names
}
