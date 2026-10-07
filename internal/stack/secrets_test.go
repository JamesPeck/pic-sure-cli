package stack

import (
	"errors"
	"io/fs"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
)

const syntheticClientSecret = "synthetic-client-secret-0123456789abcdef"

func TestEnsureSecretsGenerates(t *testing.T) {
	s := newStack(t)
	sec, err := s.EnsureSecrets(seeded(1), EnsureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	generated := []struct {
		name string
		v    string
		re   *regexp.Regexp
	}{
		{"DBRootPassword", string(sec.DBRootPassword), passwordRE},
		{"DBPicsurePassword", string(sec.DBPicsurePassword), passwordRE},
		{"DBAuthPassword", string(sec.DBAuthPassword), passwordRE},
		{"DBAirflowPassword", string(sec.DBAirflowPassword), passwordRE},
		{"DictionaryDBPassword", string(sec.DictionaryDBPassword), passwordRE},
		{"QueryServiceInternalToken", string(sec.QueryServiceInternalToken), hex64RE},
		{"PicsureApplicationToken", string(sec.PicsureApplicationToken), hex64RE},
		{"LoggingAPIKey", string(sec.LoggingAPIKey), hex64RE},
		{"AggregateObfuscationSalt", string(sec.AggregateObfuscationSalt), hex32RE},
		{"ApplicationUUID", sec.ApplicationUUID, uuidV4RE},
		{"ResourceUUID", sec.ResourceUUID, uuidV4RE},
		{"VisualizationUUID", sec.VisualizationUUID, uuidV4RE},
	}
	seen := map[string]bool{}
	for _, c := range generated {
		if !c.re.MatchString(c.v) {
			t.Errorf("%s = %q, want %s", c.name, c.v, c.re)
		}
		if seen[c.v] {
			t.Errorf("%s: value generated twice", c.name)
		}
		seen[c.v] = true
	}
	// The operator's secrets and the token aren't generated.
	if sec.Auth0ClientSecret != "" || sec.DBRemoteRootPassword != "" || sec.EmailPassword != "" || sec.IntrospectionToken != "" || !sec.IntrospectionTokenExpiry.IsZero() {
		t.Errorf("generated a secret that isn't generated: %#v", *sec)
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
	// A run that died after making the key but before saving secrets.yaml.
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
	wantContent(t, s.Path(HPDSKeyFile), oldKey+"\n")

	first.DBAuthPassword = ""
	first.ResourceUUID = ""
	if err := s.SaveSecrets(first); err != nil {
		t.Fatal(err)
	}
	second, err := s.EnsureSecrets(seeded(2), EnsureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if second.DBAuthPassword == "" || second.ResourceUUID == "" {
		t.Error("EnsureSecrets didn't fill the emptied secrets")
	}
	second.DBAuthPassword, second.ResourceUUID = "", ""
	if !reflect.DeepEqual(first, second) {
		t.Error("EnsureSecrets replaced a stored secret")
	}

	before, err := os.ReadFile(s.Path(SecretsFile))
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(s.Path(SecretsFile))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureSecrets(seeded(3), EnsureOptions{}); err != nil {
		t.Fatal(err)
	}
	wantContent(t, s.Path(SecretsFile), string(before))
	if fi2, err := os.Stat(s.Path(SecretsFile)); err != nil || !os.SameFile(fi, fi2) {
		t.Error("an EnsureSecrets with nothing to do rewrote secrets.yaml")
	}
	wantContent(t, s.Path(HPDSKeyFile), oldKey+"\n")
}

func TestEnsureSecretsSupplied(t *testing.T) {
	s := newStack(t)
	user := UserSecrets{Auth0ClientSecret: syntheticClientSecret, EmailPassword: "synthetic-email-password"}
	sec, err := s.EnsureSecrets(seeded(1), EnsureOptions{Supplied: user})
	if err != nil {
		t.Fatal(err)
	}
	if sec.Auth0ClientSecret != user.Auth0ClientSecret || sec.EmailPassword != user.EmailPassword {
		t.Fatalf("supplied secrets not stored: %#v", *sec)
	}

	// Supplying the same values again changes nothing.
	if _, err := s.EnsureSecrets(seeded(2), EnsureOptions{Supplied: user}); err != nil {
		t.Fatal(err)
	}

	// A different one is refused: changing a secret is secrets rotate's job.
	before, err := os.ReadFile(s.Path(SecretsFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []UserSecrets{
		{Auth0ClientSecret: syntheticClientSecret + "-new"},
		{EmailPassword: "synthetic-other-password"},
	} {
		_, err := s.EnsureSecrets(seeded(3), EnsureOptions{Supplied: u})
		if exitcode.FromError(err) != exitcode.CodeUsage || !strings.Contains(err.Error(), "secrets rotate") {
			t.Errorf("supplying a different secret: err = %v, want exit 2 naming secrets rotate", err)
		}
	}
	wantContent(t, s.Path(SecretsFile), string(before))
}

func TestEnsureSecretsRemoteDB(t *testing.T) {
	s := newStack(t)
	_, err := s.EnsureSecrets(seeded(1), EnsureOptions{RemoteDB: true})
	if exitcode.FromError(err) != exitcode.CodePrecondition || !strings.Contains(err.Error(), "root password") {
		t.Fatalf("remote DB without a root password: err = %v", err)
	}
	wantNotExist(t, s.Path(SecretsFile))
	wantNotExist(t, s.Path(HPDSKeyFile))

	sec, err := s.EnsureSecrets(seeded(1), EnsureOptions{RemoteDB: true, Supplied: UserSecrets{DBRemoteRootPassword: "synthetic-remote-root"}})
	if err != nil {
		t.Fatal(err)
	}
	if sec.DBRemoteRootPassword != "synthetic-remote-root" || sec.DBAuthPassword == "" {
		t.Errorf("remote DB secrets: %#v", *sec)
	}
	// Later runs needn't supply it again.
	if _, err := s.EnsureSecrets(seeded(2), EnsureOptions{RemoteDB: true}); err != nil {
		t.Error(err)
	}

	// A local stack switched to a remote database still needs the remote
	// server's password: the local one isn't it.
	local := newStack(t)
	if _, err := local.EnsureSecrets(seeded(1), EnsureOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := local.EnsureSecrets(seeded(2), EnsureOptions{RemoteDB: true}); exitcode.FromError(err) != exitcode.CodePrecondition {
		t.Errorf("local stack switched to remote: err = %v, want exit 3", err)
	}
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

func TestEnsureSecretsNeverReplacesTheKey(t *testing.T) {
	s := newStack(t)
	if _, err := s.EnsureSecrets(seeded(1), EnsureOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "not-a-key\n", strings.Repeat("g", 32)} {
		if err := s.WriteFile(HPDSKeyFile, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.EnsureSecrets(seeded(2), EnsureOptions{}); err == nil || !strings.Contains(err.Error(), "not an HPDS key") {
			t.Errorf("key file %q: err = %v", bad, err)
		}
		wantContent(t, s.Path(HPDSKeyFile), bad)
	}

	// Data loaded with a lost key would be unreadable under a new one.
	if err := os.Remove(s.Path(HPDSKeyFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureSecrets(seeded(3), EnsureOptions{}); err == nil || !strings.Contains(err.Error(), "is missing") {
		t.Errorf("lost key file: err = %v", err)
	}
	wantNotExist(t, s.Path(HPDSKeyFile))
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

	// Errors name the file but never quote a value.
	for _, bad := range []string{
		"db_root_password: [a, b]\n",
		"auth0_client_secret: !!int synthetic-client-secret\n",
		"introspection_token_expiry: synthetic-client-secret\n",
		"auth0_client_secret: synthetic-client-secret\n  bad: [\n",
	} {
		if err := s.WriteFile(SecretsFile, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := s.LoadSecrets()
		if err == nil || !strings.Contains(err.Error(), s.Path(SecretsFile)) || strings.Contains(err.Error(), "synthetic") {
			t.Errorf("malformed %q: err = %v", bad, err)
		}
	}
}

// TestValuesListsEverySecret keeps values(), which decides what is
// registered with the redactor, in step with the Secrets fields.
func TestValuesListsEverySecret(t *testing.T) {
	var sec Secrets
	v := reflect.ValueOf(&sec).Elem()
	var want []string
	for i := range v.NumField() {
		if f := v.Field(i); f.Type() == reflect.TypeFor[Secret]() {
			f.SetString(v.Type().Field(i).Name)
			want = append(want, v.Type().Field(i).Name)
		}
	}
	var got []string
	for _, s := range sec.values() {
		got = append(got, string(s))
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("values() = %q, want every Secret field %q", got, want)
	}
}

func wantNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s exists (err = %v)", path, err)
	}
}
