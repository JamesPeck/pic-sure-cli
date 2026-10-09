package stack

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// FieldKind is the type of a config value.
type FieldKind string

// Field kinds.
const (
	KindString FieldKind = "string"
	KindInt    FieldKind = "int"
	KindBool   FieldKind = "bool"
	KindList   FieldKind = "list" // a list of strings
)

// Condition is when a field is required.
type Condition struct {
	Desc  string // for messages and docs, e.g. "auth.mode is not open"
	Holds func(*Config) bool
}

// Field describes one config key, for `pic-sure config`, validation, the
// setup wizard, init's flags and the generated docs.
type Field struct {
	// Key is the dotted key path. A "*" segment matches any one segment,
	// for keys under a map such as services.
	Key  string
	Kind FieldKind
	// Flag is init's flag for the field, without the dashes, or "" when
	// init has none. A secret's flag reads the value from stdin.
	Flag string
	Help string
	// Secret fields live in .pic-sure/secrets.yaml, not pic-sure.yaml, and
	// `pic-sure config` never reads or writes them.
	Secret bool
	// ReadOnly fields are fixed once the stack exists: `config set` and
	// `config edit` refuse to change them.
	ReadOnly bool
	// Options are an enum's allowed values.
	Options []string
	// RequiredWhen is when the field must be non-empty; the zero value
	// means it's optional.
	RequiredWhen Condition
}

// Required reports whether f must have a value in c.
func (f Field) Required(c *Config) bool {
	return f.RequiredWhen.Holds != nil && f.RequiredWhen.Holds(c)
}

var (
	always = Condition{Desc: "always", Holds: func(*Config) bool { return true }}

	authNotOpen = Condition{
		Desc:  "auth.mode is not open",
		Holds: func(c *Config) bool { return c.Auth.Mode != AuthOpen },
	}
	dbRemote = Condition{
		Desc:  "db.mode is remote",
		Holds: func(c *Config) bool { return c.DB.Mode == DBRemote },
	}
	hpdsShared = Condition{
		Desc:  "hpds.data is shared",
		Holds: func(c *Config) bool { return c.HPDS.Data == HPDSShared },
	}
	tlsProvided = Condition{
		Desc:  "tls.mode is provided",
		Holds: func(c *Config) bool { return c.TLS.Mode == TLSProvided },
	}
)

// Fields is every config key, plus the secrets that setup asks for, in
// setup order.
var Fields = []Field{
	{Key: "schema", Kind: KindInt, ReadOnly: true,
		Help: "Config schema version. pic-sure update migrates it."},
	{Key: "name", Kind: KindString, Flag: "name", ReadOnly: true, RequiredWhen: always,
		Help: "Stack name, used as the compose project name and in container and volume names: lowercase letters, digits, - and _, starting with a letter or digit. Fixed once the stack exists."},

	{Key: "auth.mode", Kind: KindString, Flag: "auth-mode", Options: enumValues(AuthRequired, AuthOpen, AuthExplore),
		Help: "required: no access without login. open: the Discover page without login, with no export or API. explore: the query builder without login; export asks for login. open and explore make the data queryable without login."},
	{Key: "auth.auth0.tenant", Kind: KindString, Flag: "auth0-tenant", RequiredWhen: authNotOpen,
		Help: "Auth0 tenant. Keep the default unless you run your own tenant."},
	{Key: "auth.auth0.client_id", Kind: KindString, Flag: "auth0-client-id", RequiredWhen: authNotOpen,
		Help: "Auth0 client ID. Evaluation credentials are at avillachlabsupport.hms.harvard.edu."},
	{Key: "auth.auth0.client_secret", Kind: KindString, Flag: "auth0-client-secret-stdin", Secret: true, RequiredWhen: authNotOpen,
		Help: "Auth0 client secret, paired with the client ID. Read from stdin."},
	{Key: "auth.admin_email", Kind: KindString, Flag: "admin-email", RequiredWhen: always,
		Help: "Email of the first admin user; with Auth0 it must be a Google account."},
	{Key: "auth.tos", Kind: KindBool,
		Help: "Show a terms-of-service dialog on first login."},
	{Key: "auth.consent_authorization", Kind: KindBool,
		Help: "Consent-based authorization. pic-sure seeds no consents, so turn it on only with a consent workflow."},

	{Key: "network.hostname", Kind: KindString, RequiredWhen: always,
		Help: "Host name users reach the stack at. It goes into the generated certificate and the printed URLs."},
	{Key: "network.http_port", Kind: KindInt, Flag: "http-port",
		Help: "Host port for HTTP, which redirects to HTTPS."},
	{Key: "network.https_port", Kind: KindInt, Flag: "https-port",
		Help: "Host port for HTTPS. Must differ from the HTTP port."},
	{Key: "network.dev_ports.base", Kind: KindInt,
		Help: "First of the 7 host ports dev overlays use: debug ports base to base+5, HMR on base+6. Chosen free at init."},

	{Key: "db.mode", Kind: KindString, Flag: "db-mode", Options: enumValues(DBLocal, DBRemote),
		Help: "local: the bundled MySQL container. remote: an external MySQL such as RDS."},
	{Key: "db.remote.host", Kind: KindString, Flag: "db-host", RequiredWhen: dbRemote,
		Help: "Host name or IP of the external MySQL."},
	{Key: "db.remote.port", Kind: KindInt, Flag: "db-port",
		Help: "Port of the external MySQL."},
	{Key: "db.remote.root_user", Kind: KindString, Flag: "db-root-user", RequiredWhen: dbRemote,
		Help: "Admin user on the external MySQL, used to create the PIC-SURE databases and users."},
	{Key: "db.remote.root_password", Kind: KindString, Flag: "db-root-password-stdin", Secret: true, RequiredWhen: dbRemote,
		Help: "Password of the external MySQL admin user. Read from stdin."},

	{Key: "hpds.data", Kind: KindString, Flag: "hpds-data", Options: enumValues(HPDSLocal, HPDSShared),
		Help: "local: this stack's own data. shared: a published shared data set, mounted read-only. init's --hpds-data takes local or shared:NAME."},
	{Key: "hpds.shared_name", Kind: KindString, RequiredWhen: hpdsShared,
		Help: "Name of the shared data set to mount."},
	{Key: "hpds.profile", Kind: KindString,
		Help: "HPDS Spring profile. Empty uses the shared data set's recorded profile, or none for local data."},
	{Key: "hpds.java_opts", Kind: KindString,
		Help: "HPDS JVM options, including the heap size."},

	{Key: "frontend.theme", Kind: KindString, Flag: "theme", Options: Themes,
		Help: "Frontend theme."},
	{Key: "frontend.docs_enabled", Kind: KindBool,
		Help: "Serve the public API documentation."},
	{Key: "frontend.analytics.google_analytics_id", Kind: KindString,
		Help: "Google Analytics ID baked into the frontend; empty for none."},
	{Key: "frontend.analytics.google_tag_manager_id", Kind: KindString,
		Help: "Google Tag Manager ID baked into the frontend; empty for none."},

	{Key: "tls.mode", Kind: KindString, Options: enumValues(TLSGenerated, TLSProvided),
		Help: "generated: a self-signed certificate pic-sure makes. provided: your own certificate and key files."},
	{Key: "tls.cert_file", Kind: KindString, RequiredWhen: tlsProvided,
		Help: "Server certificate (PEM) in provided mode. A relative path is relative to the stack directory."},
	{Key: "tls.key_file", Kind: KindString, RequiredWhen: tlsProvided,
		Help: "Server private key (PEM) in provided mode. A relative path is relative to the stack directory."},
	{Key: "tls.chain_file", Kind: KindString,
		Help: "Optional certificate chain (PEM) in provided mode."},
	{Key: "trust.custom_certs_dir", Kind: KindString,
		Help: "Directory of extra CA certificates (.crt, .pem, .cer, .der) for the services to trust. A relative path is relative to the stack directory."},

	{Key: "release.repo", Kind: KindString, RequiredWhen: always,
		Help: "Release-control repository holding build-spec.json. Change it only if you fork release control."},
	{Key: "release.branch", Kind: KindString, Flag: "release-branch", RequiredWhen: always,
		Help: "Release-control branch or tag. A tag pins a release; a branch follows it."},
	{Key: "release.cli_compat", Kind: KindString, Options: enumValues(CompatWarn, CompatStrict),
		Help: "What happens when the release was validated with an older pic-sure than this one: warn and carry on, or refuse (strict). A release that needs a newer pic-sure goes through the self-update gate in either mode."},

	{Key: "components.pic-sure.ref", Kind: KindString, Help: componentRefHelp},
	{Key: "components.pic-sure.source", Kind: KindString, Help: componentSourceHelp},
	{Key: "components.frontend.ref", Kind: KindString, Help: componentRefHelp},
	{Key: "components.frontend.source", Kind: KindString, Help: componentSourceHelp},
	{Key: "components.migrations.ref", Kind: KindString, Help: componentRefHelp},
	{Key: "components.migrations.source", Kind: KindString, Help: componentSourceHelp},
	{Key: "components.migrations.project", Kind: KindString, RequiredWhen: always,
		Help: "Migrations project directory to run."},
	{Key: "components.dictionary-etl.ref", Kind: KindString, Help: componentRefHelp},
	{Key: "components.dictionary-etl.source", Kind: KindString, Help: componentSourceHelp},

	{Key: "images.mode", Kind: KindString, Options: enumValues(ImagesBuild, ImagesPull),
		Help: "build: build the images from source. pull: pull published images (not available yet)."},
	{Key: "images.registry", Kind: KindString,
		Help: "Registry to pull from in pull mode; empty for ghcr.io/hms-dbmi."},
	{Key: "dev.services", Kind: KindList,
		Help: "Services running from dev overlays. Use pic-sure dev on and dev off to change it."},

	{Key: "proxy.http", Kind: KindString, Flag: "http-proxy",
		Help: "Proxy URL for outbound HTTP, such as http://proxy.example.com:3128. Empty for none."},
	{Key: "proxy.https", Kind: KindString, Flag: "https-proxy",
		Help: "Proxy URL for outbound HTTPS, also http://, since the proxy is spoken to in plain HTTP. Empty for none."},
	{Key: "proxy.no_proxy", Kind: KindString, Flag: "no-proxy",
		Help: "Comma-separated hosts to reach without the proxy. The stack's services, localhost and 127.0.0.1 are always added."},

	{Key: "services.*.java_opts", Kind: KindString,
		Help: "Advanced: JVM options for one service, replacing its defaults."},
	{Key: "services.*.env.*", Kind: KindString,
		Help: "Advanced: an extra environment variable for one service."},

	{Key: "email.user", Kind: KindString,
		Help: "Account for outbound email such as activation notices; empty disables email."},
	{Key: "email.password", Kind: KindString, Secret: true,
		Help: "Password of the outbound email account."},
}

const (
	componentRefHelp    = "Git ref to build. Empty uses the release's build-spec."
	componentSourceHelp = "Local checkout to build instead of the cached source; init sets it with --source COMPONENT=PATH."
)

func enumValues[T ~string](vs ...T) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = string(v)
	}
	return out
}

// LookupField returns the field for a key path.
func LookupField(key string) (Field, bool) {
	segs := strings.Split(key, ".")
	for _, f := range Fields {
		if keyMatches(f.Key, segs) {
			return f, true
		}
	}
	return Field{}, false
}

func keyMatches(pattern string, segs []string) bool {
	ps := strings.Split(pattern, ".")
	if len(ps) != len(segs) {
		return false
	}
	for i, p := range ps {
		if p != "*" && p != segs[i] {
			return false
		}
	}
	return true
}

// KeyError is a config key `pic-sure config` can't act on: unknown, a
// secret, or read-only.
type KeyError struct {
	Key    string
	Reason string
}

func (e *KeyError) Error() string { return fmt.Sprintf("%s: %s", e.Key, e.Reason) }

// Get returns the value at a key path: a leaf (string, int, bool or
// []string) or a whole section. A key under services that isn't set yields
// its zero value.
func (c *Config) Get(key string) (any, error) {
	if f, ok := LookupField(key); ok && f.Secret {
		return nil, secretKeyError(key)
	}
	v, ok := valueAt(reflect.ValueOf(c).Elem(), splitKey(key))
	if !ok {
		return nil, &KeyError{Key: key, Reason: "unknown config key"}
	}
	return v.Interface(), nil
}

func secretKeyError(key string) *KeyError {
	return &KeyError{Key: key, Reason: "is a secret, kept in .pic-sure/secrets.yaml; pic-sure config doesn't read or write secrets"}
}

func splitKey(key string) []string {
	if key == "" {
		return nil
	}
	return strings.Split(key, ".")
}

// valueAt follows segs from v through yaml-tagged struct fields and
// string-keyed maps. A missing map entry yields the element's zero value.
func valueAt(v reflect.Value, segs []string) (reflect.Value, bool) {
	for _, seg := range segs {
		switch v.Kind() {
		case reflect.Struct:
			f, ok := fieldByYAMLName(v, seg)
			if !ok {
				return reflect.Value{}, false
			}
			v = f
		case reflect.Map:
			e := v.MapIndex(reflect.ValueOf(seg).Convert(v.Type().Key()))
			if !e.IsValid() {
				e = reflect.Zero(v.Type().Elem())
			}
			v = e
		default:
			return reflect.Value{}, false
		}
	}
	return v, len(segs) > 0
}

// fieldByYAMLName returns the field of struct v whose yaml key is name,
// looking inside ",inline" fields too.
func fieldByYAMLName(v reflect.Value, name string) (reflect.Value, bool) {
	t := v.Type()
	for i := range t.NumField() {
		key, inline := yamlKey(t.Field(i))
		switch {
		case inline:
			if f, ok := fieldByYAMLName(v.Field(i), name); ok {
				return f, true
			}
		case key == name:
			return v.Field(i), true
		}
	}
	return reflect.Value{}, false
}

func yamlKey(f reflect.StructField) (key string, inline bool) {
	name, opts, _ := strings.Cut(f.Tag.Get("yaml"), ",")
	return name, strings.Contains(opts, "inline")
}

// parseValue turns a command-line VALUE into the field's type. A list is
// comma-separated, or a YAML flow sequence like [a, b].
func parseValue(f Field, key, raw string) (any, error) {
	switch f.Kind {
	case KindInt:
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			return nil, problemError(key, "want a whole number, got %q", raw)
		}
		return n, nil
	case KindBool:
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
		return nil, problemError(key, "want true or false, got %q", raw)
	case KindList:
		return parseList(key, raw)
	default:
		return raw, nil
	}
}

func problemError(path, format string, args ...any) *ConfigError {
	return &ConfigError{Problems: []Problem{{Path: path, Msg: fmt.Sprintf(format, args...)}}}
}

func parseList(key, raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "[") {
		var out []string
		if err := yaml.Unmarshal([]byte(raw), &out); err != nil {
			return nil, problemError(key, "want a list like [a, b] or a,b, got %q", raw)
		}
		return out, nil
	}
	out := []string{}
	for item := range strings.SplitSeq(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out, nil
}
