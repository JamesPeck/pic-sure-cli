package wizard

import (
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

func newTestForm(t *testing.T) *Form {
	t.Helper()
	base := stack.DefaultConfig()
	base.Name = "demo"
	return NewForm(base)
}

func set(f *Form, key, v string) { *f.vals[key] = v }

func TestGroupItemsAreConfigFields(t *testing.T) {
	for _, g := range Groups {
		for _, it := range g.Items {
			if _, ok := stack.LookupField(it.Key); !ok {
				t.Errorf("group %q asks for %s, which stack.Fields doesn't have", g.Title, it.Key)
			}
		}
	}
}

func TestResultOpenModeNeedsNoSecret(t *testing.T) {
	f := newTestForm(t)
	set(f, "auth.mode", "open")
	set(f, "auth.admin_email", "admin@example.com")
	set(f, "network.http_port", "8080")
	set(f, "network.https_port", "8443")
	if err := f.Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}
	doc, sec, err := f.Result()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := doc.Config()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "demo" || cfg.Auth.Mode != stack.AuthOpen || cfg.Network.HTTPPort != 8080 || cfg.Network.HTTPSPort != 8443 {
		t.Errorf("config = %+v", cfg)
	}
	if sec.Auth0ClientSecret != "" {
		t.Errorf("open mode supplied a client secret %q; init generates one", sec.Auth0ClientSecret)
	}
}

func TestRequiredModeNeedsALongEnoughClientSecret(t *testing.T) {
	f := newTestForm(t)
	set(f, "auth.admin_email", "admin@example.com")
	set(f, "auth.auth0.client_id", "abc")
	check := f.validator(clientSecretKey)
	if err := check(""); err == nil || !strings.Contains(err.Error(), "required") {
		t.Errorf("empty secret: %v, want required", err)
	}
	if err := check("short"); err == nil || !strings.Contains(err.Error(), "at least 32") {
		t.Errorf("short secret: %v, want the PSAMA minimum", err)
	}
	long := strings.Repeat("s", 40)
	if err := check(long); err != nil {
		t.Errorf("40-byte secret: %v", err)
	}
	if err := f.Check(); err == nil {
		t.Error("Check passed with no client secret entered")
	}
	set(f, clientSecretKey, long)
	_, sec, err := f.Result()
	if err != nil || string(sec.Auth0ClientSecret) != long {
		t.Errorf("Result secret = %q, %v", sec.Auth0ClientSecret, err)
	}
}

func TestValidatorReportsTheConfigProblemAtItsKey(t *testing.T) {
	f := newTestForm(t)
	set(f, "network.http_port", "8080")
	if err := f.validator("network.https_port")("8080"); err == nil {
		t.Error("an HTTPS port equal to the HTTP port passed")
	}
	if err := f.validator("network.http_port")("eighty"); err == nil || !strings.Contains(err.Error(), "whole number") {
		t.Errorf("non-numeric port: %v", err)
	}
	if err := f.validator("name")("Bad Name"); err == nil {
		t.Error("an invalid stack name passed")
	}
	// Another field's problem (the empty admin email) isn't this field's.
	if err := f.validator("release.branch")("main"); err != nil {
		t.Errorf("release.branch: %v", err)
	}
}

func TestRemoteDatabaseFieldsApplyOnlyInRemoteMode(t *testing.T) {
	f := newTestForm(t)
	set(f, "auth.mode", "open")
	set(f, "auth.admin_email", "admin@example.com")
	set(f, "db.remote.host", "db.example.com")
	set(f, rootPasswordKey, "pw")
	doc, sec, err := f.Result()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := doc.Config()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DB.Remote.Host != "" || sec.DBRemoteRootPassword != "" {
		t.Errorf("local mode kept the remote fields: host %q, password set %v", cfg.DB.Remote.Host, sec.DBRemoteRootPassword != "")
	}
	set(f, "db.mode", "remote")
	set(f, rootPasswordKey, "")
	if err := f.Check(); err == nil || !strings.Contains(err.Error(), "db-root-password-stdin") {
		t.Errorf("remote mode without a root password: %v", err)
	}
}

func TestHTTPSProxyFollowsHTTPUntilEdited(t *testing.T) {
	f := newTestForm(t)
	set(f, proxyHTTPKey, "http://proxy:3128")
	f.syncHTTPSProxy()
	if got := f.Value(proxyHTTPSKey); got != "http://proxy:3128" {
		t.Fatalf("https = %q, want it pre-filled from http", got)
	}
	set(f, proxyHTTPKey, "http://proxy:8080")
	f.syncHTTPSProxy()
	if got := f.Value(proxyHTTPSKey); got != "http://proxy:8080" {
		t.Fatalf("https = %q, want it to follow http", got)
	}
	set(f, proxyHTTPSKey, "") // the user clears it
	set(f, proxyHTTPKey, "http://other:1")
	f.syncHTTPSProxy()
	if got := f.Value(proxyHTTPSKey); got != "" {
		t.Errorf("https = %q, want the user's clearing to stick", got)
	}
}

func TestNoProxyClearsTheProxyFields(t *testing.T) {
	base := stack.DefaultConfig()
	base.Name = "demo"
	base.Proxy.HTTP = "http://proxy:3128"
	f := NewForm(base)
	if !f.useProxy {
		t.Fatal("a config with a proxy didn't open with \"Use a proxy\"")
	}
	f.useProxy = false
	set(f, "auth.admin_email", "admin@example.com")
	set(f, "auth.mode", "open")
	doc, _, err := f.Result()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := doc.Config()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Proxy.HTTP != "" || cfg.Proxy.HTTPS != "" {
		t.Errorf("proxy = %+v, want none", cfg.Proxy)
	}
}

func TestSummaryMasksSecretsAndSkipsHiddenGroups(t *testing.T) {
	f := newTestForm(t)
	set(f, "auth.admin_email", "admin@example.com")
	set(f, clientSecretKey, strings.Repeat("s", 40))
	got := f.summary()
	if strings.Contains(got, "sss") || !strings.Contains(got, "********") {
		t.Errorf("summary shows the secret:\n%s", got)
	}
	if strings.Contains(got, "Admin password") {
		t.Errorf("summary lists the remote database in local mode:\n%s", got)
	}
	if !strings.Contains(got, "Proxy") || !strings.Contains(got, "none") {
		t.Errorf("summary doesn't say there is no proxy:\n%s", got)
	}
	set(f, "auth.mode", "open")
	if got := f.summary(); strings.Contains(got, "Auth0") {
		t.Errorf("open-mode summary lists Auth0:\n%s", got)
	}
}

func TestDirty(t *testing.T) {
	f := newTestForm(t)
	if f.Dirty() {
		t.Fatal("a fresh form is dirty")
	}
	set(f, "auth.admin_email", "a@example.com")
	if !f.Dirty() {
		t.Error("an edited form isn't dirty")
	}
}
