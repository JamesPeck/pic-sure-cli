package stack

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// validConfig is DefaultConfig plus the fields that have no default.
func validConfig() Config {
	c := DefaultConfig()
	c.Name = "demo"
	c.Auth.Auth0.ClientID = "client-id"
	c.Auth.AdminEmail = "admin@example.org"
	return c
}

func TestDefaultConfigNeedsOnlyTheFieldsWithoutDefaults(t *testing.T) {
	c := DefaultConfig()
	got := problemPaths(t, c.Validate())
	want := []string{"auth.admin_email", "auth.auth0.client_id", "name"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("problems at %q, want %q", got, want)
	}

	c = validConfig()
	if err := c.Validate(); err != nil {
		t.Errorf("validConfig: %v", err)
	}
}

func TestNewConfigDocRoundTrip(t *testing.T) {
	c := validConfig()
	c.Network.HTTPPort = 8080
	c.Auth.Mode = AuthExplore
	c.Dev.Services = []string{"hpds", "psama"}
	c.Services = map[string]ServiceOverride{"hpds": {JavaOpts: "-Xmx2g", Env: map[string]string{"HPDS_CACHE_SIZE": "100"}}}
	c.Frontend.Analytics.GoogleAnalyticsID = "G-123"
	c.Components.PicSure.Ref = "v3.1.0"

	doc, err := NewConfigDoc(&c)
	if err != nil {
		t.Fatal(err)
	}
	data, err := doc.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# pic-sure stack config.", "mode: explore # required | open | explore\n", "services: [hpds, psama]\n"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("new file lacks %q:\n%s", want, data)
		}
	}

	got, err := ParseConfig(data)
	if err != nil {
		t.Fatalf("parse back: %v\n%s", err, data)
	}
	if !reflect.DeepEqual(*got, c) {
		t.Errorf("round trip:\n got  %+v\n want %+v", *got, c)
	}

	doc2, err := ParseConfigDoc(data)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := doc2.Bytes(); string(again) != string(data) {
		t.Errorf("re-encoding changed the file:\n%s\nwas\n%s", again, data)
	}
}

func TestParseConfigFillsDefaults(t *testing.T) {
	got, err := ParseConfig([]byte(`schema: 1
name: demo
network:
  http_port: 8080
  hostname: ~        # null keeps the default
auth:
  auth0: { client_id: abc }
  admin_email: admin@example.org
services:
  hpds: { java_opts: -Xmx2g }
`))
	if err != nil {
		t.Fatal(err)
	}
	want := validConfig()
	want.Network.HTTPPort = 8080
	want.Auth.Auth0.ClientID = "abc"
	want.Services = map[string]ServiceOverride{"hpds": {JavaOpts: "-Xmx2g"}}
	if !reflect.DeepEqual(*got, want) {
		t.Errorf("got  %+v\nwant %+v", *got, want)
	}
}

func TestParseConfigIsStrict(t *testing.T) {
	const head = "schema: 1\nname: demo\nauth: {admin_email: a@example.org, auth0: {client_id: x}}\n"
	tests := []struct {
		name, yaml string
		want       []string // substrings of the error
	}{
		{"unknown top-level key", head + "colour: blue\n", []string{"colour (line 4): unknown key"}},
		{"unknown nested key", head + "network:\n  htp_port: 80\n", []string{"network.htp_port (line 5): unknown key"}},
		{"unknown key in a map entry", head + "services:\n  hpds:\n    heap: 2g\n", []string{"services.hpds.heap (line 6): unknown key"}},
		{"unknown key next to an inline field", head + "components:\n  pic-sure: {project: x}\n", []string{"components.pic-sure.project (line 5): unknown key"}},
		{"int as text", head + "network:\n  http_port: eighty\n", []string{`network.http_port (line 5): want a whole number, got "eighty"`}},
		{"quoted int", head + "network:\n  http_port: \"80\"\n", []string{`network.http_port (line 5): want a whole number, got "80"`}},
		{"bad bool", head + "frontend:\n  docs_enabled: maybe\n", []string{`frontend.docs_enabled (line 5): want true or false, got "maybe"`}},
		{"scalar for a section", head + "tls: generated\n", []string{`tls (line 4): want a mapping of keys, got "generated"`}},
		{"list for a string", head + "name2: x\nhpds:\n  profile: [a]\n", []string{"hpds.profile (line 6): want a string, got a list"}},
		{"scalar for a list", head + "dev:\n  services: hpds\n", []string{`dev.services (line 5): want a list of strings, got "hpds"`}},
		{"duplicate key", head + "network:\n  http_port: 80\n  http_port: 81\n", []string{"network.http_port (line 6): duplicate key (first on line 5)"}},
		{"several problems", head + "a: 1\nb: 2\n", []string{"a (line 4)", "b (line 5)"}},
		{"syntax error", "schema: 1\nname: [\n", []string{"invalid pic-sure.yaml: line 2: did not find expected node content"}},
		{"two documents", head + "---\nname: other\n", []string{"line 4: only one YAML document is allowed"}},
		{"top level not a mapping", "- a\n- b\n", []string{"line 1: want a mapping of keys at the top level"}},
		{"complex key", head + "? [a]\n: b\n", []string{"keys must be plain names"}},
		{"missing schema", "name: demo\n", []string{"schema: required; this pic-sure writes schema 1"}},
		{"schema not a number", "schema: one\n", []string{`schema (line 1): want a whole number, got "one"`}},
		{"empty file", "", []string{"schema: required"}},
		{"only comments", "# nothing yet\n", []string{"schema: required"}},
		{"invalid value", head + "network:\n  http_port: 0\n", []string{"network.http_port (line 5): must be a port from 1 to 65535, got 0"}},
		{"float for an int", head + "network:\n  http_port: 8080.5\n", []string{`network.http_port (line 5): want a whole number, got "8080.5"`}},
		{"float schema", "schema: 1.5\n", []string{`schema (line 1): want a whole number, got "1.5"`}},
		{"on for a bool", head + "frontend:\n  docs_enabled: on\n", []string{`frontend.docs_enabled (line 5): want true or false, got "on"`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(tt.yaml))
			var ce *ConfigError
			if !errors.As(err, &ce) {
				t.Fatalf("err = %v, want a *ConfigError", err)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error lacks %q:\n%v", w, err)
				}
			}
		})
	}
}

func TestParseConfigOtherSchema(t *testing.T) {
	for _, n := range []int{0, 2} {
		_, err := ParseConfig([]byte("schema: " + string(rune('0'+n)) + "\nwhatever: the new schema has\n"))
		var se *SchemaVersionError
		if !errors.As(err, &se) || se.Found != n {
			t.Errorf("schema %d: err = %v, want a *SchemaVersionError", n, err)
		}
	}
}

func TestParseConfigFollowsAliases(t *testing.T) {
	got, err := ParseConfig([]byte(`schema: 1
name: demo
auth: {admin_email: a@example.org, auth0: {client_id: x}}
services:
  hpds: &jvm {java_opts: -Xmx2g}
  psama: *jvm
`))
	if err != nil {
		t.Fatal(err)
	}
	if got.Services["psama"].JavaOpts != "-Xmx2g" {
		t.Errorf("services = %+v", got.Services)
	}
}

func TestSetKeepsCommentsAndOtherKeys(t *testing.T) {
	doc, err := ParseConfigDoc([]byte(`# My stack.

schema: 1
name: demo # project name
network:
  # The port moved off 80.
  http_port: 8080 # was 80
  https_port: 8443
auth:
  auth0: { client_id: abc }
  admin_email: a@example.org
dev:
  services: [hpds] # dev things
services: {}
# The end.
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range [][2]string{
		{"network.http_port", "9090"},
		{"dev.services", "hpds, psama"},
		{"auth.auth0.tenant", "mytenant"},
		{"services.hpds.env.HPDS_CACHE_SIZE", "100"},
		{"proxy.http", "http://proxy.example.org:3128"},
	} {
		if err := doc.Set(kv[0], kv[1]); err != nil {
			t.Fatalf("Set(%q, %q): %v", kv[0], kv[1], err)
		}
	}
	got, err := doc.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	want := `# My stack.

schema: 1
name: demo # project name
network:
  # The port moved off 80.
  http_port: 9090 # was 80
  https_port: 8443
auth:
  auth0: {client_id: abc, tenant: mytenant}
  admin_email: a@example.org
dev:
  services: [hpds, psama] # dev things
services:
  hpds:
    env:
      HPDS_CACHE_SIZE: "100"
proxy:
  http: http://proxy.example.org:3128
# The end.
`
	if string(got) != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if _, err := doc.Config(); err != nil {
		t.Errorf("Config: %v", err)
	}
}

func TestSetReplacesNullSections(t *testing.T) {
	doc, err := ParseConfigDoc([]byte("schema: 1\nname: demo\nauth: {admin_email: a@example.org, auth0: {client_id: x}}\ntls: ~\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Set("tls.chain_file", "certs/chain.pem"); err != nil {
		t.Fatal(err)
	}
	c, err := doc.Config()
	if err != nil {
		t.Fatal(err)
	}
	if c.TLS.ChainFile != "certs/chain.pem" || c.TLS.Mode != TLSGenerated {
		t.Errorf("tls = %+v", c.TLS)
	}
}

// Aliases are expanded on parse, so a set changes only its own key and the
// saved file never refers to an anchor it no longer has.
func TestSetWithAnchors(t *testing.T) {
	doc, err := ParseConfigDoc([]byte(`schema: 1
name: demo
auth: {admin_email: a@example.org, auth0: {client_id: x}}
hpds:
  java_opts: &opts -Xmx4g
services:
  hpds: &svc {java_opts: *opts}
  psama: *svc
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range [][2]string{{"hpds.java_opts", "-Xmx8g"}, {"services.hpds.java_opts", "-Xmx2g"}} {
		if err := doc.Set(kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	data, err := doc.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	c, err := ParseConfig(data)
	if err != nil {
		t.Fatalf("saved file doesn't parse: %v\n%s", err, data)
	}
	if c.HPDS.JavaOpts != "-Xmx8g" || c.Services["hpds"].JavaOpts != "-Xmx2g" || c.Services["psama"].JavaOpts != "-Xmx4g" {
		t.Errorf("hpds %q, services %+v\n%s", c.HPDS.JavaOpts, c.Services, data)
	}
}

func TestReadOnlyChanges(t *testing.T) {
	parse := func(yaml string) *ConfigDoc {
		t.Helper()
		d, err := ParseConfigDoc([]byte(yaml))
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	// before is invalid (port 0), which doesn't stop the check.
	before := parse("schema: 1\nname: demo\nnetwork: {http_port: 0}\n")
	for yaml, want := range map[string]string{
		"schema: 1\nname: demo\nnetwork: {http_port: 8080}\n": "",
		"schema: 1\nname: \"demo\"\n":                         "",
		"schema: 1\nname: other\n":                            "invalid pic-sure.yaml: name: is read-only; it was demo",
		"schema: 1\n":                                         "invalid pic-sure.yaml: name: is read-only; it was demo",
		"schema: 2\nname: demo\n":                             "invalid pic-sure.yaml: schema: is read-only; it was 1",
	} {
		err := parse(yaml).ReadOnlyChanges(before)
		if got := fmt.Sprint(err); (want == "" && err != nil) || (want != "" && got != want) {
			t.Errorf("%q: %v, want %q", yaml, err, want)
		}
	}
	// A key the old file lacked may be added.
	if err := parse("schema: 1\nname: demo\n").ReadOnlyChanges(parse("schema: 1\n")); err != nil {
		t.Errorf("adding name: %v", err)
	}
}

func TestSetParsesValuesByKind(t *testing.T) {
	tests := []struct {
		key, value string
		want       any
	}{
		{"network.http_port", " 8080 ", 8080},
		{"auth.tos", "TRUE", true},
		{"auth.tos", "false", false},
		{"dev.services", "hpds,psama", []string{"hpds", "psama"}},
		{"dev.services", "[hpds, httpd-hmr]", []string{"hpds", "httpd-hmr"}},
		{"dev.services", "", []string{}},
		{"auth.admin_email", "8080", "8080"}, // strings are taken literally
		{"hpds.profile", "null", "null"},
	}
	for _, tt := range tests {
		doc, _ := ParseConfigDoc([]byte("schema: 1\n"))
		if err := doc.Set(tt.key, tt.value); err != nil {
			t.Errorf("Set(%q, %q): %v", tt.key, tt.value, err)
			continue
		}
		c := DefaultConfig()
		dec := decoder{lines: map[string]int{}}
		dec.decode(doc.root.Content[0], reflect.ValueOf(&c).Elem(), "")
		got, _ := c.Get(tt.key)
		if len(dec.problems) > 0 || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("Set(%q, %q): got %#v (%v), want %#v", tt.key, tt.value, got, dec.problems, tt.want)
		}
	}
}

func TestSetRejects(t *testing.T) {
	tests := []struct {
		key, value string
		want       string
		keyErr     bool
	}{
		{"network.http_prot", "80", "network.http_prot: unknown config key", true},
		{"network", "x", "network: unknown config key", true},
		{"services.hpds", "x", "services.hpds: unknown config key", true},
		{"auth.auth0.client_secret", "s3cret", "is a secret", true},
		{"email.password", "s3cret", "is a secret", true},
		{"schema", "2", "schema: is read-only", true},
		{"name", "other", "name: is read-only", true},
		{"network.http_port", "eighty", `network.http_port: want a whole number, got "eighty"`, false},
		{"network.http_port", "80.5", "want a whole number", false},
		{"auth.tos", "yes", `auth.tos: want true or false, got "yes"`, false},
		{"dev.services", "[a, [b]]", "want a list", false},
	}
	for _, tt := range tests {
		doc, _ := ParseConfigDoc([]byte("schema: 1\n"))
		err := doc.Set(tt.key, tt.value)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("Set(%q, %q) = %v, want %q", tt.key, tt.value, err, tt.want)
			continue
		}
		var ke *KeyError
		if errors.As(err, &ke) != tt.keyErr {
			t.Errorf("Set(%q, %q) = %T, KeyError wanted: %v", tt.key, tt.value, err, tt.keyErr)
		}
	}
}

func TestSetValueChecksTheType(t *testing.T) {
	doc, _ := ParseConfigDoc([]byte("schema: 1\n"))
	if err := doc.SetValue("dev.services", []string{"hpds"}); err != nil {
		t.Errorf("list: %v", err)
	}
	if err := doc.SetValue("network.http_port", "80"); err == nil {
		t.Error("a string for an int field: no error")
	}
	if err := doc.SetValue("auth.tos", 1); err == nil {
		t.Error("an int for a bool field: no error")
	}
}

func TestGet(t *testing.T) {
	c := validConfig()
	c.Services = map[string]ServiceOverride{"hpds": {JavaOpts: "-Xmx2g"}}
	tests := []struct {
		key  string
		want any
	}{
		{"name", "demo"},
		{"network.http_port", 80},
		{"auth.mode", AuthRequired},
		{"frontend.docs_enabled", true},
		{"dev.services", []string{}},
		{"network.dev_ports", DevPorts{Base: 15000}},
		{"components.migrations.ref", ""},
		{"components.migrations.project", "Baseline"},
		{"services.hpds.java_opts", "-Xmx2g"},
		{"services.psama.java_opts", ""},
		{"services.psama.env.X", ""},
	}
	for _, tt := range tests {
		got, err := c.Get(tt.key)
		if err != nil || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("Get(%q) = %#v, %v; want %#v", tt.key, got, err, tt.want)
		}
	}
	for _, key := range []string{"", "nope", "network.http_port.x", "auth.auth0.client_secret", "components.pic-sure.project"} {
		var ke *KeyError
		if _, err := c.Get(key); !errors.As(err, &ke) {
			t.Errorf("Get(%q) err = %v, want a *KeyError", key, err)
		}
	}
}

func TestDeriveAuthFlags(t *testing.T) {
	tests := []struct {
		mode AuthMode
		want string
	}{
		{AuthRequired, "OPEN_IDP_PROVIDER_IS_ENABLED=false GATEWAY_OPEN_ACCESS_ENABLED=false ENABLE_PUBLIC_ACCESS=false VITE_OPEN=false VITE_OPEN_EXPLORER=false VITE_DISCOVER=false"},
		{AuthOpen, "OPEN_IDP_PROVIDER_IS_ENABLED=true GATEWAY_OPEN_ACCESS_ENABLED=true ENABLE_PUBLIC_ACCESS=false VITE_OPEN=true VITE_OPEN_EXPLORER=false VITE_DISCOVER=true"},
		{AuthExplore, "OPEN_IDP_PROVIDER_IS_ENABLED=true GATEWAY_OPEN_ACCESS_ENABLED=true ENABLE_PUBLIC_ACCESS=true VITE_OPEN=true VITE_OPEN_EXPLORER=true VITE_DISCOVER=false"},
		{"bogus", "OPEN_IDP_PROVIDER_IS_ENABLED=false GATEWAY_OPEN_ACCESS_ENABLED=false ENABLE_PUBLIC_ACCESS=false VITE_OPEN=false VITE_OPEN_EXPLORER=false VITE_DISCOVER=false"},
	}
	for _, tt := range tests {
		if got := strings.Join(DeriveAuthFlags(tt.mode).Env(), " "); got != tt.want {
			t.Errorf("%s:\n got  %s\n want %s", tt.mode, got, tt.want)
		}
	}
}

// problemPaths returns the paths of err's problems, failing unless err is a
// *ConfigError.
func problemPaths(t *testing.T, err error) []string {
	t.Helper()
	var ce *ConfigError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want a *ConfigError", err)
	}
	var paths []string
	for _, p := range ce.Problems {
		paths = append(paths, p.Path)
	}
	return paths
}
