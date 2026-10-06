package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
)

// BuildOpts is one `docker build`.
type BuildOpts struct {
	// Context is the build context directory, an absolute path.
	Context string
	// File is the Dockerfile, an absolute path; empty means
	// Context/Dockerfile.
	File string
	// Tag names the image, e.g. "hms-dbmi/pic-sure-hpds:0123456789ab".
	Tag    string
	Labels map[string]string
	// BuildArgs are NAME=value entries, passed as a bare --build-arg NAME
	// with the value in docker's environment, so proxy credentials stay out
	// of argv.
	BuildArgs []string
	// Stdout and Stderr receive the build's output as it runs; nil discards
	// it. BuildKit writes its progress to stderr.
	Stdout, Stderr io.Writer
}

type imageInspect struct {
	ID     string `json:"Id"`
	Config struct {
		Labels map[string]string
	}
}

func (e *cliEngine) inspectImage(ctx context.Context, ref string) (imageInspect, error) {
	res, err := e.run(ctx, Cmd{Argv: []string{"docker", "image", "inspect", ref}})
	if err != nil {
		return imageInspect{}, err
	}
	var imgs []imageInspect
	if err := decodeJSON(res.Stdout, &imgs); err != nil {
		return imageInspect{}, fmt.Errorf("parsing docker image inspect %s: %w", ref, err)
	}
	if len(imgs) != 1 {
		return imageInspect{}, fmt.Errorf("docker image inspect %s: got %d images, want 1", ref, len(imgs))
	}
	return imgs[0], nil
}

func (e *cliEngine) ImageExists(ctx context.Context, ref string) (bool, error) {
	_, err := e.inspectImage(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (e *cliEngine) ImageID(ctx context.Context, ref string) (string, error) {
	img, err := e.inspectImage(ctx, ref)
	return img.ID, err
}

func (e *cliEngine) ImageLabels(ctx context.Context, ref string) (map[string]string, error) {
	img, err := e.inspectImage(ctx, ref)
	if err != nil {
		return nil, err
	}
	if img.Config.Labels == nil {
		return map[string]string{}, nil
	}
	return img.Config.Labels, nil
}

func (e *cliEngine) Build(ctx context.Context, opts BuildOpts) error {
	if opts.Tag == "" {
		return errors.New("docker build: no tag")
	}
	if !filepath.IsAbs(opts.Context) {
		return fmt.Errorf("docker build: context %q is not an absolute path", opts.Context)
	}
	if opts.File != "" && !filepath.IsAbs(opts.File) {
		return fmt.Errorf("docker build: Dockerfile %q is not an absolute path", opts.File)
	}
	names, env, err := splitEnv(opts.BuildArgs)
	if err != nil {
		return fmt.Errorf("docker build: build arg %w", err)
	}

	argv := []string{"docker", "build"}
	if opts.File != "" {
		argv = append(argv, "-f", opts.File)
	}
	argv = append(argv, "-t", opts.Tag)
	argv = append(argv, labelArgs(opts.Labels)...)
	for _, n := range names {
		argv = append(argv, "--build-arg", n)
	}
	argv = append(argv, opts.Context)

	c := Cmd{Argv: argv, Env: env}
	code, stderr, err := e.stream(ctx, c, opts.Stdout, opts.Stderr)
	if err != nil {
		return err
	}
	if code != 0 {
		return exitError(argv, code, stderr)
	}
	return nil
}

func (e *cliEngine) Pull(ctx context.Context, ref string, out io.Writer) error {
	argv := []string{"docker", "pull", ref}
	// docker pull writes progress to stdout and only its error to stderr,
	// which ends up in the returned error.
	code, stderr, err := e.stream(ctx, Cmd{Argv: argv}, out, nil)
	if err != nil {
		return err
	}
	if code != 0 {
		return exitError(argv, code, stderr)
	}
	return nil
}

func (e *cliEngine) RemoveImage(ctx context.Context, ref string) error {
	_, err := e.run(ctx, Cmd{Argv: []string{"docker", "image", "rm", ref}})
	return ignoreNotFound(err)
}

// labelArgs renders labels as --label KEY=VALUE pairs, sorted by key so argv
// is stable.
func labelArgs(labels map[string]string) []string {
	var args []string
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		args = append(args, "--label", k+"="+labels[k])
	}
	return args
}
