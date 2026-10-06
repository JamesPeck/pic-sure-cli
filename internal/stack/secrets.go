package stack

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Files that hold the stack's secrets, both mode 0600 (§6.1).
const (
	SecretsFile = CLIDir + "/secrets.yaml"
	HPDSKeyFile = hpdsKeyDir + "/encryption_key"

	hpdsKeyDir = CLIDir + "/hpds"
)

// Secrets is .pic-sure/secrets.yaml (§6.3). Every value is a Secret, so
// printing a Secrets shows none of them. The UUIDs aren't sensitive, but they
// live here as the spec has it and are treated the same.
//
// Don't keep a Secrets or *Secrets in an unexported struct field: fmt can't
// call methods through one, so printing the outer struct would show the
// values.
type Secrets struct {
	// DBRootPassword is the MySQL root password: generated for the local
	// database, or the operator's for a remote one (db.mode: remote).
	DBRootPassword       Secret `yaml:"db_root_password"`
	DBPicsurePassword    Secret `yaml:"db_picsure_password"`
	DBAuthPassword       Secret `yaml:"db_auth_password"`
	DBAirflowPassword    Secret `yaml:"db_airflow_password"`
	DictionaryDBPassword Secret `yaml:"dictionary_db_password"`

	// Auth0ClientSecret is the operator's Auth0 application secret.
	Auth0ClientSecret Secret `yaml:"auth0_client_secret"`

	QueryServiceInternalToken Secret `yaml:"query_service_internal_token"`
	PicsureApplicationToken   Secret `yaml:"picsure_application_token"`
	LoggingAPIKey             Secret `yaml:"logging_api_key"`
	AggregateObfuscationSalt  Secret `yaml:"aggregate_obfuscation_salt"`

	// IntrospectionToken is PSAMA's introspection token (§9.4), issued
	// from Auth0ClientSecret and ApplicationUUID, and renewed by update
	// before IntrospectionTokenExpiry.
	IntrospectionToken       Secret    `yaml:"introspection_token"`
	IntrospectionTokenExpiry time.Time `yaml:"introspection_token_expiry,omitempty"`

	ApplicationUUID   Secret `yaml:"application_uuid"`
	ResourceUUID      Secret `yaml:"resource_uuid"`
	VisualizationUUID Secret `yaml:"visualization_uuid"`

	// EmailPassword is the operator's password for email.user.
	EmailPassword Secret `yaml:"email_password"`
}

// Format prints sec with every value redacted, whatever the verb. Without
// it, a verb that doesn't suit a struct (%s of a struct holding a *Secrets)
// makes fmt print the fields without calling their Format.
func (sec Secrets) Format(f fmt.State, _ rune) {
	type fields Secrets // without this method
	_, _ = fmt.Fprintf(f, "%+v", fields(sec))
}

// values returns every secret in sec, empty ones included.
func (sec *Secrets) values() []Secret {
	return []Secret{
		sec.DBRootPassword, sec.DBPicsurePassword, sec.DBAuthPassword, sec.DBAirflowPassword, sec.DictionaryDBPassword,
		sec.Auth0ClientSecret,
		sec.QueryServiceInternalToken, sec.PicsureApplicationToken, sec.LoggingAPIKey, sec.AggregateObfuscationSalt,
		sec.IntrospectionToken,
		sec.ApplicationUUID, sec.ResourceUUID, sec.VisualizationUUID,
		sec.EmailPassword,
	}
}

const secretsHeader = `# This stack's secrets (written by pic-sure; keep this file private).
# Change one with "pic-sure secrets rotate", not by editing this file.
`

// LoadSecrets reads secrets.yaml and registers every value with the log
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
		return nil, fmt.Errorf("reading %s: %w", s.Path(SecretsFile), err)
	}
	registerSecrets(sec.values()...)
	return &sec, nil
}

// SaveSecrets registers sec's values with the log redactor, then atomically
// writes secrets.yaml with mode 0600.
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
	Auth0ClientSecret Secret
	// DBRootPassword is the root password of the remote database server
	// (db.mode: remote).
	DBRootPassword Secret
	EmailPassword  Secret
}

// EnsureOptions configures EnsureSecrets.
type EnsureOptions struct {
	// RemoteDB is set when the stack uses the operator's own MySQL server
	// (db.mode: remote), whose root password is then supplied, never
	// generated.
	RemoteDB bool
	// Supplied holds the secrets the operator gave this run. Each non-empty
	// one replaces the stored value.
	Supplied UserSecrets
}

// EnsureSecrets gives the stack every secret it needs, for init and any
// converging command. It loads secrets.yaml (starting empty if there is
// none), stores the supplied secrets, generates every generated secret that
// is still empty from rnd (ops.Deps.Rand), and saves the file if anything
// changed. It never replaces a generated secret (§9.11). A new Auth0 client
// secret clears the introspection token, so the caller issues one PSAMA can
// verify. It then creates the HPDS key file if there is none. Every value is
// registered with the log redactor.
//
// It fails before writing anything if RemoteDB is set and no root password
// is stored or supplied, or if a root password is supplied without RemoteDB.
func (s *Stack) EnsureSecrets(rnd io.Reader, opts EnsureOptions) (*Secrets, error) {
	sec, err := s.LoadSecrets()
	if errors.Is(err, fs.ErrNotExist) {
		sec, err = &Secrets{}, nil
	}
	if err != nil {
		return nil, err
	}
	switch {
	case !opts.RemoteDB && opts.Supplied.DBRootPassword != "":
		// The local database was, or will be, created with the generated one.
		return nil, errors.New("a root password is supplied only for a remote database")
	case opts.RemoteDB && sec.DBRootPassword == "" && opts.Supplied.DBRootPassword == "":
		return nil, errors.New("the stack uses a remote database, but no root password was given for it")
	}
	changed := sec.supply(opts.Supplied)
	filled, err := sec.generate(rnd, opts.RemoteDB)
	if err != nil {
		return nil, err
	}
	if changed || filled {
		if err := s.SaveSecrets(sec); err != nil {
			return nil, err
		}
	}
	if err := s.ensureHPDSKey(rnd); err != nil {
		return nil, err
	}
	return sec, nil
}

// supply stores the non-empty user secrets in sec and reports whether that
// changed anything.
func (sec *Secrets) supply(u UserSecrets) (changed bool) {
	if u.Auth0ClientSecret != "" && u.Auth0ClientSecret != sec.Auth0ClientSecret {
		// PSAMA verifies the token with the client secret, so one issued
		// from the old secret no longer works.
		sec.IntrospectionToken, sec.IntrospectionTokenExpiry = "", time.Time{}
	}
	set := func(f *Secret, v Secret) {
		if v != "" && v != *f {
			*f, changed = v, true
		}
	}
	set(&sec.Auth0ClientSecret, u.Auth0ClientSecret)
	set(&sec.DBRootPassword, u.DBRootPassword)
	set(&sec.EmailPassword, u.EmailPassword)
	return changed
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
// unless there is one. A key file that is there but unreadable or malformed
// is an error, never replaced: data loaded with the old key would be lost.
func (s *Stack) ensureHPDSKey(rnd io.Reader) error {
	_, err := s.LoadHPDSKey()
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	key, err := hpdsKey(rnd)
	if err != nil {
		return err
	}
	registerSecrets(key)
	if err := s.MkdirAll(hpdsKeyDir, 0o700); err != nil {
		return err
	}
	return s.WriteFile(HPDSKeyFile, []byte(key+"\n"), 0o600)
}
