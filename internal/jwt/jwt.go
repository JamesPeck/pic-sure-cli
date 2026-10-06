package jwt

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// DefaultTTL is the introspection token's lifetime.
const DefaultTTL = 365 * 24 * time.Hour

// MinSecretLen is the shortest HMAC key, in bytes, that PSAMA accepts: jjwt
// refuses HS256 keys under 256 bits.
const MinSecretLen = 32

var header = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256"}`))

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// claims is in the order jwt-creator writes them (its jjwt serialises a Java
// HashMap), so a token issued in the same second matches the JAR's byte for
// byte.
type claims struct {
	Sub string `json:"sub"`
	Iss string `json:"iss"`
	Exp int64  `json:"exp"`
	Iat int64  `json:"iat"`
	Jti string `json:"jti"`
}

// Introspection returns the introspection token for the application
// appUUID, issued at now and valid for ttl, and the expiry it carries
// (whole seconds, UTC). PSAMA accepts the token only if
// auth.application.token holds it byte for byte.
//
// The HMAC key is the first line of secret, as jwt-creator read it from a
// file. PSAMA verifies with the whole secret it is configured with, line
// breaks included, so the token works only if that secret is exactly this
// first line: strip the newline from a secret read from stdin before
// storing it.
func Introspection(secret, appUUID string, now time.Time, ttl time.Duration) (string, time.Time, error) {
	key := secret
	if i := strings.IndexAny(key, "\r\n"); i >= 0 {
		key = key[:i]
	}
	if len(key) < MinSecretLen {
		return "", time.Time{}, fmt.Errorf("the Auth0 client secret is too short to sign the introspection token: its first line is %d bytes and PSAMA needs at least %d", len(key), MinSecretLen)
	}
	if !uuidPattern.MatchString(appUUID) {
		return "", time.Time{}, fmt.Errorf("application UUID %q is not a UUID", appUUID)
	}
	if ttl <= 0 {
		return "", time.Time{}, errors.New("introspection token lifetime must be positive")
	}

	exp := now.Add(ttl).Unix()
	payload, err := json.Marshal(claims{
		Sub: "PSAMA_APPLICATION|" + appUUID,
		Iss: "bar",
		Exp: exp,
		Iat: now.Unix(),
		Jti: "Foo",
	})
	if err != nil {
		return "", time.Time{}, err
	}
	signed := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(signed))
	return signed + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), time.Unix(exp, 0).UTC(), nil
}
