package log

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Redacted replaces a secret in log output.
const Redacted = "[REDACTED]"

// Redactor is a registry of secret values. A Run passes everything it
// writes through its Redactor, so a registered value never reaches stderr
// or a log file, wherever it appears in a record: the message, an attr, an
// error, a struct, or attrs added with Logger.With before the value was
// registered. The zero value is ready to use, and a Redactor is safe for
// concurrent use.
type Redactor struct {
	mu       sync.RWMutex
	patterns map[string]struct{}
	replacer *strings.Replacer // nil until a value is registered
}

// registry is the process-wide Redactor that a Run uses by default.
var registry Redactor

// RegisterSecrets adds values to the process-wide registry. Register a
// secret as soon as it is read or generated: output written before then
// isn't redacted.
func RegisterSecrets(values ...string) { registry.Register(values...) }

// Redact replaces every value in the process-wide registry that appears in
// s with Redacted.
func Redact(s string) string { return registry.Redact(s) }

// MinSecret is the shortest value Register accepts. A shorter one would
// match all through unrelated text and make the log unreadable. Such a
// value is left to secret-named attrs and stack.Secret's own redaction.
const MinSecret = 4

// Register adds values to r, ignoring those shorter than MinSecret. Each value is also
// registered as slog's JSON and text handlers escape it, so one containing
// quotes, backslashes or control characters is caught in their output too.
func (r *Redactor) Register(values ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.patterns == nil {
		r.patterns = map[string]struct{}{}
	}
	before := len(r.patterns)
	for _, v := range values {
		if len(v) < MinSecret {
			continue
		}
		for _, p := range escapedForms(v) {
			r.patterns[p] = struct{}{}
		}
	}
	if len(r.patterns) == before {
		return
	}
	// At each position, strings.Replacer takes the first pattern in
	// argument order that matches, so longest first replaces a secret that
	// contains another one whole.
	ps := slices.SortedFunc(maps.Keys(r.patterns), func(a, b string) int {
		if d := len(b) - len(a); d != 0 {
			return d
		}
		return strings.Compare(a, b)
	})
	pairs := make([]string, 0, 2*len(ps))
	for _, p := range ps {
		pairs = append(pairs, p, Redacted)
	}
	r.replacer = strings.NewReplacer(pairs...)
}

// Redact replaces every registered value that appears in s with Redacted,
// and the userinfo of every URL, which can hold a proxy or Git password
// that was never registered: http://user:pw@host becomes
// http://[REDACTED]@host.
func (r *Redactor) Redact(s string) string {
	r.mu.RLock()
	rep := r.replacer
	r.mu.RUnlock()
	if rep != nil {
		s = rep.Replace(s)
	}
	if strings.Contains(s, "@") {
		s = urlUserinfo.ReplaceAllString(s, "${1}"+Redacted+"@")
	}
	return s
}

// urlUserinfo matches a URL's scheme and userinfo. As in url.Parse, the
// userinfo runs to the authority's last "@", so a password may hold a raw
// "@" or "'". It stops at white space, at the characters that end an
// authority, and at the quotes and angle brackets that can't appear in a
// URL but delimit one in log output.
var urlUserinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^\s/?#"<>]+@`)

// escapedForms returns v as it can appear in log output: raw, escaped by
// slog's JSON handler for strings and by encoding/json for values it
// marshals (they differ on \b and \f), and quoted by the text handler.
// Both escapers work rune by rune, so a secret inside a longer string is
// escaped the same way as on its own.
func escapedForms(v string) []string {
	quoted := strconv.Quote(v)
	forms := []string{v, slogJSONString(v), quoted[1 : len(quoted)-1]}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false) // as slog's JSON handler does
	if enc.Encode(v) == nil {
		s := strings.TrimSuffix(b.String(), "\n")
		forms = append(forms, s[1:len(s)-1])
	}
	return forms
}

// slogJSONString returns v as slog's JSON handler writes a string, without
// the quotes.
func slogJSONString(v string) string {
	var b bytes.Buffer
	h := slog.NewJSONHandler(&b, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && (a.Key == slog.LevelKey || a.Key == slog.MessageKey) {
				return slog.Attr{}
			}
			return a
		},
	})
	r := slog.NewRecord(time.Time{}, slog.LevelInfo, "", 0)
	r.AddAttrs(slog.String("v", v))
	_ = h.Handle(context.Background(), r)
	// b is {"v":"<escaped>"} and a newline.
	return strings.TrimSuffix(strings.TrimPrefix(b.String(), `{"v":"`), "\"}\n")
}

// redactWriter passes each write through a Redactor. slog's built-in
// handlers write each record with a single Write, so a secret never
// straddles two writes.
type redactWriter struct {
	r *Redactor
	w io.Writer
}

func (w redactWriter) Write(p []byte) (int, error) {
	if _, err := io.WriteString(w.w, w.r.Redact(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}

// secretWords are the last words of attr names that hold secrets, as in
// client_secret, DB_ROOT_PASSWORD or introspectionToken. A plural counts
// too. A name ending in another word, like token_expiry or password_file,
// isn't secret-named.
var secretWords = map[string]bool{
	"apikey": true, "authorization": true, "cookie": true, "credential": true,
	"jwt": true, "passphrase": true, "passwd": true, "password": true,
	"pwd": true, "salt": true, "secret": true, "token": true,
}

// keyQualifiers are the words that make a name ending in "key" secret, as
// in LOGGING_API_KEY or encryption_key. A bare "key" isn't.
var keyQualifiers = map[string]bool{
	"access": true, "api": true, "encryption": true, "private": true,
	"secret": true, "signing": true,
}

// IsSecretName reports whether an attr or group named name holds a secret,
// judged by its last word (see secretWords and keyQualifiers). Words are
// split at punctuation and at camelCase boundaries, ignoring case.
func IsSecretName(name string) bool {
	words := splitWords(name)
	if len(words) == 0 {
		return false
	}
	last := words[len(words)-1]
	if secretWords[last] || secretWords[strings.TrimSuffix(last, "s")] {
		return true
	}
	return (last == "key" || last == "keys") && len(words) > 1 && keyQualifiers[words[len(words)-2]]
}

// splitWords splits name into lower-case words at every rune that isn't a
// letter or digit, and at camelCase boundaries: "APIKey" and "api_key" are
// both [api key].
func splitWords(name string) []string {
	var words []string
	var cur []rune
	rs := []rune(name)
	flush := func() {
		if len(cur) > 0 {
			words = append(words, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	for i, c := range rs {
		if !unicode.IsLetter(c) && !unicode.IsDigit(c) {
			flush()
			continue
		}
		if unicode.IsUpper(c) && len(cur) > 0 {
			prev := cur[len(cur)-1]
			nextLower := i+1 < len(rs) && unicode.IsLower(rs[i+1])
			if !unicode.IsUpper(prev) || nextLower {
				flush()
			}
		}
		cur = append(cur, c)
	}
	flush()
	return words
}

// nameHandler replaces the value of every secret-named attr (IsSecretName)
// with Redacted, at any depth, and every attr inside a secret-named group.
// An empty string stays empty, since knowing a secret is unset helps debug
// and gives nothing away.
type nameHandler struct {
	inner slog.Handler
	// secret is set inside a WithGroup whose name is secret-named.
	secret bool
}

func (h nameHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h nameHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(redactAttr(a, h.secret))
		return true
	})
	return h.inner.Handle(ctx, out)
}

func (h nameHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		out[i] = redactAttr(a, h.secret)
	}
	return nameHandler{inner: h.inner.WithAttrs(out), secret: h.secret}
}

func (h nameHandler) WithGroup(name string) slog.Handler {
	return nameHandler{inner: h.inner.WithGroup(name), secret: h.secret || IsSecretName(name)}
}

func redactAttr(a slog.Attr, secret bool) slog.Attr {
	a.Value = a.Value.Resolve()
	secret = secret || IsSecretName(a.Key)
	switch {
	case a.Value.Kind() == slog.KindGroup:
		group := a.Value.Group()
		out := make([]slog.Attr, len(group))
		for i, g := range group {
			out[i] = redactAttr(g, secret)
		}
		a.Value = slog.GroupValue(out...)
	case secret && (a.Value.Kind() != slog.KindString || a.Value.String() != ""):
		a.Value = slog.StringValue(Redacted)
	}
	return a
}
