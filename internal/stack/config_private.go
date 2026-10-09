package stack

import (
	"strings"

	"github.com/JamesPeck/pic-sure-cli/internal/log"
)

// Private reports whether f's value is kept out of the run logs and support
// bundles whatever it looks like: a secret, or the admin email, which is
// personal data.
func (f Field) Private() bool {
	return f.Secret || f.Key == "auth.admin_email"
}

// PrivateKey reports whether the value of the dotted config key is kept out
// of the run logs and support bundles: a Private field's, or, for a key that
// isn't a field or matches a wildcard field such as an env var, a
// secret-named key's (log.IsSecretName). A field that isn't Private, such
// as auth.consent_authorization, never is.
func PrivateKey(key string) bool {
	f, ok := LookupField(key)
	switch {
	case f.Private():
		return true
	case ok && !strings.HasSuffix(f.Key, ".*"):
		return false
	}
	return log.IsSecretName(key)
}
