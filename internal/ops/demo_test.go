package ops

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/render"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

func TestDemoFileURLs(t *testing.T) {
	want := map[string]string{
		"nhanes":      "https://raw.githubusercontent.com/hms-dbmi/pic-sure-public-datasets/e549e48803c31a4ab6bf985a87c1ef1d9d4a4416/NHANES%20abbreviated%20allConcepts.csv.tgz",
		"synthea":     "https://raw.githubusercontent.com/hms-dbmi/pic-sure-public-datasets/e549e48803c31a4ab6bf985a87c1ef1d9d4a4416/synthea_10k_picsure_format.csv.zip",
		"1000genomes": "https://raw.githubusercontent.com/hms-dbmi/pic-sure-public-datasets/e549e48803c31a4ab6bf985a87c1ef1d9d4a4416/open_access-1000Genomes_allConcepts_new_search_with_data_analyzer.csv",
	}
	for _, f := range DemoFiles {
		if got := f.URL(""); got != want[f.Dataset] {
			t.Errorf("%s URL = %s, want %s", f.Dataset, got, want[f.Dataset])
		}
		if len(f.SHA256) != 64 || strings.Trim(f.SHA256, "0123456789abcdef") != "" || f.Size <= 0 {
			t.Errorf("%s isn't pinned: size %d, sha256 %q", f.Dataset, f.Size, f.SHA256)
		}
	}
	if got := DemoFiles[0].URL("http://mirror/x/"); got != "http://mirror/x/NHANES%20abbreviated%20allConcepts.csv.tgz" {
		t.Errorf("mirror URL = %s", got)
	}
	if got := DemoFiles[0].cacheName(); got != "946305e3138255dc-NHANES_abbreviated_allConcepts.csv.tgz" {
		t.Errorf("cache name = %s", got)
	}
}

func TestDemoDatasets(t *testing.T) {
	if got := DemoDatasets(); !slices.Equal(got, []string{"nhanes", "synthea", "1000genomes", "all"}) {
		t.Errorf("datasets = %q", got)
	}
	all, err := demoFiles(DemoAll)
	if err != nil || len(all) != 3 {
		t.Errorf("all = %v, %v", all, err)
	}
	one, err := demoFiles("synthea")
	if err != nil || len(one) != 1 || one[0].Dataset != "synthea" {
		t.Errorf("synthea = %v, %v", one, err)
	}
	if _, err := demoFiles("frobnicate"); exitcode.FromError(err) != 2 {
		t.Errorf("unknown dataset: %v (exit %d)", err, exitcode.FromError(err))
	}
}

// demoServer serves body as "/f one.csv" and counts the requests.
func demoServer(t *testing.T, body []byte) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.EscapedPath() != "/f%20one.csv" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func pinned(body []byte) DemoFile {
	sum := sha256.Sum256(body)
	return DemoFile{Dataset: "test", Path: "f one.csv", Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}
}

func demoOpts(t *testing.T, base string) DemoOptions {
	t.Helper()
	c, err := cache.Open(t.TempDir(), cache.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return DemoOptions{Cache: c, HTTP: http.DefaultClient, BaseURL: base + "/"}
}

func downloads(t *testing.T, c *cache.Cache) []string {
	t.Helper()
	ents, err := os.ReadDir(c.DownloadsDir())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

func TestFetchDemoFileDownloadsOnceAndReuses(t *testing.T) {
	body := []byte("PATIENT_NUM,CONCEPT_PATH,NVAL_NUM,TVAL_CHAR,TIMESTAMP\n1,\\a\\,1,,\n")
	srv, hits := demoServer(t, body)
	opts := demoOpts(t, srv.URL)
	f := pinned(body)
	var rec events.Recorder

	path, err := fetchDemoFile(context.Background(), &rec, opts, f)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, body) {
		t.Errorf("downloaded %q", got)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v", fi.Mode())
	}
	if again, err := fetchDemoFile(context.Background(), &rec, opts, f); err != nil || again != path {
		t.Fatalf("second fetch = %s, %v", again, err)
	}
	if hits.Load() != 1 {
		t.Errorf("requests = %d, want 1 (the second fetch uses the cache)", hits.Load())
	}

	// A damaged cached file is replaced.
	if err := os.WriteFile(path, []byte("damaged"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := fetchDemoFile(context.Background(), &rec, opts, f); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, body) || hits.Load() != 2 {
		t.Errorf("after damage: %q, %d requests", got, hits.Load())
	}
	if !slices.ContainsFunc(rec.Events(), func(e events.Event) bool {
		w, ok := e.(events.Warning)
		return ok && strings.Contains(w.Text, "doesn't match its pinned SHA-256")
	}) {
		t.Error("no warning about the damaged file")
	}
	if got := downloads(t, opts.Cache); len(got) != 1 {
		t.Errorf("downloads/ = %q", got)
	}
}

func TestFetchDemoFileRejectsWrongContent(t *testing.T) {
	body := []byte("the pinned content\n")
	for name, tc := range map[string]struct {
		serve []byte
		want  string
	}{
		"other bytes": {[]byte("the pinned c0ntent\n"), "its SHA-256 is"},
		"too long":    {append(slices.Clone(body), "more"...), "but the pinned file has 19"},
		"too short":   {body[:5], "got 5 bytes"},
	} {
		t.Run(name, func(t *testing.T) {
			srv, _ := demoServer(t, tc.serve)
			opts := demoOpts(t, srv.URL)
			_, err := fetchDemoFile(context.Background(), &events.Recorder{}, opts, pinned(body))
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "downloading f one.csv") {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if got := downloads(t, opts.Cache); len(got) != 0 {
				t.Errorf("left in downloads/: %q", got)
			}
		})
	}
}

func TestFetchDemoFileHTTPError(t *testing.T) {
	srv, _ := demoServer(t, nil)
	opts := demoOpts(t, srv.URL)
	f := pinned([]byte("x"))
	f.Path = "missing.csv"
	_, err := fetchDemoFile(context.Background(), &events.Recorder{}, opts, f)
	if err == nil || !strings.Contains(err.Error(), "404 Not Found") {
		t.Fatalf("err = %v", err)
	}
}

func TestFetchDemoFileStall(t *testing.T) {
	old := demoStall
	demoStall = 50 * time.Millisecond
	t.Cleanup(func() { demoStall = old })
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	opts := demoOpts(t, srv.URL)
	_, err := fetchDemoFile(context.Background(), &events.Recorder{}, opts, pinned(make([]byte, 100)))
	if err == nil || !strings.Contains(err.Error(), "nothing received for 50ms") {
		t.Fatalf("err = %v", err)
	}
}

func writeDemoFile(t *testing.T, path string, data []byte) string {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func tgz(t *testing.T, name string, data []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write(data)
	if err := errors.Join(tw.Close(), gz.Close()); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func zipped(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for name, data := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(data)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestPrepareDemoCSVMergesLikeTheDatasets(t *testing.T) {
	c, err := cache.Open(t.TempDir(), cache.Options{})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// As in the real files: NHANES in a tgz with a quoted header, Synthea
	// in a zip with macOS metadata and an unquoted header; here also a BOM,
	// CRLF and a missing final newline.
	nhanes := writeDemoFile(t, filepath.Join(dir, "nhanes.tgz"), tgz(t, "allConcepts.csv",
		[]byte("\"PATIENT_NUM\",\"CONCEPT_PATH\",\"NVAL_NUM\",\"TVAL_CHAR\",\"TIMESTAMP\"\n\"1\",\"\\Nhanes\\AGE\\\",\"8\",\"\",\"\"")))
	synthea := writeDemoFile(t, filepath.Join(dir, "synthea.zip"), zipped(t, map[string][]byte{
		"synthea.csv":            []byte("\uFEFFPATIENT_NUM,CONCEPT_PATH,NVAL_NUM,TVAL_CHAR,TIMESTAMP\r\n15000,\\Synthea\\Sex\\,,Female,1\r\n"),
		"__MACOSX/._synthea.csv": []byte("junk"),
	}))
	genomes := writeDemoFile(t, filepath.Join(dir, "1000g.csv"),
		[]byte("\"PATIENT_NUM\",\"CONCEPT_PATH\",\"NVAL_NUM\",\"TVAL_CHAR\",\"TIMESTAMP\"\n\"731234\",\"µ1000Genomesµ\",\"\",\"TRUE\",\"\"\n"))

	csv, cleanup, err := prepareDemoCSV(context.Background(), &events.Recorder{}, c, []string{nhanes, synthea, genomes}, []string{"nhanes.tgz", "synthea.zip", "1000g.csv"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(csv)
	if err != nil {
		t.Fatal(err)
	}
	want := "\"PATIENT_NUM\",\"CONCEPT_PATH\",\"NVAL_NUM\",\"TVAL_CHAR\",\"TIMESTAMP\"\n" +
		"\"1\",\"\\Nhanes\\AGE\\\",\"8\",\"\",\"\"\n" +
		"15000,\\Synthea\\Sex\\,,Female,1\r\n" +
		"\"731234\",\"µ1000Genomesµ\",\"\",\"TRUE\",\"\"\n"
	if string(got) != want {
		t.Errorf("merged =\n%s\nwant\n%s", got, want)
	}
	// Only the merge is left in tmp/: each extraction is removed once merged.
	tmp, _ := os.ReadDir(filepath.Dir(filepath.Dir(csv)))
	if len(tmp) != 1 {
		t.Errorf("tmp/ holds %d entries", len(tmp))
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(csv); !os.IsNotExist(err) {
		t.Errorf("merge not removed: %v", err)
	}
}

func TestPrepareDemoCSVRefusesOtherHeaders(t *testing.T) {
	c, err := cache.Open(t.TempDir(), cache.Options{})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	a := writeDemoFile(t, filepath.Join(dir, "a.csv"), []byte("PATIENT_NUM,CONCEPT_PATH,NVAL_NUM,TVAL_CHAR,TIMESTAMP\n1,\\a\\,1,,\n"))
	b := writeDemoFile(t, filepath.Join(dir, "b.csv"), []byte("PATIENT_NUM,CONCEPT_PATH,NUMERIC_VALUE,TEXT_VALUE\n2,\\b\\,1,\n"))
	_, _, err = prepareDemoCSV(context.Background(), &events.Recorder{}, c, []string{a, b}, []string{"a.csv", "b.csv"})
	if err == nil || !strings.Contains(err.Error(), "b.csv's header") || !strings.Contains(err.Error(), "doesn't match a.csv's") {
		t.Fatalf("err = %v", err)
	}
	if tmp, _ := os.ReadDir(filepath.Join(filepath.Dir(c.DownloadsDir()), "tmp")); len(tmp) != 0 {
		t.Errorf("tmp/ not cleaned: %d entries", len(tmp))
	}
}

func TestPrepareDemoCSVSingleFile(t *testing.T) {
	c, err := cache.Open(t.TempDir(), cache.Options{})
	if err != nil {
		t.Fatal(err)
	}
	plain := writeDemoFile(t, filepath.Join(t.TempDir(), "g.csv"), []byte("PATIENT_NUM,CONCEPT_PATH,NVAL_NUM,TVAL_CHAR,TIMESTAMP\n"))
	csv, cleanup, err := prepareDemoCSV(context.Background(), &events.Recorder{}, c, []string{plain}, []string{"g.csv"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup() }()
	if csv != plain {
		t.Errorf("a plain CSV is used in place, got %s", csv)
	}
}

func TestDemoDictionarySteps(t *testing.T) {
	x := newDictFixture(t)
	x.stackUp()
	x.f.On(fakerunner.Glob("docker run * --name demo-columnmeta-* *"))
	x.f.On(fakerunner.Glob("docker run * --name demo-dictionary-weights-* *"))
	x.curl = []docker.Result{{Stdout: []byte("Success")}, {Stdout: []byte(`{"categoriesCreated":2,"categoriesUpdated":0,"facetsCreated":0,"facetsUpdated":0}`)}}
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
	writeDemoFile(t, weights, []byte("concept_node.DISPLAY,A\n"))
	facets, err := render.DemoFacetConfig()
	if err != nil {
		t.Fatal(err)
	}

	d := x.dict()
	defer func() { _ = d.Close(context.Background()) }()
	err = steps.Run(context.Background(), x.d.Sink, demoDictionarySteps(d, DemoOptions{HeapMB: 1024, Cache: c}, facets), steps.Options{})
	if err != nil {
		t.Fatal(err)
	}
	curl := x.curlCalls()
	if len(curl) != 2 {
		t.Fatalf("curl calls = %d", len(curl))
	}
	if got, want := string(curl[0].Stdin), `{"clearDatabase":true,"includeDefaultFacets":true}`; got != want {
		t.Errorf("hydrate request = %s, want %s", got, want)
	}
	if !hasArgs(curl[1].Argv, "-X", "POST") || curl[1].Argv[len(curl[1].Argv)-1] != "http://dictionaryetl:8086/api/facet/loader/load" ||
		!bytes.Equal(curl[1].Stdin, facets) || !strings.Contains(string(facets), `"dataset_id"`) {
		t.Errorf("facet request = %q %s", curl[1].Argv, curl[1].Stdin)
	}
	x.f.AssertOrder(
		fakerunner.Glob("docker run * --name demo-columnmeta-* *"),
		fakerunner.Glob("docker run * --name demo-dictionary-curl-* *"),
		fakerunner.Glob("docker run * --name demo-dictionary-weights-* *"),
		fakerunner.Glob("docker compose * restart dictionary-api"),
	)
	if !slices.ContainsFunc(x.rec.Events(), func(e events.Event) bool {
		p, ok := e.(events.Progress)
		return ok && p.ID == StepFacetConfig && strings.HasPrefix(p.Text, "2 facet categories created")
	}) {
		t.Error("no facet-config progress")
	}
	x.refreshed()
	x.noSecretInArgv()
}

func TestFacetConfigRejectsAnUnexpectedAnswer(t *testing.T) {
	x := newDictFixture(t)
	x.stackUp()
	for _, answer := range []string{"<html>oops</html>", "{}", "null"} {
		x.curl = []docker.Result{{Stdout: []byte(answer)}}
		d := x.dict()
		err := steps.Run(context.Background(), x.d.Sink, d.FacetConfigSteps([]byte("[]")), steps.Options{})
		if err == nil || !strings.Contains(err.Error(), "isn't its result: "+answer) {
			t.Errorf("%s: err = %v", answer, err)
		}
		_ = d.Close(context.Background())
	}
}

// TestDemoFilesMatchTheirPins downloads the real files (56 MB) and checks
// them against the pins. It needs the network, so it runs only with
// PIC_SURE_TEST_DEMO_DOWNLOADS=1.
func TestDemoFilesMatchTheirPins(t *testing.T) {
	if os.Getenv("PIC_SURE_TEST_DEMO_DOWNLOADS") != "1" {
		t.Skip("set PIC_SURE_TEST_DEMO_DOWNLOADS=1 to download the demo files")
	}
	opts := demoOpts(t, "")
	opts.BaseURL = ""
	for _, f := range DemoFiles {
		if _, err := fetchDemoFile(context.Background(), &events.Recorder{}, opts, f); err != nil {
			t.Error(err)
		}
	}
}

// demoFixture is a dictFixture whose stack also has its HPDS key, the
// weights file and the loader's containers, with DemoFiles replaced by
// files served from a test server.
type demoFixture struct {
	*dictFixture
	opts   DemoOptions
	hits   *atomic.Int32
	marker []byte
	// duringLoad runs inside the fake loader container.
	duringLoad func()
}

func newDemoFixture(t *testing.T, files map[string][]byte) *demoFixture {
	t.Helper()
	x := &demoFixture{dictFixture: newDictFixture(t)}
	// The loader writes only to a volume labelled as the stack's.
	x.f.On(fakerunner.Exact("docker", "volume", "inspect", "demo_hpds-data")).Stdout(
		`[{"Name":"demo_hpds-data","CreatedAt":"2026-10-07T12:00:00Z","Labels":{"` + stack.LabelStack + `":"demo","` + stack.LabelStackDir + `":"` + x.st.Dir + `"}}]`)
	x.stackUp()
	if _, err := x.st.EnsureSecrets(rand.Reader, stack.EnsureOptions{OpenAuth: true}); err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write(files[strings.TrimPrefix(r.URL.Path, "/")])
	}))
	t.Cleanup(srv.Close)
	x.hits = &hits
	old := DemoFiles
	t.Cleanup(func() { DemoFiles = old })
	DemoFiles = nil
	for _, name := range slices.Sorted(maps.Keys(files)) {
		f := pinned(files[name])
		f.Dataset, f.Path = strings.TrimSuffix(name, ".csv"), name
		DemoFiles = append(DemoFiles, f)
	}

	x.opts = demoOpts(t, srv.URL)
	src, err := x.opts.Cache.SourceDir("pic-sure", strings.Repeat("a", 40))
	if err != nil {
		t.Fatal(err)
	}
	weights := filepath.Join(src, filepath.FromSlash(defaultWeightsFile))
	if err := os.MkdirAll(filepath.Dir(weights), 0o755); err != nil {
		t.Fatal(err)
	}
	writeDemoFile(t, weights, []byte("concept_node.DISPLAY,A\n"))

	f := x.f
	f.On(fakerunner.Glob("docker run --rm --name demo-hpds-input-* *")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		for _, a := range c.Argv {
			if src, ok := strings.CutSuffix(a, ":/input/allConcepts.csv:ro"); ok {
				fi, err := os.Stat(src)
				if err != nil {
					return docker.Result{ExitCode: 1}, nil
				}
				return docker.Result{Stdout: []byte(fmt.Sprintf("%d\n", fi.Size()))}, nil
			}
		}
		return docker.Result{ExitCode: 1}, nil
	})
	f.On(fakerunner.Glob("docker rm -v -f demo-hpds-*"))
	f.On(fakerunner.Glob("docker compose * stop hpds"))
	f.On(fakerunner.Glob("docker run --rm --name demo-hpds-wipe-* *"))
	f.On(fakerunner.Glob("docker run -i --rm --name demo-hpds-key-* *"))
	f.On(fakerunner.Glob("docker run --rm --name demo-hpds-etl-* *")).Do(func(context.Context, fakerunner.Call) (docker.Result, error) {
		if x.duringLoad != nil {
			x.duringLoad()
		}
		return docker.Result{}, nil
	})
	f.On(fakerunner.Glob("docker run -i --rm --name demo-hpds-marker-* *")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		x.marker = c.Stdin
		return docker.Result{}, nil
	})
	f.On(fakerunner.Glob("docker compose * up -d --wait --wait-timeout 900 hpds"))
	f.On(fakerunner.Glob("docker compose * ps --all --format json hpds")).Stdout(dictPs("hpds", "running", "healthy"))
	f.On(fakerunner.Glob("docker run * --name demo-columnmeta-* *"))
	f.On(fakerunner.Glob("docker run * --name demo-dictionary-weights-* *"))
	x.curl = []docker.Result{{Stdout: []byte("Success")}, {Stdout: []byte(`{"categoriesCreated":2,"categoriesUpdated":0,"facetsCreated":0,"facetsUpdated":0}`)}}
	return x
}

func (x *demoFixture) run(dataset string) (string, error) {
	x.opts.Dataset = dataset
	return DataDemo(context.Background(), x.d, x.st, x.cfg, x.sec, x.state, x.opts)
}

// pruneLockFree reports whether cache prune could take its lock now.
func (x *demoFixture) pruneLockFree() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	l, err := x.opts.Cache.LockPrune(ctx)
	if err != nil {
		return false
	}
	_ = l.Unlock()
	return true
}

func TestDataDemoLoadsThenRebuildsTheDictionary(t *testing.T) {
	csv := []byte("PATIENT_NUM,CONCEPT_PATH,NVAL_NUM,TVAL_CHAR,TIMESTAMP\n1,\\a\\,1,,\n")
	x := newDemoFixture(t, map[string][]byte{"a.csv": csv, "b.csv": csv})
	var lockedDuringLoad bool
	x.duringLoad = func() { lockedDuringLoad = !x.pruneLockFree() }

	dataset, err := x.run(DemoAll)
	if err != nil {
		t.Fatal(err)
	}
	if dataset != "demo:all" || strings.TrimSpace(string(x.marker)) != "demo:all" {
		t.Errorf("dataset = %q, marker %q", dataset, x.marker)
	}
	x.f.AssertOrder(
		fakerunner.Glob("docker compose * stop hpds"),
		fakerunner.Glob("docker run --rm --name demo-hpds-etl-* *"),
		fakerunner.Glob("docker compose * up -d --wait --wait-timeout 900 hpds"),
		fakerunner.Glob("docker run * --name demo-columnmeta-* *"),
		fakerunner.Glob("docker run * --name demo-dictionary-curl-* *"),
		fakerunner.Glob("docker run * --name demo-dictionary-weights-* *"),
		fakerunner.Glob("docker compose * restart dictionary-api"),
	)
	if len(x.curlCalls()) != 2 {
		t.Errorf("curl calls = %d, want hydrate and facet-config", len(x.curlCalls()))
	}
	if !lockedDuringLoad {
		t.Error("the cache use lock wasn't held while the loader ran")
	}
	if !x.pruneLockFree() {
		t.Error("the cache use lock is still held")
	}
	if tmp, _ := os.ReadDir(filepath.Join(filepath.Dir(x.opts.Cache.DownloadsDir()), "tmp")); len(tmp) != 0 {
		t.Errorf("tmp/ not cleaned: %d entries", len(tmp))
	}
	// hpds's start is a compose up, which refreshed() forbids.
	if state, err := x.st.LoadState(); err != nil || slices.Contains(state.PendingRestarts, dictionaryAPI) {
		t.Errorf("pending restarts after the refresh: %v, %v", state, err)
	}
	x.noSecretInArgv()
}

func TestDataDemoChecksTheDictionaryBeforeTouchingHPDS(t *testing.T) {
	x := newDemoFixture(t, map[string][]byte{"a.csv": []byte("PATIENT_NUM\n1\n")})
	delete(x.state.Images, "dictionary-weights")
	_, err := x.run("a")
	if exitcode.FromError(err) != exitcode.CodePrecondition || !strings.Contains(err.Error(), "no dictionary-weights image") {
		t.Fatalf("err = %v", err)
	}
	if x.hits.Load() != 0 {
		t.Errorf("downloaded %d files before the preflight failed", x.hits.Load())
	}
	x.f.AssertNotCalled(fakerunner.Glob("docker compose * stop hpds"))
}

func TestDataDemoMergeFailureLeavesHPDSAlone(t *testing.T) {
	x := newDemoFixture(t, map[string][]byte{
		"a.csv": []byte("PATIENT_NUM,CONCEPT_PATH\n1,\\a\\\n"),
		"b.csv": []byte("PATIENT_NUM,OTHER\n2,x\n"),
	})
	_, err := x.run(DemoAll)
	var se *steps.Error
	if !errors.As(err, &se) || se.Step != StepDemoPrepare || !strings.Contains(err.Error(), "b.csv's header") {
		t.Fatalf("err = %v", err)
	}
	x.f.AssertNotCalled(fakerunner.Glob("docker compose * stop hpds"))
	if !x.pruneLockFree() {
		t.Error("the cache use lock is still held")
	}
	if tmp, _ := os.ReadDir(filepath.Join(filepath.Dir(x.opts.Cache.DownloadsDir()), "tmp")); len(tmp) != 0 {
		t.Errorf("tmp/ not cleaned: %d entries", len(tmp))
	}
}
