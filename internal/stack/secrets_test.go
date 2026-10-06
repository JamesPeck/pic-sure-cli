package stack

import (
	"errors"
	"io/fs"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

const syntheticClientSecret = "synthetic-client-secret-0123456789abcdef"

func TestEnsureSecretsGenerates(t *testing.T) {
	s := newStack(t)
	sec, err := s.EnsureSecrets(seeded(1), EnsureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		v    Secret
		re   *regexp.Regexp
	}{
		{"DBRootPassword", sec.DBRootPassword, passwordRE},
		{"DBPicsurePassword", sec.DBPicsurePassword, passwordRE},
		{"DBAuthPassword", sec.DBAuthPassword, passwordRE},
		{"DBAirflowPassword", sec.DBAirflowPassword, passwordRE},
		{"DictionaryDBPassword", sec.DictionaryDBPassword, passwordRE},
		{"QueryServiceInternalToken", sec.QueryServiceInternalToken, hex64RE},
		{"PicsureApplicationToken", sec.PicsureApplicationToken, hex64RE},
		{"LoggingAPIKey", sec.LoggingAPIKey, hex64RE},
		{"AggregateObfuscationSalt", sec.AggregateObfuscationSalt, hex32RE},
		{"ApplicationUUID", sec.ApplicationUUID, uuidV4RE},
		{"ResourceUUID", sec.ResourceUUID, uuidV4RE},
		{"VisualizationUUID", sec.VisualizationUUID, uuidV4RE},
	} {
		if !c.re.MatchString(string(c.v)) {
			t.Errorf("%s = %q, want %s", c.name, c.v, c.re)
		}
	}
	// The operator's secrets and the token aren't generated.
	if sec.Auth0ClientSecret != "" || sec.EmailPassword != "" || sec.IntrospectionToken != "" || !sec.IntrospectionTokenExpiry.IsZero() {
		t.Errorf("generated a secret that isn't generated: %#v", *sec)
	}
	seen := map[Secret]bool{}
	for _, v := range sec.values() {
		if v != "" && seen[v] {
			t.Errorf("value %q generated twice", v)
		}
		seen[v] = true
	}

	key, err := s.LoadHPDSKey()
	if err != nil {
		t.Fatal(err)
	}
	if !hex32RE.MatchString(string(key)) {
		t.Errorf("HPDS key %q, want 32 hex characters", key)
	}
	wantContent(t, s.Path(HPDSKeyFile), string(key)+"\n")

	wantMode(t, s.Path(SecretsFile), 0o600)
	wantMode(t, s.Path(HPDSKeyFile), 0o600)
	wantMode(t, s.Path(hpdsKeyDir), fs.ModeDir|0o700)
	m, err := s.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{SecretsFile, hpdsKeyDir, HPDSKeyFile} {
		if !m.Has(p) {
			t.Errorf("%s isn't recorded in the manifest", p)
		}
	}

	got, err := s.LoadSecrets()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, sec) {
		t.Error("LoadSecrets doesn't return what EnsureSecrets saved")
	}
}

func TestEnsureSecretsNeverRegenerates(t *testing.T) {
	s := newStack(t)
	pre := &Secrets{DBPicsurePassword: "synthetic-kept-password", LoggingAPIKey: "synthetic-kept-key"}
	if err := s.SaveSecrets(pre); err != nil {
		t.Fatal(err)
	}
	if err := s.MkdirAll(hpdsKeyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const oldKey = "0123456789ABCDEF0123456789ABCDEF" // a bash-made key is uppercase
	if err := s.WriteFile(HPDSKeyFile, []byte(oldKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	first, err := s.EnsureSecrets(seeded(1), EnsureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if first.DBPicsurePassword != pre.DBPicsurePassword || first.LoggingAPIKey != pre.LoggingAPIKey {
		t.Error("EnsureSecrets replaced a stored secret")
	}
	if first.DBAuthPassword == "" || first.ResourceUUID == "" {
		t.Error("EnsureSecrets didn't fill the empty secrets")
	}
	wantContent(t, s.Path(HPDSKeyFile), oldKey+"\n")

	before, err := os.ReadFile(s.Path(SecretsFile))
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(s.Path(SecretsFile))
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.EnsureSecrets(seeded(2), EnsureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Error("a second EnsureSecrets changed the secrets")
	}
	wantContent(t, s.Path(SecretsFile), string(before))
	if fi2, err := os.Stat(s.Path(SecretsFile)); err != nil || !os.SameFile(fi, fi2) {
		t.Error("a second EnsureSecrets with nothing to do rewrote secrets.yaml")
	}
	wantContent(t, s.Path(HPDSKeyFile), oldKey+"\n")
}

func TestEnsureSecretsSupplied(t *testing.T) {
	s := newStack(t)
	sec, err := s.EnsureSecrets(seeded(1), EnsureOptions{Supplied: UserSecrets{
		Auth0ClientSecret: syntheticClientSecret,
		EmailPassword:     "synthetic-email-password",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if sec.Auth0ClientSecret != syntheticClientSecret || sec.EmailPassword != "synthetic-email-password" {
		t.Fatalf("supplied secrets not stored: %#v", *sec)
	}

	// init issues the token; supplying the same client secret again keeps it.
	sec.IntrospectionToken = "synthetic.token"
	sec.IntrospectionTokenExpiry = time.Date(2027, 10, 6, 0, 0, 0, 0, time.UTC)
	if err := s.SaveSecrets(sec); err != nil {
		t.Fatal(err)
	}
	sec, err = s.EnsureSecrets(seeded(2), EnsureOptions{Supplied: UserSecrets{Auth0ClientSecret: syntheticClientSecret}})
	if err != nil {
		t.Fatal(err)
	}
	if sec.IntrospectionToken != "synthetic.token" {
		t.Error("the same client secret cleared the introspection token")
	}

	// A new client secret replaces the old one and drops the token issued
	// from it.
	sec, err = s.EnsureSecrets(seeded(3), EnsureOptions{Supplied: UserSecrets{Auth0ClientSecret: syntheticClientSecret + "-new"}})
	if err != nil {
		t.Fatal(err)
	}
	if sec.Auth0ClientSecret != syntheticClientSecret+"-new" || sec.IntrospectionToken != "" || !sec.IntrospectionTokenExpiry.IsZero() {
		t.Errorf("after a new client secret: %#v", *sec)
	}
	if sec.EmailPassword != "synthetic-email-password" {
		t.Error("an unsupplied secret was cleared")
	}
	if got, err := s.LoadSecrets(); err != nil || !reflect.DeepEqual(got, sec) {
		t.Errorf("the change wasn't saved: %v", err)
	}
}

func TestEnsureSecretsRemoteDB(t *testing.T) {
	s := newStack(t)
	if _, err := s.EnsureSecrets(seeded(1), EnsureOptions{RemoteDB: true}); err == nil || !strings.Contains(err.Error(), "root password") {
		t.Fatalf("remote DB without a root password: err = %v", err)
	}
	wantNotExist(t, s.Path(SecretsFile))
	wantNotExist(t, s.Path(HPDSKeyFile))

	sec, err := s.EnsureSecrets(seeded(1), EnsureOptions{RemoteDB: true, Supplied: UserSecrets{DBRootPassword: "synthetic-remote-root"}})
	if err != nil {
		t.Fatal(err)
	}
	if sec.DBRootPassword != "synthetic-remote-root" || sec.DBAuthPassword == "" {
		t.Errorf("remote DB secrets: %#v", *sec)
	}
	// Later runs needn't supply it again.
	if _, err := s.EnsureSecrets(seeded(2), EnsureOptions{RemoteDB: true}); err != nil {
		t.Error(err)
	}

	local := newStack(t)
	if _, err := local.EnsureSecrets(seeded(1), EnsureOptions{Supplied: UserSecrets{DBRootPassword: "synthetic-root"}}); err == nil {
		t.Error("a root password supplied for the local database was accepted")
	}
	wantNotExist(t, local.Path(SecretsFile))
}

func TestEnsureSecretsRandFailureWritesNothing(t *testing.T) {
	s := newStack(t)
	boom := errors.New("boom")
	if _, err := s.EnsureSecrets(errReader{boom}, EnsureOptions{}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	wantNotExist(t, s.Path(SecretsFile))
	wantNotExist(t, s.Path(HPDSKeyFile))
}

func TestEnsureSecretsKeepsABadKeyFile(t *testing.T) {
	s := newStack(t)
	if err := s.MkdirAll(hpdsKeyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "not-a-key\n", strings.Repeat("g", 32)} {
		if err := s.WriteFile(HPDSKeyFile, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.EnsureSecrets(seeded(1), EnsureOptions{}); err == nil || !strings.Contains(err.Error(), "not an HPDS key") {
			t.Errorf("key file %q: err = %v", bad, err)
		}
		wantContent(t, s.Path(HPDSKeyFile), bad)
	}
}

func TestLoadSecrets(t *testing.T) {
	s := newStack(t)
	if _, err := s.LoadSecrets(); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("before any save: err = %v, want ErrNotExist", err)
	}
	if _, err := s.LoadHPDSKey(); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("LoadHPDSKey before any save: err = %v, want ErrNotExist", err)
	}

	sec := &Secrets{
		DBRootPassword:           "synthetic-root",
		IntrospectionToken:       "synthetic.token",
		IntrospectionTokenExpiry: time.Date(2027, 10, 6, 12, 30, 0, 0, time.UTC),
		ApplicationUUID:          "4a3f1c2e-5b6d-4e7f-8a9b-0c1d2e3f4a5b",
	}
	if err := s.SaveSecrets(sec); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(s.Path(SecretsFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# This stack's secrets", "db_root_password: synthetic-root\n", "introspection_token_expiry: 2027-10-06T12:30:00Z\n", "email_password: \"\"\n"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("secrets.yaml lacks %q:\n%s", want, data)
		}
	}
	got, err := s.LoadSecrets()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, sec) {
		t.Errorf("round trip: got %#v", *got)
	}

	// A newer CLI's extra keys are ignored.
	if err := s.WriteFile(SecretsFile, append(data, "some_new_secret: x\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := s.LoadSecrets(); err != nil || !reflect.DeepEqual(got, sec) {
		t.Errorf("with an unknown key: %v", err)
	}

	if err := s.WriteFile(SecretsFile, []byte("db_root_password: [a, b]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadSecrets(); err == nil || !strings.Contains(err.Error(), s.Path(SecretsFile)) {
		t.Errorf("malformed file: err = %v", err)
	}
}

func wantNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s exists (err = %v)", path, err)
	}
}
