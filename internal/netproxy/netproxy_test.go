package netproxy_test

import (
	"bytes"
	"encoding/xml"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/netproxy"
)

func mustNew(t *testing.T, c netproxy.Config, services ...string) *netproxy.Proxy {
	t.Helper()
	p, err := netproxy.New(c, services)
	if err != nil {
		t.Fatalf("New(%+v): %v", c, err)
	}
	return p
}

func TestParseURL(t *testing.T) {
	tests := []struct {
		in      string
		want    string // the normalized URL, or "" for none
		wantErr string
	}{
		{in: "", want: ""},
		{in: "http://proxy.example.org:3128", want: "http://proxy.example.org:3128"},
		{in: "http://proxy.example.org:3128/", want: "http://proxy.example.org:3128"},
		{in: "http://proxy.example.org", want: "http://proxy.example.org:80"},
		{in: "https://proxy.example.org", wantErr: `want an http:// URL, got "https://proxy.example.org": the JVM and Maven can only speak plain HTTP to a proxy, so write http://`},
		{in: "HTTP://Proxy.Example.org:08080", want: "http://Proxy.Example.org:8080"},
		{in: "http://user:p%40ss@proxy:3128", want: "http://user:p%40ss@proxy:3128"},
		{in: "http://user@proxy", want: "http://user@proxy:80"},
		{in: "http://[2001:db8::1]:3128", want: "http://[2001:db8::1]:3128"},
		{in: "https://user:hunter2@[2001:db8::1]", wantErr: `got "https://user:xxxxx@[2001:db8::1]"`},
		{in: "http://10.0.0.5", want: "http://10.0.0.5:80"},
		{in: "http://proxy.example.org.:3128", want: "http://proxy.example.org.:3128"},

		{in: "proxy:3128", wantErr: "want an http:// URL"},
		{in: "socks5://user:hunter2@proxy:1080", wantErr: `want an http:// URL, got "socks5://user:xxxxx@proxy:1080"`},
		{in: "http://user:hunter2@:3128", wantErr: `has no host: "http://user:xxxxx@:3128"`},
		{in: "http://proxy/path", wantErr: "want only a scheme, host and port"},
		{in: "http://proxy?x=1", wantErr: "want only a scheme, host and port"},
		{in: "http://user:hunter2@proxy:port", wantErr: "is not a valid URL"},
		{in: "http://user:hunter2@proxy:0", wantErr: `has a port outside 1-65535: "http://user:xxxxx@proxy:0"`},
		{in: "http://user:hunter2@proxy:65536", wantErr: "has a port outside 1-65535"},
		{in: " http://proxy", wantErr: "is not a valid URL"},
		{in: "http://user:hunter2@proxy.example.org;3128", wantErr: `has an invalid host: "http://user:xxxxx@proxy.example.org;3128"`},
		{in: "http://proxy.example.org,3128", wantErr: "has an invalid host"},
		{in: `http://pr"oxy:3128`, wantErr: "has an invalid host"},
		{in: "http://a$(id)b:3128", wantErr: "has an invalid host"},
		{in: "http://10.1.2.300:3128", wantErr: "has an invalid host"},
		{in: "http://10.1.2.99999999999999999999:3128", wantErr: "has an invalid host"},
		{in: "http://.:3128", wantErr: "has an invalid host"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			u, err := netproxy.ParseURL(tt.in)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ParseURL(%q) = %v, %v; want error containing %q", tt.in, u, err, tt.wantErr)
				}
				if strings.Contains(err.Error(), "hunter2") {
					t.Errorf("the error shows the password: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseURL(%q): %v", tt.in, err)
			}
			got := ""
			if u != nil {
				got = u.String()
			}
			if got != tt.want {
				t.Errorf("ParseURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseNoProxy(t *testing.T) {
	tests := []struct {
		in      string
		want    []string
		wantErr string
	}{
		{in: "", want: nil},
		{in: " , ,", want: nil},
		{in: "Example.COM, .internal.example.org,*.corp.example", want: []string{"example.com", ".internal.example.org", ".corp.example"}},
		{in: "*", want: []string{"*"}},
		{in: "10.1.2.3/8,2001:db8::/32", want: []string{"10.0.0.0/8", "2001:db8::/32"}},
		{in: "10.0.0.1,::1,[2001:db8::5],2001:DB8::6", want: []string{"10.0.0.1", "::1", "2001:db8::5", "2001:db8::6"}},
		{in: "registry.example.org:5000,10.0.0.1:8080,[::1]:8443", want: []string{"registry.example.org:5000", "10.0.0.1:8080", "[::1]:8443"}},
		{in: "my_host,a-b.c", want: []string{"my_host", "a-b.c"}},

		{in: "ok, bad host", wantErr: `isn't a host, domain, IP address or CIDR range: "bad host"`},
		{in: "a|b", wantErr: `"a|b"`},
		{in: "-a.example", wantErr: `"-a.example"`},
		{in: "a..example", wantErr: `"a..example"`},
		{in: "example.*", wantErr: `"example.*"`},
		{in: "*example.com", wantErr: `"*example.com"`},
		{in: "10.0.0.0/8:80", wantErr: `"10.0.0.0/8:80"`},
		{in: "host:0", wantErr: `has an entry with an invalid port: "host:0"`},
		{in: "host:http", wantErr: `has an entry with an invalid port: "host:http"`},
		{in: "[::1", wantErr: `"[::1"`},
		{in: "999.1.1.1", wantErr: `"999.1.1.1"`},
		{in: "1.2.3", wantErr: `"1.2.3"`},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := netproxy.ParseNoProxy(tt.in)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ParseNoProxy(%q) = %q, %v; want error containing %q", tt.in, got, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseNoProxy(%q): %v", tt.in, err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("ParseNoProxy(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestNewErrorsNameTheKey(t *testing.T) {
	tests := []struct {
		c    netproxy.Config
		want string
	}{
		{netproxy.Config{HTTP: "ftp://proxy"}, "proxy.http: "},
		{netproxy.Config{HTTPS: "http://proxy/x"}, "proxy.https: "},
		{netproxy.Config{HTTP: "http://proxy", NoProxy: "a b"}, "proxy.no_proxy: "},
		// A bad no_proxy is an error even with no proxy to apply it to.
		{netproxy.Config{NoProxy: "a b"}, "proxy.no_proxy: "},
	}
	for _, tt := range tests {
		_, err := netproxy.New(tt.c, nil)
		if err == nil || !strings.HasPrefix(err.Error(), tt.want) {
			t.Errorf("New(%+v) = %v, want an error starting %q", tt.c, err, tt.want)
		}
	}
}

func TestEffectiveNoProxy(t *testing.T) {
	p := mustNew(t, netproxy.Config{
		HTTP:    "http://proxy:3128",
		NoProxy: "Example.com,psama,*.example.com,.example.com,127.0.0.1",
	}, "psama", "hpds", "httpd")
	want := []string{"example.com", "psama", ".example.com", "127.0.0.1", "hpds", "httpd", "localhost"}
	if got := p.NoProxy(); !slices.Equal(got, want) {
		t.Errorf("NoProxy() = %q, want %q", got, want)
	}
}

func TestCatalogServices(t *testing.T) {
	got := netproxy.CatalogServices()
	if len(got) != len(catalog.Services()) || !slices.Contains(got, "psama") || !slices.Contains(got, "picsure-db") {
		t.Errorf("CatalogServices() = %q, want every catalog service", got)
	}
}

func TestNoProxyMeansEveryOutputIsEmpty(t *testing.T) {
	for _, p := range []*netproxy.Proxy{
		mustNew(t, netproxy.Config{}, "psama"),
		mustNew(t, netproxy.Config{NoProxy: "example.com"}, "psama"),
		{},
	} {
		if p.Enabled() {
			t.Errorf("%v: Enabled() = true", p)
		}
		if got := p.NoProxy(); got != nil {
			t.Errorf("NoProxy() = %q, want nil", got)
		}
		if got := p.Env(); got != nil {
			t.Errorf("Env() = %q, want nil", got)
		}
		if got := p.BuildArgs(); got != nil {
			t.Errorf("BuildArgs() = %q, want nil", got)
		}
		if got := p.JVMOpts(); got != nil {
			t.Errorf("JVMOpts() = %q, want nil", got)
		}
		if got := p.MavenSettings(); got != nil {
			t.Errorf("MavenSettings() = %q, want nil", got)
		}
		req := httptest.NewRequest("GET", "https://example.org/", nil)
		if u, err := p.ProxyURL(req); u != nil || err != nil {
			t.Errorf("ProxyURL() = %v, %v; want nil, nil", u, err)
		}
		if got := p.String(); got != "no proxy" {
			t.Errorf("String() = %q", got)
		}
	}
}

func TestEnv(t *testing.T) {
	tests := []struct {
		name string
		c    netproxy.Config
		want []string
	}{
		{"http only", netproxy.Config{HTTP: "http://proxy.example.org:3128"}, []string{
			"HTTP_PROXY=http://proxy.example.org:3128", "http_proxy=http://proxy.example.org:3128",
			"HTTPS_PROXY=", "https_proxy=", "ALL_PROXY=", "all_proxy=",
			"NO_PROXY=localhost,127.0.0.1", "no_proxy=localhost,127.0.0.1",
		}},
		{"https only, missing port", netproxy.Config{HTTPS: "http://proxy.example.org"}, []string{
			"HTTP_PROXY=", "http_proxy=",
			"HTTPS_PROXY=http://proxy.example.org:80", "https_proxy=http://proxy.example.org:80",
			"ALL_PROXY=", "all_proxy=",
			"NO_PROXY=localhost,127.0.0.1", "no_proxy=localhost,127.0.0.1",
		}},
		{"both, credentials, IPv6, user entries", netproxy.Config{
			HTTP:    "http://user:p%40ss@[2001:db8::1]",
			HTTPS:   "http://user:p%40ss@[2001:db8::1]:3128/",
			NoProxy: "*.corp.example,10.0.0.0/8",
		}, []string{
			"HTTP_PROXY=http://user:p%40ss@[2001:db8::1]:80", "http_proxy=http://user:p%40ss@[2001:db8::1]:80",
			"HTTPS_PROXY=http://user:p%40ss@[2001:db8::1]:3128", "https_proxy=http://user:p%40ss@[2001:db8::1]:3128",
			"ALL_PROXY=", "all_proxy=",
			"NO_PROXY=.corp.example,10.0.0.0/8,localhost,127.0.0.1", "no_proxy=.corp.example,10.0.0.0/8,localhost,127.0.0.1",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := mustNew(t, tt.c)
			if got := p.Env(); !slices.Equal(got, tt.want) {
				t.Errorf("Env() =\n%q\nwant\n%q", got, tt.want)
			}
			if got := p.BuildArgs(); !slices.Equal(got, tt.want) {
				t.Errorf("BuildArgs() =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

func TestEnvAddsServices(t *testing.T) {
	p := mustNew(t, netproxy.Config{HTTP: "http://proxy:3128"}, "psama", "hpds")
	want := "NO_PROXY=hpds,psama,localhost,127.0.0.1"
	if env := p.Env(); !slices.Contains(env, want) {
		t.Errorf("Env() = %q, want it to contain %q", env, want)
	}
}

// fixedJava is the end of every nonProxyHosts for a stack with one service,
// psama: the service, localhost, 127.0.0.1, and the JVM's loopback defaults.
const fixedJava = "psama|*.psama|localhost|*.localhost|127.0.0.1|127.*|[::1]"

func TestJVMOpts(t *testing.T) {
	tests := []struct {
		name string
		c    netproxy.Config
		want []string
	}{
		{"http only", netproxy.Config{HTTP: "http://proxy.example.org:3128"}, []string{
			"-Dhttp.proxyHost=proxy.example.org", "-Dhttp.proxyPort=3128",
			"-Dhttp.nonProxyHosts=" + fixedJava,
		}},
		{"both, credentials dropped, missing ports", netproxy.Config{
			HTTP: "http://user:hunter2@proxy.example.org", HTTPS: "http://user:hunter2@proxy.example.org",
		}, []string{
			"-Dhttp.proxyHost=proxy.example.org", "-Dhttp.proxyPort=80",
			"-Dhttps.proxyHost=proxy.example.org", "-Dhttps.proxyPort=80",
			"-Dhttp.nonProxyHosts=" + fixedJava,
		}},
		{"IPv6 proxy", netproxy.Config{HTTPS: "http://[2001:db8::1]:3128"}, []string{
			"-Dhttps.proxyHost=2001:db8::1", "-Dhttps.proxyPort=3128",
			"-Dhttp.nonProxyHosts=" + fixedJava,
		}},
		{"no_proxy translated", netproxy.Config{
			HTTP: "http://proxy:3128",
			NoProxy: "example.com,.sub.example,*.star.example,registry.example:5000,10.0.0.0/8," +
				"192.168.1.0/24,2001:db8::/32,10.0.0.1:8080,2001:db8::5,[::1]:8443,single",
		}, []string{
			"-Dhttp.proxyHost=proxy", "-Dhttp.proxyPort=3128",
			"-Dhttp.nonProxyHosts=example.com|*.example.com|*.sub.example|*.star.example|registry.example|*.registry.example|" +
				// [::1] is already listed, so it isn't repeated at the end.
				"10.*|192.168.1.*|10.0.0.1|[2001:db8::5]|[::1]|single|*.single|psama|*.psama|localhost|*.localhost|127.0.0.1|127.*",
		}},
		{"star", netproxy.Config{HTTP: "http://proxy:3128", NoProxy: "*"}, []string{
			"-Dhttp.proxyHost=proxy", "-Dhttp.proxyPort=3128",
			"-Dhttp.nonProxyHosts=*|" + fixedJava,
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mustNew(t, tt.c, "psama").JVMOpts()
			if !slices.Equal(got, tt.want) {
				t.Errorf("JVMOpts() =\n%q\nwant\n%q", got, tt.want)
			}
			for _, o := range got {
				if strings.ContainsAny(o, " \t\n") || strings.Contains(o, "hunter2") || strings.Contains(o, "user") {
					t.Errorf("option %q has white space or credentials", o)
				}
			}
		})
	}
}

// Every IPv4 range is written exactly, as prefix patterns or addresses.
func TestJVMOptsIPv4Ranges(t *testing.T) {
	octets := func(from, to int) string {
		var p []string
		for i := from; i <= to; i++ {
			p = append(p, strconv.Itoa(i)+".*")
		}
		return strings.Join(p, "|")
	}
	tests := []struct {
		cidr string
		want string
	}{
		{"10.0.0.0/8", "10.*|" + fixedJava},
		{"172.16.0.0/12", "172.16.*|172.17.*|172.18.*|172.19.*|172.20.*|172.21.*|172.22.*|172.23.*|" +
			"172.24.*|172.25.*|172.26.*|172.27.*|172.28.*|172.29.*|172.30.*|172.31.*|" + fixedJava},
		{"10.20.0.0/15", "10.20.*|10.21.*|" + fixedJava},
		{"192.168.1.0/24", "192.168.1.*|" + fixedJava},
		{"10.9.8.4/30", "10.9.8.4|10.9.8.5|10.9.8.6|10.9.8.7|" + fixedJava},
		{"10.9.8.7/32", "10.9.8.7|" + fixedJava},
		{"128.0.0.0/1", octets(128, 255) + "|" + fixedJava},
		// 127.* is among the range's patterns, so it isn't repeated.
		{"0.0.0.0/0", octets(0, 255) + "|psama|*.psama|localhost|*.localhost|127.0.0.1|[::1]"},
		{"2001:db8::/32", fixedJava},
	}
	for _, tt := range tests {
		t.Run(tt.cidr, func(t *testing.T) {
			want := "-Dhttp.nonProxyHosts=" + tt.want
			opts := mustNew(t, netproxy.Config{HTTP: "http://proxy:3128", NoProxy: tt.cidr}, "psama").JVMOpts()
			if got := opts[len(opts)-1]; got != want {
				t.Errorf("got  %s\nwant %s", got, want)
			}
		})
	}
}

func TestMavenSettings(t *testing.T) {
	tests := []struct {
		name string
		c    netproxy.Config
		want string
	}{
		// Maven would send https through the http proxy.
		{"http only", netproxy.Config{HTTP: "http://proxy.example.org:3128"}, ""},
		{"https only, no credentials", netproxy.Config{HTTPS: "http://proxy.example.org:3128", NoProxy: ".corp.example,2001:db8::5"}, `<?xml version="1.0" encoding="UTF-8"?>
<settings xmlns="http://maven.apache.org/SETTINGS/1.0.0">
  <proxies>
    <proxy>
      <id>https-proxy</id>
      <active>true</active>
      <protocol>https</protocol>
      <host>proxy.example.org</host>
      <port>3128</port>
      <nonProxyHosts>*.corp.example|psama|*.psama|localhost|*.localhost|127.0.0.1|127.*</nonProxyHosts>
    </proxy>
  </proxies>
</settings>
`},
		{"both, credentials escaped, IPv6, missing port", netproxy.Config{
			HTTP:  "http://us%3Cer:p%26ss%3C%22@[2001:db8::1]",
			HTTPS: "http://other@proxy.example.org",
		}, `<?xml version="1.0" encoding="UTF-8"?>
<settings xmlns="http://maven.apache.org/SETTINGS/1.0.0">
  <proxies>
    <proxy>
      <id>http-proxy</id>
      <active>true</active>
      <protocol>http</protocol>
      <host>2001:db8::1</host>
      <port>80</port>
      <username>us&lt;er</username>
      <password>p&amp;ss&lt;&#34;</password>
      <nonProxyHosts>psama|*.psama|localhost|*.localhost|127.0.0.1|127.*</nonProxyHosts>
    </proxy>
    <proxy>
      <id>https-proxy</id>
      <active>true</active>
      <protocol>https</protocol>
      <host>proxy.example.org</host>
      <port>80</port>
      <username>other</username>
      <nonProxyHosts>psama|*.psama|localhost|*.localhost|127.0.0.1|127.*</nonProxyHosts>
    </proxy>
  </proxies>
</settings>
`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mustNew(t, tt.c, "psama").MavenSettings()
			if string(got) != tt.want {
				t.Errorf("MavenSettings() =\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}

// A password survives the trip through settings.xml whatever it holds.
func TestMavenSettingsRoundTripsCredentials(t *testing.T) {
	pass := `a&b<c>d"e'f]]>g`
	u := url.URL{Scheme: "http", User: url.UserPassword("me", pass), Host: "proxy:3128"}
	p := mustNew(t, netproxy.Config{HTTPS: u.String()})
	var s struct {
		Proxies []struct {
			Username string `xml:"username"`
			Password string `xml:"password"`
		} `xml:"proxies>proxy"`
	}
	if err := xml.Unmarshal(p.MavenSettings(), &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Proxies) != 1 || s.Proxies[0].Username != "me" || s.Proxies[0].Password != pass {
		t.Errorf("parsed back %+v, want me / %q", s.Proxies, pass)
	}
}

func TestProxyURL(t *testing.T) {
	p := mustNew(t, netproxy.Config{
		HTTP:    "http://user:pw@hproxy:3128",
		HTTPS:   "http://sproxy:3129",
		NoProxy: "example.com,.sub.example,registry.example:5000,10.0.0.0/8,192.168.1.1,[2001:db8::5]:8443",
	}, "psama")
	tests := []struct {
		url  string
		want string // the proxy, or "" for direct
	}{
		{"http://github.com/x", "http://user:pw@hproxy:3128"},
		{"https://github.com/x", "http://sproxy:3129"},
		{"ftp://github.com/x", ""},
		{"https://localhost:8443/", ""},
		{"http://127.0.0.1/", ""},
		{"http://127.0.0.2/", ""},
		{"http://[::1]:80/", ""},
		{"http://psama:8090/", ""},
		{"http://PSAMA:8090/", ""},
		{"https://example.com/", ""},
		{"https://www.example.com/", ""},
		{"https://notexample.com/", "http://sproxy:3129"},
		{"https://sub.example/", "http://sproxy:3129"},
		{"https://a.sub.example/", ""},
		{"https://registry.example:5000/", ""},
		{"https://registry.example/", "http://sproxy:3129"},
		{"http://10.2.3.4/", ""},
		{"http://11.2.3.4/", "http://user:pw@hproxy:3128"},
		{"http://192.168.1.1/", ""},
		{"http://192.168.1.2/", "http://user:pw@hproxy:3128"},
		{"https://[2001:db8::5]:8443/", ""},
		{"https://[2001:db8::5]/", "http://sproxy:3129"},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			u, err := p.ProxyURL(httptest.NewRequest("GET", tt.url, nil))
			if err != nil {
				t.Fatal(err)
			}
			got := ""
			if u != nil {
				got = u.String()
			}
			if got != tt.want {
				t.Errorf("ProxyURL(%s) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}

	star := mustNew(t, netproxy.Config{HTTP: "http://proxy:3128", NoProxy: "*"})
	if u, _ := star.ProxyURL(httptest.NewRequest("GET", "http://github.com/", nil)); u != nil {
		t.Errorf("no_proxy * still proxies: %v", u)
	}
}

// The transport sends a request through the proxy with its credentials.
func TestTransportUsesTheProxy(t *testing.T) {
	type seen struct{ uri, auth string }
	got := make(chan seen, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- seen{r.RequestURI, r.Header.Get("Proxy-Authorization")}
		_, _ = io.WriteString(w, "via proxy")
	}))
	defer proxy.Close()

	pu, _ := url.Parse(proxy.URL)
	pu.User = url.UserPassword("user", "pw")
	p := mustNew(t, netproxy.Config{HTTP: pu.String()})
	client := &http.Client{Transport: &http.Transport{Proxy: p.ProxyURL}}
	resp, err := client.Get("http://release.example.invalid/build-spec.json")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "via proxy" {
		t.Errorf("body = %q", body)
	}
	s := <-got
	if s.uri != "http://release.example.invalid/build-spec.json" || s.auth != "Basic dXNlcjpwdw==" {
		t.Errorf("the proxy saw %+v, want the absolute URL and basic auth for user:pw", s)
	}
}

func TestLogsRedactThePassword(t *testing.T) {
	p := mustNew(t, netproxy.Config{
		HTTP:  "http://user:hunter2@proxy:3128",
		HTTPS: "http://user:hunter2@[2001:db8::1]",
	}, "psama")
	want := "http=http://user:xxxxx@proxy:3128 https=http://user:xxxxx@[2001:db8::1]:80 no_proxy=psama,localhost,127.0.0.1"
	if got := p.String(); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}

	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("resolved", "proxy", p)
	slog.New(slog.NewTextHandler(&buf, nil)).Info("resolved", "proxy", p)
	if strings.Contains(buf.String(), "hunter2") || !strings.Contains(buf.String(), "user:xxxxx@proxy:3128") {
		t.Errorf("logged %s", buf.String())
	}
}
