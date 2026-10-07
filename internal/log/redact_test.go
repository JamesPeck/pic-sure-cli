package log

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestRedactorReplacesRegisteredValues(t *testing.T) {
	var r Redactor
	if got := r.Redact("nothing registered"); got != "nothing registered" {
		t.Errorf("empty Redactor changed %q", got)
	}
	r.Register("", "abc", "hunter2-abc", "hunter2-abc-longer")
	got := r.Redact("a hunter2-abc-longer b hunter2-abc c")
	if want := "a [REDACTED] b [REDACTED] c"; got != want {
		t.Errorf("Redact = %q, want %q", got, want)
	}
	if got := r.Redact("abc"); got != "abc" {
		t.Errorf("a value shorter than MinSecret redacted %q", got)
	}
}

func TestRedactScrubsURLUserinfo(t *testing.T) {
	var r Redactor
	for in, want := range map[string]string{
		"via http://alice:s3cr%40t@proxy:3128/x":      "via http://[REDACTED]@proxy:3128/x",
		`{"url":"https://tok@github.com/o/r.git"}`:    `{"url":"https://[REDACTED]@github.com/o/r.git"}`,
		"HTTPS_PROXY=socks5h://u:p@h:1080 NO_PROXY=a": "HTTPS_PROXY=socks5h://[REDACTED]@h:1080 NO_PROXY=a",
		"http://proxy:3128/a@b and admin@example.com": "http://proxy:3128/a@b and admin@example.com",
		"args=[http://u:pa'ss@h:3128]":                "args=[http://[REDACTED]@h:3128]",
		"http://u:p@ss@h:3128/x?to=a@b":               "http://[REDACTED]@h:3128/x?to=a@b",
		`"http://a@b" "c@d"`:                          `"http://[REDACTED]@b" "c@d"`,
	} {
		if got := r.Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRegisterSecretsUsesTheProcessRegistry(t *testing.T) {
	RegisterSecrets("process-wide-7f3a")
	if got := Redact("x process-wide-7f3a y"); got != "x [REDACTED] y" {
		t.Errorf("Redact = %q", got)
	}
}

type creds struct {
	User string
	Pass string
}

// TestRunRedactsSecretValuesEverywhere logs a secret in every place a
// record can carry one and checks that neither output has it, raw or
// escaped.
func TestRunRedactsSecretValuesEverywhere(t *testing.T) {
	const plain = "Zq8xW2pLm4Rt6Yv9Bn3Kc5Hd"
	const tricky = "p\"a\\s\ts\x01w<o>&rd\u2028x"
	var red Redactor
	var stderr bytes.Buffer
	run := New(Options{Level: slog.LevelDebug, Stderr: &stderr, File: true, Redactor: &red})
	// Attrs added before the secrets are registered are still redacted.
	logger := run.Logger().With("early", plain)
	red.Register(plain, tricky)

	for _, s := range []string{plain, tricky} {
		logger.Info("connecting with " + s)
		logger.Debug("attrs",
			"str", "user:"+s,
			"err", fmt.Errorf("auth failed for %s", s),
			"struct", creds{User: "root", Pass: s},
			"bytes", []byte(s),
			slog.Group("db", "dsn", "root:"+s+"@tcp(db)"),
		)
	}
	dir := t.TempDir()
	path, err := run.OpenFile(dirStore(dir), t0)
	if err != nil {
		t.Fatal(err)
	}
	logger.Warn("after open " + plain)
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	// The secrets as they are written raw, by the JSON handler, and quoted
	// by the text handler.
	forms := []string{plain, tricky, `p\"a\\s\ts\u0001w<o>&rd\u2028x`, `p\"a\\s\ts\x01w<o>&rd\u2028x`}
	file := readFile(t, path)
	for name, out := range map[string]string{"stderr": stderr.String(), "file": file} {
		for _, form := range forms {
			if strings.Contains(out, form) {
				t.Errorf("%s has secret form %q:\n%s", name, form, out)
			}
		}
		if n := strings.Count(out, Redacted); n < 10 {
			t.Errorf("%s has %d %s, want at least 10:\n%s", name, n, Redacted, out)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(file), "\n") {
		if !json.Valid([]byte(line)) {
			t.Errorf("file line isn't JSON: %s", line)
		}
	}
}

func TestEscapedFormsMatchSlogOutput(t *testing.T) {
	for _, v := range []string{"pa\"ss", "back\\slash", "tab\tnl\n", "ctl\x01\b\f", "sep\u2028", "a b=c"} {
		var text, js bytes.Buffer
		slog.New(slog.NewTextHandler(&text, nil)).Info("msg "+v, "k", v)
		slog.New(slog.NewJSONHandler(&js, nil)).Info("msg "+v, "k", v, "s", struct{ V string }{v})
		var r Redactor
		r.Register(v)
		for name, out := range map[string]string{"text": text.String(), "json": js.String()} {
			got := r.Redact(out)
			if n := strings.Count(got, Redacted); n < 2 {
				t.Errorf("%q in %s output: %d redactions in %s", v, name, n, got)
			}
		}
	}
}

func TestIsSecretName(t *testing.T) {
	secret := []string{
		"password", "DB_ROOT_PASSWORD", "mysqlPwd", "client_secret", "clientSecret",
		"Auth0ClientSecret", "QUERY_SERVICE_INTERNAL_TOKEN", "introspectionToken",
		"LOGGING_API_KEY", "apiKey", "APIKey", "apikey", "encryption_key",
		"AGGREGATE_OBFUSCATION_SALT", "secrets", "tokens", "Authorization",
		"private-key", "jwt", "credentials",
	}
	notSecret := []string{
		"", "key", "config_key", "token_expiry", "password_file", "secret_name",
		"host", "keys", "user", "passes", "status", "monkey",
	}
	for _, n := range secret {
		if !IsSecretName(n) {
			t.Errorf("IsSecretName(%q) = false, want true", n)
		}
	}
	for _, n := range notSecret {
		if IsSecretName(n) {
			t.Errorf("IsSecretName(%q) = true, want false", n)
		}
	}
}

type tokenValuer struct{}

func (tokenValuer) LogValue() slog.Value { return slog.StringValue("resolved") }

func TestSecretNamedAttrsAreRedacted(t *testing.T) {
	var stderr bytes.Buffer
	run := New(Options{Level: slog.LevelDebug, Stderr: &stderr, File: true, Redactor: &Redactor{}})
	logger := run.Logger()
	logger.With("api_key", "with-attrs").Info("record",
		"client_secret", "abc",
		"token_expiry", "2026-10-06",
		"password", "",
		"access_token", tokenValuer{},
		"port", 3306,
		slog.Group("db", "root_password", "pw", "host", "db"),
		slog.Group("credentials", "user", "u1", slog.Group("inner", "x", "y")),
	)
	logger.WithGroup("secrets").Info("group", "db", "dbpass", slog.Group("g", "v", "w"))
	logger.WithGroup("auth").Info("group", "client_id", "id", "client_secret", "s")

	path, err := run.OpenFile(dirStore(t.TempDir()), t0)
	if err != nil {
		t.Fatal(err)
	}
	_ = run.Close()
	recs := readRecords(t, path)
	if len(recs) != 3 {
		t.Fatalf("got %d records, want 3", len(recs))
	}
	want := []map[string]any{{
		"api_key":       Redacted,
		"client_secret": Redacted,
		"token_expiry":  "2026-10-06",
		"password":      "",
		"access_token":  Redacted,
		"port":          float64(3306),
		"db":            map[string]any{"root_password": Redacted, "host": "db"},
		"credentials":   map[string]any{"user": Redacted, "inner": map[string]any{"x": Redacted}},
	}, {
		"secrets": map[string]any{"db": Redacted, "g": map[string]any{"v": Redacted}},
	}, {
		"auth": map[string]any{"client_id": "id", "client_secret": Redacted},
	}}
	for i, w := range want {
		for k, v := range w {
			if got := recs[i][k]; fmt.Sprint(got) != fmt.Sprint(v) {
				t.Errorf("record %d: %s = %v, want %v", i, k, got, v)
			}
		}
	}
	for _, s := range []string{"with-attrs", "abc", "resolved", "pw", "u1", "dbpass", `"s"`} {
		if strings.Contains(stderr.String(), s) {
			t.Errorf("stderr has %q:\n%s", s, stderr.String())
		}
	}
}
