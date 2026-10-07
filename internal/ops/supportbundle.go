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
	// bundleLogsTimeout bounds each service's `compose logs`.
	bundleLogsTimeout = 30 * time.Second
)

// SupportBundleOptions configures SupportBundle.
type SupportBundleOptions struct {
	// Stack is the stack to collect from, or nil for the host checks
	// alone (doctor without a stack).
	Stack *stack.Stack
	// Status is passed to Status, which runs only with a Stack.
	Status StatusOptions
	// Doctor is passed to Doctor.
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
// problem in the report and README.txt. Only failing to write w is an
// error.
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
		b.compose(ctx, d.Compose)
		b.stackFile(st, stack.ConfigFile, "stack/pic-sure.yaml", redactConfigKeys)
		b.stackFile(st, stack.StateFile, "stack/state.json", nil)
		b.stackFile(st, stack.ManifestFile, "stack/manifest.json", nil)
	}
	// A compose error can quote a secret.
	for i, p := range b.report.Problems {
		b.report.Problems[i] = b.red.Redact(p)
	}
	b.add("README.txt", b.readme())

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

// add redacts data and adds it as name.
func (b *bundle) add(name string, data []byte) {
	b.files = append(b.files, bundleFile{name, []byte(b.red.Redact(string(data)))})
	b.report.Files = append(b.report.Files, name)
}

// json adds report as `--json` prints it.
func (b *bundle) json(name string, report any) {
	var buf bytes.Buffer
	if err := events.WriteReport(&buf, report); err != nil {
		b.problem("%s: %v", name, err)
		return
	}
	b.add(name, buf.Bytes())
}

func (b *bundle) stackFile(st *stack.Stack, rel, name string, scrub func([]byte) []byte) {
	data, err := st.ReadFile(rel)
	if err != nil {
		b.problem("%s: %v", rel, err)
		return
	}
	if scrub != nil {
		data = scrub(data)
	}
	b.add(name, data)
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
		b.stackFile(st, log.Dir+"/"+n, "logs/"+n, nil)
	}
}

// compose adds `compose ps` and each service's last BundleLogTail log
// lines.
func (b *bundle) compose(ctx context.Context, c docker.Composer) {
	if c == nil {
		b.problem("compose: the stack has not been rendered, or compose is unavailable")
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
	b.add("compose/ps.json", append(data, '\n'))
	if !b.secretsKnown {
		b.problem("compose logs: left out, since without secrets.yaml the secrets they may quote can't be redacted")
		return
	}
	var services []string
	for _, s := range ps {
		if s.Service != "" && !slices.Contains(services, s.Service) {
			services = append(services, s.Service)
		}
	}
	slices.Sort(services)
	for _, svc := range services {
		var out, errOut bytes.Buffer
		lctx, cancel := context.WithTimeout(ctx, bundleLogsTimeout)
		err := c.Logs(lctx, docker.ComposeLogsOpts{Services: []string{svc}, Tail: BundleLogTail, Out: &out, Err: &errOut})
		cancel()
		if err != nil {
			b.problem("compose logs %s: %v", svc, err)
		}
		if out.Len() > 0 || err == nil {
			b.add("compose/logs/"+svc+".log", out.Bytes())
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
%d secret(s) are shorter than %d characters. They are redacted only where
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
// HPDS key file, and of every secret-named key's value in pic-sure.yaml.
// secrets.yaml is read as plain YAML, not as Secrets, so a key a newer
// pic-sure added is redacted too; one that isn't valid YAML has every
// line's value taken as a secret. known is false when secrets.yaml can't
// be read: containers an earlier secrets.yaml configured may still log
// secrets the redactor doesn't know.
func bundleSecrets(st *stack.Stack, problem func(string, ...any)) (r *bundleRedactor, known bool) {
	r = &bundleRedactor{}
	data, err := st.ReadFile(stack.SecretsFile)
	known = err == nil
	switch {
	case err != nil:
		problem("%s: %v", stack.SecretsFile, err)
	default:
		var doc yaml.Node
		if yaml.Unmarshal(data, &doc) == nil {
			registerYAMLSecrets(r, &doc, "")
		} else {
			problem("%s: not valid YAML; every line's value is redacted", stack.SecretsFile)
			for line := range strings.Lines(string(data)) {
				var m map[string]string
				if yaml.Unmarshal([]byte(line), &m) == nil {
					for _, v := range m {
						r.register(v)
					}
				} else if _, v, ok := strings.Cut(line, ":"); ok {
					r.register(strings.Trim(strings.TrimSpace(v), `"'`))
				}
			}
		}
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

// registerYAMLSecrets registers every scalar under n except the values of
// bundleNotSecret keys at the top level, booleans and nulls.
func registerYAMLSecrets(r *bundleRedactor, n *yaml.Node, key string) {
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			registerYAMLSecrets(r, c, key)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i].Value
			if key == "" && bundleNotSecret[k] {
				continue
			}
			registerYAMLSecrets(r, n.Content[i+1], k)
		}
	case yaml.ScalarNode:
		if n.Tag != "!!bool" && n.Tag != "!!null" {
			r.register(n.Value)
		}
	}
}

// secretKeyLine is a YAML line holding a block-style key and its value.
var secretKeyLine = regexp.MustCompile(`(?m)^(\s*(?:-\s+)?["']?([A-Za-z0-9_.-]+)["']?\s*:[ \t]+)([^\s#].*)$`)

// redactConfigKeys blanks the value of every secret-named key
// (log.IsSecretName) in pic-sure.yaml. A valid config has none, since its
// secrets live in secrets.yaml, but an operator may have pasted one in.
// A flow-style mapping is parsed and re-encoded, which drops its comments.
func redactConfigKeys(data []byte) []byte {
	var doc yaml.Node
	if yaml.Unmarshal(data, &doc) != nil {
		return redactSecretKeyLines(data)
	}
	if !blankSecretKeys(&doc) {
		return data
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return redactSecretKeyLines(data)
	}
	return out
}

// blankSecretKeys replaces the value of every secret-named key under n,
// and reports whether it changed anything.
func blankSecretKeys(n *yaml.Node) bool {
	changed := false
	walkSecretKeys(n, func(parent *yaml.Node, i int) {
		parent.Content[i] = &yaml.Node{Kind: yaml.ScalarNode, Value: log.Redacted}
		changed = true
	})
	return changed
}

// walkSecretKeys calls fn for the value of every secret-named key under n
// that isn't an empty scalar: parent.Content[i] is the value.
func walkSecretKeys(n *yaml.Node, fn func(parent *yaml.Node, i int)) {
	if n.Kind == yaml.MappingNode {
		for i := 1; i < len(n.Content); i += 2 {
			if v := n.Content[i]; log.IsSecretName(n.Content[i-1].Value) && (v.Kind != yaml.ScalarNode || v.Value != "") {
				fn(n, i)
			}
		}
	}
	for _, c := range n.Content {
		walkSecretKeys(c, fn)
	}
}

// configSecretValues returns the values redactConfigKeys blanks, so they
// are redacted wherever else they appear.
func configSecretValues(data []byte) []string {
	var values []string
	var doc yaml.Node
	if yaml.Unmarshal(data, &doc) != nil {
		for _, m := range secretKeyLine.FindAllSubmatch(data, -1) {
			if log.IsSecretName(string(m[2])) {
				values = append(values, strings.Trim(strings.TrimSpace(string(m[3])), `"'`))
			}
		}
		return values
	}
	walkSecretKeys(&doc, func(parent *yaml.Node, i int) {
		var collect func(n *yaml.Node)
		collect = func(n *yaml.Node) {
			if n.Kind == yaml.ScalarNode {
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

// redactSecretKeyLines is blankSecretKeys for a file that isn't valid
// YAML: line by line, block style only.
func redactSecretKeyLines(data []byte) []byte {
	return secretKeyLine.ReplaceAllFunc(data, func(line []byte) []byte {
		m := secretKeyLine.FindSubmatch(line)
		if !log.IsSecretName(string(m[2])) {
			return line
		}
		return append(m[1][:len(m[1]):len(m[1])], log.Redacted...)
	})
}
