package stack

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"

	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
)

// Secret is a secret value. fmt, slog and encoding/json show a non-empty
// one as "[REDACTED]" and an empty one as "". string(s) is the value; YAML
// encoding uses it too, since it writes secrets.yaml.
//
// A struct with Secret fields needs a Format method like Secrets.Format.
// When fmt reports a bad verb, as for %s of a struct holding a pointer to
// one, it prints the fields without calling their methods.
type Secret string

// Redacted is what a non-empty Secret prints as.
const Redacted = "[REDACTED]"

func (s Secret) String() string {
	if s == "" {
		return ""
	}
	return Redacted
}

// Format makes every fmt verb, %#v and %x included, print String().
func (s Secret) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, s.String()) }

func (s Secret) LogValue() slog.Value { return slog.StringValue(s.String()) }

func (s Secret) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

var registrar struct {
	sync.Mutex
	fn func(values ...string)
}

// SetSecretRegistrar sets the function that LoadSecrets, SaveSecrets,
// EnsureSecrets and LoadHPDSKey hand every secret value to, so the log
// handler can redact it (§6.3). The cli layer sets it once, at startup, to
// the log redactor's registration function. Until then values go nowhere.
func SetSecretRegistrar(fn func(values ...string)) {
	registrar.Lock()
	defer registrar.Unlock()
	registrar.fn = fn
}

// registerSecrets passes the non-empty values to the registrar.
func registerSecrets(values ...Secret) {
	registrar.Lock()
	fn := registrar.fn
	registrar.Unlock()
	if fn == nil {
		return
	}
	vs := make([]string, 0, len(values))
	for _, v := range values {
		if v != "" {
			vs = append(vs, string(v))
		}
	}
	if len(vs) > 0 {
		fn(vs...)
	}
}

// maxUserSecret bounds what ReadUserSecret reads, so pointing it at the
// wrong file can't exhaust memory.
const maxUserSecret = 64 << 10

// ReadUserSecret reads a secret the operator supplies (the Auth0 client
// secret, a remote database's root password, the email password) from r,
// which is stdin or a file: never a flag value, which would put it in argv
// (§6.3). One trailing "\n" or "\r\n" is stripped, because `echo s |` adds
// one. Any other CR or LF, or an empty secret, is an exit-2 usage error;
// source names where the secret came from, such as the flag
// "--auth0-client-secret-stdin". Rejecting line breaks keeps the
// introspection token's key, the secret's first line (§9.4), identical to
// the secret PSAMA verifies with.
func ReadUserSecret(r io.Reader, source string) (Secret, error) {
	// Room for the longest secret, its "\r\n", and a byte to show it's longer.
	data, err := io.ReadAll(io.LimitReader(r, maxUserSecret+3))
	if err != nil {
		return "", fmt.Errorf("%s: reading the secret: %w", source, err)
	}
	s, ok := strings.CutSuffix(string(data), "\n")
	if ok {
		s = strings.TrimSuffix(s, "\r")
	}
	switch {
	case len(s) > maxUserSecret:
		return "", exitcode.Usage("%s: the secret is longer than %d bytes", source, maxUserSecret)
	case strings.ContainsAny(s, "\r\n"):
		return "", exitcode.Usage("%s: the secret contains a line break; give it on a single line", source)
	case s == "":
		return "", exitcode.Usage("%s: the secret is empty", source)
	}
	return Secret(s), nil
}
