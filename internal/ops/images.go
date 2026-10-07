package ops

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/netproxy"
	"github.com/JamesPeck/pic-sure-cli/internal/render"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

// Labels on the images built outside the reactor (§7.2 steps 4 and 5). A
// build is skipped when the image exists with the labels it would get.
const (
	// FrontendSrcLabel names the full frontend commit pic-sure-httpd was
	// built from.
	FrontendSrcLabel = "org.hms-dbmi.picsure.frontend-src"
	// FrontendConfigLabel holds the full FrontendConfigHash the image's
	// .env had; its tag carries only the first 8 characters.
	FrontendConfigLabel = "org.hms-dbmi.picsure.frontend-config"
	// DictionaryETLSrcLabel names the full dictionary-etl commit the image
	// was built from.
	DictionaryETLSrcLabel = "org.hms-dbmi.picsure.dictionary-etl-src"
)

// ImageBuildOptions configures BuildFrontend and BuildDictionaryETL.
type ImageBuildOptions struct {
	// Cache holds the source trees, the build directories and the locks.
	Cache *cache.Cache
	// SHA is the full commit of the component to build.
	SHA string
	// Source is the tree to build. Empty means the cache's tree for SHA,
	// made with EnsureSource only if the image needs building.
	Source string
	// Tag replaces the image's default tag (§7.2), for builds from a
	// local source (§7.3).
	Tag string
	// Proxy, if non-nil, gives docker build its proxy build args.
	Proxy *netproxy.Proxy
	// Force builds the image even if it is up to date.
	Force bool
	// LogDir, if set, is an existing directory that receives the build's
	// full output as <image>.log. Without it the output is kept only for a
	// failure's tail.
	LogDir string
	// Step is the step ID the build's events carry.
	Step string
}

// ImageBuildResult is what BuildFrontend or BuildDictionaryETL did.
type ImageBuildResult struct {
	// Tag is the image's tag, e.g. "0123456789ab-89abcdef".
	Tag string
	// Ref is the image's full reference, e.g.
	// "hms-dbmi/pic-sure-httpd:0123456789ab-89abcdef".
	Ref string
	// Built is false when the image was already up to date.
	Built bool
}

// FrontendConfigHash is the sha256, in hex, of the frontend's build-time
// configuration: render.ViteEnv's VITE_* set, which includes the theme. Map
// order doesn't matter, and any change to a name or value changes it.
// Stacks whose hashes are equal share a frontend image (§7.2 step 4).
func FrontendConfigHash(env map[string]string) string {
	// encoding/json writes map keys sorted, so this is canonical.
	b, err := json.Marshal(env)
	if err != nil {
		panic(err) // a map[string]string always marshals
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

var viteName = regexp.MustCompile(`^VITE_[A-Z0-9_]+$`)

// FrontendDotEnv renders env as the frontend's .env, one NAME=value line
// per variable, sorted. Vite reads it with dotenv and then dotenv-expand,
// so each value is quoted to stay literal: single quotes, or backquotes
// when the value has a single quote, with every $ written \$ so that
// dotenv-expand neither expands nor drops it. A value with a line break,
// or with both a single quote and a backquote, has no such form and is an
// error.
func FrontendDotEnv(env map[string]string) ([]byte, error) {
	var b bytes.Buffer
	for _, name := range slices.Sorted(maps.Keys(env)) {
		v := env[name]
		if !viteName.MatchString(name) {
			return nil, fmt.Errorf("frontend setting %q is not a VITE_* name", name)
		}
		if strings.ContainsAny(v, "\r\n") {
			return nil, fmt.Errorf("frontend setting %s contains a line break", name)
		}
		quote := "'"
		if strings.Contains(v, "'") {
			if strings.Contains(v, "`") {
				return nil, fmt.Errorf("frontend setting %s contains both ' and `, which a .env value can't hold", name)
			}
			quote = "`"
		}
		fmt.Fprintf(&b, "%s=%s%s%s\n", name, quote, strings.ReplaceAll(v, "$", `\$`), quote)
	}
	return b.Bytes(), nil
}

// FrontendTag is pic-sure-httpd's tag for a frontend commit and config
// hash: <sha12>-<cfghash8> (§7.2 step 4).
func FrontendTag(sha, cfghash string) string {
	return sha[:12] + "-" + cfghash[:8]
}

// BuildFrontend builds hms-dbmi/pic-sure-httpd for cfg from the frontend
// at opts.SHA (§7.2 step 4). The image bakes in render.ViteEnv(cfg), so it
// is tagged FrontendTag(SHA, FrontendConfigHash(...)) and shared by every
// stack with the same frontend commit and config. The source is copied to
// the cache's FrontendBuildDir, the generated .env written into the copy,
// and the copy removed after the build. Under the image's lock, the build
// is skipped when the image exists with matching labels.
func BuildFrontend(ctx context.Context, d *Deps, cfg *stack.Config, opts ImageBuildOptions) (ImageBuildResult, error) {
	env := render.ViteEnv(cfg)
	dotenv, err := FrontendDotEnv(env)
	if err != nil {
		return ImageBuildResult{}, err
	}
	hash := FrontendConfigHash(env)
	if err := checkImageSHA(opts.SHA); err != nil {
		return ImageBuildResult{}, err
	}
	tag := opts.Tag
	if tag == "" {
		tag = FrontendTag(opts.SHA, hash)
	}
	labels := map[string]string{FrontendSrcLabel: opts.SHA, FrontendConfigLabel: hash}
	return buildSourceImage(ctx, d, catalog.Frontend, opts, tag, labels, func(src string) (string, func(), error) {
		dir, err := opts.Cache.FrontendBuildDir(opts.SHA, hash[:8])
		if err != nil {
			return "", nil, err
		}
		cleanup := func() { _ = os.RemoveAll(dir) }
		// A killed build may have left its copy behind; the lock makes it
		// this build's to replace.
		cleanup()
		if err := copyTree(src, dir); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("copying the frontend source: %w", err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".env"), dotenv, 0o644); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("writing the frontend .env: %w", err)
		}
		return dir, cleanup, nil
	})
}

// BuildDictionaryETL builds hms-dbmi/dictionary-etl:<sha12> from the
// dictionary-etl tree at opts.SHA (§7.2 step 5). Under the image's lock,
// the build is skipped when the image exists with a matching label.
func BuildDictionaryETL(ctx context.Context, d *Deps, opts ImageBuildOptions) (ImageBuildResult, error) {
	if err := checkImageSHA(opts.SHA); err != nil {
		return ImageBuildResult{}, err
	}
	tag := opts.Tag
	if tag == "" {
		tag = opts.SHA[:12]
	}
	labels := map[string]string{DictionaryETLSrcLabel: opts.SHA}
	return buildSourceImage(ctx, d, catalog.DictionaryETL, opts, tag, labels, func(src string) (string, func(), error) {
		return src, func() {}, nil
	})
}

var fullCommit = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

func checkImageSHA(sha string) error {
	if !fullCommit.MatchString(sha) {
		return fmt.Errorf("%q is not a full lowercase commit sha", sha)
	}
	return nil
}

// buildSourceImage builds the catalog image of component under its lock.
// prepare turns the source tree into the build context and returns a
// cleanup for it.
func buildSourceImage(ctx context.Context, d *Deps, component string, opts ImageBuildOptions, tag string, labels map[string]string,
	prepare func(src string) (dir string, cleanup func(), err error)) (res ImageBuildResult, err error) {
	img, err := sourceImage(component)
	if err != nil {
		return ImageBuildResult{}, err
	}
	ref := img.Repository() + ":" + tag
	res = ImageBuildResult{Tag: tag, Ref: ref}
	lock, err := opts.Cache.LockImage(ctx, ref)
	if err != nil {
		return res, err
	}
	defer func() { _ = lock.Unlock() }()
	if !opts.Force {
		if fresh, err := imageHasLabels(ctx, d, ref, labels); err != nil || fresh {
			return res, err
		}
	}

	src := opts.Source
	if src == "" {
		if src, err = opts.Cache.EnsureSource(ctx, component, opts.SHA); err != nil {
			return res, err
		}
	}
	d.Sink.Emit(events.Progress{ID: opts.Step, Text: "building " + ref})
	dir, cleanup, err := prepare(src)
	if err != nil {
		return res, err
	}
	defer cleanup()

	out, err := newImageLog(opts.LogDir, img.Name)
	if err != nil {
		return res, err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	build := docker.BuildOpts{
		Context: dir,
		File:    filepath.Join(dir, filepath.FromSlash(img.Dockerfile)),
		Tag:     ref,
		Labels:  labels,
		Stdout:  out,
		Stderr:  out,
	}
	if opts.Proxy != nil {
		build.BuildArgs = opts.Proxy.BuildArgs()
	}
	if err := d.Docker.Build(ctx, build); err != nil {
		if ctx.Err() == nil {
			out.emitTail(d.Sink, opts.Step)
		}
		return res, fmt.Errorf("building %s: %w%s", ref, err, out.hint())
	}
	res.Built = true
	return res, nil
}

// sourceImage is the catalog image built from component's own tree.
func sourceImage(component string) (catalog.Image, error) {
	for _, img := range catalog.Images() {
		if img.Component == component {
			if img.Context != "." {
				return catalog.Image{}, fmt.Errorf("image %s: context %q isn't the source root", img.Name, img.Context)
			}
			return img, nil
		}
	}
	return catalog.Image{}, fmt.Errorf("no image is built from %s", component)
}

// imageHasLabels reports whether ref exists with every label in want.
func imageHasLabels(ctx context.Context, d *Deps, ref string, want map[string]string) (bool, error) {
	have, err := d.Docker.ImageLabels(ctx, ref)
	if errors.Is(err, docker.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for k, v := range want {
		if have[k] != v {
			return false, nil
		}
	}
	return true, nil
}

// copyTree copies the directory src to dst, which must not exist. It copies
// regular files with their permission bits, directories at 0755 and
// symlinks as they are; the cache's trees hold nothing else.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case e.IsDir():
			if rel == "." {
				return os.MkdirAll(target, 0o755)
			}
			return os.Mkdir(target, 0o755)
		case e.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case e.Type().IsRegular():
			info, err := e.Info()
			if err != nil {
				return err
			}
			return copyFile(path, target, info.Mode().Perm())
		default:
			return fmt.Errorf("%s: not a file, directory or symlink", path)
		}
	})
}

func copyFile(src, dst string, perm fs.FileMode) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, perm)
}

// imageLogTail is how many lines of a failed build's output are shown.
const imageLogTail = 30

// imageLog takes one docker build's output: it copies it to a log file, if
// there is one, and keeps the last lines for a failure's message. Writes
// are serialized, since stdout and stderr share it.
type imageLog struct {
	mu      sync.Mutex
	file    *os.File
	path    string
	partial []byte
	tail    []string
}

func newImageLog(dir, name string) (*imageLog, error) {
	o := &imageLog{}
	if dir == "" {
		return o, nil
	}
	o.path = filepath.Join(dir, name+".log")
	f, err := os.OpenFile(o.path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening the build log: %w", err)
	}
	o.file = f
	return o, nil
}

func (o *imageLog) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.file != nil {
		if _, err := o.file.Write(p); err != nil {
			return 0, fmt.Errorf("writing the build log: %w", err)
		}
	}
	o.partial = append(o.partial, p...)
	for {
		i := bytes.IndexByte(o.partial, '\n')
		if i < 0 {
			break
		}
		o.line(string(bytes.TrimRight(o.partial[:i], "\r")))
		o.partial = o.partial[i+1:]
	}
	return len(p), nil
}

func (o *imageLog) line(l string) {
	if len(o.tail) == imageLogTail {
		o.tail = o.tail[1:]
	}
	o.tail = append(o.tail, l)
}

// Close closes the log file.
func (o *imageLog) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.file == nil {
		return nil
	}
	err := o.file.Close()
	o.file = nil
	return err
}

// emitTail shows the last lines of output, for a failure.
func (o *imageLog) emitTail(sink events.Sink, step string) {
	o.mu.Lock()
	tail := slices.Clone(o.tail)
	if len(o.partial) > 0 {
		tail = append(tail, string(o.partial))
	}
	o.mu.Unlock()
	for _, l := range tail {
		sink.Emit(events.Log{ID: step, Stream: events.StreamStderr, Line: l})
	}
}

// hint points at the full log, if there is one.
func (o *imageLog) hint() string {
	if o.path == "" {
		return ""
	}
	return "; full log: " + o.path
}
