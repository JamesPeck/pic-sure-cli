package jwt_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/jwt"
)

const (
	// Synthetic, 40 bytes.
	secret  = "test-client-secret-0123456789abcdefghijk"
	appUUID = "0d7f4c8e-3b2a-4e61-9f0a-5c4d3e2b1a09"
)

// t0 has a fractional second, which iat and exp drop.
var t0 = time.Date(2026, 10, 6, 12, 30, 15, 750_000_000, time.UTC)

// split returns the token's three segments, base64url-decoded. Decoding
// with RawURLEncoding fails on padding or on standard-alphabet characters.
func split(t *testing.T, token string) (header, payload, sig []byte) {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments, want 3: %s", len(parts), token)
	}
	var out [3][]byte
	for i, p := range parts {
		b, err := base64.RawURLEncoding.DecodeString(p)
		if err != nil {
			t.Fatalf("segment %d is not unpadded base64url: %v", i, err)
		}
		out[i] = b
	}
	return out[0], out[1], out[2]
}

// verify checks the token's signature with key, computed here rather than
// through the package.
func verify(t *testing.T, token string, key string) bool {
	t.Helper()
	_, _, sig := split(t, token)
	signed := token[:strings.LastIndex(token, ".")]
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(signed))
	return hmac.Equal(sig, mac.Sum(nil))
}

func TestIntrospectionHeaderAndClaims(t *testing.T) {
	token, exp, err := jwt.Introspection(secret, appUUID, t0, jwt.DefaultTTL)
	if err != nil {
		t.Fatal(err)
	}
	header, payload, _ := split(t, token)

	if string(header) != `{"alg":"HS256"}` {
		t.Errorf("header = %s, want {\"alg\":\"HS256\"}", header)
	}

	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var got map[string]any
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("claims are not JSON: %v: %s", err, payload)
	}
	iat := t0.Unix()
	wantExp := iat + 365*24*60*60
	want := map[string]any{
		"sub": "PSAMA_APPLICATION|" + appUUID,
		"jti": "Foo",
		"iss": "bar",
		"iat": json.Number(strconv.FormatInt(iat, 10)),
		"exp": json.Number(strconv.FormatInt(wantExp, 10)),
	}
	if len(got) != len(want) {
		t.Errorf("claims = %s, want exactly %v", payload, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("claim %s = %v, want %v", k, got[k], v)
		}
	}

	if !exp.Equal(time.Unix(wantExp, 0)) || exp.Location() != time.UTC {
		t.Errorf("exp = %v, want %v in UTC", exp, time.Unix(wantExp, 0).UTC())
	}
}

func TestIntrospectionSignature(t *testing.T) {
	token, _, err := jwt.Introspection(secret, appUUID, t0, jwt.DefaultTTL)
	if err != nil {
		t.Fatal(err)
	}
	if !verify(t, token, secret) {
		t.Error("signature does not verify with HMAC-SHA256 over header.payload")
	}
	if verify(t, token, secret+"x") {
		t.Error("signature verifies with the wrong key")
	}
}

func TestIntrospectionMatchesJWTCreator(t *testing.T) {
	// Printed by jwt-creator v1.0.0's generateJwt.jar, run as
	// `java -jar generateJwt.jar secret.txt sub "PSAMA_APPLICATION|<appUUID>" 365 day`
	// with secret.txt holding the input below. Its clock read Unix second
	// 1791318031.
	const want = "eyJhbGciOiJIUzI1NiJ9." +
		"eyJzdWIiOiJQU0FNQV9BUFBMSUNBVElPTnwwZDdmNGM4ZS0zYjJhLTRlNjEtOWYwYS01YzRkM2UyYjFhMDkiLCJpc3MiOiJiYXIiLCJleHAiOjE4MjI4NTQwMzEsImlhdCI6MTc5MTMxODAzMSwianRpIjoiRm9vIn0." +
		"zq0HiyuwWCrN5HoBDqPetEe4Ih1Z3EXZJcD9Rp2wHEA"
	got, _, err := jwt.Introspection(secret+"\nsecond-line", appUUID, time.Unix(1791318031, 0), jwt.DefaultTTL)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("token = %s\nwant jwt-creator's %s", got, want)
	}
}

func TestIntrospectionKeysOnFirstLine(t *testing.T) {
	want, _, err := jwt.Introspection(secret, appUUID, t0, jwt.DefaultTTL)
	if err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]string{
		"trailing LF":   secret + "\n",
		"trailing CRLF": secret + "\r\n",
		"LF":            secret + "\nsecond line",
		"CRLF":          secret + "\r\nsecond line",
		"CR":            secret + "\rsecond line",
	} {
		t.Run(name, func(t *testing.T) {
			got, _, err := jwt.Introspection(s, appUUID, t0, jwt.DefaultTTL)
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Errorf("token differs from the one keyed by the first line alone")
			}
			if !verify(t, got, secret) {
				t.Error("signature does not verify with the first line as key")
			}
		})
	}
}

func TestIntrospectionRejectsShortSecret(t *testing.T) {
	short := secret[:jwt.MinSecretLen-1]
	for name, s := range map[string]string{
		"empty":            "",
		"31 bytes":         short,
		"short first line": short + "\n" + secret,
	} {
		t.Run(name, func(t *testing.T) {
			token, _, err := jwt.Introspection(s, appUUID, t0, jwt.DefaultTTL)
			if err == nil {
				t.Fatalf("got token %s, want an error", token)
			}
			if !strings.Contains(err.Error(), "at least 32") {
				t.Errorf("error %q does not give the minimum", err)
			}
			if s != "" && strings.Contains(err.Error(), short) {
				t.Errorf("error %q leaks the secret", err)
			}
		})
	}

	if _, _, err := jwt.Introspection(secret[:jwt.MinSecretLen], appUUID, t0, jwt.DefaultTTL); err != nil {
		t.Errorf("32-byte secret: %v", err)
	}
}

func TestIntrospectionRejectsBadInput(t *testing.T) {
	for name, tc := range map[string]struct {
		uuid string
		ttl  time.Duration
	}{
		"empty UUID":   {"", jwt.DefaultTTL},
		"not a UUID":   {"PICSURE", jwt.DefaultTTL},
		"pipe in UUID": {appUUID + "|x", jwt.DefaultTTL},
		"zero TTL":     {appUUID, 0},
		"negative TTL": {appUUID, -time.Hour},
	} {
		t.Run(name, func(t *testing.T) {
			if token, _, err := jwt.Introspection(secret, tc.uuid, t0, tc.ttl); err == nil {
				t.Errorf("got token %s, want an error", token)
			}
		})
	}
}
