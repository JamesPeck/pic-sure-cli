package stack

import (
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"regexp"
	"strings"
	"testing"
)

// seeded returns a deterministic randomness source for tests.
func seeded(seed byte) io.Reader {
	var s [32]byte
	s[0] = seed
	return rand.NewChaCha8(s)
}

var (
	passwordRE = regexp.MustCompile(`^[A-Za-z0-9]{24}$`)
	hex64RE    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	hex32RE    = regexp.MustCompile(`^[0-9a-f]{32}$`)
	uuidV4RE   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
)

func TestGeneratorFormats(t *testing.T) {
	r := seeded(1)
	for range 200 {
		for _, c := range []struct {
			gen func(io.Reader) (Secret, error)
			re  *regexp.Regexp
		}{
			{password, passwordRE},
			{hexToken(32), hex64RE},
			{hexToken(16), hex32RE},
			{uuidV4, uuidV4RE},
			{hpdsKey, hex32RE},
		} {
			v, err := c.gen(r)
			if err != nil {
				t.Fatal(err)
			}
			if !c.re.MatchString(string(v)) {
				t.Fatalf("%q doesn't match %s", v, c.re)
			}
		}
	}
}

func TestPasswordDropsBiasedBytes(t *testing.T) {
	// 248 and up would favour the first characters, so they are skipped.
	in := append([]byte{255, 248, 0, 61, 62, 247}, bytes.Repeat([]byte{1}, 60)...)
	got, err := password(bytes.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if want := "A9A9" + strings.Repeat("B", 20); string(got) != want {
		t.Errorf("password = %q, want %q", got, want)
	}
}

func TestGeneratorsReportRandFailure(t *testing.T) {
	boom := errors.New("boom")
	for _, gen := range []func(io.Reader) (Secret, error){password, hexToken(32), uuidV4} {
		if _, err := gen(io.MultiReader(bytes.NewReader([]byte{1, 2}), errReader{boom})); !errors.Is(err, boom) {
			t.Errorf("err = %v, want boom", err)
		}
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }
