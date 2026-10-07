package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// DefaultWaitDelay is ExecRunner's grace period when WaitDelay is unset.
const DefaultWaitDelay = 5 * time.Second

// ExecRunner is the production Runner. It runs each command as a real
// process, in a process group of its own and with a minimal environment.
// The zero value is ready to use, and an ExecRunner is safe for concurrent
// use.
//
// Environment. A child gets these variables from the runner's environment,
// then Cmd.Env, where a later entry wins over an earlier one with the same
// name:
//   - PATH, HOME, TERM and SSH_AUTH_SOCK;
//   - every variable whose name starts with DOCKER_ or XDG_;
//   - HTTP_PROXY, HTTPS_PROXY, NO_PROXY and ALL_PROXY, in upper or lower
//     case.
//
// Nothing else is inherited: COMPOSE_PROJECT_NAME, LANG or a token in the
// user's shell reach a child only if the caller puts them in Cmd.Env.
//
// Cancellation. When ctx ends, the runner sends SIGTERM to the command's
// process group, and SIGKILL to whatever is still in the group WaitDelay
// later. The call returns once the group is empty or has been killed, so
// nothing the command started (the compose plugin under docker, say)
// outlives it or keeps its output pipes open.
//
// Stdin. A Cmd.Stdin that isn't an *os.File is copied to the child by the
// runner. If ctx ends while a read from it is blocked, the call returns
// anyway and the read is left to finish on its own; a read error otherwise
// fails the call.
//
// Lingering pipes. If a command exits on its own but leaves a background
// process holding stdout or stderr, the runner stops reading WaitDelay
// after the exit and returns the command's own result. It leaves that
// process alone.
//
// Terminal. Children run in a background process group, so a child that
// opens /dev/tty to prompt (ssh for a passphrase, git for credentials) is
// stopped by SIGTTIN until ctx ends. Callers must turn such prompts off,
// for example with GIT_TERMINAL_PROMPT=0, or use Foreground.
//
// Logging. Each call logs its argv, dir and env names at debug level, and
// then its exit code and duration. Env values and stdin are never logged.
type ExecRunner struct {
	// Log receives the debug records; nil logs nothing.
	Log *slog.Logger
	// Environ is the environment the base environment is taken from; nil
	// means os.Environ() at each call.
	Environ []string
	// WaitDelay is the grace period described above; zero means
	// DefaultWaitDelay.
	WaitDelay time.Duration
	// Foreground is for an interactive command, such as `pic-sure compose
	// -- exec hpds sh`. The child stays in the CLI's process group, so it
	// can read from and control the terminal, and gets Ctrl-C from it
	// directly. Stream hands its writers to the child as they are, without
	// line buffering, so an *os.File such as the terminal becomes the
	// child's own stdout or stderr, and its guarantees about whole lines
	// and serialized writes don't hold. The runner never signals a
	// foreground child: Ctrl-C has already reached it, and a second signal
	// would count as a second Ctrl-C (compose's force-kill). The call waits
	// for the child to exit, then reports ctx's error if ctx ended first.
	Foreground bool
}

var _ Runner = (*ExecRunner)(nil)

// Run implements Runner. ExitCode is -1 when the process never started.
func (r *ExecRunner) Run(ctx context.Context, c Cmd) (Result, error) {
	var stdout, stderr bytes.Buffer
	code, err := r.run(ctx, c, &stdout, &stderr)
	return Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: code}, err
}

// Stream implements Runner. Each Write to stdout or stderr is one whole
// line, newline included, except for a final unterminated line and for
// lines longer than 64 KiB, which arrive in pieces. The two writers are
// never called concurrently, so they may be the same writer. A writer's
// error ends the copy and is returned; a writer that blocks keeps the call
// from returning, though the group is still killed on time. The exit code
// is -1 when the process never started.
func (r *ExecRunner) Stream(ctx context.Context, c Cmd, stdout, stderr io.Writer) (int, error) {
	if r.Foreground {
		return r.run(ctx, c, stdout, stderr)
	}
	var mu sync.Mutex
	outW, out := newLineWriter(stdout, &mu)
	errW, errOut := newLineWriter(stderr, &mu)
	code, err := r.run(ctx, c, outW, errW)
	for _, lw := range []*lineWriter{out, errOut} {
		if werr := lw.flush(); werr != nil && err == nil {
			err = fmt.Errorf("%s: writing output: %w", FormatArgv(c.Argv), werr)
		}
	}
	return code, err
}

func (r *ExecRunner) run(ctx context.Context, c Cmd, stdout, stderr io.Writer) (int, error) {
	if len(c.Argv) == 0 {
		return -1, errors.New("exec: empty argv")
	}
	argv := FormatArgv(c.Argv)
	delay := r.WaitDelay
	if delay <= 0 {
		delay = DefaultWaitDelay
	}

	if r.Foreground {
		return r.runForeground(ctx, c, stdout, stderr, delay)
	}
	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...)
	cmd.Env = append(baseEnv(r.environ()), c.Env...)
	cmd.Dir = c.Dir
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = delay
	// Cancel runs on exec's context watcher, which Wait synchronizes with,
	// so Wait's caller can read cancelled without a lock. The group is
	// killed on its own timer, not after Wait, because Wait can still be
	// blocked on a writer after the command has died.
	var cancelled bool
	killed := make(chan struct{})
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		cancelled = true
		go func() {
			killGroupBy(cmd.Process.Pid, time.Now().Add(delay))
			close(killed)
		}()
		return err
	}

	r.debug(ctx, "exec", "argv", argv, "dir", c.Dir, "env", envNames(c.Env))
	start := time.Now()
	stdin, err := startStdin(cmd, c.Stdin)
	if err != nil {
		return -1, fmt.Errorf("%s: %w", argv, err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.abandon()
		r.debug(ctx, "exec failed to start", "argv", argv, "err", err)
		if ctx.Err() != nil {
			return -1, ctxError(ctx, argv)
		}
		return -1, fmt.Errorf("%s: %w", argv, err)
	}
	stdin.started()
	waitErr := cmd.Wait()
	stdinErr := stdin.abandon()
	if cancelled {
		<-killed
	}
	code := exitCode(cmd.ProcessState)
	r.debug(ctx, "exec done", "argv", argv, "exit", code, "elapsed", time.Since(start).Round(time.Millisecond))

	var exitErr *exec.ExitError
	switch {
	case cancelled:
		return code, ctxError(ctx, argv)
	case waitErr == nil && stdinErr != nil:
		return code, fmt.Errorf("%s: reading stdin: %w", argv, stdinErr)
	case waitErr == nil, errors.As(waitErr, &exitErr):
		return code, nil
	case errors.Is(waitErr, exec.ErrWaitDelay):
		r.debug(ctx, "exec: output left open by a background process; stopped reading", "argv", argv)
		return code, nil
	default:
		return code, fmt.Errorf("%s: %w", argv, waitErr)
	}
}

// runForeground runs c in the CLI's process group, ignoring ctx until the
// child exits (see Foreground).
func (r *ExecRunner) runForeground(ctx context.Context, c Cmd, stdout, stderr io.Writer, delay time.Duration) (int, error) {
	argv := FormatArgv(c.Argv)
	if ctx.Err() != nil {
		return -1, ctxError(ctx, argv)
	}
	cmd := exec.Command(c.Argv[0], c.Argv[1:]...)
	cmd.Env = append(baseEnv(r.environ()), c.Env...)
	cmd.Dir = c.Dir
	cmd.Stdin = c.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = delay // after the exit, for lingering pipes and stdin
	r.debug(ctx, "exec in the foreground", "argv", argv, "dir", c.Dir, "env", envNames(c.Env))
	start := time.Now()
	err := cmd.Run()
	code := exitCode(cmd.ProcessState)
	if code == -1 && err != nil {
		return -1, fmt.Errorf("%s: %w", argv, err)
	}
	r.debug(ctx, "exec done", "argv", argv, "exit", code, "elapsed", time.Since(start).Round(time.Millisecond))
	var exitErr *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return code, ctxError(ctx, argv)
	case err == nil, errors.As(err, &exitErr), errors.Is(err, exec.ErrWaitDelay):
		return code, nil
	default:
		return code, fmt.Errorf("%s: %w", argv, err)
	}
}

// stdinCopy feeds a Cmd.Stdin that isn't an *os.File to the child through
// a pipe the runner owns. Left to os/exec, the copy would make Wait block
// until the reader returns, which for a stalled producer is never.
type stdinCopy struct {
	pr, pw *os.File
	src    io.Reader
	done   chan error
}

func startStdin(cmd *exec.Cmd, src io.Reader) (*stdinCopy, error) {
	if _, ok := src.(*os.File); ok || src == nil {
		cmd.Stdin = src
		return &stdinCopy{}, nil
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdin = pr
	return &stdinCopy{pr: pr, pw: pw, src: src, done: make(chan error, 1)}, nil
}

// started closes the child's end in the parent and starts the copy.
func (s *stdinCopy) started() {
	if s.pw == nil {
		return
	}
	_ = s.pr.Close()
	go func() {
		w := &pipeWriter{f: s.pw}
		_, err := io.Copy(w, s.src)
		if err != nil && err == w.err && (errors.Is(err, syscall.EPIPE) || errors.Is(err, os.ErrClosed)) {
			err = nil // the child stopped reading
		}
		s.done <- err // before the close, so a child that exits on EOF sees it
		_ = s.pw.Close()
	}()
}

// abandon closes the pipe once the child is done and returns the copy's read
// error if it has finished. A copy still blocked in src.Read is left
// behind; it ends when the read returns.
func (s *stdinCopy) abandon() error {
	if s.pw == nil {
		return nil
	}
	_ = s.pr.Close() // already closed if the child started
	_ = s.pw.Close()
	select {
	case err := <-s.done:
		return err
	default:
		return nil
	}
}

// pipeWriter remembers its last write error, so a write error can be told
// from the source's read error.
type pipeWriter struct {
	f   *os.File
	err error
}

func (w *pipeWriter) Write(p []byte) (int, error) {
	n, err := w.f.Write(p)
	if err != nil {
		w.err = err
	}
	return n, err
}

func (r *ExecRunner) environ() []string {
	if r.Environ != nil {
		return r.Environ
	}
	return os.Environ()
}

func (r *ExecRunner) debug(ctx context.Context, msg string, args ...any) {
	if r.Log != nil {
		r.Log.DebugContext(ctx, msg, args...)
	}
}

// baseEnvNames and baseEnvPrefixes are the variables ExecRunner passes
// through; keep its doc comment in step.
var (
	baseEnvNames = map[string]bool{
		"PATH": true, "HOME": true, "TERM": true, "SSH_AUTH_SOCK": true,
		"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true, "ALL_PROXY": true,
		"http_proxy": true, "https_proxy": true, "no_proxy": true, "all_proxy": true,
	}
	baseEnvPrefixes = []string{"DOCKER_", "XDG_"}
)

// baseEnv returns the entries of environ that ExecRunner passes through. It
// never returns nil, because a nil exec.Cmd.Env inherits everything.
func baseEnv(environ []string) []string {
	env := make([]string, 0, 16)
	for _, kv := range environ {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if baseEnvNames[name] || hasAnyPrefix(name, baseEnvPrefixes) {
			env = append(env, kv)
		}
	}
	return env
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func envNames(env []string) []string {
	names := make([]string, len(env))
	for i, kv := range env {
		names[i], _, _ = strings.Cut(kv, "=")
	}
	return names
}

// ctxError is the error for a command that ctx ended. It wraps ctx.Err()
// and, when the context was given one, its cause, so a signal's
// exitcode.Signaled survives.
func ctxError(ctx context.Context, argv string) error {
	err := ctx.Err()
	if cause := context.Cause(ctx); cause != nil && cause != err {
		return fmt.Errorf("%s: %w: %w", argv, err, cause)
	}
	return fmt.Errorf("%s: %w", argv, err)
}

// groupPollInterval is how often killGroupBy checks whether a cancelled
// group has emptied.
const groupPollInterval = 20 * time.Millisecond

// killGroupBy waits for the process group pgid to empty and sends SIGKILL to
// whatever is still in it at the deadline. A group ID isn't reused while
// any member, the unreaped leader included, is alive.
func killGroupBy(pgid int, deadline time.Time) {
	for syscall.Kill(-pgid, 0) == nil {
		wait := time.Until(deadline)
		if wait <= 0 {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			return
		}
		time.Sleep(min(wait, groupPollInterval))
	}
}

// exitCode is the process's exit status, or 128+N for death by signal N.
func exitCode(ps *os.ProcessState) int {
	if ps == nil {
		return -1
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}

// maxLine is the longest line lineWriter holds back waiting for its newline.
const maxLine = 64 << 10

// lineWriter passes whole lines to w, holding back a partial line until its
// newline arrives or flush is called. Writes to w happen under mu, which a
// Stream's two lineWriters share. After w fails, Write reports the error
// and passes nothing more on.
type lineWriter struct {
	w   io.Writer
	mu  *sync.Mutex
	buf []byte
	err error
}

// newLineWriter returns the writer to give exec (an untyped nil for a nil w,
// so exec discards the stream) and the lineWriter to flush afterwards.
func newLineWriter(w io.Writer, mu *sync.Mutex) (io.Writer, *lineWriter) {
	if w == nil {
		return nil, &lineWriter{}
	}
	lw := &lineWriter{w: w, mu: mu}
	return lw, lw
}

func (l *lineWriter) Write(p []byte) (int, error) {
	if l.err != nil {
		return 0, l.err
	}
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			l.buf = append(l.buf, p...)
			if len(l.buf) >= maxLine {
				l.emit(l.buf)
				l.buf = l.buf[:0]
			}
			break
		}
		line := p[:i+1]
		p = p[i+1:]
		if len(l.buf) > 0 {
			line = append(l.buf, line...)
			l.buf = line[:0]
		}
		l.emit(line)
		if l.err != nil {
			break
		}
	}
	if l.err != nil {
		return 0, l.err
	}
	return n, nil
}

// flush passes on a final partial line and returns the first write error.
func (l *lineWriter) flush() error {
	if len(l.buf) > 0 && l.err == nil {
		l.emit(l.buf)
		l.buf = nil
	}
	return l.err
}

func (l *lineWriter) emit(b []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.w.Write(b); err != nil {
		l.err = err
	}
}
