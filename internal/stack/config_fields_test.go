package stack

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// leafKinds walks t's yaml keys and returns each leaf's key path and kind,
// with "*" for a map key.
func leafKinds(t reflect.Type, path string, out map[string]FieldKind) {
	switch t.Kind() {
	case reflect.Struct:
		for i := range t.NumField() {
			key, inline := yamlKey(t.Field(i))
			if inline {
				leafKinds(t.Field(i).Type, path, out)
			} else {
				leafKinds(t.Field(i).Type, joinKey(path, key), out)
			}
		}
	case reflect.Map:
		leafKinds(t.Elem(), joinKey(path, "*"), out)
	case reflect.Int:
		out[path] = KindInt
	case reflect.Bool:
		out[path] = KindBool
	case reflect.Slice:
		out[path] = KindList
	default:
		out[path] = KindString
	}
}

func TestFieldsDescribeEveryKey(t *testing.T) {
	leaves := map[string]FieldKind{}
	leafKinds(reflect.TypeFor[Config](), "", leaves)

	seen := map[string]bool{}
	for _, f := range Fields {
		if seen[f.Key] {
			t.Errorf("%s is in Fields twice", f.Key)
		}
		seen[f.Key] = true
		if f.Help == "" {
			t.Errorf("%s has no help", f.Key)
		}
		if f.Secret {
			if _, ok := leaves[f.Key]; ok {
				t.Errorf("secret %s is a pic-sure.yaml key", f.Key)
			}
			continue
		}
		kind, ok := leaves[f.Key]
		if !ok {
			t.Errorf("%s is not a pic-sure.yaml key", f.Key)
			continue
		}
		if kind != f.Kind {
			t.Errorf("%s: Kind %s, but the Config field is a %s", f.Key, f.Kind, kind)
		}
		if len(f.Options) > 0 && f.Kind != KindString {
			t.Errorf("%s: an enum must be a string", f.Key)
		}
	}
	for key := range leaves {
		if !seen[key] {
			t.Errorf("pic-sure.yaml key %s is missing from Fields", key)
		}
	}
}

func TestEnumDefaultsAreOptions(t *testing.T) {
	c := DefaultConfig()
	for _, f := range Fields {
		if len(f.Options) == 0 {
			continue
		}
		v, err := c.Get(f.Key)
		if err != nil || !slices.Contains(f.Options, reflect.ValueOf(v).String()) {
			t.Errorf("%s: default %v isn't one of %q", f.Key, v, f.Options)
		}
	}
}

// TestFieldFlagsAreInitsFlags checks the flag column against the
// non-interactive init flags in spec §5, plus --name. --auto-ports and
// --source aren't fields: one picks the ports, the other is
// COMPONENT=PATH.
func TestFieldFlagsAreInitsFlags(t *testing.T) {
	want := []string{
		"admin-email", "auth-mode", "auth0-client-id", "auth0-client-secret-stdin", "auth0-tenant",
		"db-host", "db-mode", "db-port", "db-root-password-stdin", "db-root-user",
		"hpds-data", "http-port", "http-proxy", "https-port", "https-proxy",
		"name", "no-proxy", "release-branch", "theme",
	}
	var got []string
	for _, f := range Fields {
		if f.Flag == "" {
			continue
		}
		got = append(got, f.Flag)
		if f.Secret != strings.HasSuffix(f.Flag, "-stdin") {
			t.Errorf("%s: a secret's flag, and only a secret's, ends in -stdin", f.Key)
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("flags:\n got  %q\n want %q", got, want)
	}
}

func TestLookupField(t *testing.T) {
	for key, want := range map[string]string{
		"network.http_port":            "network.http_port",
		"services.hpds.java_opts":      "services.*.java_opts",
		"services.psama.env.JAVA_HOME": "services.*.env.*",
		"auth.auth0.client_secret":     "auth.auth0.client_secret",
	} {
		f, ok := LookupField(key)
		if !ok || f.Key != want {
			t.Errorf("LookupField(%q) = %q, %v; want %q", key, f.Key, ok, want)
		}
	}
	for _, key := range []string{"network", "services.hpds", "services.hpds.env", "network.http_port.x", ""} {
		if f, ok := LookupField(key); ok {
			t.Errorf("LookupField(%q) = %q, want none", key, f.Key)
		}
	}
}

func TestRequiredWhen(t *testing.T) {
	c := DefaultConfig()
	required := func() []string {
		var keys []string
		for _, f := range Fields {
			if f.Required(&c) {
				keys = append(keys, f.Key)
			}
		}
		return keys
	}
	base := []string{
		"name", "auth.auth0.tenant", "auth.auth0.client_id", "auth.auth0.client_secret", "auth.admin_email",
		"network.hostname", "release.repo", "release.branch", "components.migrations.project",
	}
	if got := required(); !slices.Equal(got, base) {
		t.Errorf("defaults: required %q, want %q", got, base)
	}

	c.Auth.Mode = AuthOpen
	c.DB.Mode = DBRemote
	c.HPDS.Data = HPDSShared
	c.TLS.Mode = TLSProvided
	want := []string{
		"name", "auth.admin_email", "network.hostname",
		"db.remote.host", "db.remote.root_user", "db.remote.root_password",
		"hpds.shared_name", "tls.cert_file", "tls.key_file",
		"release.repo", "release.branch", "components.migrations.project",
	}
	if got := required(); !slices.Equal(got, want) {
		t.Errorf("open, remote, shared, provided: required %q, want %q", got, want)
	}
}
