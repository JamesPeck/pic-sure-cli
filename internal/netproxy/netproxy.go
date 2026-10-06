package netproxy

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
)

// Config is the proxy block of pic-sure.yaml, with the fields of
// stack.Proxy.
type Config struct {
	HTTP    string // proxy URL for http:// destinations; empty for none
	HTTPS   string // proxy URL for https:// destinations; empty for none
	NoProxy string // the user's comma-separated no-proxy entries
}

// Proxy is a resolved proxy configuration. Each proxy applies only to its
// own scheme: with just HTTP set, https:// traffic goes direct. Every
// output is empty when neither proxy is set, as it is for a zero Proxy.
type Proxy struct {
	http, https *url.URL // as ParseURL returns them
	noProxy     []entry  // the effective list; nil without a proxy
}

// New resolves c for a stack whose compose services are named services.
// The effective no-proxy list is c's entries, then the service names
// (sorted), localhost and 127.0.0.1, without duplicates. Callers without a
// rendered stack pass CatalogServices().
func New(c Config, services []string) (*Proxy, error) {
	httpURL, err := ParseURL(c.HTTP)
	if err != nil {
		return nil, fmt.Errorf("proxy.http: %w", err)
	}
	httpsURL, err := ParseURL(c.HTTPS)
	if err != nil {
		return nil, fmt.Errorf("proxy.https: %w", err)
	}
	entries, err := parseList(c.NoProxy)
	if err != nil {
		return nil, fmt.Errorf("proxy.no_proxy: %w", err)
	}
	p := &Proxy{http: httpURL, https: httpsURL}
	if !p.Enabled() {
		return p, nil
	}
	for _, s := range append(slices.Sorted(slices.Values(services)), "localhost", "127.0.0.1") {
		e, err := parseEntry(s)
		if err != nil {
			return nil, fmt.Errorf("service name: %w", err)
		}
		entries = append(entries, e)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if !seen[e.text] {
			seen[e.text] = true
			p.noProxy = append(p.noProxy, e)
		}
	}
	return p, nil
}

// CatalogServices returns the name of every service in the catalog.
func CatalogServices() []string {
	var names []string
	for _, s := range catalog.Services() {
		names = append(names, s.Name)
	}
	return names
}

// ParseURL parses a proxy URL from the config: http:// or https://, a host,
// an optional port and optional user info, with nothing after the host but
// "/". The result has only those parts, and the port filled in (80 or 443
// by scheme when missing). An empty s means no proxy: nil and no error.
// Errors show the URL with its password redacted, and are phrased to
// follow the config key.
func ParseURL(s string) (*url.URL, error) {
	if s == "" {
		return nil, nil
	}
	u, err := url.Parse(s)
	switch {
	case err != nil:
		// url's error quotes the whole URL, password and all.
		return nil, errors.New("is not a valid URL")
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, fmt.Errorf("want an http:// or https:// URL, got %q", u.Redacted())
	case u.Hostname() == "":
		return nil, fmt.Errorf("has no host: %q", u.Redacted())
	case (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "":
		return nil, fmt.Errorf("want only a scheme, host and port, got %q", u.Redacted())
	}
	port := u.Port()
	if port == "" {
		port = defaultPort(u.Scheme)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return nil, fmt.Errorf("has a port outside 1-65535: %q", u.Redacted())
	}
	return &url.URL{Scheme: u.Scheme, User: u.User, Host: net.JoinHostPort(u.Hostname(), strconv.Itoa(n))}, nil
}

func defaultPort(scheme string) string {
	if scheme == "https" {
		return "443"
	}
	return "80"
}

// Enabled reports whether either proxy is set.
func (p *Proxy) Enabled() bool {
	return p.http != nil || p.https != nil
}

// NoProxy returns the effective no-proxy list in its NO_PROXY form.
func (p *Proxy) NoProxy() []string {
	var out []string
	for _, e := range p.noProxy {
		out = append(out, e.text)
	}
	return out
}

// Env returns HTTP_PROXY, HTTPS_PROXY and NO_PROXY, each in upper and lower
// case, as NAME=value entries for git, node and runtime containers. A proxy
// that isn't set has no entries. The proxy URLs keep their user and
// password, so pass the entries as secrets are passed (Cmd.Env, or ${VAR}
// in the rendered compose file), never in argv or a file.
func (p *Proxy) Env() []string {
	if !p.Enabled() {
		return nil
	}
	var env []string
	add := func(name, value string) {
		env = append(env, name+"="+value, strings.ToLower(name)+"="+value)
	}
	if p.http != nil {
		add("HTTP_PROXY", p.http.String())
	}
	if p.https != nil {
		add("HTTPS_PROXY", p.https.String())
	}
	add("NO_PROXY", strings.Join(p.NoProxy(), ","))
	return env
}

// BuildArgs returns docker build's predefined proxy args, which have the
// same names and values as Env, as NAME=value entries for
// docker.BuildOpts.BuildArgs. BuildKit doesn't pass the proxy to a build
// unless they are set, and docker leaves them out of the image history.
func (p *Proxy) BuildArgs() []string {
	return p.Env()
}

// JVMOpts returns the system properties that send a JVM's outbound HTTP and
// HTTPS through the proxy, for JAVA_OPTS: -Dhttp.proxyHost and
// -Dhttp.proxyPort, the https pair, and -Dhttp.nonProxyHosts, which the JVM
// applies to both. No option contains white space. They carry no
// credentials, because the JVM has no system property for them.
func (p *Proxy) JVMOpts() []string {
	if !p.Enabled() {
		return nil
	}
	var opts []string
	if p.http != nil {
		opts = append(opts, "-Dhttp.proxyHost="+p.http.Hostname(), "-Dhttp.proxyPort="+p.http.Port())
	}
	if p.https != nil {
		opts = append(opts, "-Dhttps.proxyHost="+p.https.Hostname(), "-Dhttps.proxyPort="+p.https.Port())
	}
	return append(opts, "-Dhttp.nonProxyHosts="+p.javaNonProxyHosts())
}

// ProxyURL is an http.Transport.Proxy for the CLI's own HTTP. It returns
// the proxy for req's scheme, or nil for another scheme, localhost, a
// loopback address or a host on the no-proxy list, as Go's
// http.ProxyFromEnvironment does.
func (p *Proxy) ProxyURL(req *http.Request) (*url.URL, error) {
	var proxy *url.URL
	switch req.URL.Scheme {
	case "http":
		proxy = p.http
	case "https":
		proxy = p.https
	}
	if proxy == nil || p.bypass(req.URL) {
		return nil, nil
	}
	u := *proxy
	return &u, nil
}

// String describes p for logs, with the proxy passwords redacted.
func (p *Proxy) String() string {
	if !p.Enabled() {
		return "no proxy"
	}
	var parts []string
	if p.http != nil {
		parts = append(parts, "http="+p.http.Redacted())
	}
	if p.https != nil {
		parts = append(parts, "https="+p.https.Redacted())
	}
	return strings.Join(append(parts, "no_proxy="+strings.Join(p.NoProxy(), ",")), " ")
}

// LogValue logs p as String describes it, so a logged Proxy never shows a
// password.
func (p *Proxy) LogValue() slog.Value {
	return slog.StringValue(p.String())
}
