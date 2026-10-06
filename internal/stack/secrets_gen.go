package stack

import (
	"encoding/hex"
	"fmt"
	"io"
)

// The generators below produce the bash's formats (§6.3) from the reader the
// caller passes, crypto/rand.Reader in production (ops.Deps.Rand).

const (
	passwordLen  = 24
	alphanumeric = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	// passwordCutoff is the largest multiple of len(alphanumeric) that fits
	// in a byte. Bytes from it up are dropped, so every character is equally
	// likely, as with the bash's `tr -dc 'A-Za-z0-9' </dev/urandom`.
	passwordCutoff = 256 / len(alphanumeric) * len(alphanumeric)
)

// password returns 24 characters from [A-Za-z0-9].
func password(r io.Reader) (Secret, error) {
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
	return Secret(out), nil
}

// hexToken returns a generator of n random bytes as lowercase hex, like
// `openssl rand -hex n`.
func hexToken(n int) func(io.Reader) (Secret, error) {
	return func(r io.Reader) (Secret, error) {
		b := make([]byte, n)
		if _, err := io.ReadFull(r, b); err != nil {
			return "", randError(err)
		}
		return Secret(hex.EncodeToString(b)), nil
	}
}

// uuidV4 returns a random (version 4) UUID in lowercase, as the bash's
// `uuidgen | tr '[:upper:]' '[:lower:]'` does.
func uuidV4(r io.Reader) (Secret, error) {
	var b [16]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return "", randError(err)
	}
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // RFC 4122 variant
	h := hex.EncodeToString(b[:])
	return Secret(h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]), nil
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
// whether it filled any. It never replaces a value (§9.11). It leaves the
// operator's secrets alone, and the root password too when remoteDB is set,
// since that is then the remote server's. The introspection token is issued
// from the client secret, not generated here.
func (sec *Secrets) generate(r io.Reader, remoteDB bool) (filled bool, err error) {
	fill := func(f *Secret, gen func(io.Reader) (Secret, error)) {
		if err != nil || *f != "" {
			return
		}
		var v Secret
		if v, err = gen(r); err == nil {
			*f, filled = v, true
		}
	}
	if !remoteDB {
		fill(&sec.DBRootPassword, password)
	}
	fill(&sec.DBPicsurePassword, password)
	fill(&sec.DBAuthPassword, password)
	fill(&sec.DBAirflowPassword, password)
	fill(&sec.DictionaryDBPassword, password)
	fill(&sec.QueryServiceInternalToken, hexToken(32))
	fill(&sec.PicsureApplicationToken, hexToken(32))
	fill(&sec.LoggingAPIKey, hexToken(32))
	fill(&sec.AggregateObfuscationSalt, hexToken(16))
	fill(&sec.ApplicationUUID, uuidV4)
	fill(&sec.ResourceUUID, uuidV4)
	fill(&sec.VisualizationUUID, uuidV4)
	return filled, err
}
