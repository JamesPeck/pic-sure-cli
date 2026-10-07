package ops_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/log"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

// bundleSecrets holds one value of every secret kind in secrets.yaml:
// generated ones, the operator's (the remote root password, with
// characters JSON escapes, and a short email password), open mode's
// generated Auth0 secret, and a key a newer pic-sure might add.
var bundleSecretValues = map[string]string{
	"db_root_password":             "LocalRoot0aaaaaaaaaaaaaa",
	"db_remote_root_password":      `Rem"ote\root<pw>&`,
	"db_picsure_password":          "Picsure0bbbbbbbbbbbbbbbb",
	"db_auth_password":             "AuthDb0cccccccccccccccccc",
	"db_airflow_password":          "Airflow0dddddddddddddddd",
	"dictionary_db_password":       "Dictionary0eeeeeeeeeeeee",
	"auth0_client_secret":          "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	"query_service_internal_token": "1111aaaa2222bbbb3333cccc4444dddd5555eeee6666ffff7777aaaa8888bbbb",
	"picsure_application_token":    "2222aaaa2222bbbb3333cccc4444dddd5555eeee6666ffff7777aaaa8888bbbb",
	"logging_api_key":              "3333aaaa2222bbbb3333cccc4444dddd5555eeee6666ffff7777aaaa8888bbbb",
	"aggregate_obfuscation_salt":   "4444aaaa2222bbbb3333cccc4444dddd",
	"introspection_token":          "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJQU0FNQSJ9.c2lnbmF0dXJlMDEyMzQ1Njc4OQ",
	"email_password":               "k9",
	"future_secret":                "Future0ffffffffffffffff",
}

const (
	bundleHPDSKey   = "0123456789abcdef0123456789abcdef"
	bundleProxyPass = "Proxy0Pass0gggg"
	bundlePasted    = "Pasted0Secret0hhhh"
)

const bundleConfig = `schema: 1
name: demo
auth:
  mode: open
  admin_email: admin@example.com
  auth0:
    client_secret: ` + bundlePasted + `   # pasted here by mistake
proxy: {http: "http://user:` + bundleProxyPass + `@proxy.example.com:3128"}
`

// newBundleStack is a stack with every secret planted in secrets.yaml,
// the HPDS key file, run logs and state.json.
func newBundleStack(t *testing.T) *stack.Stack {
	t.Helper()
	st := newStatusStack(t, bundleConfig)
	saveStatusState(t, st)
	var sec strings.Builder
	sec.WriteString("auth0_client_secret_generated: true\napplication_uuid: 7f2c0e8a-1111-4222-8333-944445555666\n")
	for k, v := range bundleSecretValues {
		data, _ := json.Marshal(v)
		fmt.Fprintf(&sec, "%s: %s\n", k, data)
	}
	writeStackFile(t, st, stack.SecretsFile, sec.String())
	writeStackFile(t, st, stack.HPDSKeyFile, bundleHPDSKey+"\n")

	state, err := st.ReadFile(stack.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	writeStackFile(t, st, stack.StateFile, strings.Replace(string(state), "james_mono", "james_mono "+bundleSecretValues["db_root_password"], 1))

	// Seven run logs, each quoting every secret; the oldest two stay out.
	for i := range 7 {
		writeStackFile(t, st, fmt.Sprintf("%s/cli-20261007T10000%d.000Z-42.log", log.Dir, i), leakyText())
	}
	writeStackFile(t, st, log.Dir+"/cli-20261007T100006.000Z-42-1.log", leakyText())
	return st
}

func writeStackFile(t *testing.T, st *stack.Stack, rel, data string) {
	t.Helper()
	if err := st.MkdirAll(rel[:strings.LastIndex(rel, "/")], 0o700); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteFile(rel, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// leakyText quotes every secret raw, JSON-escaped and in URLs, as a
// container or an old log might.
func leakyText() string {
	var b strings.Builder
	for k, v := range bundleSecretValues {
		js, _ := json.Marshal(v)
		fmt.Fprintf(&b, "%s=%s\n{\"msg\":\"login\",\"%s\":%s}\njdbc:mysql://root:%s@picsure-db:3306/picsure\n", strings.ToUpper(k), v, k, js, v)
	}
	fmt.Fprintf(&b, "HPDS key %s loaded\nproxy http://user:%s@proxy.example.com:3128\nconfig client_secret=%s\n", bundleHPDSKey, bundleProxyPass, bundlePasted)
	return b.String()
}

func bundleRunner(t *testing.T) *fakerunner.Runner {
	t.Helper()
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker compose * ps --all --format json")).Stdout(
		`{"Name":"demo-gateway-1","Service":"gateway","State":"exited","ExitCode":1}` + "\n" +
			`{"Name":"demo-picsure-db-1","Service":"picsure-db","State":"exited","ExitCode":0}` + "\n" +
			`{"Name":"demo-hpds-1","Service":"hpds","State":"exited","ExitCode":137}` + "\n")
	f.On(fakerunner.Glob("docker compose * logs --tail 500 gateway")).Stdout(leakyText())
	f.On(fakerunner.Glob("docker compose * logs --tail 500 picsure-db")).Stdout(leakyText())
	f.On(fakerunner.Glob("docker compose * logs --tail 500 hpds")).Exit(1).
		Stderr("Error: password " + bundleSecretValues["db_auth_password"] + " rejected\n")
	f.On(fakerunner.Glob("docker *")).Exit(1).Stderr("no daemon\n")
	return f
}

func buildBundle(t *testing.T, st *stack.Stack, f *fakerunner.Runner) (*ops.SupportBundleReport, map[string]string) {
	t.Helper()
	d := statusDeps(t, f, st)
	var buf bytes.Buffer
	opts := ops.SupportBundleOptions{
		Stack:  st,
		Status: statusOpts(),
		Doctor: ops.DoctorOptions{Host: &fakeHost{diskFree: 100 * gib}, Stack: st},
		Prefix: "bundle",
	}
	opts.Status.Deep = true
	if st == nil {
		opts.Doctor.Stack = nil
	}
	r, err := ops.SupportBundle(context.Background(), d, &buf, opts)
	if err != nil {
		t.Fatal(err)
	}
	return r, untarBundle(t, &buf)
}

func untarBundle(t *testing.T, r io.Reader) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(r)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	files := map[string]string{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		if h.Mode != 0o600 {
			t.Errorf("%s: mode %o", h.Name, h.Mode)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		files[strings.TrimPrefix(h.Name, "bundle/")] = string(data)
	}
}

// assertNoSecrets fails for any secret, in any form leakyText uses, in any
// file. A short secret must not stand alone.
func assertNoSecrets(t *testing.T, files map[string]string) {
	t.Helper()
	values := []string{bundleHPDSKey, bundleProxyPass, bundlePasted}
	for _, v := range bundleSecretValues {
		values = append(values, v)
		js, _ := json.Marshal(v)
		values = append(values, string(js[1:len(js)-1]))
	}
	for name, data := range files {
		for _, v := range values {
			if len(v) < log.MinSecret {
				if regexp.MustCompile(`(^|[^A-Za-z0-9])` + regexp.QuoteMeta(v) + `($|[^A-Za-z0-9])`).MatchString(data) {
					t.Errorf("%s holds the short secret %q standing alone", name, v)
				}
				continue
			}
			if strings.Contains(data, v) {
				t.Errorf("%s holds the secret %q", name, v)
			}
		}
	}
}

func TestSupportBundleRedactsEverySecret(t *testing.T) {
	st := newBundleStack(t)
	r, files := buildBundle(t, st, bundleRunner(t))

	assertNoSecrets(t, files)
	want := []string{
		"README.txt", "compose/logs/gateway.log", "compose/logs/picsure-db.log", "compose/ps.json", "doctor.json",
		"logs/cli-20261007T100003.000Z-42.log", "logs/cli-20261007T100004.000Z-42.log", "logs/cli-20261007T100005.000Z-42.log",
		"logs/cli-20261007T100006.000Z-42-1.log", "logs/cli-20261007T100006.000Z-42.log",
		"stack/manifest.json", "stack/pic-sure.yaml", "stack/state.json", "status.json",
	}
	got := slices.Sorted(func(yield func(string) bool) {
		for n := range files {
			if !yield(n) {
				return
			}
		}
	})
	if !slices.Equal(got, want) {
		t.Errorf("files\n got %q\nwant %q", got, want)
	}
	if !slices.Equal(slices.Sorted(slices.Values(r.Files)), want) {
		t.Errorf("report files %q", r.Files)
	}
	if r.ShortSecrets != 1 {
		t.Errorf("short secrets %d, want 1", r.ShortSecrets)
	}

	// Redaction leaves the rest readable.
	for _, s := range []string{
		"EMAIL_PASSWORD=[REDACTED]\n", "jdbc:mysql://[REDACTED]@picsure-db", "HPDS key [REDACTED] loaded",
		"http://[REDACTED]@proxy.example.com", "config client_secret=[REDACTED]",
	} {
		if !strings.Contains(files["compose/logs/gateway.log"], s) {
			t.Errorf("gateway.log lacks %q", s)
		}
	}
	if !strings.Contains(files["stack/pic-sure.yaml"], "client_secret: '[REDACTED]'") || !strings.Contains(files["stack/pic-sure.yaml"], "admin_email: admin@example.com") {
		t.Errorf("pic-sure.yaml:\n%s", files["stack/pic-sure.yaml"])
	}
	if !strings.Contains(files["status.json"], `"schema_version":2`) || !strings.Contains(files["status.json"], `"deep":`) {
		t.Errorf("status.json: %s", files["status.json"])
	}
	if !strings.Contains(files["compose/ps.json"], `"Service": "hpds"`) {
		t.Errorf("ps.json: %s", files["compose/ps.json"])
	}

	// hpds's logs failed; the problem says so, redacted.
	if len(r.Problems) != 1 || !strings.Contains(r.Problems[0], "compose logs hpds") || !strings.Contains(r.Problems[0], "password [REDACTED] rejected") {
		t.Errorf("problems %q", r.Problems)
	}
	readme := files["README.txt"]
	for _, s := range []string{"compose logs hpds", "1 secret(s) are shorter than 4"} {
		if !strings.Contains(readme, s) {
			t.Errorf("README lacks %q:\n%s", s, readme)
		}
	}
}

func TestSupportBundleWithInvalidSecretsYAML(t *testing.T) {
	st := newBundleStack(t)
	data, err := st.ReadFile(stack.SecretsFile)
	if err != nil {
		t.Fatal(err)
	}
	writeStackFile(t, st, stack.SecretsFile, string(data)+"broken: [\n")
	r, files := buildBundle(t, st, bundleRunner(t))
	assertNoSecrets(t, files)
	if !strings.Contains(strings.Join(r.Problems, "\n"), "not valid YAML") {
		t.Errorf("problems %q", r.Problems)
	}
}

func TestSupportBundleLeavesOutComposeLogsWithoutTheSecrets(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000 file")
	}
	st := newBundleStack(t)
	if err := os.Chmod(st.Path(stack.SecretsFile), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(st.Path(stack.SecretsFile), 0o600) })
	f := bundleRunner(t)
	r, files := buildBundle(t, st, f)
	f.AssertNotCalled(fakerunner.Glob("docker compose * logs *"))
	for n := range files {
		if strings.HasPrefix(n, "compose/logs/") {
			t.Errorf("bundle has %s", n)
		}
	}
	if !strings.Contains(strings.Join(r.Problems, "\n"), "compose logs: left out") {
		t.Errorf("problems %q", r.Problems)
	}
}

func TestSupportBundleWithoutAStack(t *testing.T) {
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker *")).Exit(1).Stderr("no daemon\n")
	d := &ops.Deps{Runner: f, Docker: docker.NewEngine(f), Clock: ops.FixedClock(statusNow), Sink: events.Discard}
	var buf bytes.Buffer
	r, err := ops.SupportBundle(context.Background(), d, &buf, ops.SupportBundleOptions{Doctor: ops.DoctorOptions{Host: &fakeHost{diskFree: 100 * gib}}, Prefix: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.Files, []string{"doctor.json", "README.txt"}) {
		t.Errorf("files %q", r.Files)
	}
	if !strings.Contains(strings.Join(r.Problems, "\n"), "no stack") {
		t.Errorf("problems %q", r.Problems)
	}
}

func TestSupportBundleWriteFailure(t *testing.T) {
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker *")).Exit(1)
	d := &ops.Deps{Runner: f, Docker: docker.NewEngine(f), Clock: ops.FixedClock(statusNow), Sink: events.Discard}
	_, err := ops.SupportBundle(context.Background(), d, failingWriter{}, ops.SupportBundleOptions{Doctor: ops.DoctorOptions{Host: &fakeHost{}}})
	if err == nil {
		t.Fatal("no error")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }
