package ops

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

const dictPassword = "Dict-synthetic-password-0123456789"

type dictFixture struct {
	t     *testing.T
	f     *fakerunner.Runner
	d     *Deps
	rec   *events.Recorder
	st    *stack.Stack
	cfg   *stack.Config
	sec   *stack.Secrets
	state *stack.State
	// curl answers each curl call in turn; the last answer repeats.
	curl []docker.Result
	// concepts is the dictionary's concept count after a hydrate.
	concepts string
}

func dictPs(service, state, health string) string {
	return fmt.Sprintf(`{"ID":"id-%s","Service":%q,"State":%q,"Health":%q}`+"\n", service, service, state, health)
}

func newDictFixture(t *testing.T) *dictFixture {
	t.Helper()
	ready, poll := dictionaryETLReady, dictionaryETLPoll
	dictionaryETLReady, dictionaryETLPoll = 200*time.Millisecond, time.Millisecond
	t.Cleanup(func() { dictionaryETLReady, dictionaryETLPoll = ready, poll })

	st, err := stack.Create(filepath.Join(t.TempDir(), "demo"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	state := &stack.State{
		Images:     map[string]string{"dictionary-etl": "c97a813b95a1", "pic-sure-hpds-etl": "658e340e3cfc", "dictionary-weights": "658e340e3cfc"},
		Components: map[string]stack.Component{"pic-sure": {Commit: strings.Repeat("a", 40)}},
	}
	if err := st.SaveState(state); err != nil {
		t.Fatal(err)
	}
	cfg := stack.DefaultConfig()
	cfg.Name = "demo"
	x := &dictFixture{t: t, f: fakerunner.New(t), rec: &events.Recorder{}, st: st, cfg: &cfg, state: state,
		sec:  &stack.Secrets{DictionaryDBPassword: dictPassword},
		curl: []docker.Result{{Stdout: []byte("Success")}}, concepts: "12"}
	x.d = &Deps{Runner: x.f, Docker: docker.NewEngine(x.f), Rand: rand.Reader, Clock: FixedClock(time.Unix(0, 0)), Sink: x.rec,
		Compose: &docker.Compose{Runner: x.f, Files: []string{"/stack/.pic-sure/render/compose.yaml"}, ProjectDir: "/stack",
			Env: func() []string { return []string{"DB_DICTIONARY_PASSWORD=" + dictPassword} }}}
	return x
}

// stackUp fakes a running stack: dictionary-db healthy, dictionary-api
// running, the images present and the data volume there.
func (x *dictFixture) stackUp() {
	x.f.On(fakerunner.Glob("docker compose * ps --all --format json dictionary-db")).Stdout(dictPs("dictionary-db", "running", "healthy"))
	x.f.On(fakerunner.Glob("docker compose * ps --all --format json dictionary-api")).Stdout(dictPs("dictionary-api", "running", "healthy"))
	x.f.On(fakerunner.Glob("docker image inspect *")).Stdout(`[{"Id":"sha256:1","Config":{"Labels":null}}]`)
	x.f.On(fakerunner.Glob("docker volume inspect *")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		out, err := json.Marshal([]map[string]any{{"Name": c.Argv[3], "CreatedAt": "2026-10-07T12:00:00Z"}})
		return docker.Result{Stdout: out}, err
	})
	x.f.On(fakerunner.Glob("docker run -d --name demo-dictionaryetl-* *")).Stdout("cid\n")
	x.f.On(fakerunner.Glob("docker container inspect demo-dictionaryetl-*")).Stdout(`[{"Id":"cid","State":{"Status":"running","Running":true}}]`)
	x.f.On(fakerunner.Glob("docker exec demo-dictionaryetl-* wget -qO- http://localhost:8086/actuator/health")).Stdout(`{"status":"UP"}`)
	x.f.On(fakerunner.Glob("docker run * --name demo-dictionary-curl-* *")).Do(func(context.Context, fakerunner.Call) (docker.Result, error) {
		r := x.curl[0]
		if len(x.curl) > 1 {
			x.curl = x.curl[1:]
		}
		return r, nil
	})
	x.f.On(fakerunner.Glob("docker rm -v -f demo-dictionaryetl-*"))
	x.f.On(fakerunner.Glob("docker exec -i * id-dictionary-db psql *")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		if strings.Contains(string(c.Stdin), "count(*) FROM dict.concept_node") {
			return docker.Result{Stdout: []byte(x.concepts + "\n")}, nil
		}
		return docker.Result{}, nil
	})
	x.f.On(fakerunner.Glob("docker compose * restart dictionary-api"))
}

func (x *dictFixture) dict() *Dictionary { return NewDictionary(x.d, x.st, x.cfg, x.sec, x.state) }

func (x *dictFixture) curlCalls() []fakerunner.Call {
	return x.f.CallsMatching(fakerunner.Glob("docker run * --name demo-dictionary-curl-* *"))
}

// noSecretInArgv checks the dictionary password reached no argv, and every
// container that needs it got it by name.
func (x *dictFixture) noSecretInArgv() {
	x.t.Helper()
	for _, c := range x.f.Calls() {
		if strings.Contains(strings.Join(c.Argv, " "), dictPassword) {
			x.t.Errorf("the password reached argv: %q", c.Argv)
		}
	}
}

func (x *dictFixture) refreshed() {
	x.t.Helper()
	x.f.AssertOrder(
		fakerunner.Glob("docker rm -v -f demo-dictionaryetl-*"),
		fakerunner.Glob("docker exec -i * id-dictionary-db psql *"),
		fakerunner.Glob("docker compose * restart dictionary-api"),
		fakerunner.Glob("docker compose * ps --all --format json dictionary-api"),
	)
	var touches int
	for _, c := range x.f.CallsMatching(fakerunner.Glob("docker exec -i * id-dictionary-db psql *")) {
		if strings.Contains(string(c.Stdin), "UPDATE dict.update_info SET last_updated = NOW()") {
			touches++
		}
	}
	if touches != 1 {
		x.t.Errorf("update_info touched %d times", touches)
	}
	x.f.AssertNotCalled(fakerunner.Glob("docker compose * up *"))
	state, err := x.st.LoadState()
	if err != nil {
		x.t.Fatal(err)
	}
	if slices.Contains(state.PendingRestarts, dictionaryAPI) {
		x.t.Errorf("dictionary-api still pending a restart")
	}
}

func hasArgs(argv []string, want ...string) bool {
	for i := range argv {
		if i+len(want) <= len(argv) && slices.Equal(argv[i:i+len(want)], want) {
			return true
		}
	}
	return false
}

func TestDictionaryHydrateLocal(t *testing.T) {
	x := newDictFixture(t)
	x.stackUp()
	x.f.On(fakerunner.Glob("docker run * --name demo-columnmeta-* *"))

	err := DictionaryHydrate(context.Background(), x.d, x.st, x.cfg, x.sec, x.state, HydrateOptions{Clear: true, Heap: 1024}, nil)
	if err != nil {
		t.Fatal(err)
	}
	x.f.AssertOrder(
		fakerunner.Glob("docker run * --name demo-columnmeta-* *"),
		fakerunner.Glob("docker run -d --name demo-dictionaryetl-* *"),
		fakerunner.Glob("docker exec demo-dictionaryetl-* wget *"),
		fakerunner.Glob("docker run * --name demo-dictionary-curl-* *"),
	)
	cm := x.f.CallsMatching(fakerunner.Glob("docker run * --name demo-columnmeta-* *"))[0]
	for _, want := range [][]string{{"--rm"}, {"--user", "0:0"}, {"--network", "none"}, {"-v", "demo_hpds-data:/opt/local/hpds"}, {"hms-dbmi/pic-sure-hpds-etl:658e340e3cfc"}} {
		if !hasArgs(cm.Argv, want...) {
			t.Errorf("columnmeta argv %q lacks %q", cm.Argv, want)
		}
	}
	for _, env := range []string{"HEAPSIZE", "LOADER_NAME", "JAVA_OPTS"} {
		if !cm.HasEnv(env) {
			t.Errorf("columnmeta lacks env %s", env)
		}
	}

	etl := x.f.CallsMatching(fakerunner.Glob("docker run -d --name demo-dictionaryetl-* *"))[0]
	for _, want := range [][]string{{"--network", "demo_data"}, {"--network-alias", "dictionaryetl"}, {"-v", "demo_hpds-data:/opt/local/hpds"}, {"hms-dbmi/dictionary-etl:c97a813b95a1"}, {"-e", "POSTGRES_PASSWORD"}} {
		if !hasArgs(etl.Argv, want...) {
			t.Errorf("etl argv %q lacks %q", etl.Argv, want)
		}
	}
	for _, env := range []string{"POSTGRES_HOST", "POSTGRES_DB", "POSTGRES_USER", "POSTGRES_PASSWORD"} {
		if !etl.HasEnv(env) {
			t.Errorf("etl lacks env %s", env)
		}
	}
	if hasArgs(etl.Argv, "--rm") {
		t.Errorf("etl runs with --rm, which loses its logs: %q", etl.Argv)
	}

	curl := x.curlCalls()
	if len(curl) != 1 {
		t.Fatalf("curl calls = %d", len(curl))
	}
	for _, want := range [][]string{{"--rm"}, {"--network", "demo_data"}, {"-X", "POST"}, {"-H", "Content-Type: application/json"}, {"--data-binary", "@-"}, {"http://dictionaryetl:8086/load/initialize"}} {
		if !hasArgs(curl[0].Argv, want...) {
			t.Errorf("curl argv %q lacks %q", curl[0].Argv, want)
		}
	}
	if got, want := string(curl[0].Stdin), `{"clearDatabase":true,"includeDefaultFacets":false}`; got != want {
		t.Errorf("request = %s, want %s", got, want)
	}
	x.refreshed()
	x.noSecretInArgv()
}

func TestDictionaryHydrateShared(t *testing.T) {
	x := newDictFixture(t)
	x.cfg.HPDS.Data, x.cfg.HPDS.SharedName = stack.HPDSShared, "picsure-demo"
	x.stackUp()
	x.f.On(fakerunner.Glob("docker run * --name demo-columnmeta-check-* *"))

	if err := DictionaryHydrate(context.Background(), x.d, x.st, x.cfg, x.sec, x.state, HydrateOptions{IncludeDatasetFacets: true}, nil); err != nil {
		t.Fatal(err)
	}
	check := x.f.CallsMatching(fakerunner.Glob("docker run * --name demo-columnmeta-check-* *"))[0]
	if !hasArgs(check.Argv, "-v", "picsure-demo_hpds-data:/data:ro") || !hasArgs(check.Argv, "test", "-s", "/data/columnMeta.csv") {
		t.Errorf("check argv = %q", check.Argv)
	}
	x.f.AssertNotCalled(fakerunner.Glob("docker run * hms-dbmi/pic-sure-hpds-etl:*"))
	etl := x.f.CallsMatching(fakerunner.Glob("docker run -d --name demo-dictionaryetl-* *"))[0]
	if !hasArgs(etl.Argv, "-v", "picsure-demo_hpds-data:/opt/local/hpds:ro") {
		t.Errorf("etl argv %q doesn't mount the shared set read-only", etl.Argv)
	}
	if got, want := string(x.curlCalls()[0].Stdin), `{"clearDatabase":false,"errorDirectory":"/tmp/columnMetaErrors.csv","includeDefaultFacets":true}`; got != want {
		t.Errorf("request = %s, want %s", got, want)
	}
	x.refreshed()
}

func TestDictionaryHydrateSharedWithoutColumnMeta(t *testing.T) {
	x := newDictFixture(t)
	x.cfg.HPDS.Data, x.cfg.HPDS.SharedName = stack.HPDSShared, "picsure-demo"
	x.stackUp()
	x.f.On(fakerunner.Glob("docker run * --name demo-columnmeta-check-* *")).Exit(1)

	err := DictionaryHydrate(context.Background(), x.d, x.st, x.cfg, x.sec, x.state, HydrateOptions{}, nil)
	if exitcode.FromError(err) != exitcode.CodePrecondition || !strings.Contains(err.Error(), "has no columnMeta.csv") {
		t.Fatalf("err = %v", err)
	}
	x.f.AssertNotCalled(fakerunner.Glob("docker run -d *"))
}

func TestDictionaryHydrateFailures(t *testing.T) {
	cases := []struct {
		name  string
		setup func(x *dictFixture)
		want  string
	}{
		{"columnmeta fails", func(x *dictFixture) {
			x.f.On(fakerunner.Glob("docker run * --name demo-columnmeta-* *")).Exit(1)
		}, "CreateColumnmetaCSV exited 1"},
		{"not success", func(x *dictFixture) {
			x.f.On(fakerunner.Glob("docker run * --name demo-columnmeta-* *"))
			x.curl = []docker.Result{{Stdout: []byte("This task is already running. Skipping execution.")}}
		}, "didn't hydrate: This task is already running"},
		{"no concepts", func(x *dictFixture) {
			x.f.On(fakerunner.Glob("docker run * --name demo-columnmeta-* *"))
			x.f.On(fakerunner.Glob("docker logs *")).Stdout("ERROR reading /opt/local/hpds/columnMeta.csv\n")
			x.concepts = "0"
		}, "dictionary-etl reported success, but the dictionary has no concepts"},
		{"no columnMeta.csv", func(x *dictFixture) {
			x.f.On(fakerunner.Glob("docker run * --name demo-columnmeta-check-* *")).Exit(1)
			x.f.On(fakerunner.Glob("docker run * --name demo-columnmeta-* *"))
		}, "demo_hpds-data has no columnMeta.csv"},
		{"http error", func(x *dictFixture) {
			x.f.On(fakerunner.Glob("docker run * --name demo-columnmeta-* *"))
			x.curl = []docker.Result{{Stdout: []byte(`{"status":500}`), Stderr: []byte("curl: (22) The requested URL returned error: 500"), ExitCode: 22}}
		}, `POST /load/initialize: curl: (22) The requested URL returned error: 500: {"status":500}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			x := newDictFixture(t)
			c.setup(x)
			x.stackUp()
			err := DictionaryHydrate(context.Background(), x.d, x.st, x.cfg, x.sec, x.state, HydrateOptions{}, nil)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			x.f.AssertNotCalled(fakerunner.Glob("docker compose * restart dictionary-api"))
			if len(x.f.CallsMatching(fakerunner.Glob("docker run -d *"))) > 0 {
				x.f.AssertCalled(fakerunner.Glob("docker rm -v -f demo-dictionaryetl-*"))
			}
			state, _ := x.st.LoadState()
			if c.name != "columnmeta fails" && c.name != "no columnMeta.csv" && !slices.Contains(state.PendingRestarts, dictionaryAPI) {
				t.Errorf("a failed write left no pending restart: %v", state.PendingRestarts)
			}
		})
	}
}

func TestDictionaryETLNeverReady(t *testing.T) {
	x := newDictFixture(t)
	x.f.On(fakerunner.Glob("docker exec demo-dictionaryetl-* wget *")).Exit(1)
	x.f.On(fakerunner.Glob("docker logs *")).Stdout("Caused by: org.postgresql.util.PSQLException: password authentication failed\n")
	x.f.On(fakerunner.Glob("docker run * --name demo-columnmeta-* *"))
	x.stackUp()

	err := DictionaryHydrate(context.Background(), x.d, x.st, x.cfg, x.sec, x.state, HydrateOptions{}, nil)
	if err == nil || !strings.Contains(err.Error(), "dictionary-etl wasn't ready within") {
		t.Fatalf("err = %v", err)
	}
	x.f.AssertCalled(fakerunner.Glob("docker rm -v -f demo-dictionaryetl-*"))
	x.f.AssertNotCalled(fakerunner.Glob("docker run * --name demo-dictionary-curl-* *"))
	var logged bool
	for _, e := range x.rec.Events() {
		if l, ok := e.(events.Log); ok && strings.Contains(l.Line, "password authentication failed") {
			logged = true
		}
	}
	if !logged {
		t.Error("the ETL's log tail wasn't shown")
	}
}

func TestDictionaryETLExits(t *testing.T) {
	x := newDictFixture(t)
	x.f.On(fakerunner.Glob("docker container inspect demo-dictionaryetl-*")).Stdout(`[{"Id":"cid","State":{"Status":"exited","Running":false,"ExitCode":1}}]`)
	x.f.On(fakerunner.Glob("docker logs *")).Stdout("APPLICATION FAILED TO START\n")
	x.f.On(fakerunner.Glob("docker run * --name demo-columnmeta-* *"))
	x.stackUp()

	err := DictionaryHydrate(context.Background(), x.d, x.st, x.cfg, x.sec, x.state, HydrateOptions{}, nil)
	if err == nil || !strings.Contains(err.Error(), "dictionary-etl exited 1 before it was ready") {
		t.Fatalf("err = %v", err)
	}
	x.f.AssertCalled(fakerunner.Glob("docker rm -v -f demo-dictionaryetl-*"))
}

func TestDictionaryNeedsHealthyDB(t *testing.T) {
	x := newDictFixture(t)
	x.f.On(fakerunner.Glob("docker compose * ps --all --format json dictionary-db")).Stdout(dictPs("dictionary-db", "running", "unhealthy"))
	x.f.On(fakerunner.Glob("docker run * --name demo-columnmeta-* *"))
	x.stackUp()

	err := DictionaryHydrate(context.Background(), x.d, x.st, x.cfg, x.sec, x.state, HydrateOptions{}, nil)
	if exitcode.FromError(err) != exitcode.CodePrecondition || !strings.Contains(err.Error(), "run `pic-sure up`") {
		t.Fatalf("err = %v", err)
	}
	x.f.AssertNotCalled(fakerunner.Glob("docker run -d *"))
}

func TestDictionaryMissingImage(t *testing.T) {
	x := newDictFixture(t)
	delete(x.state.Images, "pic-sure-hpds-etl")
	x.stackUp()

	err := DictionaryHydrate(context.Background(), x.d, x.st, x.cfg, x.sec, x.state, HydrateOptions{}, nil)
	if exitcode.FromError(err) != exitcode.CodePrecondition || !strings.Contains(err.Error(), "no pic-sure-hpds-etl image; run `pic-sure build`") {
		t.Fatalf("err = %v", err)
	}
}

func TestDictionaryLoadCSV(t *testing.T) {
	x := newDictFixture(t)
	x.stackUp()
	dir := t.TempDir()
	datasets := filepath.Join(dir, "datasets.csv")
	if err := os.WriteFile(datasets, []byte("ref,full_name,abbreviation,description\nphs 1&2,A,A,a\nphs10,B,B,b\nnone,C,C,c\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	header := "dataset_ref,name,display,concept_type,concept_path,parent_concept_path,values\n"
	concepts := writeZip(t, dir, "concepts.zip", map[string]string{
		"concepts_1.csv": header + "phs 1&2,a,a,categorical,\\a\\,,\nphs10,b,b,categorical,\\b\\,,\nstray,s,s,categorical,\\s\\,,\n",
	}, "concepts_1.csv")

	err := DictionaryLoadCSV(context.Background(), x.d, x.st, x.cfg, x.sec, x.state,
		LoadCSVOptions{Datasets: datasets, Concepts: concepts, Clear: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	curl := x.curlCalls()
	type req struct{ method, url, body string }
	var got []req
	for _, c := range curl {
		r := req{url: c.Argv[len(c.Argv)-1], body: string(c.Stdin)}
		for i, a := range c.Argv {
			if a == "-X" {
				r.method = c.Argv[i+1]
			}
		}
		got = append(got, r)
	}
	want := []req{
		{"DELETE", "http://dictionaryetl:8086/clear/all", ""},
		{"PUT", "http://dictionaryetl:8086/api/dataset/csv", "ref,full_name,abbreviation,description\nphs 1&2,A,A,a\nphs10,B,B,b\nnone,C,C,c\n"},
		{"PUT", "http://dictionaryetl:8086/api/concept/csv?datasetRef=phs+1%262", header + "phs 1&2,a,a,categorical,\\a\\,,\n"},
		{"PUT", "http://dictionaryetl:8086/api/concept/csv?datasetRef=phs10", header + "phs10,b,b,categorical,\\b\\,,\n"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("requests =\n%q\nwant\n%q", got, want)
	}
	if hasArgs(curl[0].Argv, "--data-binary", "@-") {
		t.Errorf("DELETE sends a body: %q", curl[0].Argv)
	}
	var warnings []string
	for _, e := range x.rec.Events() {
		if w, ok := e.(events.Warning); ok {
			warnings = append(warnings, w.Text)
		}
	}
	if !slices.ContainsFunc(warnings, func(s string) bool { return strings.Contains(s, `dataset "none" has no concepts`) }) ||
		!slices.ContainsFunc(warnings, func(s string) bool { return strings.Contains(s, `doesn't list: "stray"`) }) {
		t.Errorf("warnings = %q", warnings)
	}
	x.refreshed()
	x.noSecretInArgv()
}

func TestDictionaryLoadCSVBadInputClearsNothing(t *testing.T) {
	x := newDictFixture(t)
	dir := t.TempDir()
	datasets := filepath.Join(dir, "datasets.csv")
	if err := os.WriteFile(datasets, []byte("ref\nphs1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := DictionaryLoadCSV(context.Background(), x.d, x.st, x.cfg, x.sec, x.state,
		LoadCSVOptions{Datasets: datasets, Concepts: datasets, Clear: true}, nil)
	if exitcode.FromError(err) != exitcode.CodeUsage {
		t.Fatalf("err = %v", err)
	}
	if n := len(x.f.Calls()); n != 0 {
		t.Errorf("%d docker calls before the input was checked", n)
	}
}

func TestDictionaryLoadFacets(t *testing.T) {
	x := newDictFixture(t)
	x.stackUp()
	dir := t.TempDir()
	var files []string
	for _, n := range []string{"categories", "facets", "concepts"} {
		p := filepath.Join(dir, n+".csv")
		if err := os.WriteFile(p, []byte(n+" body\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		files = append(files, p)
	}
	err := DictionaryLoadFacets(context.Background(), x.d, x.st, x.cfg, x.sec, x.state,
		FacetOptions{Categories: files[0], Facets: files[1], Concepts: files[2]}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range x.curlCalls() {
		got = append(got, c.Argv[len(c.Argv)-1]+" "+string(c.Stdin))
	}
	want := []string{
		"http://dictionaryetl:8086/api/facet/category/csv categories body\n",
		"http://dictionaryetl:8086/api/facet/csv facets body\n",
		"http://dictionaryetl:8086/api/facet/concept/csv concepts body\n",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("requests = %q, want %q", got, want)
	}
	x.refreshed()
}

func TestDictionaryLoadFacetsStopsAtFirstFailure(t *testing.T) {
	x := newDictFixture(t)
	x.curl = []docker.Result{{Stdout: []byte("Success")}, {Stderr: []byte("curl: (22) The requested URL returned error: 400"), ExitCode: 22}}
	x.stackUp()
	dir := t.TempDir()
	p := filepath.Join(dir, "f.csv")
	if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := DictionaryLoadFacets(context.Background(), x.d, x.st, x.cfg, x.sec, x.state, FacetOptions{Categories: p, Facets: p, Concepts: p}, nil)
	if err == nil || !strings.Contains(err.Error(), "PUT /api/facet/csv") {
		t.Fatalf("err = %v", err)
	}
	if n := len(x.curlCalls()); n != 2 {
		t.Errorf("curl calls = %d, want 2", n)
	}
}

func TestDictionaryWeights(t *testing.T) {
	x := newDictFixture(t)
	x.stackUp()
	x.f.On(fakerunner.Glob("docker run * --name demo-dictionary-weights-* *"))
	c, err := cache.Open(t.TempDir(), cache.Options{})
	if err != nil {
		t.Fatal(err)
	}
	src, err := c.SourceDir("pic-sure", strings.Repeat("a", 40))
	if err != nil {
		t.Fatal(err)
	}
	weights := filepath.Join(src, filepath.FromSlash(defaultWeightsFile))
	if err := os.MkdirAll(filepath.Dir(weights), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(weights, []byte("concept_node.DISPLAY,A\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := DictionaryWeights(context.Background(), x.d, x.st, x.cfg, x.sec, x.state, WeightsOptions{Cache: c}, nil); err != nil {
		t.Fatal(err)
	}
	run := x.f.CallsMatching(fakerunner.Glob("docker run * --name demo-dictionary-weights-* *"))[0]
	for _, want := range [][]string{{"--rm"}, {"--network", "demo_data"}, {"-v", weights + ":/weights.csv:ro"}, {"hms-dbmi/dictionary-weights:658e340e3cfc"}} {
		if !hasArgs(run.Argv, want...) {
			t.Errorf("weights argv %q lacks %q", run.Argv, want)
		}
	}
	if !run.HasEnv("POSTGRES_PASSWORD") {
		t.Error("weights lacks POSTGRES_PASSWORD")
	}
	x.f.AssertNotCalled(fakerunner.Glob("docker run -d *"))
	x.f.AssertOrder(
		fakerunner.Glob("docker run * --name demo-dictionary-weights-* *"),
		fakerunner.Glob("docker exec -i * id-dictionary-db psql *"),
		fakerunner.Glob("docker compose * restart dictionary-api"),
	)
	x.noSecretInArgv()
}

func TestDictionaryWeightsFails(t *testing.T) {
	x := newDictFixture(t)
	x.f.On(fakerunner.Glob("docker run * --name demo-dictionary-weights-* *")).Exit(1)
	x.stackUp()
	p := filepath.Join(t.TempDir(), "w.csv")
	if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := DictionaryWeights(context.Background(), x.d, x.st, x.cfg, x.sec, x.state, WeightsOptions{Weights: p}, nil)
	if err == nil || !strings.Contains(err.Error(), "dictionary-weights exited 1") {
		t.Fatalf("err = %v", err)
	}
	x.f.AssertNotCalled(fakerunner.Glob("docker compose * restart dictionary-api"))
}

func TestDictionaryWeightsNoDefaultFile(t *testing.T) {
	x := newDictFixture(t)
	x.stackUp()
	c, err := cache.Open(t.TempDir(), cache.Options{})
	if err != nil {
		t.Fatal(err)
	}
	err = DictionaryWeights(context.Background(), x.d, x.st, x.cfg, x.sec, x.state, WeightsOptions{Cache: c}, nil)
	if exitcode.FromError(err) != exitcode.CodePrecondition || !strings.Contains(err.Error(), "pass --weights FILE") {
		t.Fatalf("err = %v", err)
	}
}

func TestDictionaryRefreshSkipsStoppedAPI(t *testing.T) {
	x := newDictFixture(t)
	x.f.On(fakerunner.Glob("docker compose * ps --all --format json dictionary-api")).Stdout(dictPs("dictionary-api", "exited", ""))
	x.f.On(fakerunner.Glob("docker run * --name demo-dictionary-weights-* *"))
	x.stackUp()
	p := filepath.Join(t.TempDir(), "w.csv")
	if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := DictionaryWeights(context.Background(), x.d, x.st, x.cfg, x.sec, x.state, WeightsOptions{Weights: p}, nil); err != nil {
		t.Fatal(err)
	}
	x.f.AssertNotCalled(fakerunner.Glob("docker compose * restart dictionary-api"))
	state, _ := x.st.LoadState()
	if len(state.PendingRestarts) != 0 {
		t.Errorf("pending = %v", state.PendingRestarts)
	}
}

func TestDictionaryCloseAfterCancel(t *testing.T) {
	x := newDictFixture(t)
	x.stackUp()
	dict := x.dict()
	ctx, cancel := context.WithCancel(context.Background())
	if err := dict.startETL(ctx, x.rec, "t"); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := dict.Close(ctx); err != nil {
		t.Fatal(err)
	}
	x.f.AssertCalled(fakerunner.Glob("docker rm -v -f demo-dictionaryetl-*"))
	if err := dict.Close(ctx); err != nil || len(x.f.CallsMatching(fakerunner.Glob("docker rm *"))) != 1 {
		t.Errorf("a second Close removed again (err %v)", err)
	}
}
