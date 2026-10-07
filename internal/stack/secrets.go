package stack

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
)

// Files that hold the stack's secrets, both mode 0600 (§6.1).
const (
	SecretsFile = CLIDir + "/secrets.yaml"
	HPDSKeyFile = hpdsKeyDir + "/encryption_key"

	hpdsKeyDir = CLIDir + "/hpds"
)

// Secrets is .pic-sure/secrets.yaml (§6.3). Every secret is a Secret, so
// printing a Secrets shows none of them. The UUIDs aren't secret and are
// plain strings.
//
// Don't keep a Secrets or *Secrets in an unexported struct field: fmt can't
// call methods through one, so printing the outer struct would show the
// values.
type Secrets struct {
	// DBRootPassword is the local MySQL server's root password
	// (db.mode: local).
	DBRootPassword Secret `yaml:"db_root_password"`
	// DBRemoteRootPassword is the operator's root password for their own
	// MySQL server (db.mode: remote).
	DBRemoteRootPassword Secret `yaml:"db_remote_root_password"`
	DBPicsurePassword    Secret `yaml:"db_picsure_password"`
	DBAuthPassword       Secret `yaml:"db_auth_password"`
	DBAirflowPassword    Secret `yaml:"db_airflow_password"`
	DictionaryDBPassword Secret `yaml:"dictionary_db_password"`

	Auth0ClientSecret Secret `yaml:"auth0_client_secret"`
	// Auth0ClientSecretGenerated says Auth0ClientSecret is random, made
	// for open mode (§9.1 step 3), where Auth0 is never contacted but PSAMA
	// still signs the introspection token with it. Outside open mode
	// EnsureSecrets refuses it until the application's real secret is
	// supplied, which replaces it.
	Auth0ClientSecretGenerated bool `yaml:"auth0_client_secret_generated,omitempty"`

	QueryServiceInternalToken Secret `yaml:"query_service_internal_token"`
	PicsureApplicationToken   Secret `yaml:"picsure_application_token"`
	LoggingAPIKey             Secret `yaml:"logging_api_key"`
	AggregateObfuscationSalt  Secret `yaml:"aggregate_obfuscation_salt"`

	// IntrospectionToken is PSAMA's introspection token (§9.4), issued
	// from Auth0ClientSecret and ApplicationUUID, and renewed by update
	// before IntrospectionTokenExpiry.
	IntrospectionToken       Secret    `yaml:"introspection_token"`
	IntrospectionTokenExpiry time.Time `yaml:"introspection_token_expiry,omitempty"`

	ApplicationUUID   string `yaml:"application_uuid"`
	ResourceUUID      string `yaml:"resource_uuid"`
	VisualizationUUID string `yaml:"visualization_uuid"`

	// EmailPassword is the operator's password for email.user.
	EmailPassword Secret `yaml:"email_password"`
}

// Format prints sec with every secret redacted, whatever the verb. Without
// it, a verb that doesn't suit a struct (%s of a struct holding a *Secrets)
// makes fmt print the fields without calling their Format.
func (sec Secrets) Format(f fmt.State, _ rune) {
	type noFormat Secrets
	_, _ = fmt.Fprintf(f, "%+v", noFormat(sec))
}

// values returns every secret in sec, empty ones included.
func (sec *Secrets) values() []Secret {
	return []Secret{
		sec.DBRootPassword, sec.DBRemoteRootPassword, sec.DBPicsurePassword, sec.DBAuthPassword, sec.DBAirflowPassword, sec.DictionaryDBPassword,
		sec.Auth0ClientSecret,
		sec.QueryServiceInternalToken, sec.PicsureApplicationToken, sec.LoggingAPIKey, sec.AggregateObfuscationSalt,
		sec.IntrospectionToken,
		sec.EmailPassword,
	}
}

const secretsHeader = `# This stack's secrets (written by pic-sure; keep this file private).
# Change one with "pic-sure secrets rotate", not by editing this file.
`

// LoadSecrets reads secrets.yaml and registers every secret with the log
// redactor (see SetSecretRegistrar). Before the stack has one, the error
// wraps fs.ErrNotExist. Unknown keys are ignored, so a read-only command of
// an older CLI can read a newer CLI's file (§10.6).
func (s *Stack) LoadSecrets() (*Secrets, error) {
	data, err := s.root.ReadFile(SecretsFile)
	if err != nil {
		return nil, err
	}
	var sec Secrets
	if err := yaml.Unmarshal(data, &sec); err != nil {
		return nil, fmt.Errorf("reading %s: not a valid secrets file%s", s.Path(SecretsFile), yamlErrorLine(err))
	}
	registerSecrets(sec.values()...)
	return &sec, nil
}

var yamlErrorLineRE = regexp.MustCompile(`^yaml: (?:unmarshal errors:\n\s*)?(line \d+):`)

// yamlErrorLine returns " (line N)" for a yaml error that starts with a
// line number, or "". The rest of yaml's message isn't used: it can quote
// the file's text, which here is secrets.
func yamlErrorLine(err error) string {
	if m := yamlErrorLineRE.FindStringSubmatch(err.Error()); m != nil {
		return " (" + m[1] + ")"
	}
	return ""
}

// SaveSecrets registers sec's secrets with the log redactor, then
// atomically writes secrets.yaml with mode 0600. A stack's first
// secrets.yaml comes from EnsureSecrets, which makes the HPDS key file
// first.
func (s *Stack) SaveSecrets(sec *Secrets) error {
	registerSecrets(sec.values()...)
	data, err := yaml.Marshal(sec)
	if err != nil {
		return err
	}
	return s.WriteFile(SecretsFile, append([]byte(secretsHeader), data...), 0o600)
}

// UserSecrets are the secrets the operator supplies (§6.3), each read with
// ReadUserSecret.
type UserSecrets struct {
	Auth0ClientSecret    Secret
	DBRemoteRootPassword Secret
	EmailPassword        Secret
}

// Format prints u with every secret redacted, as Secrets.Format does.
func (u UserSecrets) Format(f fmt.State, _ rune) {
	type noFormat UserSecrets
	_, _ = fmt.Fprintf(f, "%+v", noFormat(u))
}

// EnsureOptions configures EnsureSecrets.
type EnsureOptions struct {
	// RemoteDB is set when the stack uses the operator's own MySQL server
	// (db.mode: remote), whose root password must then be stored or
	// supplied.
	RemoteDB bool
	// OpenAuth is set when auth.mode is open. With no Auth0 client secret
	// stored or supplied, a random one is generated; without it, a
	// generated one is refused.
	OpenAuth bool
	// Supplied holds the secrets the operator gave this run.
	Supplied UserSecrets
}

// Format prints o with every secret redacted, as Secrets.Format does.
func (o EnsureOptions) Format(f fmt.State, _ rune) {
	type noFormat EnsureOptions
	_, _ = fmt.Fprintf(f, "%+v", noFormat(o))
}

// EnsureSecrets gives the stack every secret it needs, for init and other
// converging commands. It loads secrets.yaml (starting empty if there is
// none), stores each supplied secret the stack doesn't have yet, generates
// every generated secret that is still empty from rnd (ops.Deps.Rand),
// creates the HPDS key file if this is the stack's first secrets.yaml, and
// saves secrets.yaml if anything changed. Every secret is registered with
// the log redactor.
//
// It never replaces a secret (§9.11), with one exception: a supplied Auth0
// client secret replaces one generated for open mode, and clears the
// introspection token issued from it. Any other supplied secret that
// differs from the stored one is an exit-2 error: changing one is `secrets
// rotate`'s job. It also fails, before writing anything, with exit 3 when
// RemoteDB is set and there is no remote root password, when OpenAuth isn't
// set and the client secret is a generated one, and when secrets.yaml
// exists but the HPDS key file doesn't: data loaded with the lost key would
// be unreadable under a new one.
func (s *Stack) EnsureSecrets(rnd io.Reader, opts EnsureOptions) (*Secrets, error) {
	sec, err := s.LoadSecrets()
	existed := err == nil
	if errors.Is(err, fs.ErrNotExist) {
		sec, err = &Secrets{}, nil
	}
	if err != nil {
		return nil, err
	}
	supplied, err := sec.supply(opts.Supplied)
	if err != nil {
		return nil, err
	}
	if opts.RemoteDB && sec.DBRemoteRootPassword == "" {
		return nil, exitcode.Precondition("the stack uses a remote database, but no root password was given for it")
	}
	if !opts.OpenAuth && sec.Auth0ClientSecretGenerated {
		return nil, exitcode.Precondition("auth.mode is no longer open, but the stack's Auth0 client secret is a random one made for open mode; " +
			"give the Auth0 application's client secret with --auth0-client-secret-stdin")
	}
	filled, err := sec.generate(rnd)
	if err != nil {
		return nil, err
	}
	if opts.OpenAuth && sec.Auth0ClientSecret == "" {
		v, err := hexToken(32)(rnd)
		if err != nil {
			return nil, err
		}
		sec.Auth0ClientSecret, sec.Auth0ClientSecretGenerated, filled = Secret(v), true, true
		registerSecrets(sec.Auth0ClientSecret)
	}
	// The key goes first, so once secrets.yaml exists a missing key file
	// was lost rather than never made.
	if err := s.ensureHPDSKey(rnd, existed); err != nil {
		return nil, err
	}
	if supplied || filled {
		if err := s.SaveSecrets(sec); err != nil {
			return nil, err
		}
	}
	return sec, nil
}

// supply stores each user secret sec doesn't have yet and reports whether
// it stored any. A supplied Auth0 client secret replaces a generated one,
// and the token issued from it. Any other that differs from the stored
// value is an exit-2 error.
func (sec *Secrets) supply(u UserSecrets) (stored bool, err error) {
	if u.Auth0ClientSecret != "" && sec.Auth0ClientSecretGenerated {
		sec.Auth0ClientSecret, sec.Auth0ClientSecretGenerated = "", false
		sec.IntrospectionToken, sec.IntrospectionTokenExpiry = "", time.Time{}
	}
	for _, f := range []struct {
		name string
		dst  *Secret
		v    Secret
	}{
		{"Auth0 client secret", &sec.Auth0ClientSecret, u.Auth0ClientSecret},
		{"remote database root password", &sec.DBRemoteRootPassword, u.DBRemoteRootPassword},
		{"email password", &sec.EmailPassword, u.EmailPassword},
	} {
		switch {
		case f.v == "" || f.v == *f.dst:
		case *f.dst == "":
			*f.dst, stored = f.v, true
		default:
			return false, exitcode.Usage("the stack already has a different %s; change it with pic-sure secrets rotate", f.name)
		}
	}
	return stored, nil
}

// LoadHPDSKey reads the HPDS encryption key file and registers the key with
// the log redactor. Before the stack has one, the error wraps
// fs.ErrNotExist.
func (s *Stack) LoadHPDSKey() (Secret, error) {
	data, err := s.root.ReadFile(HPDSKeyFile)
	if err != nil {
		return "", err
	}
	// HPDS trims the file too.
	key := Secret(strings.TrimSpace(string(data)))
	if !isHPDSKey(key) {
		return "", fmt.Errorf("%s is not an HPDS key: it should hold 32 hex characters", s.Path(HPDSKeyFile))
	}
	registerSecrets(key)
	return key, nil
}

func isHPDSKey(k Secret) bool {
	_, err := hex.DecodeString(string(k))
	return len(k) == 32 && err == nil
}

// ensureHPDSKey generates the HPDS key file, mode 0600 in a 0700 directory,
// if there is none. A malformed key file is an error, never replaced, and so
// is a missing one when mustExist is set: data loaded with the old key would
// be unreadable under a new one.
func (s *Stack) ensureHPDSKey(rnd io.Reader, mustExist bool) error {
	_, err := s.LoadHPDSKey()
	switch {
	case !errors.Is(err, fs.ErrNotExist):
		return err
	case mustExist:
		return exitcode.Precondition("%s is missing, but the stack's secrets were made with it; restore it from a backup, or replace it with pic-sure secrets rotate hpds-key", s.Path(HPDSKeyFile))
	}
	return s.writeHPDSKey(rnd)
}

// ReplaceHPDSKey writes a new HPDS key file, whether or not there is one,
// for `secrets rotate hpds-key`. Data encrypted with the old key is
// unreadable under the new one: the caller must have deleted it.
func (s *Stack) ReplaceHPDSKey(rnd io.Reader) error {
	return s.writeHPDSKey(rnd)
}

// writeHPDSKey writes a new key file, mode 0600 in a 0700 directory.
func (s *Stack) writeHPDSKey(rnd io.Reader) error {
	key, err := hpdsKey(rnd)
	if err != nil {
		return err
	}
	registerSecrets(Secret(key))
	if err := s.MkdirAll(hpdsKeyDir, 0o700); err != nil {
		return err
	}
	return s.WriteFile(HPDSKeyFile, []byte(key+"\n"), 0o600)
}
