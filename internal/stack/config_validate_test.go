package stack

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Config)
		want   []string // "path: message" substrings, one per expected problem; nil means valid
	}{
		{"valid", func(*Config) {}, nil},

		{"name required", func(c *Config) { c.Name = "" }, []string{"name: required"}},
		{"name uppercase", func(c *Config) { c.Name = "Demo" }, []string{`name: must be lowercase letters, digits, - and _, starting with a letter or digit; got "Demo"`}},
		{"name leading dash", func(c *Config) { c.Name = "-demo" }, []string{"name: must be lowercase"}},
		{"name with dot", func(c *Config) { c.Name = "my.stack" }, []string{"name: must be lowercase"}},
		{"name with digits, _ and -", func(c *Config) { c.Name = "0ws-v2_006" }, nil},
		{"schema", func(c *Config) { c.Schema = 2 }, []string{"schema: must be 1, got 2"}},

		{"hostname required", func(c *Config) { c.Network.Hostname = "" }, []string{"network.hostname: required"}},
		{"hostname IP", func(c *Config) { c.Network.Hostname = "10.0.0.5" }, nil},
		{"hostname IPv6", func(c *Config) { c.Network.Hostname = "::1" }, nil},
		{"hostname FQDN", func(c *Config) { c.Network.Hostname = "picsure.example.org" }, nil},
		{"hostname with a port", func(c *Config) { c.Network.Hostname = "localhost:8443" }, []string{`network.hostname: want a host name or IP address, got "localhost:8443"`}},
		{"hostname with underscore", func(c *Config) { c.Network.Hostname = "my_host" }, []string{"network.hostname: want a host name"}},
		{"hostname empty label", func(c *Config) { c.Network.Hostname = "a..b" }, []string{"network.hostname: want a host name"}},
		{"http port 0", func(c *Config) { c.Network.HTTPPort = 0 }, []string{"network.http_port: must be a port from 1 to 65535, got 0"}},
		{"https port too big", func(c *Config) { c.Network.HTTPSPort = 65536 }, []string{"network.https_port: must be a port from 1 to 65535, got 65536"}},
		{"ports at the limits", func(c *Config) { c.Network.HTTPPort, c.Network.HTTPSPort = 1, 65535 }, nil},
		{"same ports", func(c *Config) { c.Network.HTTPPort, c.Network.HTTPSPort = 8080, 8080 }, []string{"network.https_port: must differ from network.http_port (both are 8080)"}},
		{"dev base 0", func(c *Config) { c.Network.DevPorts.Base = 0 }, []string{"network.dev_ports.base: must be from 1 to 65529, so the 7 dev ports fit; got 0"}},
		{"dev block past 65535", func(c *Config) { c.Network.DevPorts.Base = 65530 }, []string{"network.dev_ports.base: must be from 1 to 65529"}},
		{"dev block at the top", func(c *Config) { c.Network.DevPorts.Base = 65529 }, nil},
		{"dev block holds https", func(c *Config) { c.Network.HTTPSPort = 15006 }, []string{"network.dev_ports.base: the dev ports 15000-15006 include network.https_port (15006)"}},

		{"tls mode", func(c *Config) { c.TLS.Mode = "selfsigned" }, []string{`tls.mode: must be one of generated, provided; got "selfsigned"`}},
		{"auth mode", func(c *Config) { c.Auth.Mode = "" }, []string{`auth.mode: must be one of required, open, explore; got ""`}},
		{"db mode", func(c *Config) { c.DB.Mode = "Remote" }, []string{"db.mode: must be one of local, remote"}},
		{"hpds data", func(c *Config) { c.HPDS.Data = "both" }, []string{"hpds.data: must be one of local, shared"}},
		{"theme", func(c *Config) { c.Frontend.Theme = "dark" }, []string{"frontend.theme: must be one of picsure, bdc, aim-ahead, local"}},
		{"theme bdc", func(c *Config) { c.Frontend.Theme = "bdc" }, nil},
		{"cli compat", func(c *Config) { c.Release.CLICompat = "lenient" }, []string{"release.cli_compat: must be one of warn, strict"}},
		{"images mode", func(c *Config) { c.Images.Mode = "pull" }, nil},
		{"images mode bad", func(c *Config) { c.Images.Mode = "fetch" }, []string{"images.mode: must be one of build, pull"}},

		{"auth0 fields required", func(c *Config) { c.Auth.Auth0 = Auth0{} }, []string{
			"auth.auth0.client_id: required when auth.mode is not open",
			"auth.auth0.tenant: required when auth.mode is not open",
		}},
		{"auth0 fields optional when open", func(c *Config) { c.Auth.Mode, c.Auth.Auth0 = AuthOpen, Auth0{} }, nil},
		{"auth0 fields required for explore", func(c *Config) { c.Auth.Mode, c.Auth.Auth0.ClientID = AuthExplore, "" }, []string{"auth.auth0.client_id: required when auth.mode is not open"}},
		{"tenant with region", func(c *Config) { c.Auth.Auth0.Tenant = "avillachlab.us" }, nil},
		{"tenant as URL", func(c *Config) { c.Auth.Auth0.Tenant = "https://avillachlab.auth0.com" }, []string{"auth.auth0.tenant: want a host name"}},
		{"admin email required", func(c *Config) { c.Auth.AdminEmail = "" }, []string{"auth.admin_email: required"}},
		{"admin email required when open", func(c *Config) { c.Auth.Mode, c.Auth.AdminEmail = AuthOpen, "" }, []string{"auth.admin_email: required"}},
		{"admin email invalid", func(c *Config) { c.Auth.AdminEmail = "admin" }, []string{`auth.admin_email: want an email address like admin@example.org, got "admin"`}},
		{"admin email with a display name", func(c *Config) { c.Auth.AdminEmail = "Admin <admin@example.org>" }, []string{"auth.admin_email: want an email address"}},

		{"remote db fields required", func(c *Config) { c.DB.Mode, c.DB.Remote = DBRemote, RemoteDB{Port: 3306} }, []string{
			"db.remote.host: required when db.mode is remote",
			"db.remote.root_user: required when db.mode is remote",
		}},
		{"remote db complete", func(c *Config) {
			c.DB.Mode, c.DB.Remote = DBRemote, RemoteDB{Host: "db.example.org", Port: 3306, RootUser: "admin"}
		}, nil},
		{"remote db bad host and port", func(c *Config) {
			c.DB.Mode, c.DB.Remote = DBRemote, RemoteDB{Host: "db example", Port: 70000, RootUser: "admin"}
		}, []string{"db.remote.host: want a host name", "db.remote.port: must be a port from 1 to 65535, got 70000"}},
		{"remote fields ignored when local", func(c *Config) { c.DB.Remote = RemoteDB{Host: "db example", Port: 0} }, nil},

		{"shared name required", func(c *Config) { c.HPDS.Data = HPDSShared }, []string{"hpds.shared_name: required when hpds.data is shared"}},
		{"shared name set", func(c *Config) { c.HPDS.Data, c.HPDS.SharedName = HPDSShared, "nhanes-2026.1" }, nil},
		{"shared name bad", func(c *Config) { c.HPDS.Data, c.HPDS.SharedName = HPDSShared, "../x" }, []string{`hpds.shared_name: must be letters, digits, '.', - and _, starting with a letter or digit; got "../x"`}},
		{"java opts with a newline", func(c *Config) { c.HPDS.JavaOpts = "-Xmx1g\n-Xms1g" }, []string{"hpds.java_opts: must not contain control characters"}},

		{"provided tls needs files", func(c *Config) { c.TLS = TLS{Mode: TLSProvided} }, []string{
			"tls.cert_file: required when tls.mode is provided",
			"tls.key_file: required when tls.mode is provided",
		}},
		{"generated tls ignores files", func(c *Config) { c.TLS = TLS{Mode: TLSGenerated} }, nil},

		{"release repo required", func(c *Config) { c.Release.Repo = "" }, []string{"release.repo: required"}},
		{"release branch option", func(c *Config) { c.Release.Branch = "--upload-pack=x" }, []string{`release.branch: must not start with -, got "--upload-pack=x"`}},
		{"component ref option", func(c *Config) { c.Components.Frontend.Ref = "-x" }, []string{"components.frontend.ref: must not start with -"}},
		{"migrations project required", func(c *Config) { c.Components.Migrations.Project = "" }, []string{"components.migrations.project: required"}},
		{"migrations project path", func(c *Config) { c.Components.Migrations.Project = "a/b" }, []string{`components.migrations.project: must be one directory name, got "a/b"`}},
		{"migrations project dotdot", func(c *Config) { c.Components.Migrations.Project = ".." }, []string{"components.migrations.project: must be one directory name"}},

		{"dev services", func(c *Config) { c.Dev.Services = []string{"hpds", "httpd-hmr"} }, nil},
		{"dev service twice", func(c *Config) { c.Dev.Services = []string{"hpds", "hpds"} }, []string{`dev.services: "hpds" is listed twice`}},
		{"dev service bad", func(c *Config) { c.Dev.Services = []string{""} }, []string{`dev.services: want service names, got ""`}},
		{"service override name", func(c *Config) { c.Services = map[string]ServiceOverride{"HPDS": {}} }, []string{`services.HPDS: "HPDS" is not a service name`}},
		{"service env name", func(c *Config) {
			c.Services = map[string]ServiceOverride{"hpds": {Env: map[string]string{"1X": "a", "OK_2": "b"}}}
		}, []string{`services.hpds.env.1X: "1X" is not an environment variable name`}},
		{"service env value with a newline", func(c *Config) {
			c.Services = map[string]ServiceOverride{"hpds": {Env: map[string]string{"X": "a\nb"}}}
		}, []string{"services.hpds.env.X: must not contain control characters"}},

		{"proxies", func(c *Config) {
			c.Proxy = Proxy{HTTP: "http://proxy.example.org:3128", HTTPS: "https://user:pw@[2001:db8::1]:8443/", NoProxy: ".internal,10.0.0.0/8"}
		}, nil},
		{"proxy without scheme", func(c *Config) { c.Proxy.HTTP = "proxy.example.org:3128" }, []string{"proxy.http: want an http:// or https:// URL"}},
		{"proxy socks", func(c *Config) { c.Proxy.HTTPS = "socks5://proxy:1080" }, []string{`proxy.https: want an http:// or https:// URL, got "socks5://proxy:1080"`}},
		{"proxy without host", func(c *Config) { c.Proxy.HTTP = "http://:3128" }, []string{"proxy.http: has no host"}},
		{"proxy with a path", func(c *Config) { c.Proxy.HTTP = "http://proxy/x" }, []string{"proxy.http: want only a scheme, host and port"}},
		{"proxy bad port", func(c *Config) { c.Proxy.HTTP = "http://user:hunter2@proxy:port" }, []string{"proxy.http: is not a valid URL"}},
		{"proxy password redacted", func(c *Config) { c.Proxy.HTTP = "ftp://user:hunter2@proxy:21" }, []string{`proxy.http: want an http:// or https:// URL, got "ftp://user:xxxxx@proxy:21"`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			tt.change(&c)
			err := c.Validate()
			if tt.want == nil {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			var ce *ConfigError
			if !errors.As(err, &ce) {
				t.Fatalf("Validate = %v, want a *ConfigError", err)
			}
			if len(ce.Problems) != len(tt.want) {
				t.Errorf("got %d problems, want %d:\n%v", len(ce.Problems), len(tt.want), err)
			}
			for i, w := range tt.want {
				if i < len(ce.Problems) && !strings.Contains(ce.Problems[i].String(), w) {
					t.Errorf("problem %d = %q, want it to contain %q", i, ce.Problems[i], w)
				}
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Errorf("the error shows the proxy password: %v", err)
			}
		})
	}
}

func TestConfigErrorFormat(t *testing.T) {
	one := &ConfigError{Problems: []Problem{{Path: "name", Msg: "required"}}}
	if got, want := one.Error(), "invalid pic-sure.yaml: name: required"; got != want {
		t.Errorf("one problem: %q, want %q", got, want)
	}
	many := &ConfigError{Problems: []Problem{
		{Path: "network.http_port", Line: 4, Msg: "bad"},
		{Line: 7, Msg: "syntax"},
		{Msg: "whole file"},
	}}
	if got, want := many.Error(), "invalid pic-sure.yaml:\n  network.http_port (line 4): bad\n  line 7: syntax\n  whole file"; got != want {
		t.Errorf("many problems:\n%s\nwant\n%s", got, want)
	}
}

func TestCheckFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(rel string) string {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("certs/tls/server.crt")
	key := write("elsewhere/server.key")
	src := filepath.Join(dir, "src", "pic-sure")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}

	c := validConfig()
	c.TLS = TLS{Mode: TLSProvided, CertFile: "certs/tls/server.crt", KeyFile: key}
	c.Components.PicSure.Source = "src/pic-sure"
	c.Components.Frontend.Source = src
	if err := c.CheckFiles(dir); err != nil {
		t.Errorf("all present: %v", err)
	}

	c.TLS.ChainFile = "certs/tls/chain.pem"
	c.TLS.CertFile = "certs/tls"
	c.Components.Migrations.Source = key
	c.Components.DictionaryETL.Source = "missing"
	got := problemPaths(t, c.CheckFiles(dir))
	want := []string{"components.dictionary-etl.source", "components.migrations.source", "tls.cert_file", "tls.chain_file"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("problems at %q, want %q (%v)", got, want, c.CheckFiles(dir))
	}

	c = validConfig()
	c.TLS.CertFile = "missing.crt"
	if err := c.CheckFiles(dir); err != nil {
		t.Errorf("generated mode: %v", err)
	}
}
