package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/netproxy"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

// Minimum versions doctor requires. Compose 2.29.0 is the first release
// with `--progress json`, which the adapter passes under --json; every
// other compose flag the CLI uses (`up --wait`, `--wait-timeout`,
// `--progress`, `run --rm` exit codes, JSON-lines `ps`) is older.
const (
	MinComposeVersion = "2.29.0"
	MinBuildxVersion  = "0.17.0"
)

// Disk space below DiskWarnBytes is a warning, and below DiskFailBytes a
// failure, both for the host cache and for Docker's data root.
const (
	DiskWarnBytes = 20 << 30
	DiskFailBytes = 5 << 30
)

// hpdsOverheadBytes is roughly what a stack's services other than HPDS
// need, for the memory check's warning.
const hpdsOverheadBytes = 4 << 30

const (
	probeTimeout = 20 * time.Second
	pullTimeout  = 2 * time.Minute
)

// CheckStatus is a check's outcome.
type CheckStatus string

const (
	CheckOK   CheckStatus = "ok"
	CheckWarn CheckStatus = "warn"
	CheckFail CheckStatus = "fail"
)

// Check is one doctor check. Name is stable, for scripts and support.
type Check struct {
	Name    string      `json:"name"`
	Status  CheckStatus `json:"status"`
	Message string      `json:"message"`
	// Detail is extra guidance, such as how to configure the Docker
	// daemon's proxy. It may span lines.
	Detail string `json:"detail,omitempty"`
}

// DoctorReport is `doctor --json`'s report.
type DoctorReport struct {
	// Stack is the stack directory checked; empty without one.
	Stack  string  `json:"stack,omitempty"`
	Checks []Check `json:"checks"`
}

// Failed reports whether any check failed.
func (r *DoctorReport) Failed() bool { return r.Count(CheckFail) > 0 }

// Count returns how many checks have status s.
func (r *DoctorReport) Count(s CheckStatus) int {
	n := 0
	for _, c := range r.Checks {
		if c.Status == s {
			n++
		}
	}
	return n
}

// Host is what doctor asks of the host besides Docker and git. The cli
// layer supplies the real one; tests a fake.
type Host interface {
	// LookPath finds an executable on PATH.
	LookPath(name string) (string, error)
	// DiskFree returns the bytes available to this user on the file system
	// holding path, which exists.
	DiskFree(path string) (uint64, error)
	// PortFree reports whether a TCP port can be bound on all interfaces.
	PortFree(port int) bool
	// Reach makes an HTTP request to rawURL through proxy (nil for
	// direct) and returns the response status. Any status means the host
	// was reached.
	Reach(ctx context.Context, rawURL string, proxy func(*http.Request) (*url.URL, error)) (int, error)
}

// DoctorOptions says what doctor checks.
type DoctorOptions struct {
	Host Host
	// CacheDir is the host cache root (cache.DefaultRoot), or "" with
	// CacheErr saying why there is none.
	CacheDir string
	CacheErr error
	// Stack is the stack to check, or nil for host checks only.
	Stack *stack.Stack
	// ComposeErr is why d.Compose is nil for a stack: wrapping
	// docker.ErrNotRendered before the first render.
	ComposeErr error
	// Network adds the reachability checks.
	Network bool
	// Building makes an old or missing buildx a failure, not a warning.
	// A stack with images.mode build sets it too.
	Building bool
}

// The network targets (§9.9) besides release-control.
var networkTargets = []struct{ name, url string }{
	{"github", "https://github.com/"},
	{"maven-central", "https://repo.maven.apache.org/maven2/"},
	{"npm-registry", "https://registry.npmjs.org/"},
	{"alpine-cdn", "https://dl-cdn.alpinelinux.org/alpine/"},
}

// Doctor runs the host, Docker, stack and (with opts.Network) network
// checks, and returns them all. A failing check is in the report, not an
// error: the caller exits 1 when Failed. It changes nothing, apart from a
// throwaway container that measures Docker's free disk space, which it
// removes.
func Doctor(ctx context.Context, d *Deps, opts DoctorOptions) *DoctorReport {
	c := &doctor{d: d, opts: opts, report: &DoctorReport{}}
	if opts.Stack != nil {
		c.report.Stack = opts.Stack.Dir
		c.loadStack()
	}
	c.host(ctx)
	if opts.Stack != nil {
		c.stack(ctx)
	}
	if opts.Network {
		c.network(ctx)
	}
	return c.report
}

type doctor struct {
	d      *Deps
	opts   DoctorOptions
	report *DoctorReport

	cfg    *stack.Config // nil without a stack or with an invalid config
	cfgErr error

	info      docker.Info
	daemonOK  bool
	runtime   string
	dockerCLI bool
}

func (c *doctor) add(name string, status CheckStatus, format string, args ...any) *Check {
	c.report.Checks = append(c.report.Checks, Check{Name: name, Status: status, Message: fmt.Sprintf(format, args...)})
	return &c.report.Checks[len(c.report.Checks)-1]
}

func (c *doctor) loadStack() {
	c.cfg, c.cfgErr = c.opts.Stack.LoadConfig()
	if c.cfgErr != nil {
		c.cfg = nil
	}
}

func (c *doctor) building() bool {
	return c.opts.Building || (c.cfg != nil && c.cfg.Images.Mode == stack.ImagesBuild)
}

// proxy is the stack's resolved proxy, or a direct one.
func (c *doctor) proxy() *netproxy.Proxy {
	var cfg netproxy.Config
	if c.cfg != nil {
		cfg = netproxy.Config(c.cfg.Proxy)
	}
	p, err := netproxy.New(cfg, netproxy.CatalogServices())
	if err != nil { // Validate already refused it
		p, _ = netproxy.New(netproxy.Config{}, nil)
	}
	return p
}

func (c *doctor) host(ctx context.Context) {
	c.dockerCLIChecks(ctx)
	c.gitCheck()
	c.cacheDiskCheck()
	if !c.daemonOK {
		return
	}
	c.dockerDiskCheck(ctx)
	c.memoryCheck(ctx)
	c.archCheck(ctx)
}

func (c *doctor) dockerCLIChecks(ctx context.Context) {
	if _, err := c.opts.Host.LookPath("docker"); err != nil {
		c.add("docker-cli", CheckFail, "docker is not on PATH: install Docker Desktop, Colima, OrbStack or Docker Engine")
		return
	}
	c.dockerCLI = true
	c.add("docker-cli", CheckOK, "docker is on PATH")

	info, err := c.d.Docker.Info(ctx)
	c.info = info
	switch {
	case err == nil:
		c.daemonOK = true
		c.add("docker-daemon", CheckOK, "the daemon answers: server %s on %s/%s", info.ServerVersion, info.OSType, info.Architecture)
	case errors.Is(err, docker.ErrDaemonUnreachable):
		c.add("docker-daemon", CheckFail, "the Docker daemon isn't reachable: %v", err).Detail =
			"Start Docker Desktop, Colima or OrbStack, or the docker service, and check `docker context ls`."
	default:
		c.add("docker-daemon", CheckFail, "docker info failed: %v", err)
	}

	c.pluginCheck("compose-version", "compose", MinComposeVersion, true)
	c.pluginCheck("buildx-version", "buildx", MinBuildxVersion, c.building())
	if c.daemonOK {
		c.runtimeCheck(ctx)
	}
}

// pluginCheck compares a docker CLI plugin's version with min. A missing or
// older plugin fails when required and warns otherwise.
func (c *doctor) pluginCheck(name, plugin, min string, required bool) {
	bad := CheckWarn
	if required {
		bad = CheckFail
	}
	var version string
	for _, p := range c.info.ClientInfo.Plugins {
		if p.Name == plugin {
			version = p.Version
		}
	}
	if version == "" {
		if len(c.info.ClientInfo.Plugins) == 0 && !c.daemonOK {
			// docker info printed nothing usable, so we can't tell; the
			// docker-daemon check has already failed.
			c.add(name, CheckWarn, "couldn't read the docker %s plugin's version from docker info", plugin)
			return
		}
		c.add(name, bad, "docker %s isn't installed; pic-sure needs %s or later", plugin, min)
		return
	}
	cmp, ok := compareVersion(version, min)
	switch {
	case !ok:
		c.add(name, CheckWarn, "can't parse docker %s version %q; pic-sure needs %s or later", plugin, version, min)
	case cmp < 0:
		c.add(name, bad, "docker %s %s is older than %s; upgrade Docker", plugin, version, min)
	default:
		c.add(name, CheckOK, "docker %s %s (need %s or later)", plugin, version, min)
	}
}

var versionRE = regexp.MustCompile(`^v?(\d+)\.(\d+)(?:\.(\d+))?`)

// compareVersion compares the leading major.minor[.patch] of a plugin
// version such as "v2.29.7" or "0.35.0-desktop.2" with want. A pre-release
// or build suffix is ignored.
func compareVersion(have, want string) (int, bool) {
	a, ok1 := parseVersion(have)
	b, ok2 := parseVersion(want)
	if !ok1 || !ok2 {
		return 0, false
	}
	return slices.Compare(a[:], b[:]), true
}

func parseVersion(s string) ([3]int, bool) {
	m := versionRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return [3]int{}, false
	}
	var v [3]int
	for i, part := range m[1:] {
		if part != "" {
			v[i], _ = strconv.Atoi(part)
		}
	}
	return v, true
}

// Runtimes doctor recognises.
const (
	RuntimeDockerDesktop = "Docker Desktop"
	RuntimeColima        = "Colima"
	RuntimeOrbStack      = "OrbStack"
	RuntimePodman        = "Podman"
	RuntimeDockerEngine  = "Docker Engine"
)

func (c *doctor) runtimeCheck(ctx context.Context) {
	c.runtime = detectRuntime(c.info, c.serverVersion(ctx))
	where := c.info.ClientInfo.Context
	if where == "" {
		where = "default"
	}
	if c.runtime == RuntimePodman {
		c.add("docker-runtime", CheckWarn, "%s (context %s): support is best-effort", c.runtime, where)
		return
	}
	c.add("docker-runtime", CheckOK, "%s (context %s)", c.runtime, where)
}

// serverVersion is the daemon's half of docker version, or nil.
func (c *doctor) serverVersion(ctx context.Context) *docker.ServerVersion {
	v, err := c.d.Docker.Version(ctx)
	if err != nil {
		return nil
	}
	return v.Server
}

func detectRuntime(info docker.Info, server *docker.ServerVersion) string {
	has := func(s, sub string) bool { return strings.Contains(strings.ToLower(s), sub) }
	if server != nil {
		if has(server.Platform.Name, "podman") {
			return RuntimePodman
		}
		for _, comp := range server.Components {
			if has(comp.Name, "podman") {
				return RuntimePodman
			}
		}
	}
	switch {
	case has(info.OperatingSystem, "docker desktop"):
		return RuntimeDockerDesktop
	case has(info.OperatingSystem, "orbstack") || has(info.ClientInfo.Context, "orbstack"):
		return RuntimeOrbStack
	case has(info.ClientInfo.Context, "colima") || has(info.Name, "colima"):
		return RuntimeColima
	case has(info.OperatingSystem, "podman") || has(info.Name, "podman"):
		return RuntimePodman
	}
	return RuntimeDockerEngine
}

func (c *doctor) gitCheck() {
	if _, err := c.opts.Host.LookPath("git"); err != nil {
		c.add("git", CheckFail, "git is not on PATH; pic-sure uses it to fetch release-control and sources")
		return
	}
	c.add("git", CheckOK, "git is on PATH")
}

func (c *doctor) cacheDiskCheck() {
	if c.opts.CacheErr != nil {
		c.add("disk-cache", CheckFail, "no usable host cache: %v", c.opts.CacheErr)
		return
	}
	free, err := c.opts.Host.DiskFree(c.opts.CacheDir)
	if err != nil {
		c.add("disk-cache", CheckWarn, "couldn't measure free space for %s: %v", c.opts.CacheDir, err)
		return
	}
	c.diskResult("disk-cache", "the host cache ("+c.opts.CacheDir+")", free)
}

func (c *doctor) diskResult(name, what string, free uint64) {
	switch {
	case free < DiskFailBytes:
		c.add(name, CheckFail, "%s free on %s; pic-sure needs at least %s", gib(free), what, gib(DiskFailBytes))
	case free < DiskWarnBytes:
		c.add(name, CheckWarn, "%s free on %s; builds and data loads want %s or more", gib(free), what, gib(DiskWarnBytes))
	default:
		c.add(name, CheckOK, "%s free on %s", gib(free), what)
	}
}

func gib(n uint64) string { return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30)) }

// dockerDiskCheck measures the free space of Docker's data root from inside
// a throwaway container, whose root file system lives there. That works the
// same whether the daemon runs here or in a VM.
// It uses the pinned alpine only when it is already pulled, so plain doctor
// never downloads anything.
func (c *doctor) dockerDiskCheck(ctx context.Context) {
	ref := imageRef("alpine")
	switch ok, err := c.d.Docker.ImageExists(ctx, ref); {
	case err != nil:
		c.add("disk-docker", CheckWarn, "couldn't measure Docker's free space: %v", err)
		return
	case !ok:
		c.add("disk-docker", CheckWarn, "Docker's free space not measured: %s isn't pulled yet (`docker pull %s`, or run doctor again after `pic-sure up`)", ref, ref)
		return
	}
	name, err := docker.UniqueName("pic-sure-doctor", c.d.Rand)
	if err != nil {
		c.add("disk-docker", CheckWarn, "couldn't measure Docker's free space: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	defer func() {
		rmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = c.d.Docker.Rm(rmCtx, name, true)
	}()
	var out, errOut strings.Builder
	code, err := c.d.Docker.Run(ctx, docker.RunOpts{
		Image: ref, Args: []string{"df", "-Pk", "/"}, Name: name, Remove: true,
		Network: "none", Stdout: &out, Stderr: &errOut,
	})
	if err == nil && code != 0 {
		err = fmt.Errorf("df exited %d: %s", code, strings.TrimSpace(errOut.String()))
	}
	if err != nil {
		c.add("disk-docker", CheckWarn, "couldn't measure Docker's free space with %s: %v", ref, err)
		return
	}
	free, err := parseDF(out.String())
	if err != nil {
		c.add("disk-docker", CheckWarn, "couldn't measure Docker's free space: %v", err)
		return
	}
	root := c.info.DockerRootDir
	if root == "" {
		root = "the Docker data root"
	}
	c.diskResult("disk-docker", root, free)
}

// parseDF reads the available space from `df -Pk` output.
func parseDF(out string) (uint64, error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return 0, fmt.Errorf("unexpected df output %q", out)
	}
	f := strings.Fields(lines[len(lines)-1])
	if len(f) < 4 {
		return 0, fmt.Errorf("unexpected df output %q", out)
	}
	kb, err := strconv.ParseUint(f[3], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("unexpected df output %q", out)
	}
	return kb << 10, nil
}

func imageRef(name string) string {
	img, _ := catalog.LookupImage(name)
	return img.Ref
}

// hpdsContainer is the part of `docker inspect` the memory check reads.
type hpdsContainer struct {
	Config struct {
		Env    []string
		Labels map[string]string
	}
}

// memoryCheck compares the memory Docker has with the HPDS heaps (-Xmx) of
// every running stack, plus this stack's if it isn't running (§6.5).
func (c *doctor) memoryCheck(ctx context.Context) {
	mem := c.info.MemTotal
	if mem <= 0 {
		c.add("memory", CheckWarn, "docker info doesn't report the memory available to containers")
		return
	}
	running, err := c.runningHPDS(ctx)
	if err != nil {
		c.add("memory", CheckWarn, "couldn't list running HPDS containers: %v", err)
		return
	}
	var runningTotal int64
	var parts []string
	thisRunning := false
	for _, h := range running {
		heap := maxHeap(envValue(h.Config.Env, "JAVA_OPTS"))
		runningTotal += heap
		owner := h.Config.Labels[stack.LabelStack]
		if c.opts.Stack != nil && h.Config.Labels[stack.LabelStackDir] == c.opts.Stack.Dir {
			thisRunning = true
			owner += " (this stack)"
		}
		parts = append(parts, fmt.Sprintf("%s %s", owner, gib(uint64(heap))))
	}
	total := runningTotal
	if c.cfg != nil && !thisRunning {
		heap := maxHeap(c.hpdsJavaOpts())
		total += heap
		parts = append(parts, fmt.Sprintf("this stack %s when up", gib(uint64(heap))))
	}
	if len(parts) == 0 {
		c.add("memory", CheckOK, "Docker has %s; no stack's HPDS is running", gib(uint64(mem)))
		return
	}
	summary := fmt.Sprintf("Docker has %s; HPDS heaps total %s (%s)", gib(uint64(mem)), gib(uint64(total)), strings.Join(parts, ", "))
	const help = "Give Docker more memory (Docker Desktop: Settings > Resources) or lower hpds.java_opts' -Xmx."
	switch {
	case runningTotal > mem:
		// Running heaps can grow past what Docker has, and the kernel then
		// kills a container.
		c.add("memory", CheckFail, "%s", summary).Detail = help
	case total > mem:
		// -Xmx is a ceiling, not a reservation, so a stack that isn't up
		// yet only warns.
		c.add("memory", CheckWarn, "%s, more than Docker has", summary).Detail = help
	case total+hpdsOverheadBytes > mem:
		c.add("memory", CheckWarn, "%s, leaving under %s for the other services", summary, gib(hpdsOverheadBytes))
	default:
		c.add("memory", CheckOK, "%s", summary)
	}
}

func (c *doctor) hpdsJavaOpts() string {
	if o := c.cfg.Services["hpds"].JavaOpts; o != "" {
		return o
	}
	return c.cfg.HPDS.JavaOpts
}

func (c *doctor) runningHPDS(ctx context.Context) ([]hpdsContainer, error) {
	res, err := docker.RunChecked(ctx, c.d.Runner, docker.Cmd{Argv: []string{
		"docker", "ps", "--quiet", "--no-trunc",
		"--filter", "label=" + stack.LabelStack,
		"--filter", "label=com.docker.compose.service=hpds",
	}})
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(string(res.Stdout))
	if len(ids) == 0 {
		return nil, nil
	}
	res, err = docker.RunChecked(ctx, c.d.Runner, docker.Cmd{Argv: append([]string{"docker", "container", "inspect"}, ids...)})
	if err != nil {
		return nil, err
	}
	var out []hpdsContainer
	if err := json.Unmarshal(res.Stdout, &out); err != nil {
		return nil, fmt.Errorf("parsing docker container inspect: %w", err)
	}
	return out, nil
}

func envValue(env []string, name string) string {
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, name+"="); ok {
			return v
		}
	}
	return ""
}

var xmxRE = regexp.MustCompile(`^-Xmx(\d+)([kKmMgGtT]?)$`)

// maxHeap is the last -Xmx in Java options, in bytes, or 0 when there is
// none.
func maxHeap(opts string) int64 {
	var heap int64
	for _, f := range strings.Fields(opts) {
		m := xmxRE.FindStringSubmatch(f)
		if m == nil {
			continue
		}
		n, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			continue
		}
		heap = n << map[string]int{"": 0, "k": 10, "m": 20, "g": 30, "t": 40}[strings.ToLower(m[2])]
	}
	return heap
}

// archCheck warns, on an arm64 daemon, about pinned third-party images
// present locally only for another architecture: they run under emulation.
// Images not pulled yet are left to the pull, which picks the native one
// when the image has it.
func (c *doctor) archCheck(ctx context.Context) {
	arch := c.info.Architecture
	if arch != "aarch64" && arch != "arm64" {
		c.add("arm64-images", CheckOK, "not an arm64 daemon (%s)", arch)
		return
	}
	var foreign, checked []string
	for _, img := range catalog.Images() {
		if img.Ref == "" || !strings.Contains(img.Ref, ":") {
			continue
		}
		argv := []string{"docker", "image", "inspect", "--format", "{{.Architecture}}", img.Ref}
		res, err := c.d.Runner.Run(ctx, docker.Cmd{Argv: argv})
		if err != nil {
			c.add("arm64-images", CheckWarn, "couldn't inspect %s: %v", img.Ref, err)
			return
		}
		if res.ExitCode != 0 {
			if strings.Contains(strings.ToLower(string(res.Stderr)), "no such image") {
				continue // not pulled
			}
			c.add("arm64-images", CheckWarn, "couldn't inspect %s: %v", img.Ref, &docker.ExitError{Argv: argv, ExitCode: res.ExitCode, Stderr: res.Stderr})
			return
		}
		checked = append(checked, img.Ref)
		if a := strings.TrimSpace(string(res.Stdout)); a != "arm64" {
			foreign = append(foreign, img.Ref+" ("+a+")")
		}
	}
	switch {
	case len(foreign) > 0:
		c.add("arm64-images", CheckWarn, "these images aren't arm64 and run under emulation: %s", strings.Join(foreign, ", ")).Detail =
			"Remove them with `docker image rm` so the next pull fetches the arm64 variant."
	case len(checked) == 0:
		c.add("arm64-images", CheckOK, "arm64 daemon; no pinned images pulled yet")
	default:
		c.add("arm64-images", CheckOK, "arm64 daemon; pulled images are arm64: %s", strings.Join(checked, ", "))
	}
}

func (c *doctor) stack(ctx context.Context) {
	st := c.opts.Stack
	switch {
	case c.cfgErr != nil:
		c.add("config", CheckFail, "%v", c.cfgErr)
	case c.cfg.CheckFiles(st.Dir) != nil:
		c.add("config", CheckFail, "%v", c.cfg.CheckFiles(st.Dir))
	default:
		c.add("config", CheckOK, "%s is valid", st.Path("pic-sure.yaml"))
	}
	c.composeCheck(ctx)
	c.overridesCheck()
	if c.cfg == nil {
		return
	}
	c.portsCheck(ctx)
	c.auth0Check()
	c.proxyCheck()
}

func (c *doctor) composeCheck(ctx context.Context) {
	switch {
	case c.d.Compose == nil && errors.Is(c.opts.ComposeErr, docker.ErrNotRendered):
		c.add("compose-config", CheckWarn, "the stack hasn't been rendered yet; run `pic-sure up`")
	case c.d.Compose == nil:
		c.add("compose-config", CheckFail, "can't read the rendered compose file: %v", c.opts.ComposeErr)
	case !c.dockerCLI:
		c.add("compose-config", CheckWarn, "not checked: docker isn't available")
	default:
		if _, err := c.d.Compose.Config(ctx, true); err != nil {
			c.add("compose-config", CheckFail, "docker compose config: %v", err)
			return
		}
		c.add("compose-config", CheckOK, "docker compose config accepts the rendered file and overrides")
	}
}

// overridesCheck warns about overrides/*.yml, which compose never sees: the
// adapter takes only *.yaml.
func (c *doctor) overridesCheck() {
	entries, err := fs.ReadDir(c.opts.Stack.FS(), "overrides")
	if err != nil {
		return // none, or unreadable: the compose check reports that
	}
	var ignored []string
	for _, e := range entries {
		if !e.IsDir() && path.Ext(e.Name()) == ".yml" && !strings.HasPrefix(e.Name(), ".") {
			ignored = append(ignored, "overrides/"+e.Name())
		}
	}
	if len(ignored) > 0 {
		c.add("overrides", CheckWarn, "ignored, because only .yaml overrides are read: %s", strings.Join(ignored, ", "))
	}
}

func (c *doctor) portsCheck(ctx context.Context) {
	ports := []int{c.cfg.Network.HTTPPort, c.cfg.Network.HTTPSPort}
	if len(c.cfg.Dev.Services) > 0 {
		for i := range stack.DevPortCount {
			ports = append(ports, c.cfg.Network.DevPorts.Base+i)
		}
	}
	var busy []int
	for _, p := range ports {
		if !c.opts.Host.PortFree(p) {
			busy = append(busy, p)
		}
	}
	if len(busy) == 0 {
		c.add("ports", CheckOK, "ports %s are free", joinInts(ports))
		return
	}
	owned := map[int]bool{}
	if c.d.Compose != nil && c.daemonOK {
		if svcs, err := c.d.Compose.Ps(ctx); err == nil {
			for _, s := range svcs {
				for _, pub := range s.Publishers {
					owned[pub.PublishedPort] = true
				}
			}
		}
	}
	var others []int
	for _, p := range busy {
		if !owned[p] {
			others = append(others, p)
		}
	}
	if len(others) > 0 {
		c.add("ports", CheckFail, "ports %s are in use by something other than this stack", joinInts(others)).Detail =
			"Free them, or change network.http_port, network.https_port or network.dev_ports.base with `pic-sure config set`."
		return
	}
	c.add("ports", CheckOK, "ports %s are in use by this stack", joinInts(busy))
}

func joinInts(ns []int) string {
	s := make([]string, len(ns))
	for i, n := range ns {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s, ", ")
}

func (c *doctor) auth0Check() {
	if c.cfg.Auth.Mode == stack.AuthOpen {
		c.add("auth0", CheckOK, "auth.mode is open: no Auth0 login needed")
		return
	}
	var missing []string
	if c.cfg.Auth.Auth0.Tenant == "" {
		missing = append(missing, "auth.auth0.tenant")
	}
	if c.cfg.Auth.Auth0.ClientID == "" {
		missing = append(missing, "auth.auth0.client_id")
	}
	sec, err := c.opts.Stack.LoadSecrets()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		missing = append(missing, "the Auth0 client secret (no secrets.yaml yet)")
	case err != nil:
		c.add("auth0", CheckFail, "reading secrets: %v", err)
		return
	case sec.Auth0ClientSecret == "":
		missing = append(missing, "the Auth0 client secret")
	case sec.Auth0ClientSecretGenerated:
		missing = append(missing, "the Auth0 client secret (the stack has only the random one made for open mode)")
	}
	if len(missing) > 0 {
		c.add("auth0", CheckFail, "auth.mode is %s but these are missing: %s", c.cfg.Auth.Mode, strings.Join(missing, ", "))
		return
	}
	c.add("auth0", CheckOK, "Auth0 tenant %s, client ID and secret are set", c.cfg.Auth.Auth0.Tenant)
}

// proxyCheck covers the proxy rules of §9.10 that validation can't: an
// http-only proxy, and credentials psama can't use.
func (c *doctor) proxyCheck() {
	p := c.cfg.Proxy
	if p.HTTP == "" && p.HTTPS == "" {
		c.add("proxy", CheckOK, "no proxy configured")
		return
	}
	var warnings []string
	if p.HTTP != "" && p.HTTPS == "" {
		warnings = append(warnings, "proxy.http is set but proxy.https is empty, so https traffic (nearly all of it: GitHub, Maven Central, npm, Auth0) goes direct; set proxy.https too")
	}
	for _, raw := range []string{p.HTTP, p.HTTPS} {
		if u, err := netproxy.ParseURL(raw); err == nil && u != nil && u.User != nil {
			warnings = append(warnings, "the proxy URL has credentials, which psama's JVM can't use: Auth0 must be reachable without them (allow-listed at the proxy, or in proxy.no_proxy)")
			break
		}
	}
	if len(warnings) > 0 {
		c.add("proxy", CheckWarn, "%s", strings.Join(warnings, "; "))
		return
	}
	c.add("proxy", CheckOK, "http=%s https=%s", redactURL(p.HTTP), redactURL(p.HTTPS))
}

func (c *doctor) network(ctx context.Context) {
	p := c.proxy()
	for _, t := range networkTargets {
		c.reach(ctx, "network-"+t.name, t.url, p)
	}
	c.releaseControlCheck(ctx, p)
	if p.Enabled() {
		c.pullCheck(ctx)
	}
}

// via says how traffic to rawURL goes: through the proxy for its scheme, or
// directly when there is none or no_proxy matches. A URL git or Go can't
// parse, such as scp-style git@host:path, is reported as direct.
func via(p *netproxy.Proxy, rawURL string) string {
	req, err := http.NewRequest(http.MethodHead, rawURL, nil)
	if err != nil {
		return "directly"
	}
	if u, err := p.ProxyURL(req); err == nil && u != nil {
		return "through the proxy"
	}
	return "directly"
}

func (c *doctor) reach(ctx context.Context, name, rawURL string, p *netproxy.Proxy) {
	via := via(p, rawURL)
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	status, err := c.opts.Host.Reach(ctx, rawURL, p.ProxyURL)
	switch {
	case err != nil && strings.Contains(err.Error(), http.StatusText(http.StatusProxyAuthRequired)):
		// Go reports a refused https CONNECT as an error with the status text.
		c.add(name, CheckFail, "the proxy wants credentials for %s (HTTP 407)", rawURL)
	case err != nil:
		c.add(name, CheckFail, "can't reach %s %s: %v", rawURL, via, err)
	case status == http.StatusProxyAuthRequired:
		c.add(name, CheckFail, "the proxy wants credentials for %s (HTTP 407)", rawURL)
	default:
		c.add(name, CheckOK, "reached %s %s (HTTP %d)", rawURL, via, status)
	}
}

func (c *doctor) releaseControlCheck(ctx context.Context, p *netproxy.Proxy) {
	repo := stack.DefaultConfig().Release.Repo
	if c.cfg != nil {
		repo = c.cfg.Release.Repo
	}
	via := via(p, repo)
	// A repo URL can carry a token, as a user name or a password.
	shown := repo
	if u, err := url.Parse(repo); err == nil && u.User != nil {
		u.User = url.User("xxxxx")
		shown = u.String()
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	if _, err := c.d.Git.WithEnv(p.Env()...).LsRemote(ctx, repo); err != nil {
		c.add("network-release-control", CheckFail, "git can't list %s %s: %s", shown, via, strings.ReplaceAll(err.Error(), repo, shown))
		return
	}
	c.add("network-release-control", CheckOK, "git listed %s %s", shown, via)
}

// pullCheck pulls a small image, because the daemon's proxy settings are
// its own and the CLI can't set them (D36).
func (c *doctor) pullCheck(ctx context.Context) {
	if !c.daemonOK {
		c.add("network-docker-pull", CheckWarn, "not checked: the Docker daemon isn't reachable")
		return
	}
	ref := imageRef("alpine")
	ctx, cancel := context.WithTimeout(ctx, pullTimeout)
	defer cancel()
	if err := c.d.Docker.Pull(ctx, ref, io.Discard); err != nil {
		c.add("network-docker-pull", CheckFail, "the Docker daemon can't pull %s: %v", ref, err).Detail = c.daemonProxyHelp()
		return
	}
	c.add("network-docker-pull", CheckOK, "the Docker daemon pulled %s", ref)
}

// daemonProxyHelp says how to give this runtime's daemon the proxy. The
// CLI never edits daemon configuration itself (D36).
func (c *doctor) daemonProxyHelp() string {
	p := c.cfg.Proxy
	httpURL, httpsURL := redactURL(p.HTTP), redactURL(p.HTTPS)
	current := "The daemon has no proxy configured."
	if c.info.HTTPProxy != "" || c.info.HTTPSProxy != "" {
		current = fmt.Sprintf("The daemon's proxy is http=%q https=%q.", c.info.HTTPProxy, c.info.HTTPSProxy)
	}
	env := fmt.Sprintf("HTTP_PROXY=%s HTTPS_PROXY=%s NO_PROXY=%s", httpURL, httpsURL, p.NoProxy)
	var how string
	switch c.runtime {
	case RuntimeDockerDesktop:
		how = "Docker Desktop: Settings > Resources > Proxies, turn on manual proxy configuration, enter the proxy URLs, then Apply & restart."
	case RuntimeColima:
		how = "Colima: run `colima start --edit`, add the variables under `env:` (" + env + "), save, and let Colima restart."
	case RuntimeOrbStack:
		how = "OrbStack: Settings > Network > Proxy, or `orb config set network_proxy " + firstNonEmpty(httpsURL, httpURL) + "`, then restart OrbStack."
	case RuntimePodman:
		how = "Podman: add `env = [\"HTTP_PROXY=…\", \"HTTPS_PROXY=…\", \"NO_PROXY=…\"]` under [engine] in containers.conf (" + env + "), then restart the Podman machine or service."
	default:
		how = "Docker Engine: create /etc/systemd/system/docker.service.d/http-proxy.conf with\n" +
			"  [Service]\n" +
			"  Environment=\"HTTP_PROXY=" + httpURL + "\" \"HTTPS_PROXY=" + httpsURL + "\" \"NO_PROXY=" + p.NoProxy + "\"\n" +
			"then run `sudo systemctl daemon-reload && sudo systemctl restart docker`."
	}
	return current + "\n" + how
}

// redactURL shows a proxy URL with its password hidden.
func redactURL(raw string) string {
	u, err := netproxy.ParseURL(raw)
	if err != nil || u == nil {
		return raw
	}
	return u.Redacted()
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
