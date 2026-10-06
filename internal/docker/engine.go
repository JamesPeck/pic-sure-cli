package docker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"
)

// Engine is the typed wrapper over the docker CLI that operations use for
// everything except compose: version and info, images, volumes, run, exec,
// cp, create, logs and labels. NewEngine returns the implementation, built
// on a Runner; ops.Deps.Docker holds one.
//
// Methods that query or change a docker object return an *ExitError when
// docker exits non-zero, and one that also matches ErrNotFound (errors.Is)
// when docker says the object doesn't exist. Removals treat a missing object
// as already removed. Methods that run a workload (Run, Start, Exec) return
// its exit code instead.
//
// Environment values never go in argv: Env and BuildArgs entries are
// NAME=value, docker gets a bare NAME in its argv, and the value travels in
// Cmd.Env. Names the docker CLI reads for itself (HOME, PATH,
// XDG_RUNTIME_DIR, SSH_AUTH_SOCK, DOCKER_*) are refused, because setting them
// in docker's own environment would change which daemon or config it uses.
type Engine interface {
	// Version runs `docker version`. If the daemon can't be reached it
	// returns the client half, a nil Server, and an error matching
	// ErrDaemonUnreachable.
	Version(ctx context.Context) (VersionInfo, error)
	// Info runs `docker info`. If the daemon can't be reached it returns
	// what the client knows (ClientInfo) and an error matching
	// ErrDaemonUnreachable.
	Info(ctx context.Context) (Info, error)

	// ImageExists reports whether the image is present locally.
	ImageExists(ctx context.Context, ref string) (bool, error)
	// ImageID returns the local image's ID (sha256:...).
	ImageID(ctx context.Context, ref string) (string, error)
	// ImageLabels returns the local image's labels, empty if it has none.
	ImageLabels(ctx context.Context, ref string) (map[string]string, error)
	// Build runs `docker build`, streaming its output.
	Build(ctx context.Context, opts BuildOpts) error
	// Pull pulls an image, copying docker's progress output to out (nil
	// discards it). Docker's error message goes in the returned error.
	Pull(ctx context.Context, ref string, out io.Writer) error
	// RemoveImage removes a local image. It fails if a container, even a
	// stopped one, uses it.
	RemoveImage(ctx context.Context, ref string) error

	// VolumeCreate creates a named volume with labels. Like `docker volume
	// create`, it succeeds without changing anything if the volume already
	// exists, whatever its labels: check with VolumeInspect first when that
	// matters.
	VolumeCreate(ctx context.Context, name string, labels map[string]string) error
	// VolumeInspect returns a volume, or an error matching ErrNotFound.
	VolumeInspect(ctx context.Context, name string) (Volume, error)
	// VolumeList returns the volumes carrying every given label filter, each
	// "KEY" or "KEY=VALUE", sorted by name. No filters lists every volume.
	VolumeList(ctx context.Context, labels ...string) ([]Volume, error)
	// VolumeRemove removes a volume. It fails if any container, even a
	// stopped one, uses it.
	VolumeRemove(ctx context.Context, name string) error
	// ContainersUsingVolume returns every container, running or stopped,
	// that mounts the volume.
	ContainersUsingVolume(ctx context.Context, name string) ([]Container, error)

	// Run runs a container and returns its exit code; with Detach, the exit
	// code of starting it. Exit code 125 means docker itself failed (bad
	// mount, missing image, name in use), so Run also returns an *ExitError
	// carrying docker's message.
	Run(ctx context.Context, opts RunOpts) (int, error)
	// Create creates a container from opts without starting it and returns
	// its ID. Detach, Stdin, Stdout and Stderr are Run-only and must be
	// unset.
	Create(ctx context.Context, opts RunOpts) (string, error)
	// Start starts a created container, streams its output, waits for it to
	// exit and returns its exit code.
	Start(ctx context.Context, container string, stdout, stderr io.Writer) (int, error)
	// Exec runs a command in a running container and returns its exit code.
	Exec(ctx context.Context, opts ExecOpts) (int, error)
	// CpFrom copies path out of a container, running or stopped, into
	// hostDir with `docker cp`. The copies belong to the invoking user. As
	// with docker cp, a path ending in "/." copies a directory's contents
	// rather than the directory, and hostDir is created if its parent
	// exists.
	CpFrom(ctx context.Context, container, path, hostDir string) error
	// Rm removes a container and its anonymous volumes; force also stops a
	// running one.
	Rm(ctx context.Context, container string, force bool) error
	// ContainerInspect returns a container's state, or an error matching
	// ErrNotFound.
	ContainerInspect(ctx context.Context, container string) (ContainerInfo, error)

	// Logs streams a container's stdout and stderr, merged; follow keeps
	// the stream open until the container stops. Close stops it early and
	// must always be called. If docker fails, the error comes from Read.
	Logs(ctx context.Context, container string, follow bool) io.ReadCloser
	// WaitForLogLine follows a container's logs, from the start, until a
	// line contains substr. It fails if timeout passes first or the
	// container stops without logging it.
	WaitForLogLine(ctx context.Context, container, substr string, timeout time.Duration) error
}

// NewEngine returns the Engine that drives the docker CLI through r. It is
// safe for concurrent use if r is.
func NewEngine(r Runner) Engine { return &cliEngine{r: r} }

type cliEngine struct{ r Runner }

// ErrNotFound matches errors for a docker object that doesn't exist.
var ErrNotFound = errors.New("not found")

// ErrDaemonUnreachable matches Version and Info errors when the docker CLI
// works but couldn't get an answer from the daemon.
var ErrDaemonUnreachable = errors.New("docker daemon unreachable")

// notFoundError is an *ExitError whose stderr says the object doesn't exist.
type notFoundError struct{ *ExitError }

func (e notFoundError) Is(target error) bool { return target == ErrNotFound }
func (e notFoundError) Unwrap() error        { return e.ExitError }

// daemonError is a Version or Info failure that got no answer from the
// daemon.
type daemonError struct{ err error }

func (e daemonError) Error() string        { return e.err.Error() }
func (e daemonError) Is(target error) bool { return target == ErrDaemonUnreachable }
func (e daemonError) Unwrap() error        { return e.err }

var notFoundRE = regexp.MustCompile(`(?i)no such (image|volume|container|object)`)

// run runs a docker command that queries or changes an object and turns a
// non-zero exit into an *ExitError, marked not-found when docker says so.
func (e *cliEngine) run(ctx context.Context, c Cmd) (Result, error) {
	res, err := e.r.Run(ctx, c)
	if err != nil {
		return res, err
	}
	if res.ExitCode != 0 {
		return res, exitError(c.Argv, res.ExitCode, res.Stderr)
	}
	return res, nil
}

// exitError builds the error for a failed docker command, leaving out the
// usage hint docker prints after its real message.
func exitError(argv []string, code int, stderr []byte) error {
	ee := &ExitError{Argv: argv, ExitCode: code, Stderr: trimUsageHint(stderr)}
	if notFoundRE.Match(ee.Stderr) {
		return notFoundError{ee}
	}
	return ee
}

// ignoreNotFound makes removing a missing object a success.
func ignoreNotFound(err error) error {
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// usageHintRE matches the line docker prints after an error, e.g. "Run
// 'docker run --help' for more information" (or "See 'docker run --help'."
// in older releases), which would otherwise be the message's last line.
var usageHintRE = regexp.MustCompile(`^(Run|See) 'docker [^']*--help'`)

func trimUsageHint(stderr []byte) []byte {
	lines := strings.Split(strings.TrimRight(string(stderr), "\n"), "\n")
	for len(lines) > 0 {
		last := strings.TrimSpace(lines[len(lines)-1])
		if last != "" && !usageHintRE.MatchString(last) {
			break
		}
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return nil
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

// stream runs a docker command with its output copied to the writers and
// keeps the end of stderr for the error message.
func (e *cliEngine) stream(ctx context.Context, c Cmd, stdout, stderr io.Writer) (int, []byte, error) {
	tail := &tailBuffer{max: maxTail}
	errW := io.Writer(tail)
	if stderr != nil {
		errW = io.MultiWriter(stderr, tail)
	}
	code, err := e.r.Stream(ctx, c, stdout, errW)
	return code, tail.Bytes(), err
}

// maxTail bounds how much of a streamed command's stderr is kept for its
// error message.
const maxTail = 4 << 10

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) Bytes() []byte { return bytes.Clone(t.buf) }
