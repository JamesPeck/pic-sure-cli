package stack

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
)

func TestReadUserSecret(t *testing.T) {
	const flag = "--auth0-client-secret-stdin"
	for _, c := range []struct {
		name, in, want string
	}{
		{"echo s |", "synthetic-secret\n", "synthetic-secret"},
		{"printf s |", "synthetic-secret", "synthetic-secret"},
		{"CRLF", "synthetic-secret\r\n", "synthetic-secret"},
		{"inner spaces kept", " a b \n", " a b "},
	} {
		got, err := ReadUserSecret(strings.NewReader(c.in), flag)
		if err != nil || string(got) != c.want {
			t.Errorf("%s: ReadUserSecret(%q) = %q, %v; want %q", c.name, c.in, got, err, c.want)
		}
	}

	for _, in := range []string{
		"",
		"\n",
		"\r\n",
		"a\nb",
		"a\rb",
		"a\n\n",
		"a\r",
		"a\n\r\n",
		strings.Repeat("a", maxUserSecret+1),
		strings.Repeat("a", maxUserSecret+1) + "\n",
		strings.Repeat("a", maxUserSecret) + "\r\nb",
	} {
		_, err := ReadUserSecret(strings.NewReader(in), flag)
		if code := exitcode.FromError(err); code != exitcode.CodeUsage {
			t.Errorf("ReadUserSecret(%.20q): exit %d (%v), want %d", in, code, err, exitcode.CodeUsage)
		}
		if err != nil && !strings.HasPrefix(err.Error(), flag+": ") {
			t.Errorf("ReadUserSecret(%.20q): %q doesn't name the flag", in, err)
		}
	}

	// The longest secret accepted, with its newline.
	long := strings.Repeat("a", maxUserSecret)
	for _, nl := range []string{"", "\n", "\r\n"} {
		if got, err := ReadUserSecret(strings.NewReader(long+nl), flag); err != nil || string(got) != long {
			t.Errorf("a %d-byte secret + %q: %v", maxUserSecret, nl, err)
		}
	}

	boom := errors.New("boom")
	if _, err := ReadUserSecret(errReader{boom}, flag); !errors.Is(err, boom) || exitcode.FromError(err) != exitcode.CodeFailed {
		t.Errorf("read failure: %v", err)
	}
}

// TestSecretsNeverPrinted checks that no secret value shows when the
// secrets, the config, the state or a single Secret is printed, logged or
// JSON-encoded.
func TestSecretsNeverPrinted(t *testing.T) {
	s := newStack(t)
	sec, err := s.EnsureSecrets(seeded(2), EnsureOptions{Supplied: UserSecrets{
		Auth0ClientSecret: "synthetic-client-secret-0123456789abcdef",
		EmailPassword:     "synthetic-email-password",
	}})
	if err != nil {
		t.Fatal(err)
	}
	sec.IntrospectionToken = "synthetic.introspection.token"
	sec.IntrospectionTokenExpiry = time.Date(2027, 10, 6, 0, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	st := &State{CLIVersion: "v2.0.0", SchemaVersion: 1}
	st.StartOperation("init", time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC))

	var out strings.Builder
	for _, v := range []any{sec, *sec, &cfg, cfg, st, *st, sec.DBRootPassword, struct{ S *Secrets }{sec}, struct{ S Secrets }{*sec}} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
			fmt.Fprintf(&out, verb+"\n", v)
		}
		fmt.Fprintln(&out, v)
		fmt.Fprintln(&out, fmt.Errorf("wrapped: %v", v))
		j, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&out, "%s\n", j)
	}
	for _, h := range []slog.Handler{slog.NewTextHandler(&out, nil), slog.NewJSONHandler(&out, nil)} {
		slog.New(h).Info("secrets", "sec", sec, "one", sec.Auth0ClientSecret, "config", &cfg, "state", st)
	}

	values := sec.values()
	key, err := s.LoadHPDSKey()
	if err != nil {
		t.Fatal(err)
	}
	values = append(values, key)
	for _, v := range values {
		if v == "" {
			t.Fatal("a secret is empty, so the test checks nothing for it")
		}
		if strings.Contains(out.String(), string(v)) {
			t.Errorf("a secret value appears in the output:\n%s", out.String())
			break
		}
	}
	if !strings.Contains(out.String(), Redacted) {
		t.Error("expected the redaction marker in the output")
	}

	var empty Secret
	if got := fmt.Sprintf("%v|%s|%q", empty, empty, empty); got != "||" {
		t.Errorf("an empty Secret prints %q, want nothing", got)
	}
}

func TestSecretRegistrar(t *testing.T) {
	var got []string
	SetSecretRegistrar(func(values ...string) { got = append(got, values...) })
	t.Cleanup(func() { SetSecretRegistrar(nil) })

	s := newStack(t)
	sec, err := s.EnsureSecrets(seeded(3), EnsureOptions{Supplied: UserSecrets{Auth0ClientSecret: "synthetic-client-secret-0123456789abcdef"}})
	if err != nil {
		t.Fatal(err)
	}
	key, err := s.LoadHPDSKey()
	if err != nil {
		t.Fatal(err)
	}
	wantRegistered := func(when string) {
		t.Helper()
		registered := map[string]bool{}
		for _, v := range got {
			if v == "" {
				t.Errorf("%s: an empty value was registered", when)
			}
			registered[v] = true
		}
		for _, v := range append(sec.values(), key) {
			if v != "" && !registered[string(v)] {
				t.Errorf("%s: a secret wasn't registered", when)
			}
		}
	}
	wantRegistered("EnsureSecrets")

	got = nil
	if _, err := s.LoadSecrets(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadHPDSKey(); err != nil {
		t.Fatal(err)
	}
	wantRegistered("LoadSecrets and LoadHPDSKey")

	got = nil
	sec.LoggingAPIKey = "synthetic-rotated-logging-key"
	if err := s.SaveSecrets(sec); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains([]byte(strings.Join(got, "\n")), []byte(sec.LoggingAPIKey)) {
		t.Error("SaveSecrets didn't register a new value")
	}
}
