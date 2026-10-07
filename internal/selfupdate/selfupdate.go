package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

// What the release pipeline publishes, and where.
const (
	DefaultRepo = "JamesPeck/pic-sure-cli"
	DefaultAPI  = "https://api.github.com"
	// ChecksumsName lists "<sha256>  <asset>" for every archive.
	ChecksumsName = "checksums.txt"
	// BundleName is the cosign keyless signature of checksums.txt.
	BundleName = ChecksumsName + ".sigstore.json"
	// binaryName is the binary at the root of each archive.
	binaryName = "pic-sure"
)

// ReexecEnv is set, to the installed version, in the environment of the
// command SelfUpdate re-executes. A second SelfUpdate in that process means
// the new binary didn't satisfy the release either, and refuses rather than
// loop.
const ReexecEnv = "PIC_SURE_SELF_UPDATED"

// AssetName is the release archive for an OS and architecture.
func AssetName(goos, goarch string) string {
	return fmt.Sprintf("pic-sure_%s_%s.tar.gz", goos, goarch)
}

// Signature states, for Result.Signature.
const (
	SignatureVerified   = "verified"   // cosign verified checksums.txt
	SignatureUnverified = "unverified" // signed, but cosign isn't installed
)

// Updater replaces the running pic-sure with a GitHub release. The zero
// value of every field but Current has a working default.
type Updater struct {
	// Current is the running version.
	Current string
	// Repo is the GitHub OWNER/NAME; DefaultRepo when empty.
	Repo string
	// APIBase is the GitHub API root; DefaultAPI when empty.
	APIBase string
	// Proxy picks the proxy for each request (netproxy's ProxyURL); nil
	// means no proxy.
	Proxy func(*http.Request) (*url.URL, error)
	// Client overrides the HTTP client built from Proxy.
	Client *http.Client
	// Executable is the binary to replace; the running one when empty.
	Executable string
	// GOOS and GOARCH pick the archive; the running platform when empty.
	GOOS, GOARCH string
	// VerifyBundle checks bundle, the cosign signature of release tag's
	// checksums. Nil means cosign isn't available.
	VerifyBundle func(ctx context.Context, tag, checksums, bundle string) error
	// RequireSignature also refuses a signed release when cosign isn't
	// available to check it. A release without a bundle is always refused.
	RequireSignature bool
	// Sink and Step receive progress and warnings.
	Sink events.Sink
	Step string

	// Args and Environ are what SelfUpdate re-executes the new binary
	// with: the process's own when nil.
	Args    []string
	Environ []string
	// Exec replaces the process; syscall.Exec when nil.
	Exec func(path string, argv, env []string) error
	// Getenv reads the environment; os.Getenv when nil.
	Getenv func(string) string
}

// Result is what Install did; it is the self-update command's report.
type Result struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Path      string `json:"path"`
	Updated   bool   `json:"updated"`
	Signature string `json:"signature,omitempty"`
}

// Install replaces the binary with release version (the newest stable v2
// release when empty) and returns without re-executing. Installing the
// running version, or "upgrading" to a newest release older than it, does
// nothing. A binary
// pic-sure mustn't replace is an exit-3 error that says what to do.
func (u *Updater) Install(ctx context.Context, version string) (*Result, error) {
	version = normalize(version)
	exe, err := u.executable()
	if err != nil {
		return nil, err
	}
	t, err := findTarget(exe, version)
	if err != nil {
		var r *refusal
		if errors.As(err, &r) {
			return nil, exitcode.Precondition("%w", err)
		}
		return nil, err
	}
	rel, err := u.resolve(ctx, version)
	if err != nil {
		return nil, err
	}
	res := &Result{From: u.Current, To: rel.Tag, Path: t.Path}
	if rel.Tag == u.Current {
		return res, nil
	}
	if order, ok := stack.CompareVersions(rel.Tag, u.Current); version == "" && ok && order < 0 {
		return res, nil
	}

	archive, ok := rel.find(AssetName(u.goos(), u.goarch()))
	if !ok {
		return nil, exitcode.Failed("pic-sure release %s has no %s; this platform isn't supported by that release",
			rel.Tag, AssetName(u.goos(), u.goarch()))
	}
	sums, ok := rel.find(ChecksumsName)
	if !ok {
		return nil, exitcode.Failed("pic-sure release %s has no %s, so its download can't be verified", rel.Tag, ChecksumsName)
	}
	tmp, err := os.MkdirTemp("", "pic-sure-update-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	u.progress("Downloading pic-sure %s", rel.Tag)
	sumsPath := filepath.Join(tmp, ChecksumsName)
	if _, err := u.download(ctx, sums, sumsPath, maxMetadata); err != nil {
		return nil, err
	}
	if res.Signature, err = u.verifySignature(ctx, rel, sumsPath, tmp); err != nil {
		return nil, err
	}
	want, err := checksumFor(sumsPath, archive.Name)
	if err != nil {
		return nil, err
	}
	archivePath := filepath.Join(tmp, archive.Name)
	got, err := u.download(ctx, archive, archivePath, maxArchive)
	if err != nil {
		return nil, err
	}
	if got != want {
		return nil, exitcode.Failed("%s doesn't match its SHA-256 in %s (got %s, want %s); not installing it",
			archive.Name, ChecksumsName, got, want)
	}
	// Past this point nothing checks ctx, so a cancelled update must stop
	// here rather than replace the binary.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := replace(t, archivePath); err != nil {
		return nil, err
	}
	res.Updated = true
	return res, nil
}

// SelfUpdate is the compatibility gate's action (release.SelfUpdater): it
// installs version and re-executes the command with its original arguments,
// so it returns only on failure. A binary pic-sure mustn't replace is an
// exit-5 error that says what to do.
func (u *Updater) SelfUpdate(ctx context.Context, version string) error {
	if prev := u.getenv(ReexecEnv); prev != "" {
		return exitcode.Incompatible("pic-sure updated itself to %s and re-ran, but this release still needs pic-sure %s; "+
			"install it with %s, or pass --ignore-cli-version", prev, version, installCommand(normalize(version), ""))
	}
	res, err := u.Install(ctx, version)
	if err != nil {
		var r *refusal
		if errors.As(err, &r) {
			return exitcode.Incompatible("%v, or pass --ignore-cli-version to keep using this pic-sure", r)
		}
		return err
	}
	if !res.Updated {
		return nil
	}
	u.progress("Updated pic-sure from %s to %s; running the command again", res.From, res.To)
	args, env := u.Args, u.Environ
	if args == nil {
		args = os.Args
	}
	if env == nil {
		env = os.Environ()
	}
	env = append(env[:len(env):len(env)], ReexecEnv+"="+res.To)
	if err := ctx.Err(); err != nil {
		return err
	}
	exec := u.Exec
	if exec == nil {
		exec = syscall.Exec
	}
	if err := exec(res.Path, args, env); err != nil {
		return exitcode.Failed("pic-sure was updated to %s, but running it failed: %v; run the command again", res.To, err)
	}
	return nil
}

// verifySignature downloads the release's cosign bundle and checks
// checksums.txt against it. It returns a Signature state. Every v2 release
// is signed, so a missing bundle means a tampered or broken release.
func (u *Updater) verifySignature(ctx context.Context, rel *release, sums, dir string) (string, error) {
	b, ok := rel.find(BundleName)
	if !ok {
		return "", exitcode.Failed("pic-sure release %s isn't signed (no %s); not installing it", rel.Tag, BundleName)
	}
	if u.VerifyBundle == nil {
		if u.RequireSignature {
			return "", exitcode.Precondition("pic-sure release %s is signed, but cosign isn't installed to verify it; "+
				"install cosign and run self-update again", rel.Tag)
		}
		u.warn("pic-sure release %s is signed, but cosign isn't installed, so only its SHA-256 checksum was verified", rel.Tag)
		return SignatureUnverified, nil
	}
	bundle := filepath.Join(dir, BundleName)
	if _, err := u.download(ctx, b, bundle, maxMetadata); err != nil {
		return "", err
	}
	if err := u.VerifyBundle(ctx, rel.Tag, sums, bundle); err != nil {
		return "", exitcode.Failed("pic-sure release %s: the signature of %s doesn't verify: %v; not installing it",
			rel.Tag, ChecksumsName, err)
	}
	return SignatureVerified, nil
}

// replace extracts the binary from archive next to t and renames it over
// t, so the binary at t.Path is always either the old one or the new one.
func replace(t target, archive string) error {
	f, err := os.CreateTemp(filepath.Dir(t.Path), ".pic-sure-update-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	done := false
	defer func() {
		if !done {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if err := extractBinary(archive, f); err != nil {
		return err
	}
	if err := f.Chmod(t.Mode); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, t.Path); err != nil {
		return fmt.Errorf("replacing %s: %w", t.Path, err)
	}
	done = true
	return nil
}

// CosignVerifier returns an Updater.VerifyBundle that runs `cosign
// verify-blob` with runner, accepting only a keyless signature made by
// repo's release workflow for that very tag, so one release's signed
// assets can't pass for another's. It returns nil when cosign isn't on
// PATH.
func CosignVerifier(runner docker.Runner, repo string, lookPath func(string) (string, error)) func(context.Context, string, string, string) error {
	if _, err := lookPath("cosign"); err != nil {
		return nil
	}
	return func(ctx context.Context, tag, checksums, bundle string) error {
		_, err := docker.RunChecked(ctx, runner, docker.Cmd{Argv: []string{
			"cosign", "verify-blob",
			"--bundle", bundle,
			"--certificate-identity", "https://github.com/" + repo + "/.github/workflows/release.yml@refs/tags/" + tag,
			"--certificate-oidc-issuer", "https://token.actions.githubusercontent.com",
			checksums,
		}})
		return err
	}
}

// normalize adds the v prefix release tags carry: --to 2.1.0 means v2.1.0.
func normalize(version string) string {
	if version != "" && version[0] >= '0' && version[0] <= '9' {
		return "v" + version
	}
	return version
}

func (u *Updater) client() *http.Client {
	if u.Client != nil {
		return u.Client
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = u.Proxy
	return &http.Client{Transport: tr}
}

func (u *Updater) executable() (string, error) {
	if u.Executable != "" {
		return u.Executable, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("finding the pic-sure binary: %w", err)
	}
	return exe, nil
}

func (u *Updater) repo() string    { return or(u.Repo, DefaultRepo) }
func (u *Updater) apiBase() string { return strings.TrimSuffix(or(u.APIBase, DefaultAPI), "/") }
func (u *Updater) goos() string    { return or(u.GOOS, runtime.GOOS) }
func (u *Updater) goarch() string  { return or(u.GOARCH, runtime.GOARCH) }

func (u *Updater) getenv(k string) string {
	if u.Getenv != nil {
		return u.Getenv(k)
	}
	return os.Getenv(k)
}

func (u *Updater) progress(format string, args ...any) {
	if u.Sink != nil {
		u.Sink.Emit(events.Progress{ID: u.Step, Text: fmt.Sprintf(format, args...)})
	}
}

func (u *Updater) warn(format string, args ...any) {
	if u.Sink != nil {
		u.Sink.Emit(events.Warning{ID: u.Step, Text: fmt.Sprintf(format, args...)})
	}
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
