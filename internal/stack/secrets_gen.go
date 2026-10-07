package stack

import (
	"encoding/hex"
	"fmt"
	"io"
)

const (
	passwordLen  = 24
	alphanumeric = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	// passwordCutoff is the largest multiple of len(alphanumeric) that fits
	// in a byte. Bytes from it up are dropped, so every character is equally
	// likely, as with the bash's `tr -dc 'A-Za-z0-9' </dev/urandom`.
	passwordCutoff = 256 / len(alphanumeric) * len(alphanumeric)
)

// password returns 24 characters from [A-Za-z0-9].
func password(r io.Reader) (string, error) {
	out := make([]byte, 0, passwordLen)
	buf := make([]byte, passwordLen)
	for len(out) < passwordLen {
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", randError(err)
		}
		for _, b := range buf {
			if int(b) < passwordCutoff && len(out) < passwordLen {
				out = append(out, alphanumeric[int(b)%len(alphanumeric)])
			}
		}
	}
	return string(out), nil
}

// hexToken returns a generator of n random bytes as lowercase hex, like
// `openssl rand -hex n`.
func hexToken(n int) func(io.Reader) (string, error) {
	return func(r io.Reader) (string, error) {
		b := make([]byte, n)
		if _, err := io.ReadFull(r, b); err != nil {
			return "", randError(err)
		}
		return hex.EncodeToString(b), nil
	}
}

// uuidV4 returns a random (version 4) UUID in lowercase, as the bash's
// `uuidgen | tr '[:upper:]' '[:lower:]'` does.
func uuidV4(r io.Reader) (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return "", randError(err)
	}
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // RFC 4122 variant
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}

// hpdsKey returns an HPDS encryption key: 32 hex characters, which HPDS
// uses as the 32 bytes of an AES-256 key. The bash took the 16-byte key
// `openssl enc -aes-128-cbc -P` printed in uppercase hex; this is lowercase,
// which HPDS accepts just the same.
var hpdsKey = hexToken(16)

func randError(err error) error {
	return fmt.Errorf("generating a secret: reading random bytes: %w", err)
}

// generate fills every empty generated secret in sec from r, and reports
// whether it filled any. It never replaces a value (§9.11), and leaves the
// operator's secrets alone. The introspection token is issued from the
// client secret, not generated here.
func (sec *Secrets) generate(r io.Reader) (bool, error) {
	g := &filler{r: r}
	fill(g, &sec.DBRootPassword, password)
	fill(g, &sec.DBPicsurePassword, password)
	fill(g, &sec.DBAuthPassword, password)
	fill(g, &sec.DBAirflowPassword, password)
	fill(g, &sec.DictionaryDBPassword, password)
	fill(g, &sec.QueryServiceInternalToken, hexToken(32))
	fill(g, &sec.PicsureApplicationToken, hexToken(32))
	fill(g, &sec.LoggingAPIKey, hexToken(32))
	fill(g, &sec.AggregateObfuscationSalt, hexToken(16))
	fill(g, &sec.ApplicationUUID, uuidV4)
	fill(g, &sec.ResourceUUID, uuidV4)
	fill(g, &sec.VisualizationUUID, uuidV4)
	return g.filled, g.err
}

// filler carries generate's reader and outcome through its fill calls.
type filler struct {
	r      io.Reader
	filled bool
	err    error
}

// fill sets an empty *f from gen, unless an earlier fill failed.
func fill[T ~string](g *filler, f *T, gen func(io.Reader) (string, error)) {
	if g.err != nil || *f != "" {
		return
	}
	var v string
	if v, g.err = gen(g.r); g.err == nil {
		*f, g.filled = T(v), true
	}
}
