package ops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/netproxy"
)

// ReactorContainer is the fixed name of the Maven container. The cache's
// reactor lock covers one cache root, but pic-sure-m2 belongs to the Docker
// daemon; docker refuses a second container with this name, so two users or
// cache roots on one daemon can't run Maven at once either (§7.1).
const ReactorContainer = "pic-sure-reactor"

// ReactorSrcLabel is the label on every reactor image that names the full
// pic-sure commit it was built from.
const ReactorSrcLabel = "org.hms-dbmi.picsure.reactor-src"

// ReactorOptions configures BuildReactor.
type ReactorOptions struct {
	// Cache holds the source trees, the build directory and the locks.
	Cache *cache.Cache
	// SHA is the full pic-sure commit to build.
	SHA string
	// Source is the pic-sure tree to build, mounted read-only. Empty means
	// the cache's tree for SHA, made with EnsureSource only if something
	// needs building.
	Source string
	// Tag is the images' tag. Empty means the first 12 characters of SHA.
	Tag string
	// Proxy, if non-nil, gives Maven its proxy settings and docker build
	// its proxy build args.
	Proxy *netproxy.Proxy
	// Force rebuilds every image, even those already built from SHA.
	Force bool
	// LogDir, if set, is an existing directory that receives each part's
	// full output: pic-sure-reactor.log for Maven and <image>.log for each
	// docker build. Without it the output is kept only for a failure's
	// tail.
	LogDir string
	// Step is the step ID the build's events carry.
	Step string
}

// ReactorResult is what BuildReactor did.
type ReactorResult struct {
	// Tag is the tag every reactor image now has.
	Tag string
	// Built lists the images this run built, in build order. It is empty
	// when every image was already up to date.
	Built []string
}

// reactorPoll is how often BuildReactor checks whether another build's
// reactor container has finished.
var reactorPoll = 2 * time.Second

// reactorRunLabel marks the reactor container with the run that started it.
const reactorRunLabel = "org.hms-dbmi.picsure.reactor-run"

// The reactor container's own process exits when heartbeatFile, which the
// build touches every heartbeatEvery, hasn't been touched for heartbeatCheck
// seconds, so a killed build's container stops within two checks.
const (
	heartbeatFile  = "/tmp/pic-sure-heartbeat"
	heartbeatCheck = "60"
)

var heartbeatEvery = 15 * time.Second

// reactorCreatedGrace is how long a reactor container may stay in the
// created state, between docker run's create and start, before a waiting
// build takes it for a dead build's.
var reactorCreatedGrace = 2 * time.Minute

// tailLines is how many lines of a failed part's output are shown.
const tailLines = 30

// ReactorUpToDate reports whether every reactor image exists at tag with
// ReactorSrcLabel set to sha, so that BuildReactor has nothing to do.
func ReactorUpToDate(ctx context.Context, d *Deps, sha, tag string) (bool, error) {
	stale, err := staleReactorImages(ctx, d, sha, tag)
	return len(stale) == 0, err
}

// staleReactorImages returns the reactor images, in build order, that are
// missing at tag or weren't built from sha.
func staleReactorImages(ctx context.Context, d *Deps, sha, tag string) ([]catalog.Image, error) {
	var stale []catalog.Image
	for _, img := range catalog.ImagesBuiltFrom(catalog.PicSure) {
		fresh, err := builtFrom(ctx, d, img.Repository()+":"+tag, sha)
		if err != nil {
			return nil, err
		}
		if !fresh {
			stale = append(stale, img)
		}
	}
	return stale, nil
}

func builtFrom(ctx context.Context, d *Deps, ref, sha string) (bool, error) {
	labels, err := d.Docker.ImageLabels(ctx, ref)
	if errors.Is(err, docker.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking image %s: %w", ref, err)
	}
	return labels[ReactorSrcLabel] == sha, nil
}

// BuildReactor builds the pic-sure service images from one commit (§7.2):
// a Maven reactor build in a container, then a docker build for each image
// from contexts copied out of it, so no root-owned file reaches the host.
// It builds only the images that are missing or weren't built from SHA,
// unless Force is set, and does nothing when every image is up to date.
//
// It holds the cache's reactor lock from the Maven run until the images are
// built, and each image's lock while building it. It removes the container
// it created, whatever happens, and the build directory after a success; a
// failed build's contexts stay in build/<sha12>/ until the next build.
func BuildReactor(ctx context.Context, d *Deps, opts ReactorOptions) (ReactorResult, error) {
	if opts.Cache == nil {
		return ReactorResult{}, errors.New("reactor build: no cache")
	}
	buildDir, err := opts.Cache.BuildDir(opts.SHA)
	if err != nil {
		return ReactorResult{}, err
	}
	tag := opts.Tag
	if tag == "" {
		tag = opts.SHA[:12]
	}
	res := ReactorResult{Tag: tag}
	c := opts.Cache.WithEvents(d.Sink, opts.Step)
	progress := func(format string, args ...any) { progressf(d.Sink, opts.Step, format, args...) }

	all := catalog.ImagesBuiltFrom(catalog.PicSure)
	toBuild := all
	if !opts.Force {
		if toBuild, err = staleReactorImages(ctx, d, opts.SHA, tag); err != nil {
			return res, err
		}
		if len(toBuild) == 0 {
			progress("All %d pic-sure images are up to date at %s", len(all), tag)
			return res, nil
		}
	}

	src := opts.Source
	if src == "" {
		if src, err = c.EnsureSource(ctx, catalog.PicSure, opts.SHA); err != nil {
			return res, err
		}
	}

	// The bash refused a pre-monorepo commit up front; without this, Maven
	// would run and only docker cp would fail.
	for _, img := range toBuild {
		_, err := os.Stat(filepath.Join(src, filepath.FromSlash(img.Context)))
		if errors.Is(err, fs.ErrNotExist) {
			return res, fmt.Errorf("the pic-sure source %s has no %s: it predates the monorepo layout this build needs", src, img.Context)
		}
		if err != nil {
			return res, err
		}
	}

	lock, err := c.LockReactor(ctx)
	if err != nil {
		return res, err
	}
	defer func() { _ = lock.Unlock() }()
	if !opts.Force {
		// Another command may have built them while this one waited.
		if toBuild, err = staleReactorImages(ctx, d, opts.SHA, tag); err != nil {
			return res, err
		}
		if len(toBuild) == 0 {
			progress("All %d pic-sure images are up to date at %s", len(all), tag)
			return res, nil
		}
	}

	// A failed build leaves its contexts behind; start clean.
	if err := os.RemoveAll(buildDir); err != nil {
		return res, err
	}
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		return res, err
	}
	if err := runReactor(ctx, d, c, opts, src, buildDir, toBuild); err != nil {
		return res, err
	}

	for i, img := range toBuild {
		progress("Building image %d of %d: %s:%s", i+1, len(toBuild), img.Repository(), tag)
		built, err := buildReactorImage(ctx, d, c, opts, buildDir, img, tag)
		if err != nil {
			return res, err
		}
		if built {
			res.Built = append(res.Built, img.Name)
		}
	}
	return res, os.RemoveAll(buildDir)
}

// runReactor runs Maven in the reactor container and copies the contexts of
// images out of it into buildDir.
func runReactor(ctx context.Context, d *Deps, c *cache.Cache, opts ReactorOptions, src, buildDir string, images []catalog.Image) (err error) {
	progress := func(format string, args ...any) { progressf(d.Sink, opts.Step, format, args...) }
	mavenImg, _ := catalog.LookupImage("maven")
	out, err := newPartOutput(opts.LogDir, ReactorContainer, mavenProgress(d.Sink, opts.Step))
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()

	if err := ensureImage(ctx, d, opts.Step, mavenImg.Ref, out); err != nil {
		return err
	}
	if err := cache.EnsureMavenVolume(ctx, d.Docker); err != nil {
		return fmt.Errorf("creating the Maven volume: %w", err)
	}

	runID, err := docker.UniqueName("run", d.Rand)
	if err != nil {
		return err
	}
	mvn := "mvn -B install -T1C -DskipTests"
	run := docker.RunOpts{
		Image:  mavenImg.Ref,
		Name:   ReactorContainer,
		Detach: true,
		// Maven runs through docker exec while the container's own process
		// waits on heartbeats, so the container keeps running until it has
		// been copied from, and stops soon after this process dies. Only a
		// dead build's container is ever stopped.
		Args: []string{"sh", "-c", "touch " + heartbeatFile + "; " +
			"while sleep " + heartbeatCheck + "; do rm " + heartbeatFile + " 2>/dev/null || exit 0; done"},
		Workdir: "/build",
		Labels:  map[string]string{ReactorSrcLabel: opts.SHA, reactorRunLabel: runID},
		Mounts: []docker.Mount{
			{Source: src, Target: "/src", ReadOnly: true},
			{Source: cache.MavenVolume, Target: "/root/.m2"},
		},
	}
	var settings []byte
	if opts.Proxy != nil {
		settings = opts.Proxy.MavenSettings()
	}
	if settings != nil {
		// Outside /root/.m2, so neither the file nor its mount point ends
		// up in the shared volume.
		dir, err := c.TempDir("maven-settings-")
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(dir) }()
		file := filepath.Join(dir, "settings.xml")
		if err := os.WriteFile(file, settings, 0o600); err != nil {
			return err
		}
		run.Mounts = append(run.Mounts, docker.Mount{Source: file, Target: "/pic-sure/settings.xml", ReadOnly: true})
		mvn += " -s /pic-sure/settings.xml"
	}

	id, err := startReactorContainer(ctx, d, opts.Step, run)
	if err != nil {
		return err
	}
	stopHeartbeat := heartbeat(ctx, d, id)
	removed := false
	defer func() {
		stopHeartbeat()
		if removed {
			return
		}
		// The context may be cancelled; the container must still go, and
		// `docker exec` stopping doesn't stop Maven.
		rmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if rmErr := d.Docker.Rm(rmCtx, id, true); rmErr != nil && err == nil {
			err = fmt.Errorf("removing the reactor container: %w", rmErr)
		}
	}()

	progress("Running the Maven reactor build for pic-sure %s", opts.SHA[:12])
	code, err := d.Docker.Exec(ctx, docker.ExecOpts{
		Container: id,
		Args:      []string{"sh", "-c", "cp -r /src/. /build && exec " + mvn},
		Stdout:    out,
		Stderr:    out,
	})
	if err != nil {
		return fmt.Errorf("running the Maven reactor build: %w", err)
	}
	if code != 0 {
		out.emitTail(d.Sink, opts.Step)
		return fmt.Errorf("the Maven reactor build failed with exit code %d%s", code, out.logHint())
	}

	progress("Copying the build contexts out of the reactor container")
	for _, ctxDir := range contextsToCopy(images) {
		dest := filepath.Join(buildDir, filepath.FromSlash(ctxDir))
		if err := os.MkdirAll(dest, 0o755); err != nil {
			return err
		}
		if err := d.Docker.CpFrom(ctx, id, "/build/"+ctxDir+"/.", dest); err != nil {
			return fmt.Errorf("copying %s out of the reactor container: %w", ctxDir, err)
		}
	}
	stopHeartbeat()
	if err := d.Docker.Rm(ctx, id, true); err != nil {
		return fmt.Errorf("removing the reactor container: %w", err)
	}
	removed = true
	return nil
}

func progressf(sink events.Sink, step, format string, args ...any) {
	sink.Emit(events.Progress{ID: step, Text: fmt.Sprintf(format, args...)})
}

// ensureImage pulls ref if it isn't present, so the pull's progress is
// visible instead of hidden inside docker run.
func ensureImage(ctx context.Context, d *Deps, step, ref string, out *partOutput) error {
	ok, err := d.Docker.ImageExists(ctx, ref)
	if err != nil || ok {
		return err
	}
	progressf(d.Sink, step, "Pulling %s", ref)
	if err := d.Docker.Pull(ctx, ref, out); err != nil {
		return fmt.Errorf("pulling %s: %w", ref, err)
	}
	return nil
}

// startReactorContainer starts the reactor container under its fixed name
// and returns its ID. A running container with the name is another build's,
// so it waits for that to finish. A stopped one is a dead build's, so it
// removes it, as it does one left in the created state for longer than
// docker takes to start a container.
func startReactorContainer(ctx context.Context, d *Deps, step string, run docker.RunOpts) (string, error) {
	timeout := time.NewTimer(cache.ReactorLockTimeout)
	defer timeout.Stop()
	waiting := false
	createdID, createdPolls := "", 0
	for {
		info, err := d.Docker.ContainerInspect(ctx, ReactorContainer)
		switch {
		case errors.Is(err, docker.ErrNotFound):
			var stdout bytes.Buffer
			run.Stdout = &stdout
			_, err := d.Docker.Run(ctx, run)
			if err == nil {
				return strings.TrimSpace(stdout.String()), nil
			}
			if nameInUse(err) {
				continue // another build took the name first
			}
			removeOwnReactor(ctx, d, run.Labels[reactorRunLabel])
			return "", fmt.Errorf("starting the reactor container: %w", err)
		case err != nil:
			return "", fmt.Errorf("checking for another reactor build: %w", err)
		case info.Running:
		case info.Status == "created" && info.ID != createdID:
			createdID, createdPolls = info.ID, 1
		case info.Status == "created" && createdPolls < int(reactorCreatedGrace/reactorPoll):
			createdPolls++
		default:
			if err := d.Docker.Rm(ctx, info.ID, true); err != nil {
				return "", fmt.Errorf("removing the stopped container %s: %w", ReactorContainer, err)
			}
			continue
		}
		if !waiting {
			progressf(d.Sink, step, "Waiting for another reactor build to finish (container %s)", ReactorContainer)
			waiting = true
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("waiting for the reactor container %s: %w", ReactorContainer, context.Cause(ctx))
		case <-timeout.C:
			return "", fmt.Errorf("waited %s for another build's container %s to go; if no build is using it, remove it with `docker rm -f %[2]s`",
				cache.ReactorLockTimeout, ReactorContainer)
		case <-time.After(reactorPoll):
		}
	}
}

// removeOwnReactor removes the reactor container a failed docker run may
// have left behind, if it is this run's: docker run creates the container
// before starting it.
func removeOwnReactor(ctx context.Context, d *Deps, runID string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	info, err := d.Docker.ContainerInspect(ctx, ReactorContainer)
	if err == nil && info.Labels[reactorRunLabel] == runID {
		_ = d.Docker.Rm(ctx, info.ID, true)
	}
}

// heartbeat keeps the reactor container's heartbeat file fresh until the
// returned function is called.
func heartbeat(ctx context.Context, d *Deps, id string) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(heartbeatEvery)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				// A missed beat is harmless: the container stops only
				// after several in a row.
				_, _ = d.Docker.Exec(ctx, docker.ExecOpts{Container: id, Args: []string{"touch", heartbeatFile}})
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
}

func nameInUse(err error) bool {
	var ee *docker.ExitError
	return errors.As(err, &ee) && bytes.Contains(ee.Stderr, []byte("is already in use"))
}

// contextsToCopy returns the build contexts of images, sorted, leaving out
// any inside another one, which copying that one already brings along.
func contextsToCopy(images []catalog.Image) []string {
	var ctxs []string
	for _, img := range images {
		ctxs = append(ctxs, path.Clean(img.Context))
	}
	slices.Sort(ctxs)
	ctxs = slices.Compact(ctxs)
	var out []string
	for _, c := range ctxs {
		inside := slices.ContainsFunc(out, func(o string) bool { return strings.HasPrefix(c, o+"/") })
		if !inside {
			out = append(out, c)
		}
	}
	return out
}

// buildReactorImage builds one image from buildDir under its lock, unless
// another command built it from the same commit meanwhile. It reports
// whether it built the image.
func buildReactorImage(ctx context.Context, d *Deps, c *cache.Cache, opts ReactorOptions, buildDir string, img catalog.Image, tag string) (built bool, err error) {
	ref := img.Repository() + ":" + tag
	lock, err := c.LockImage(ctx, ref)
	if err != nil {
		return false, err
	}
	defer func() { _ = lock.Unlock() }()
	if !opts.Force {
		if fresh, err := builtFrom(ctx, d, ref, opts.SHA); err != nil || fresh {
			return false, err
		}
	}

	out, err := newPartOutput(opts.LogDir, img.Name, nil)
	if err != nil {
		return false, err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	build := docker.BuildOpts{
		Context: filepath.Join(buildDir, filepath.FromSlash(img.Context)),
		File:    filepath.Join(buildDir, filepath.FromSlash(img.Dockerfile)),
		Tag:     ref,
		Labels:  map[string]string{ReactorSrcLabel: opts.SHA},
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
		return false, fmt.Errorf("building %s: %w%s%s", ref, err, out.logHint(), alpineHint(img, out))
	}
	return true, nil
}

// alpineHint carries over the bash's diagnostic for hpds-etl's Dockerfile,
// whose pinned Alpine packages drop out of the index from time to time.
func alpineHint(img catalog.Image, out *partOutput) string {
	if img.Name != "pic-sure-hpds-etl" || !out.sawAPKUnavailable() {
		return ""
	}
	return fmt.Sprintf("\nIts Dockerfile (%s in the pic-sure source) asks for Alpine packages the index no longer has. "+
		"Use a pic-sure commit with corrected package pins, or fix the pins upstream; pic-sure doesn't rewrite the Dockerfile.", img.Dockerfile)
}

// apkUnavailable is how apk reports a package version the index lacks.
const apkUnavailable = "unable to select packages"

// mavenModuleRE matches the line Maven logs as it starts each module:
// "[INFO] Building pic-sure-gateway 1.0-SNAPSHOT    [3/40]".
var mavenModuleRE = regexp.MustCompile(`^\[INFO\] Building (.+?) \S+\s+\[(\d+)/(\d+)\]$`)

// mavenProgress turns Maven's module lines into Progress events.
func mavenProgress(sink events.Sink, step string) func(string) {
	return func(line string) {
		if m := mavenModuleRE.FindStringSubmatch(line); m != nil {
			sink.Emit(events.Progress{ID: step, Text: fmt.Sprintf("Maven: building %s (%s/%s)", m[1], m[2], m[3])})
		}
	}
}

// partOutput takes one part of the build's output (Maven's, or one docker
// build's): it copies it to a log file, if there is one, hands each line to
// onLine, and keeps the last lines for a failure's message. Writes are
// serialized, since stdout and stderr share it.
type partOutput struct {
	mu      sync.Mutex
	file    *os.File
	path    string
	onLine  func(string)
	partial []byte
	tail    []string
	apk     bool // a line said apkUnavailable
}

func newPartOutput(dir, name string, onLine func(string)) (*partOutput, error) {
	o := &partOutput{onLine: onLine}
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

func (o *partOutput) Write(p []byte) (int, error) {
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

func (o *partOutput) line(l string) {
	if len(o.tail) == tailLines {
		o.tail = o.tail[1:]
	}
	o.tail = append(o.tail, l)
	if strings.Contains(l, apkUnavailable) {
		o.apk = true
	}
	if o.onLine != nil {
		o.onLine(l)
	}
}

// Close handles a final unterminated line and closes the log file.
func (o *partOutput) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.partial) > 0 {
		o.line(string(o.partial))
		o.partial = nil
	}
	if o.file == nil {
		return nil
	}
	err := o.file.Close()
	o.file = nil
	return err
}

func (o *partOutput) sawAPKUnavailable() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.apk
}

// emitTail shows the last lines of output, for a failure.
func (o *partOutput) emitTail(sink events.Sink, step string) {
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

// logHint points at the full log, if there is one.
func (o *partOutput) logHint() string {
	if o.path == "" {
		return ""
	}
	return "; full log: " + o.path
}
