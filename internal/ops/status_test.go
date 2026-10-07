package ops_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

var statusNow = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

const statusConfig = `schema: 1
name: demo
network: {https_port: 8443}
auth: {admin_email: admin@example.com, auth0: {client_id: abc}}
db:
  mode: remote
  remote: {host: db.example.com}
`

// newStatusStack makes a stack in the shape init and up leave it: config,
// state, secrets and a rendered compose file.
func newStatusStack(t *testing.T, config string) *stack.Stack {
	t.Helper()
	st, err := stack.Create(filepath.Join(t.TempDir(), "demo"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.WriteFile(stack.ConfigFile, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	return st
}

func saveStatusState(t *testing.T, st *stack.Stack) {
	t.Helper()
	state := &stack.State{
		CLIVersion:    "2.0.0",
		SchemaVersion: 1,
		Release:       stack.Release{Repo: "https://github.com/hms-dbmi/pic-sure-release-control", Branch: "james_mono", Commit: "49be6ec0123456789abcdef0123456789abcdef0"},
		Components: map[string]stack.Component{
			"pic-sure": {Ref: "v3.1.0", Commit: "1111111111111111111111111111111111111111"},
			"frontend": {Ref: "main", Commit: "2222222222222222222222222222222222222222"},
		},
		Images: map[string]string{"pic-sure-gateway": "111111111111", "pic-sure-hpds": "111111111111"},
	}
	state.StartOperation("up", statusNow.Add(-time.Hour))
	state.FinishOperation(nil, statusNow.Add(-50*time.Minute))
	if err := st.SaveState(state); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSecrets(&stack.Secrets{IntrospectionTokenExpiry: statusNow.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := st.MkdirAll(".pic-sure/render", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteFile(".pic-sure/render/compose.yaml", []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func statusDeps(t *testing.T, f *fakerunner.Runner, st *stack.Stack) *ops.Deps {
	t.Helper()
	d := &ops.Deps{Runner: f, Docker: docker.NewEngine(f), Clock: ops.FixedClock(statusNow), Sink: events.Discard}
	if c, err := docker.NewCompose(f, st.Dir, nil); err == nil {
		d.Compose = c
	}
	return d
}

func statusOpts() ops.StatusOptions {
	return ops.StatusOptions{CLIVersion: "2.0.0", Migrations: stack.ConfigMigrations()}
}

// statusJSON is the report as status --json prints it, with the temporary
// stack directory replaced.
func statusJSON(t *testing.T, r *ops.StatusReport) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := events.WriteReport(&buf, r); err != nil {
		t.Fatal(err)
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, buf.Bytes(), "", "  "); err != nil {
		t.Fatal(err)
	}
	return bytes.ReplaceAll(indented.Bytes(), []byte(r.Stack.Dir), []byte("STACKDIR"))
}

func TestStatusReportsAFakeStack(t *testing.T) {
	st := newStatusStack(t, statusConfig)
	saveStatusState(t, st)
	f := fakerunner.New(t)
	f.On(fakerunner.Exact("docker", "image", "inspect", "hms-dbmi/pic-sure-gateway:111111111111")).Stdout(`[{"Id":"sha256:1"}]`)
	f.On(fakerunner.Exact("docker", "image", "inspect", "hms-dbmi/pic-sure-hpds:111111111111")).
		Exit(1).Stderr("Error response from daemon: No such image: hms-dbmi/pic-sure-hpds:111111111111\n")
	f.On(fakerunner.Glob("docker compose * ps --all --format json")).Stdout(
		`{"Name":"demo-psama-1","Service":"psama","State":"running","Status":"Up 2 minutes (healthy)","Health":"healthy","Future":1}` + "\n" +
			`{"Name":"demo-gateway-1","Service":"gateway","State":"exited","Status":"Exited (1)","ExitCode":1}` + "\n")

	r := ops.Status(context.Background(), statusDeps(t, f, st), st, statusOpts())
	golden(t, "status.golden.json", statusJSON(t, r))
}

func TestStatusWithNothingRecordedOrRunning(t *testing.T) {
	st := newStatusStack(t, "schema: 1\nname: demo\nauth: {mode: open, admin_email: admin@example.com}\n")
	f := fakerunner.New(t)

	r := ops.Status(context.Background(), statusDeps(t, f, st), st, statusOpts())
	if !r.Config.Valid || r.Versions.Gate != ops.GateOK || r.StateError != "" {
		t.Errorf("config %+v, versions %+v, state error %q", r.Config, r.Versions, r.StateError)
	}
	for _, img := range r.Images {
		if img.Ref != "" || img.Present != nil {
			t.Errorf("image %+v, want no ref and present unknown", img)
		}
	}
	if len(f.Calls()) != 0 {
		t.Errorf("docker was called: %v", f.Calls())
	}
	if len(r.Services) != 0 || !strings.Contains(r.ServicesError, "run pic-sure up") {
		t.Errorf("services %v, error %q", r.Services, r.ServicesError)
	}
	if r.Token.ExpiresAt != nil || r.LastOperation != nil {
		t.Errorf("token %+v, last operation %+v", r.Token, r.LastOperation)
	}
	want := ops.StatusAuth0{Needed: false, CallbackURL: "https://localhost/login/loading/", LogoutURL: "https://localhost", WebOrigin: "https://localhost"}
	if r.Auth0 == nil || *r.Auth0 != want {
		t.Errorf("auth0 = %+v, want %+v", r.Auth0, want)
	}
	if r.DB == nil || *r.DB != (ops.StatusDB{Mode: "local"}) {
		t.Errorf("db = %+v", r.DB)
	}
}

func TestStatusStopsCheckingImagesWhenDockerFails(t *testing.T) {
	st := newStatusStack(t, statusConfig)
	saveStatusState(t, st)
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker image inspect *")).Err(errors.New("docker: executable file not found"))
	f.On(fakerunner.Glob("docker compose *")).Exit(1).Stderr("Cannot connect to the Docker daemon\n")

	r := ops.Status(context.Background(), statusDeps(t, f, st), st, statusOpts())
	if r.ImagesError == "" || !strings.Contains(r.ServicesError, "Cannot connect") {
		t.Errorf("images error %q, services error %q", r.ImagesError, r.ServicesError)
	}
	if n := len(f.CallsMatching(fakerunner.Glob("docker image inspect *"))); n != 1 {
		t.Errorf("%d image inspects, want 1 before giving up", n)
	}
	for _, img := range r.Images {
		if img.Present != nil {
			t.Errorf("image %+v, want present unknown", img)
		}
	}
	if len(r.Services) != 0 {
		t.Errorf("services = %v", r.Services)
	}
}

func TestStatusReportsAnInvalidConfig(t *testing.T) {
	st := newStatusStack(t, "schema: 1\nname: demo\nnetwork: {http_port: 99999}\nbogus: 1\n")
	r := ops.Status(context.Background(), statusDeps(t, fakerunner.New(t), st), st, statusOpts())
	if r.Config.Valid || len(r.Config.Problems) == 0 || r.Config.Error != "" {
		t.Fatalf("config = %+v", r.Config)
	}
	if r.Stack.Name != "demo" || r.DB != nil || r.Auth0 != nil {
		t.Errorf("name %q, db %+v, auth0 %+v", r.Stack.Name, r.DB, r.Auth0)
	}
}

func TestStatusReportsAConfigSchemaItCantRead(t *testing.T) {
	st := newStatusStack(t, "schema: 9\nname: demo\n")
	r := ops.Status(context.Background(), statusDeps(t, fakerunner.New(t), st), st, statusOpts())
	if r.Config.Valid || r.Config.Error == "" {
		t.Errorf("config = %+v", r.Config)
	}
	if r.Versions.Gate != ops.GateStackNewer || r.Versions.ConfigSchema != 9 {
		t.Errorf("versions = %+v", r.Versions)
	}
}

func TestStatusReportsUnreadableStateAndSecrets(t *testing.T) {
	st := newStatusStack(t, statusConfig)
	for _, f := range []string{stack.StateFile, stack.SecretsFile} {
		if err := st.WriteFile(f, []byte("{not: [valid"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r := ops.Status(context.Background(), statusDeps(t, fakerunner.New(t), st), st, statusOpts())
	if r.StateError == "" || r.Versions.Error == "" || r.Versions.Gate != ops.GateUnknown || r.Token.Error == "" {
		t.Errorf("state error %q, versions %+v, token %+v", r.StateError, r.Versions, r.Token)
	}
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs (go test -update rewrites it):\n%s", path, got)
	}
}

// TestStatusSchemaIsDocumented keeps docs/json-schemas.md in step with
// StatusReport: every JSON field is documented, and nothing else is.
func TestStatusSchemaIsDocumented(t *testing.T) {
	data, err := os.ReadFile("../../docs/json-schemas.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, _ := strings.Cut(string(data), "## `status --json`")
	section, _, _ = strings.Cut(section, "\n## ")
	// documented maps each field to whether its type column says it can
	// be omitted.
	documented := map[string]bool{}
	for _, line := range strings.Split(section, "\n") {
		if rest, ok := strings.CutPrefix(line, "| `"); ok {
			field, rest, _ := strings.Cut(rest, "`")
			cols := strings.Split(rest, "|")
			documented[field] = len(cols) > 1 && strings.Contains(cols[1], "omitted")
		}
	}
	actual := map[string]bool{"schema_version": false}
	jsonFields(reflect.TypeFor[ops.StatusReport](), "", actual)
	for f, omitted := range actual {
		doc, ok := documented[f]
		switch {
		case !ok:
			t.Errorf("%s is not documented in docs/json-schemas.md", f)
		case doc != omitted:
			t.Errorf("%s: omitted when empty is %v, but docs/json-schemas.md says %v", f, omitted, doc)
		}
	}
	for f := range documented {
		if _, ok := actual[f]; !ok {
			t.Errorf("docs/json-schemas.md documents %s, which status doesn't print", f)
		}
	}
}

// jsonFields adds the JSON paths of t's fields to into, with [] for arrays,
// each mapped to whether it is omitted when empty.
func jsonFields(t reflect.Type, prefix string, into map[string]bool) {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice {
		if t.Kind() == reflect.Slice {
			prefix += "[]"
		}
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || t == reflect.TypeFor[time.Time]() {
		return
	}
	for i := range t.NumField() {
		f := t.Field(i)
		name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			panic(t.String() + "." + f.Name + " has no json name")
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		// A plain object is only a group of fields; arrays and nullable
		// objects are documented themselves.
		if f.Type.Kind() != reflect.Struct || f.Type == reflect.TypeFor[time.Time]() {
			into[path] = strings.Contains(opts, "omit")
		}
		jsonFields(f.Type, path, into)
	}
}
