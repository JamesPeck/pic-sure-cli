package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/selfupdate"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

const validConfig = "schema: 1\nname: demo\nauth: {admin_email: admin@example.org, auth0: {client_id: abc}}\n"

func TestSelfUpdateProxy(t *testing.T) {
	withConfig := func(t *testing.T, config string) string {
		dir := newTestStack(t)
		st, err := stack.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = st.Close() }()
		if err := st.WriteFile(stack.ConfigFile, []byte(config), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	tests := []struct {
		name    string
		dir     func(t *testing.T) string
		enabled bool
		warn    bool
	}{
		{name: "outside a stack", dir: func(t *testing.T) string { return t.TempDir() }},
		{name: "a stack without a proxy", dir: func(t *testing.T) string { return withConfig(t, validConfig) }},
		{name: "a stack with a proxy", enabled: true, dir: func(t *testing.T) string {
			return withConfig(t, validConfig+"proxy:\n  https: http://me:s3cret@proxy.example:3128\n")
		}},
		{name: "a stack whose config doesn't load", warn: true, dir: func(t *testing.T) string {
			return withConfig(t, "schema: 99\n")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(tt.dir(t))
			a, _, _ := testApp(t)
			var rec events.Recorder
			p := a.selfUpdateProxy(&rec)
			if got := p != nil && p.Enabled(); got != tt.enabled {
				t.Errorf("proxy enabled = %v, want %v", got, tt.enabled)
			}
			if got := len(rec.Events()) > 0; got != tt.warn {
				t.Errorf("events = %v, want a warning: %v", rec.Events(), tt.warn)
			}
		})
	}
}

func TestNewSelfUpdaterRequireSignature(t *testing.T) {
	for v, want := range map[string]bool{"": false, "0": false, "1": true} {
		t.Setenv(selfupdate.RequireSignatureEnv, v)
		a, _, _ := testApp(t)
		if got := a.newSelfUpdater(nil, &events.Recorder{}, "self-update").RequireSignature; got != want {
			t.Errorf("%s=%q: RequireSignature = %v, want %v", selfupdate.RequireSignatureEnv, v, got, want)
		}
	}
}

// TestSelfUpdateRequireSignatureFlag runs self-update without cosign
// against a release whose checksums.txt doesn't list the archive, so no
// run gets as far as downloading or replacing anything.
func TestSelfUpdateRequireSignatureFlag(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/" + selfupdate.DefaultRepo + "/releases/tags/v9.9.9":
			var assets []string
			for _, name := range []string{selfupdate.ChecksumsName, selfupdate.BundleName, selfupdate.AssetName(runtime.GOOS, runtime.GOARCH)} {
				assets = append(assets, fmt.Sprintf(`{"name":%q,"browser_download_url":%q}`, name, srv.URL+"/dl/"+name))
			}
			_, _ = fmt.Fprintf(w, `{"tag_name":"v9.9.9","assets":[%s]}`, strings.Join(assets, ","))
		case "/dl/" + selfupdate.ChecksumsName:
			_, _ = io.WriteString(w, "abc  other.tar.gz\n")
		case "/dl/" + selfupdate.BundleName:
			_, _ = io.WriteString(w, "{}")
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv(releaseAPIEnv, srv.URL)
	t.Setenv(selfupdate.RequireSignatureEnv, "")
	t.Setenv("PATH", t.TempDir()) // no cosign
	t.Chdir(t.TempDir())
	for _, tt := range []struct {
		flag   bool
		code   int
		stderr string
	}{
		{flag: true, code: exitcode.CodePrecondition, stderr: "cosign isn't installed to check it, and a signature is required"},
		{flag: false, code: exitcode.CodeFailed, stderr: "checksums.txt has no entry"},
	} {
		a, _, stderr := testApp(t)
		args := []string{"self-update", "--to", "v9.9.9"}
		if tt.flag {
			args = append(args, "--require-signature")
		}
		if code := a.Run(context.Background(), args); code != tt.code || !strings.Contains(stderr.String(), tt.stderr) {
			t.Errorf("%v: exit %d, stderr %q; want exit %d and %q", args, code, stderr, tt.code, tt.stderr)
		}
	}
}
