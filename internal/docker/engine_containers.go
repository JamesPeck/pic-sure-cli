package docker

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// RunOpts is a container for Run, or for Create, which takes the same
// options apart from the Run-only ones.
type RunOpts struct {
	Image string
	// Args are the command and its arguments, after the image.
	Args []string
	// Name is the container's name; UniqueName makes one that can't collide
	// with another run's. Empty lets docker pick one.
	Name string
	// Remove deletes the container, and its anonymous volumes, when it
	// exits (--rm).
	Remove     bool
	User       string
	Workdir    string
	Entrypoint string
	Network    string
	// NetworkAliases are extra names for the container on Network.
	NetworkAliases []string
	Mounts         []Mount
	// Env holds NAME=value entries. Docker gets a bare -e NAME and the
	// value through its environment, so values never appear in argv.
	Env    []string
	Labels map[string]string

	// Run only.

	// Detach starts the container in the background (-d); Run returns once
	// it has started.
	Detach bool
	// Stdin, when set, is attached to the container's stdin (-i).
	Stdin io.Reader
	// Stdout and Stderr receive the container's output; nil discards it.
	Stdout, Stderr io.Writer
}

// Mount is a volume or bind mount, passed as -v SOURCE:TARGET[:ro].
type Mount struct {
	// Source is an absolute host path, for a bind mount, or a volume name.
	// A bind source must exist and must not contain ':'.
	Source string
	// Target is the absolute path in the container.
	Target   string
	ReadOnly bool
}

// ExecOpts is a command for Exec.
type ExecOpts struct {
	Container string
	Args      []string
	User      string
	Workdir   string
	// Env holds NAME=value entries, passed as for RunOpts.Env.
	Env []string
	// Stdin, when set, is attached to the command's stdin (-i).
	Stdin io.Reader
	// Stdout and Stderr receive the command's output; nil discards it.
	Stdout, Stderr io.Writer
}

// ContainerInfo is a container's state, from `docker container inspect`.
type ContainerInfo struct {
	ID   string
	Name string
	// Image is the reference the container was created from; ImageID is
	// the image it runs.
	Image   string
	ImageID string
	Labels  map[string]string
	// Status is e.g. "created", "running" or "exited".
	Status   string
	Running  bool
	ExitCode int
	// Health is "starting", "healthy" or "unhealthy", or empty when the
	// container has no healthcheck. Compare it exactly: "unhealthy"
	// contains "healthy".
	Health string
}

// UniqueName returns prefix followed by "-" and 8 random hex digits read
// from rand, for a container name that another run can't be using. Pass
// ops.Deps.Rand, so tests can make it predictable.
func UniqueName(prefix string, rand io.Reader) (string, error) {
	var b [4]byte
	if _, err := io.ReadFull(rand, b[:]); err != nil {
		return "", fmt.Errorf("container name: %w", err)
	}
	return prefix + "-" + hex.EncodeToString(b[:]), nil
}

func (e *cliEngine) Run(ctx context.Context, opts RunOpts) (int, error) {
	flags, env, err := containerFlags(opts)
	if err != nil {
		return 0, fmt.Errorf("docker run: %w", err)
	}
	argv := []string{"docker", "run"}
	if opts.Detach {
		argv = append(argv, "-d")
	}
	if opts.Stdin != nil {
		argv = append(argv, "-i")
	}
	argv = append(append(argv, flags...), opts.Image)
	argv = append(argv, opts.Args...)

	c := Cmd{Argv: argv, Env: env, Stdin: opts.Stdin}
	code, stderr, err := e.stream(ctx, c, opts.Stdout, opts.Stderr)
	if err != nil {
		return code, err
	}
	if code == 125 { // docker run's code for its own failure
		return code, exitError(argv, code, stderr)
	}
	return workloadResult(argv, code, stderr)
}

func (e *cliEngine) Create(ctx context.Context, opts RunOpts) (string, error) {
	if opts.Detach || opts.Stdin != nil || opts.Stdout != nil || opts.Stderr != nil {
		return "", errors.New("docker create: Detach, Stdin, Stdout and Stderr apply only to Run")
	}
	flags, env, err := containerFlags(opts)
	if err != nil {
		return "", fmt.Errorf("docker create: %w", err)
	}
	argv := append(append([]string{"docker", "create"}, flags...), opts.Image)
	res, err := e.run(ctx, Cmd{Argv: append(argv, opts.Args...), Env: env})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(res.Stdout)), nil
}

// containerFlags renders the options Run and Create share, checking the
// mounts and the environment first. The returned env goes in Cmd.Env.
func containerFlags(opts RunOpts) (flags, env []string, err error) {
	if opts.Image == "" {
		return nil, nil, errors.New("no image")
	}
	names, env, err := splitEnv(opts.Env)
	if err != nil {
		return nil, nil, fmt.Errorf("env %w", err)
	}
	for _, m := range opts.Mounts {
		if err := m.validate(); err != nil {
			return nil, nil, err
		}
	}

	if opts.Remove {
		flags = append(flags, "--rm")
	}
	for _, f := range []struct{ flag, value string }{
		{"--name", opts.Name},
		{"--user", opts.User},
		{"-w", opts.Workdir},
		{"--entrypoint", opts.Entrypoint},
		{"--network", opts.Network},
	} {
		if f.value != "" {
			flags = append(flags, f.flag, f.value)
		}
	}
	for _, a := range opts.NetworkAliases {
		flags = append(flags, "--network-alias", a)
	}
	flags = append(flags, labelArgs(opts.Labels)...)
	for _, n := range names {
		flags = append(flags, "-e", n)
	}
	for _, m := range opts.Mounts {
		flags = append(flags, "-v", m.arg())
	}
	return flags, env, nil
}

// volumeNameRE is docker's rule for a local volume name.
var volumeNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]+$`)

func (m Mount) validate() error {
	if !path.IsAbs(m.Target) || strings.Contains(m.Target, ":") {
		return fmt.Errorf("mount target %q must be an absolute container path without ':'", m.Target)
	}
	if !filepath.IsAbs(m.Source) {
		if !volumeNameRE.MatchString(m.Source) {
			return fmt.Errorf("mount source %q is neither an absolute host path nor a volume name", m.Source)
		}
		return nil
	}
	if strings.Contains(m.Source, ":") {
		return fmt.Errorf("bind mount source %q contains ':', which docker -v can't express", m.Source)
	}
	// Docker would create a missing bind source as an empty, root-owned
	// directory instead of failing.
	if _, err := os.Stat(m.Source); err != nil {
		return fmt.Errorf("bind mount source: %w", err)
	}
	return nil
}

func (m Mount) arg() string {
	s := m.Source + ":" + m.Target
	if m.ReadOnly {
		s += ":ro"
	}
	return s
}

// envNameRE is a portable environment variable name.
var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// splitEnv splits NAME=value entries into the bare names for argv and the
// entries for Cmd.Env.
func splitEnv(entries []string) (names, env []string, err error) {
	for i, kv := range entries {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || !envNameRE.MatchString(name) {
			// A malformed entry may be a bare secret, so none of it goes in the error.
			return nil, nil, fmt.Errorf("entry %d is not NAME=value", i)
		}
		if dockerOwnsEnv(name) {
			return nil, nil, fmt.Errorf("%s: the docker CLI reads it for itself, so it can't be passed through docker's environment", name)
		}
		names = append(names, name)
	}
	return names, entries, nil
}

// dockerOwnsEnv reports whether the docker CLI reads name to find its
// config, credential helpers, context or daemon.
func dockerOwnsEnv(name string) bool {
	switch name {
	case "HOME", "PATH", "XDG_RUNTIME_DIR", "SSH_AUTH_SOCK":
		return true
	}
	return strings.HasPrefix(name, "DOCKER_")
}

func (e *cliEngine) Start(ctx context.Context, container string, stdout, stderr io.Writer) (int, error) {
	argv := []string{"docker", "start", "-a", container}
	code, tail, err := e.stream(ctx, Cmd{Argv: argv}, stdout, stderr)
	if err != nil {
		return code, err
	}
	return workloadResult(argv, code, tail)
}

func (e *cliEngine) Exec(ctx context.Context, opts ExecOpts) (int, error) {
	if opts.Container == "" || len(opts.Args) == 0 {
		return 0, errors.New("docker exec: needs a container and a command")
	}
	names, env, err := splitEnv(opts.Env)
	if err != nil {
		return 0, fmt.Errorf("docker exec: env %w", err)
	}
	argv := []string{"docker", "exec"}
	if opts.Stdin != nil {
		argv = append(argv, "-i")
	}
	if opts.User != "" {
		argv = append(argv, "--user", opts.User)
	}
	if opts.Workdir != "" {
		argv = append(argv, "-w", opts.Workdir)
	}
	for _, n := range names {
		argv = append(argv, "-e", n)
	}
	argv = append(append(argv, opts.Container), opts.Args...)
	code, tail, err := e.stream(ctx, Cmd{Argv: argv, Env: env, Stdin: opts.Stdin}, opts.Stdout, opts.Stderr)
	if err != nil {
		return code, err
	}
	return workloadResult(argv, code, tail)
}

func (e *cliEngine) CpFrom(ctx context.Context, container, ctrPath, dest string) error {
	if !path.IsAbs(ctrPath) {
		return fmt.Errorf("docker cp: container path %q is not absolute", ctrPath)
	}
	if !filepath.IsAbs(dest) {
		return fmt.Errorf("docker cp: host path %q is not absolute", dest)
	}
	_, err := e.run(ctx, Cmd{Argv: []string{"docker", "cp", container + ":" + ctrPath, dest}})
	return err
}

func (e *cliEngine) Rm(ctx context.Context, container string, force bool) error {
	argv := []string{"docker", "rm", "-v"}
	if force {
		argv = append(argv, "-f")
	}
	_, err := e.run(ctx, Cmd{Argv: append(argv, container)})
	return ignoreNotFound(err)
}

type containerInspect struct {
	ID     string `json:"Id"`
	Name   string
	Image  string
	Config struct {
		Image  string
		Labels map[string]string
	}
	State struct {
		Status   string
		Running  bool
		ExitCode int
		Health   *struct{ Status string }
	}
}

func (e *cliEngine) ContainerInspect(ctx context.Context, container string) (ContainerInfo, error) {
	res, err := e.run(ctx, Cmd{Argv: []string{"docker", "container", "inspect", container}})
	if err != nil {
		return ContainerInfo{}, err
	}
	var cs []containerInspect
	if err := decodeJSON(res.Stdout, &cs); err != nil {
		return ContainerInfo{}, fmt.Errorf("parsing docker container inspect %s: %w", container, err)
	}
	if len(cs) != 1 {
		return ContainerInfo{}, fmt.Errorf("docker container inspect %s: got %d containers, want 1", container, len(cs))
	}
	c := cs[0]
	info := ContainerInfo{
		ID:       c.ID,
		Name:     strings.TrimPrefix(c.Name, "/"),
		Image:    c.Config.Image,
		ImageID:  c.Image,
		Labels:   c.Config.Labels,
		Status:   c.State.Status,
		Running:  c.State.Running,
		ExitCode: c.State.ExitCode,
	}
	if c.State.Health != nil {
		info.Health = c.State.Health.Status
	}
	return info, nil
}
