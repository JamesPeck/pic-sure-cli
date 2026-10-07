package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
)

const newBinary = "#!/bin/sh\necho new pic-sure\n"

// fakeRelease is one release the fake GitHub serves.
type fakeRelease struct {
	tag        string
	files      map[string][]byte // asset name -> content
	noAsset    bool              // leave the platform's archive out
	draft      bool
	prerelease bool
}

// newFakeRelease is a release of newBinary for linux/amd64 with a correct
// checksums.txt.
func newFakeRelease(t *testing.T, tag string) *fakeRelease {
	t.Helper()
	archive := tarGz(t, map[string]string{"README.md": "hi", "pic-sure": newBinary})
	sum := sha256.Sum256(archive)
	return &fakeRelease{tag: tag, files: map[string][]byte{
		AssetName("linux", "amd64"): archive,
		ChecksumsName: []byte(fmt.Sprintf("%s  %s\n%s  %s\n",
			strings.Repeat("0", 64), AssetName("darwin", "arm64"), hex.EncodeToString(sum[:]), AssetName("linux", "amd64"))),
		BundleName: []byte(`{"bundle":true}`),
	}}
}

// fakeGitHub serves the releases API and the release assets. It lists
// releases in the order given, as GitHub lists them newest-published first.
type fakeGitHub struct {
	*httptest.Server
	mu       sync.Mutex
	releases map[string]*fakeRelease // by tag
	order    []*fakeRelease
	requests []string // request paths
	proxied  int      // requests sent as a proxy request (absolute URI)
}

func newFakeGitHub(t *testing.T, rels ...*fakeRelease) *fakeGitHub {
	g := &fakeGitHub{releases: map[string]*fakeRelease{}, order: rels}
	for _, r := range rels {
		g.releases[r.tag] = r
	}
	g.Server = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.Close)
	return g
}

func (g *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.requests = append(g.requests, r.URL.Path)
	if r.URL.IsAbs() {
		g.proxied++
	}
	g.mu.Unlock()
	base := "http://" + r.Host
	switch p := r.URL.Path; {
	case p == "/repos/"+DefaultRepo+"/releases":
		g.serveList(w, r, base)
	case strings.HasPrefix(p, "/repos/"+DefaultRepo+"/releases/tags/"):
		g.serveRelease(w, base, strings.TrimPrefix(p, "/repos/"+DefaultRepo+"/releases/tags/"))
	case strings.HasPrefix(p, "/download/"):
		parts := strings.SplitN(strings.TrimPrefix(p, "/download/"), "/", 2)
		rel, ok := g.releases[parts[0]]
		if !ok || len(parts) != 2 || rel.files[parts[1]] == nil {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(rel.files[parts[1]])
	default:
		http.NotFound(w, r)
	}
}

func (g *fakeGitHub) serveRelease(w http.ResponseWriter, base, tag string) {
	rel, ok := g.releases[tag]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	_ = json.NewEncoder(w).Encode(rel.api(base))
}

// serveList serves one page of the release list, paged like GitHub's
// (per_page defaults to 30, page to 1).
func (g *fakeGitHub) serveList(w http.ResponseWriter, r *http.Request, base string) {
	perPage, page := 30, 1
	if v, err := strconv.Atoi(r.URL.Query().Get("per_page")); err == nil {
		perPage = v
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil {
		page = v
	}
	out := []release{}
	for i := (page - 1) * perPage; i < page*perPage && i < len(g.order); i++ {
		out = append(out, g.order[i].api(base))
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (r *fakeRelease) api(base string) release {
	out := release{Tag: r.tag, Draft: r.draft, Prerelease: r.prerelease}
	for name := range r.files {
		if r.noAsset && strings.HasSuffix(name, ".tar.gz") {
			continue
		}
		out.Assets = append(out.Assets, asset{Name: name, URL: base + "/download/" + r.tag + "/" + name})
	}
	return out
}

func (g *fakeGitHub) downloads() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, p := range g.requests {
		if strings.HasPrefix(p, "/download/") {
			n++
		}
	}
	return n
}

func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// installed writes an old pic-sure binary into a fresh directory and returns
// its path. Tests never touch the real binary.
func installed(t *testing.T, dir string) string {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "pic-sure")
	if err := os.WriteFile(exe, []byte("old"), 0o750); err != nil {
		t.Fatal(err)
	}
	return exe
}

func newUpdater(g *fakeGitHub, exe string, sink events.Sink) *Updater {
	return &Updater{
		Current: "v2.0.0", APIBase: g.URL, Executable: exe,
		GOOS: "linux", GOARCH: "amd64", Sink: sink, Step: "self-update",
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// assertOnlyBinary fails if anything besides pic-sure is left in dir.
func assertOnlyBinary(t *testing.T, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != "pic-sure" {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want only pic-sure", names)
	}
}

func wantCode(t *testing.T, err error, code int, substr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("err = nil, want exit %d", code)
	}
	if got := exitcode.FromError(err); got != code {
		t.Errorf("exit code = %d, want %d (%v)", got, code, err)
	}
	if !strings.Contains(err.Error(), substr) {
		t.Errorf("err = %q, want it to contain %q", err, substr)
	}
}

func TestInstallReplacesTheBinary(t *testing.T) {
	g := newFakeGitHub(t, newFakeRelease(t, "v2.1.0"))
	exe := installed(t, "")
	var rec events.Recorder
	res, err := newUpdater(g, exe, &rec).Install(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, exe); got != newBinary {
		t.Errorf("binary = %q, want the release's", got)
	}
	fi, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o750 {
		t.Errorf("mode = %v, want the old binary's 0750", fi.Mode().Perm())
	}
	assertOnlyBinary(t, filepath.Dir(exe))
	want := Result{From: "v2.0.0", To: "v2.1.0", Path: res.Path, Updated: true, Signature: SignatureUnverified}
	if *res != want {
		t.Errorf("result = %+v, want %+v", *res, want)
	}
	if !strings.Contains(fmt.Sprint(rec.Events()), "cosign isn't installed") {
		t.Errorf("events %v don't warn that the signature went unchecked", rec.Events())
	}
}

func TestInstallFollowsSymlinks(t *testing.T) {
	g := newFakeGitHub(t, newFakeRelease(t, "v2.1.0"))
	real := installed(t, "")
	link := filepath.Join(t.TempDir(), "pic-sure")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := newUpdater(g, link, nil).Install(context.Background(), "2.1.0"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, real); got != newBinary {
		t.Errorf("link target = %q, want the release's binary", got)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the symlink was replaced (%v, %v)", fi, err)
	}
}

func TestInstallRejectsAChecksumMismatch(t *testing.T) {
	rel := newFakeRelease(t, "v2.1.0")
	rel.files[AssetName("linux", "amd64")] = tarGz(t, map[string]string{"pic-sure": "tampered"})
	g := newFakeGitHub(t, rel)
	exe := installed(t, "")
	_, err := newUpdater(g, exe, nil).Install(context.Background(), "v2.1.0")
	wantCode(t, err, exitcode.CodeFailed, "doesn't match its SHA-256 in checksums.txt")
	if got := readFile(t, exe); got != "old" {
		t.Errorf("binary = %q after a mismatch, want it untouched", got)
	}
	assertOnlyBinary(t, filepath.Dir(exe))
}

func TestInstallFailures(t *testing.T) {
	tests := []struct {
		name   string
		edit   func(*fakeRelease)
		to     string
		code   int
		substr string
	}{
		{name: "unknown version", to: "v9.9.9", code: exitcode.CodePrecondition, substr: "pic-sure release v9.9.9 doesn't exist"},
		{name: "no archive for the platform", edit: func(r *fakeRelease) { r.noAsset = true },
			code: exitcode.CodeFailed, substr: "has no pic-sure_linux_amd64.tar.gz"},
		{name: "no checksums.txt", edit: func(r *fakeRelease) { delete(r.files, ChecksumsName) },
			code: exitcode.CodeFailed, substr: "has no checksums.txt"},
		{name: "archive missing from checksums.txt", edit: func(r *fakeRelease) { r.files[ChecksumsName] = []byte("abc  other.tar.gz\n") },
			code: exitcode.CodeFailed, substr: "checksums.txt has no entry for pic-sure_linux_amd64.tar.gz"},
		{name: "archive without a binary", edit: func(r *fakeRelease) {
			a := tarGz(t, map[string]string{"bin/pic-sure": "nested"})
			sum := sha256.Sum256(a)
			r.files[AssetName("linux", "amd64")] = a
			r.files[ChecksumsName] = []byte(hex.EncodeToString(sum[:]) + "  " + AssetName("linux", "amd64") + "\n")
		}, code: exitcode.CodeFailed, substr: "has no pic-sure binary"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rel := newFakeRelease(t, "v2.1.0")
			if tt.edit != nil {
				tt.edit(rel)
			}
			exe := installed(t, "")
			_, err := newUpdater(newFakeGitHub(t, rel), exe, nil).Install(context.Background(), tt.to)
			wantCode(t, err, tt.code, tt.substr)
			if got := readFile(t, exe); got != "old" {
				t.Errorf("binary = %q, want it untouched", got)
			}
			assertOnlyBinary(t, filepath.Dir(exe))
		})
	}
}

func TestInstallNothingToDo(t *testing.T) {
	for _, tt := range []struct{ name, current, to string }{
		{name: "already the newest", current: "v2.1.0"},
		{name: "newer than the newest", current: "v2.2.0"},
		{name: "--to the running version", current: "v2.0.0", to: "v2.0.0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := newFakeGitHub(t, newFakeRelease(t, "v2.1.0"), newFakeRelease(t, "v2.0.0"))
			exe := installed(t, "")
			u := newUpdater(g, exe, nil)
			u.Current = tt.current
			res, err := u.Install(context.Background(), tt.to)
			if err != nil {
				t.Fatal(err)
			}
			if res.Updated || g.downloads() != 0 {
				t.Errorf("updated = %v after %d downloads, want nothing done", res.Updated, g.downloads())
			}
		})
	}
}

func TestInstallPicksTheNewestStableV2(t *testing.T) {
	stub := func(tag string) *fakeRelease { return &fakeRelease{tag: tag} }
	pre := newFakeRelease(t, "v2.3.0")
	pre.prerelease = true
	draft := newFakeRelease(t, "v2.4.0")
	draft.draft = true
	var filler []*fakeRelease
	for i := range releasesPerPage {
		filler = append(filler, stub(fmt.Sprintf("v1.%d.0", i)))
	}
	tests := []struct {
		name string
		rels []*fakeRelease
		want string
	}{
		// Listed newest-published first, so the backport comes before v2.2.0.
		{name: "a backport published after a newer release",
			rels: []*fakeRelease{newFakeRelease(t, "v2.1.5"), newFakeRelease(t, "v2.2.0"), newFakeRelease(t, "v2.1.0")}, want: "v2.2.0"},
		{name: "a prerelease or draft newer than the newest stable",
			rels: []*fakeRelease{pre, draft, newFakeRelease(t, "v2.2.0")}, want: "v2.2.0"},
		{name: "tags that aren't stable v2.x.y",
			rels: []*fakeRelease{stub("v3.0.0"), stub("v2.9.0-rc.1"), stub("2.9.0"), stub("v2.9"), stub("v10.0.0"),
				newFakeRelease(t, "v2.2.0")}, want: "v2.2.0"},
		{name: "minor and patch compare as numbers",
			rels: []*fakeRelease{newFakeRelease(t, "v2.9.0"), newFakeRelease(t, "v2.10.0"), newFakeRelease(t, "v2.9.10")}, want: "v2.10.0"},
		{name: "the newest is past the first page",
			rels: append(append([]*fakeRelease{newFakeRelease(t, "v2.1.0")}, filler...), newFakeRelease(t, "v2.2.0")), want: "v2.2.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := newUpdater(newFakeGitHub(t, tt.rels...), installed(t, ""), nil).Install(context.Background(), "")
			if err != nil {
				t.Fatal(err)
			}
			if res.To != tt.want || !res.Updated {
				t.Errorf("installed %s (updated %v), want %s", res.To, res.Updated, tt.want)
			}
		})
	}
}

func TestInstallWithoutAStableV2Release(t *testing.T) {
	pre := newFakeRelease(t, "v2.0.1-rc.1")
	pre.prerelease = true
	for _, tt := range []struct {
		name string
		rels []*fakeRelease
	}{
		{name: "no releases"},
		{name: "only other lines and prereleases", rels: []*fakeRelease{newFakeRelease(t, "v3.0.0"), newFakeRelease(t, "v1.9.0"), pre}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := newFakeGitHub(t, tt.rels...)
			exe := installed(t, "")
			_, err := newUpdater(g, exe, nil).Install(context.Background(), "")
			wantCode(t, err, exitcode.CodePrecondition, "has no stable v2.x.y release; pass --to")
			if got := readFile(t, exe); got != "old" || g.downloads() != 0 {
				t.Errorf("binary = %q after %d downloads, want it untouched", got, g.downloads())
			}
		})
	}
}

func TestInstallReadsAtMostTenPages(t *testing.T) {
	var rels []*fakeRelease
	for i := range maxReleasePages * releasesPerPage {
		rels = append(rels, &fakeRelease{tag: fmt.Sprintf("v1.%d.0", i)})
	}
	g := newFakeGitHub(t, append(rels, newFakeRelease(t, "v2.1.0"))...)
	_, err := newUpdater(g, installed(t, ""), nil).Install(context.Background(), "")
	wantCode(t, err, exitcode.CodePrecondition, "no stable v2.x.y release")
	if n := len(g.requests); n != maxReleasePages {
		t.Errorf("%d requests, want %d pages", n, maxReleasePages)
	}
}

func TestInstallDowngradesOnRequest(t *testing.T) {
	g := newFakeGitHub(t, newFakeRelease(t, "v1.9.0"))
	exe := installed(t, "")
	res, err := newUpdater(g, exe, nil).Install(context.Background(), "v1.9.0")
	if err != nil || !res.Updated {
		t.Fatalf("Install(v1.9.0) = %+v, %v; want an update", res, err)
	}
}

func TestInstallRefusesPackageManagedBinaries(t *testing.T) {
	g := newFakeGitHub(t, newFakeRelease(t, "v2.1.0"))
	exe := installed(t, filepath.Join(t.TempDir(), "Cellar", "pic-sure", "2.0.0", "bin"))
	link := filepath.Join(t.TempDir(), "pic-sure")
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}
	_, err := newUpdater(g, link, nil).Install(context.Background(), "v2.1.0")
	wantCode(t, err, exitcode.CodePrecondition, "Homebrew manages it; update it with `brew upgrade pic-sure`")
	if g.downloads() != 0 || readFile(t, exe) != "old" {
		t.Errorf("downloaded %d files or replaced the binary despite refusing", g.downloads())
	}
}

func TestManagedSystemPrefixes(t *testing.T) {
	for _, p := range []string{"/usr/bin/pic-sure", "/nix/store/abc-pic-sure/bin/pic-sure", "/opt/homebrew/Cellar/pic-sure/2.0/bin/pic-sure"} {
		if managed(p, "v2.1.0") == nil {
			t.Errorf("managed(%s) = nil, want a refusal", p)
		}
	}
	for _, p := range []string{"/usr/local/bin/pic-sure", "/home/me/.local/bin/pic-sure", "/opt/homebrew/bin/pic-sure"} {
		if r := managed(p, "v2.1.0"); r != nil {
			t.Errorf("managed(%s) = %v, want nil", p, r)
		}
	}
	r := managed("/usr/bin/pic-sure", "v2.1.0")
	if !strings.Contains(r.Error(), "/v2/install.sh | bash -s -- --version v2.1.0`") {
		t.Errorf("refusal %q doesn't give the install.sh command", r)
	}
}

func TestInstallRefusesAnUnwritableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	g := newFakeGitHub(t, newFakeRelease(t, "v2.1.0"))
	exe := installed(t, "")
	dir := filepath.Dir(exe)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	_, err := newUpdater(g, exe, nil).Install(context.Background(), "v2.1.0")
	wantCode(t, err, exitcode.CodePrecondition, "run `sudo pic-sure self-update --to v2.1.0`")
	if g.downloads() != 0 {
		t.Errorf("downloaded %d files despite refusing", g.downloads())
	}
}

func TestInstallGoesThroughTheProxy(t *testing.T) {
	g := newFakeGitHub(t, newFakeRelease(t, "v2.1.0"))
	proxy, err := url.Parse(g.URL)
	if err != nil {
		t.Fatal(err)
	}
	u := newUpdater(g, installed(t, ""), nil)
	// GitHub's address doesn't resolve; only the proxy can reach it.
	u.APIBase = "http://api.github.invalid"
	u.Proxy = http.ProxyURL(proxy)
	if _, err := u.Install(context.Background(), "v2.1.0"); err != nil {
		t.Fatal(err)
	}
	if g.proxied < 3 {
		t.Errorf("%d requests went through the proxy, want the lookup and both downloads", g.proxied)
	}
}

func TestInstallSignature(t *testing.T) {
	errBad := errors.New("bad signature")
	tests := []struct {
		name     string
		unsigned bool
		verify   func(context.Context, string, string, string) error
		require  bool
		want     string
		code     int
		substr   string
	}{
		{name: "verified", verify: func(context.Context, string, string, string) error { return nil }, want: SignatureVerified},
		{name: "bad signature", verify: func(context.Context, string, string, string) error { return errBad },
			code: exitcode.CodeFailed, substr: "the signature of checksums.txt doesn't verify: bad signature"},
		{name: "no cosign", want: SignatureUnverified},
		{name: "no cosign, required", require: true, code: exitcode.CodePrecondition, substr: "cosign isn't installed"},
		{name: "unsigned", unsigned: true, verify: func(context.Context, string, string, string) error { return nil },
			code: exitcode.CodeFailed, substr: "isn't signed (no checksums.txt.sigstore.json)"},
		{name: "unsigned, no cosign", unsigned: true, code: exitcode.CodeFailed, substr: "isn't signed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rel := newFakeRelease(t, "v2.1.0")
			if tt.unsigned {
				delete(rel.files, BundleName)
			}
			exe := installed(t, "")
			u := newUpdater(newFakeGitHub(t, rel), exe, nil)
			var gotBundle string
			if tt.verify != nil {
				u.VerifyBundle = func(ctx context.Context, tag, sums, bundle string) error {
					if tag != "v2.1.0" {
						t.Errorf("verified the signature for tag %q, want v2.1.0", tag)
					}
					if readFile(t, sums) != string(rel.files[ChecksumsName]) {
						t.Errorf("verified %s, want the downloaded checksums.txt", sums)
					}
					gotBundle = readFile(t, bundle)
					return tt.verify(ctx, tag, sums, bundle)
				}
			}
			u.RequireSignature = tt.require
			res, err := u.Install(context.Background(), "v2.1.0")
			if tt.substr != "" {
				wantCode(t, err, tt.code, tt.substr)
				if readFile(t, exe) != "old" {
					t.Error("replaced the binary despite the failure")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if res.Signature != tt.want {
				t.Errorf("signature = %q, want %q", res.Signature, tt.want)
			}
			if tt.verify != nil && gotBundle != `{"bundle":true}` {
				t.Errorf("verified bundle %q, want the release's", gotBundle)
			}
		})
	}
}

func TestSelfUpdateReExecs(t *testing.T) {
	g := newFakeGitHub(t, newFakeRelease(t, "v2.1.0"))
	exe := installed(t, "")
	u := newUpdater(g, exe, nil)
	u.Args = []string{"pic-sure", "update", "--self-update"}
	u.Environ = []string{"HOME=/home/me"}
	u.Getenv = func(string) string { return "" }
	var gotPath string
	var gotArgv, gotEnv []string
	u.Exec = func(path string, argv, env []string) error {
		gotPath, gotArgv, gotEnv = path, argv, env
		return errors.New("exec format error")
	}
	err := u.SelfUpdate(context.Background(), "v2.1.0")
	wantCode(t, err, exitcode.CodeFailed, "pic-sure was updated to v2.1.0, but running it failed")
	real, _ := filepath.EvalSymlinks(exe)
	if gotPath != real || strings.Join(gotArgv, " ") != "pic-sure update --self-update" {
		t.Errorf("exec(%q, %q), want the new binary with the original arguments", gotPath, gotArgv)
	}
	if strings.Join(gotEnv, " ") != "HOME=/home/me "+ReexecEnv+"=v2.1.0" {
		t.Errorf("env = %q, want the original plus %s", gotEnv, ReexecEnv)
	}
}

func TestSelfUpdateRefusals(t *testing.T) {
	g := newFakeGitHub(t, newFakeRelease(t, "v2.1.0"))
	t.Run("package-managed", func(t *testing.T) {
		u := newUpdater(g, installed(t, filepath.Join(t.TempDir(), "Cellar", "bin")), nil)
		u.Getenv = func(string) string { return "" }
		err := u.SelfUpdate(context.Background(), "v2.1.0")
		wantCode(t, err, exitcode.CodeIncompatible, "`brew upgrade pic-sure`, or pass --ignore-cli-version")
	})
	t.Run("already re-executed", func(t *testing.T) {
		u := newUpdater(g, installed(t, ""), nil)
		u.Getenv = func(k string) string {
			if k == ReexecEnv {
				return "v2.1.0"
			}
			return ""
		}
		u.Exec = func(string, []string, []string) error { t.Fatal("exec'd again"); return nil }
		err := u.SelfUpdate(context.Background(), "v2.2.0")
		wantCode(t, err, exitcode.CodeIncompatible, "updated itself to v2.1.0 and re-ran, but this release still needs pic-sure v2.2.0")
	})
}

func TestCosignVerifier(t *testing.T) {
	if v := CosignVerifier(fakerunner.New(t), DefaultRepo, func(string) (string, error) { return "", exec.ErrNotFound }); v != nil {
		t.Error("CosignVerifier without cosign on PATH isn't nil")
	}
	f := fakerunner.New(t)
	f.On(fakerunner.Exact("cosign", "verify-blob", "--bundle", "/tmp/b.json",
		"--certificate-identity", "https://github.com/JamesPeck/pic-sure-cli/.github/workflows/release.yml@refs/tags/v2.1.0",
		"--certificate-oidc-issuer", "https://token.actions.githubusercontent.com", "/tmp/checksums.txt")).Exit(1).Stderr("no matching signatures")
	v := CosignVerifier(f, DefaultRepo, func(string) (string, error) { return "/usr/local/bin/cosign", nil })
	if err := v(context.Background(), "v2.1.0", "/tmp/checksums.txt", "/tmp/b.json"); err == nil || !strings.Contains(err.Error(), "no matching signatures") {
		t.Errorf("verify = %v, want cosign's failure", err)
	}
}

// cancelAfter is a RoundTripper that buffers the response for a request whose
// path ends in suffix and then calls cancel, so the download completes but
// the context is done.
type cancelAfter struct {
	suffix string
	cancel context.CancelFunc
}

func (c cancelAfter) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil || !strings.HasSuffix(req.URL.Path, c.suffix) {
		return resp, err
	}
	var buf bytes.Buffer
	_, err = buf.ReadFrom(resp.Body)
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(&buf)
	c.cancel()
	return resp, err
}

func TestInstallStopsWhenCancelledAfterDownloading(t *testing.T) {
	g := newFakeGitHub(t, newFakeRelease(t, "v2.1.0"))
	exe := installed(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	u := newUpdater(g, exe, nil)
	u.Client = &http.Client{Transport: cancelAfter{suffix: ".tar.gz", cancel: cancel}}
	u.Getenv = func(string) string { return "" }
	u.Exec = func(string, []string, []string) error { t.Fatal("exec'd after cancellation"); return nil }
	if err := u.SelfUpdate(ctx, "v2.1.0"); !errors.Is(err, context.Canceled) {
		t.Errorf("SelfUpdate = %v, want context.Canceled", err)
	}
	if got := readFile(t, exe); got != "old" {
		t.Errorf("binary = %q after cancellation, want it untouched", got)
	}
}

func TestDownloadAbandonsAStalledServer(t *testing.T) {
	old := stallTimeout
	stallTimeout = 100 * time.Millisecond
	t.Cleanup(func() { stallTimeout = old })
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		<-release
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	u := &Updater{Current: "v2.0.0"}
	_, err := u.download(context.Background(), asset{Name: "pic-sure_linux_amd64.tar.gz", URL: srv.URL}, filepath.Join(t.TempDir(), "a"), maxArchive)
	if !errors.Is(err, errStalled) {
		t.Errorf("download = %v, want %v", err, errStalled)
	}
}

func TestErrorsDontShowProxyCredentials(t *testing.T) {
	g := newFakeGitHub(t, newFakeRelease(t, "v2.1.0"))
	u := newUpdater(g, installed(t, ""), nil)
	u.APIBase = "http://api.github.invalid"
	u.Proxy = http.ProxyURL(&url.URL{Scheme: "http", User: url.UserPassword("me", "s3cret"), Host: "127.0.0.1:1"})
	_, err := u.Install(context.Background(), "v2.1.0")
	if err == nil || strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "http://") {
		t.Errorf("Install = %v, want a failure naming neither the proxy's password nor a URL", err)
	}
}
