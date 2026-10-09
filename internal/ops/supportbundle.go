package ops

import (
	"archive/tar"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/log"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

// What a support bundle collects (spec §9.9).
const (
	// BundleRunLogs is how many of the newest run logs go in.
	BundleRunLogs = 5
	// BundleLogTail is how many lines of each service's log go in.
	BundleLogTail = 500
)

// bundleLogsTimeout bounds each service's `compose logs`. Tests shorten it.
var bundleLogsTimeout = docker.ProbeTimeout

// SupportBundleOptions configures SupportBundle.
type SupportBundleOptions struct {
	// Stack is the stack to collect from, or nil for the host checks
	// alone (doctor without a stack).
	Stack *stack.Stack
	// Status is passed to Status, which runs only with a Stack.
	Status StatusOptions
	Doctor DoctorOptions
	// Prefix is the directory every file in the archive is under.
	Prefix string
}

// SupportBundleReport is `support-bundle --json`'s report.
type SupportBundleReport struct {
	// Path is the archive's path; the caller sets it.
	Path string `json:"path"`
	// Files are the archive's files, without the prefix.
	Files []string `json:"files"`
	// Problems are what couldn't be collected, and why. The bundle is
	// still written.
	Problems []string `json:"problems"`
	// ShortSecrets is how many secrets are shorter than log's minimum and
	// are redacted only where they stand alone (see bundleRedactor).
	ShortSecrets int `json:"short_secrets"`
}

// SupportBundle writes a gzipped tar of the stack's diagnostics to w: the
// deep status and doctor reports, the newest run logs, compose ps and each
// service's recent logs, and pic-sure.yaml, state.json and manifest.json
// (spec §9.9). Every file passes through a redactor of every value in
// secrets.yaml and the HPDS key file, and pic-sure.yaml's secret-named
// keys are blanked too. It reads only; a part it can't collect is a
// problem in the report and README.txt. Only failing to write w, or ctx
// ending, is an error.
func SupportBundle(ctx context.Context, d *Deps, w io.Writer, opts SupportBundleOptions) (*SupportBundleReport, error) {
	b := &bundle{report: &SupportBundleReport{Files: []string{}, Problems: []string{}}}
	st := opts.Stack
	if st != nil {
		b.red, b.secretsKnown = bundleSecrets(st, b.problem)
		b.report.ShortSecrets = len(b.red.short)
	} else {
		b.red = &bundleRedactor{}
		b.problem("no stack: only the host checks are included")
	}

	if st != nil {
		b.json("status.json", Status(ctx, d, st, opts.Status))
	}
	b.json("doctor.json", Doctor(ctx, d, opts.Doctor))
	if st != nil {
		b.runLogs(st)
		b.compose(ctx, d.Compose, opts.Doctor.ComposeErr)
		b.stackFile(st, stack.ConfigFile, "stack/pic-sure.yaml", false, redactConfigKeys)
		b.stackFile(st, stack.StateFile, "stack/state.json", true, nil)
		b.stackFile(st, stack.ManifestFile, "stack/manifest.json", true, nil)
	}
	if ctx.Err() != nil {
		return nil, context.Cause(ctx)
	}
	// A compose error can quote a secret.
	for i, p := range b.report.Problems {
		b.report.Problems[i] = b.red.Redact(p)
	}
	b.add("README.txt", b.readme(), false)

	if err := b.write(w, opts.Prefix, d.Clock.Now()); err != nil {
		return nil, err
	}
	return b.report, nil
}

type bundleFile struct {
	name string
	data []byte
}

type bundle struct {
	red          *bundleRedactor
	secretsKnown bool
	files        []bundleFile
	report       *SupportBundleReport
}

func (b *bundle) problem(format string, args ...any) {
	b.report.Problems = append(b.report.Problems, fmt.Sprintf(format, args...))
}

// add redacts data and adds it as name. A JSON file, or each JSON line of
// one that isn't valid JSON as a whole (a run log), is redacted inside its
// strings only (redactJSONStrings), so it stays valid JSON; any other text
// is redacted throughout.
func (b *bundle) add(name string, data []byte, isJSON bool) {
	switch {
	case isJSON && json.Valid(data):
		data = redactJSONStrings(data, b.red.Redact)
	case isJSON:
		var out bytes.Buffer
		for line := range bytes.Lines(data) {
			if json.Valid(line) {
				out.Write(redactJSONStrings(line, b.red.Redact))
			} else {
				out.WriteString(b.red.Redact(string(line)))
			}
		}
		data = out.Bytes()
	default:
		data = []byte(b.red.Redact(string(data)))
	}
	b.files = append(b.files, bundleFile{name, data})
	b.report.Files = append(b.report.Files, name)
}

// json adds report as `--json` prints it.
func (b *bundle) json(name string, report any) {
	var buf bytes.Buffer
	if err := events.WriteReport(&buf, report); err != nil {
		b.problem("%s: %v", name, err)
		return
	}
	b.add(name, buf.Bytes(), true)
}

func (b *bundle) stackFile(st *stack.Stack, rel, name string, isJSON bool, scrub func([]byte) []byte) {
	data, err := st.ReadFile(rel)
	if err != nil {
		b.problem("%s: %v", rel, err)
		return
	}
	if scrub != nil {
		data = scrub(data)
	}
	b.add(name, data, isJSON)
}

// runLogs adds the newest BundleRunLogs run logs, by their names'
// timestamps.
func (b *bundle) runLogs(st *stack.Stack) {
	entries, err := fs.ReadDir(st.FS(), log.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		b.problem("run logs: %v", err)
		return
	}
	var names []string
	for _, e := range entries {
		if n := e.Name(); e.Type().IsRegular() && strings.HasPrefix(n, "cli-") && strings.HasSuffix(n, ".log") {
			names = append(names, n)
		}
	}
	// As log's retention orders them: cli-<ts>-<pid>-1 is newer than
	// cli-<ts>-<pid>.
	slices.SortFunc(names, func(a, b string) int {
		return strings.Compare(strings.TrimSuffix(a, ".log"), strings.TrimSuffix(b, ".log"))
	})
	for _, n := range names[max(0, len(names)-BundleRunLogs):] {
		// Run logs are JSON lines.
		b.stackFile(st, log.Dir+"/"+n, "logs/"+n, true, nil)
	}
}

// compose adds `compose ps` and each service's last BundleLogTail log
// lines. composeErr (opts.Doctor.ComposeErr) is why c is nil.
func (b *bundle) compose(ctx context.Context, c docker.Composer, composeErr error) {
	if c == nil {
		b.problem("compose: %v", cmp.Or[error](composeErr, errors.New("unavailable")))
		return
	}
	ps, err := c.Ps(ctx)
	if err != nil {
		b.problem("compose ps: %v", err)
		return
	}
	data, err := json.MarshalIndent(ps, "", "  ")
	if err != nil {
		b.problem("compose ps: %v", err)
		return
	}
	b.add("compose/ps.json", append(data, '\n'), true)
	if !b.secretsKnown {
		b.problem("compose logs: left out, since without a readable secrets.yaml the secrets they may quote can't be redacted")
		return
	}
	var services []string
	for _, s := range ps {
		if s.Service != "" && !slices.Contains(services, s.Service) {
			services = append(services, s.Service)
		}
	}
	slices.Sort(services)
	for i, svc := range services {
		var out, errOut bytes.Buffer
		lctx, cancel := context.WithTimeout(ctx, bundleLogsTimeout)
		err := c.Logs(lctx, docker.ComposeLogsOpts{Services: []string{svc}, Tail: BundleLogTail, Out: &out, Err: &errOut})
		timedOut := err != nil && ctx.Err() == nil && lctx.Err() != nil
		cancel()
		if timedOut {
			err = fmt.Errorf("no answer within %s", bundleLogsTimeout)
		}
		if err != nil {
			b.problem("compose logs %s: %v", svc, err)
		}
		if out.Len() > 0 || err == nil {
			b.add("compose/logs/"+svc+".log", out.Bytes(), false)
		}
		if timedOut && i+1 < len(services) {
			// A daemon that stops answering would cost the timeout per
			// service.
			b.problem("compose logs: skipped %s after %s timed out", strings.Join(services[i+1:], ", "), svc)
			break
		}
	}
}

func (b *bundle) readme() []byte {
	var s strings.Builder
	s.WriteString(`pic-sure support bundle

status.json          status --deep --json
doctor.json          doctor --json
logs/                the newest pic-sure run logs
compose/ps.json      docker compose ps
compose/logs/        each service's last ` + fmt.Sprint(BundleLogTail) + ` log lines
stack/               pic-sure.yaml, state.json and manifest.json

Every value in the stack's secrets.yaml and HPDS key file is replaced with
[REDACTED] in every file, as are the values of secret-named keys in
pic-sure.yaml and passwords in URLs. Look the files over before you share
them all the same.
`)
	if n := b.report.ShortSecrets; n > 0 {
		fmt.Fprintf(&s, `
%d secret(s) are shorter than %d bytes. They are redacted only where
they stand alone, not inside longer words, so look for them before sharing,
or rotate them to longer values.
`, n, log.MinSecret)
	}
	if len(b.report.Problems) > 0 {
		s.WriteString("\nNot collected:\n")
		for _, p := range b.report.Problems {
			fmt.Fprintf(&s, "- %s\n", p)
		}
	}
	return []byte(s.String())
}

func (b *bundle) write(w io.Writer, prefix string, now time.Time) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	dirs := map[string]bool{}
	var mkdir func(dir string) error
	mkdir = func(dir string) error {
		if dir == "." || dirs[dir] {
			return nil
		}
		if err := mkdir(path.Dir(dir)); err != nil {
			return err
		}
		dirs[dir] = true
		return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: dir + "/", Mode: 0o700, ModTime: now})
	}
	for _, f := range b.files {
		name := path.Join(prefix, f.name)
		if err := mkdir(path.Dir(name)); err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o600, Size: int64(len(f.data)), ModTime: now}); err != nil {
			return err
		}
		if _, err := tw.Write(f.data); err != nil {
			return err
		}
	}
	return cmp.Or(tw.Close(), gz.Close())
}

// bundleRedactor redacts every secret it knows. A secret of at least
// log.MinSecret bytes is replaced wherever it appears, through
// log.Redactor, which also scrubs URL userinfo. A shorter one, which an
// operator can supply (a remote root or email password), would match all
// through unrelated text, so it is replaced only where it stands alone:
// with no letter or digit on either side.
type bundleRedactor struct {
	long  log.Redactor
	short []string
}

// register adds v. log.Redactor catches v's JSON-escaped form as slog
// writes it; encoding/json's default also escapes <, > and &, so that form
// is added too.
func (r *bundleRedactor) register(v string) {
	switch {
	case v == "":
	case len(v) >= log.MinSecret:
		r.long.Register(v)
		if e := jsonEscaped(v); e != v {
			r.long.Register(e)
		}
	case !slices.Contains(r.short, v):
		r.short = append(r.short, v)
	}
}

func (r *bundleRedactor) Redact(s string) string {
	s = r.long.Redact(s)
	for _, v := range r.short {
		s = replaceStandalone(s, v)
		if e := jsonEscaped(v); e != v {
			s = replaceStandalone(s, e)
		}
	}
	return s
}

// jsonEscaped is v as encoding/json writes it inside a string.
func jsonEscaped(v string) string {
	data, _ := json.Marshal(v)
	return string(data[1 : len(data)-1])
}

// replaceStandalone replaces each occurrence of v in s that has no ASCII
// letter or digit right before or after it.
func replaceStandalone(s, v string) string {
	var out strings.Builder
	for {
		i := strings.Index(s, v)
		if i < 0 {
			out.WriteString(s)
			return out.String()
		}
		end := i + len(v)
		alone := (i == 0 || !isAlnum(s[i-1])) && (end == len(s) || !isAlnum(s[end]))
		if alone {
			out.WriteString(s[:i])
			out.WriteString(log.Redacted)
			s = s[end:]
			continue
		}
		out.WriteString(s[:i+1])
		s = s[i+1:]
	}
}

func isAlnum(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9'
}

// bundleNotSecret are secrets.yaml's keys whose values aren't secret (§6.3).
var bundleNotSecret = map[string]bool{
	"application_uuid": true, "resource_uuid": true, "visualization_uuid": true,
	"introspection_token_expiry": true, "auth0_client_secret_generated": true,
}

// bundleSecrets returns a redactor of every value in secrets.yaml and the
// HPDS key file, and of the values redactConfigKeys blanks in
// pic-sure.yaml. secrets.yaml is read as plain YAML, not as Secrets, so a
// key a newer pic-sure added is redacted too. known is false when
// secrets.yaml can't be read or parsed: containers configured from it may
// still log secrets the redactor doesn't know.
func bundleSecrets(st *stack.Stack, problem func(string, ...any)) (r *bundleRedactor, known bool) {
	r = &bundleRedactor{}
	data, err := st.ReadFile(stack.SecretsFile)
	var doc yaml.Node
	switch {
	case err != nil:
		problem("%s: %v", stack.SecretsFile, err)
	case yaml.Unmarshal(data, &doc) != nil:
		// Not yaml's message, which can quote the file. Each line that
		// parses on its own still counts.
		problem("%s: not valid YAML", stack.SecretsFile)
		for line := range strings.Lines(string(data)) {
			var n yaml.Node
			if yaml.Unmarshal([]byte(line), &n) == nil {
				registerYAMLSecrets(r, &n, "")
			}
		}
	default:
		known = true
		registerYAMLSecrets(r, &doc, "")
	}
	if cfg, err := st.ReadFile(stack.ConfigFile); err == nil {
		for _, v := range configSecretValues(cfg) {
			r.register(v)
		}
	}
	key, err := st.ReadFile(stack.HPDSKeyFile)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		problem("%s: %v", stack.HPDSKeyFile, err)
	default:
		r.register(strings.TrimSpace(string(key)))
	}
	return r, known
}

// secretsKeys are secrets.yaml's keys for stack.Secret fields.
var secretsKeys = func() map[string]bool {
	m := map[string]bool{}
	t := reflect.TypeFor[stack.Secrets]()
	for i := range t.NumField() {
		if f := t.Field(i); f.Type == reflect.TypeFor[stack.Secret]() {
			name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
			m[name] = true
		}
	}
	return m
}()

// registerYAMLSecrets registers the scalars under n, whose top-level key in
// secrets.yaml is key. A stack.Secret key's value counts whatever it looks
// like (an unquoted false is still the password "false"); under another
// secret-named key (log.IsSecretName) any scalar but a boolean does; under
// any other key only strings do, which a newer pic-sure's secrets are, and
// bundleNotSecret's values don't.
func registerYAMLSecrets(r *bundleRedactor, n *yaml.Node, key string) {
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			registerYAMLSecrets(r, c, key)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := key
			if k == "" {
				k = n.Content[i].Value
			}
			if !bundleNotSecret[k] {
				registerYAMLSecrets(r, n.Content[i+1], k)
			}
		}
	case yaml.ScalarNode:
		tag := n.ShortTag()
		if secretsKeys[key] && tag != "!!null" || tag == "!!str" ||
			log.IsSecretName(key) && tag != "!!null" && tag != "!!bool" {
			r.register(n.Value)
		}
	}
}

// redactJSONStrings applies redact to each string of the valid JSON text
// data, keys included, and leaves the rest alone, so the result is valid
// JSON. redact sees a string decoded, so an escape can't hide a secret or
// be split by a replacement; a string it changes is re-encoded.
func redactJSONStrings(data []byte, redact func(string) string) []byte {
	var out bytes.Buffer
	for {
		i := bytes.IndexByte(data, '"')
		if i < 0 {
			out.Write(data)
			return out.Bytes()
		}
		out.Write(data[:i+1])
		data = data[i+1:]
		end := 0
		for end < len(data) && data[end] != '"' {
			if data[end] == '\\' {
				end++
			}
			end++
		}
		end = min(end, len(data))
		lit := data[:end]
		var v string
		if json.Unmarshal([]byte(`"`+string(lit)+`"`), &v) != nil {
			out.WriteString(redact(string(lit)))
		} else if r := redact(v); r == v {
			out.Write(lit)
		} else {
			out.WriteString(jsonEscaped(r))
		}
		data = data[end:]
		if len(data) > 0 {
			out.WriteByte('"')
			data = data[1:]
		}
	}
}

// secretKeyLine is a YAML line holding a block-style key and its value,
// and secretFlowKey a key and its value in a flow mapping. Each has the
// text before the value, the key, and the value as groups.
var (
	secretKeyLine = regexp.MustCompile(`(?m)^(\s*(?:-\s+)?["']?([A-Za-z0-9_.-]+)["']?\s*:[ \t]+)([^\s#].*)$`)
	secretFlowKey = regexp.MustCompile(`([{,]\s*["']?([A-Za-z0-9_.-]+)["']?\s*:\s*)("(?:[^"\\\n]|\\.)*"|'(?:[^'\n]|'')*'|[^\s,{}\[\]][^,}\]\n]*)`)
)

// redactConfigKeys blanks every configSecretFields key and every other
// secret-named key (log.IsSecretName) with a scalar value in pic-sure.yaml,
// whatever its type.
// A valid config has none but the admin email, since its secrets live in
// secrets.yaml, but an operator may have pasted one in. A changed file is
// re-encoded, which normalizes its layout.
func redactConfigKeys(data []byte) []byte {
	var doc yaml.Node
	if yaml.Unmarshal(data, &doc) != nil {
		return redactSecretKeyLines(data)
	}
	changed := false
	walkSecretKeys(&doc, "", func(parent *yaml.Node, i int, _ bool) {
		parent.Content[i] = &yaml.Node{Kind: yaml.ScalarNode, Value: log.Redacted}
		changed = true
	})
	if !changed {
		return data
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return redactSecretKeyLines(data)
	}
	return out
}

// configSecretFields are the dotted keys of stack.Fields' secrets, and of
// the admin email, which the run logs redact as personal data.
var configSecretFields = func() map[string]bool {
	m := map[string]bool{}
	for _, f := range stack.Fields {
		if f.Secret || f.Flag == "admin-email" {
			m[f.Key] = true
		}
	}
	return m
}()

// configSecretLeaves are the last parts of configSecretFields' keys, for a
// pic-sure.yaml that doesn't parse, where only the key itself is known.
var configSecretLeaves = func() map[string]bool {
	m := map[string]bool{}
	for k := range configSecretFields {
		m[k[strings.LastIndex(k, ".")+1:]] = true
	}
	return m
}()

// configPlainLeaves are the last parts of the other stack.Fields' keys,
// which aren't secret even when secret-named (auth.consent_authorization).
// A wildcard field, such as an env var, has none.
var configPlainLeaves = func() map[string]bool {
	m := map[string]bool{}
	for _, f := range stack.Fields {
		if leaf := f.Key[strings.LastIndex(f.Key, ".")+1:]; !configSecretFields[f.Key] && leaf != "*" {
			m[leaf] = true
		}
	}
	return m
}()

// configPlainField reports whether the dotted key p is a stack.Fields key
// that isn't secret, such as auth.consent_authorization.
func configPlainField(p string) bool {
	f, ok := stack.LookupField(p)
	return ok && !configSecretFields[f.Key] && !strings.HasSuffix(f.Key, ".*")
}

// walkSecretKeys calls fn for every value under n, at dotted path prefix,
// that redactConfigKeys blanks, unless it is an empty scalar:
// parent.Content[i] is the value, and field says its key is one of
// configSecretFields. A secret-named key's value must be a scalar other
// than null, or a sequence, and a stack.Fields key that isn't secret is
// left alone, so consent_authorization: false stays.
func walkSecretKeys(n *yaml.Node, prefix string, fn func(parent *yaml.Node, i int, field bool)) {
	if n.Kind != yaml.MappingNode {
		for _, c := range n.Content {
			walkSecretKeys(c, prefix, fn)
		}
		return
	}
	for i := 1; i < len(n.Content); i += 2 {
		key, v := n.Content[i-1].Value, n.Content[i]
		p := key
		if prefix != "" {
			p = prefix + "." + key
		}
		empty := v.Kind == yaml.ScalarNode && v.Value == ""
		value := v.Kind == yaml.ScalarNode && v.ShortTag() != "!!null" || v.Kind == yaml.SequenceNode
		if !empty && (configSecretFields[p] || log.IsSecretName(key) && value && !configPlainField(p)) {
			fn(n, i, configSecretFields[p])
			continue
		}
		walkSecretKeys(v, p, fn)
	}
}

// configSecretValues returns the values redactConfigKeys blanks, so they
// are redacted wherever else they appear. A boolean under a key that is
// only secret-named is blanked but not returned: redacting every "true" and
// "false" would wreck the bundle. A secret field's value counts whatever it
// looks like, as in secrets.yaml.
func configSecretValues(data []byte) []string {
	var values []string
	var doc yaml.Node
	if yaml.Unmarshal(data, &doc) != nil {
		for _, re := range []*regexp.Regexp{secretKeyLine, secretFlowKey} {
			for _, m := range re.FindAllSubmatch(data, -1) {
				if v, register, ok := secretLineValue(m); ok && register {
					values = append(values, v)
				}
			}
		}
		return values
	}
	walkSecretKeys(&doc, "", func(parent *yaml.Node, i int, field bool) {
		var collect func(n *yaml.Node)
		collect = func(n *yaml.Node) {
			if n.Kind == yaml.ScalarNode && (field || n.ShortTag() != "!!bool") {
				values = append(values, n.Value)
			}
			for _, c := range n.Content {
				collect(c)
			}
		}
		collect(parent.Content[i])
	})
	return values
}

// redactSecretKeyLines is redactConfigKeys for a file that isn't valid
// YAML, by key name alone.
func redactSecretKeyLines(data []byte) []byte {
	for _, re := range []*regexp.Regexp{secretKeyLine, secretFlowKey} {
		data = re.ReplaceAllFunc(data, func(match []byte) []byte {
			m := re.FindSubmatch(match)
			if _, _, ok := secretLineValue(m); !ok {
				return match
			}
			return append(m[1][:len(m[1]):len(m[1])], log.Redacted...)
		})
	}
	return data
}

// secretLineValue returns the value of a secretKeyLine or secretFlowKey
// match whose key is the last part of a configSecretFields key, or is
// secret-named and not configPlainLeaves', and whose value is a scalar other
// than null or "", whatever its type. register is configSecretValues' rule:
// false for a boolean under a key that is only secret-named.
func secretLineValue(m [][]byte) (value string, register, ok bool) {
	k := string(m[2])
	field := configSecretLeaves[k]
	if !field && (!log.IsSecretName(k) || configPlainLeaves[k]) {
		return "", false, false
	}
	var doc yaml.Node
	if yaml.Unmarshal(m[3], &doc) != nil {
		v := strings.Trim(strings.TrimSpace(string(m[3])), `"'`)
		return v, true, true
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.ScalarNode {
		return "", false, false
	}
	// The node's value leaves out a trailing comment, which m[3] holds.
	n := doc.Content[0]
	if n.ShortTag() == "!!null" || n.Value == "" {
		return "", false, false
	}
	return n.Value, field || n.ShortTag() != "!!bool", true
}
