package ops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
)

// phenotypeFixture is the demo fixture with a CSV to load and the custom
// dictionary's files.
type phenotypeFixture struct {
	*demoFixture
	opts PhenotypeOptions
}

func newPhenotypeFixture(t *testing.T) *phenotypeFixture {
	t.Helper()
	x := &phenotypeFixture{demoFixture: newDemoFixture(t, nil)}
	dir := t.TempDir()
	csv := writeDemoFile(t, filepath.Join(dir, "pheno.csv"), []byte("PATIENT_NUM,CONCEPT_PATH,NUMERIC_VALUE,TEXT_VALUE\n1,\\a\\,1,\n"))
	x.opts = PhenotypeOptions{
		Load:       PhenotypeLoadOptions{CSV: csv, MkdirTemp: x.demoFixture.opts.Cache.TempDir},
		Dictionary: DictionaryAuto,
		Cache:      x.demoFixture.opts.Cache,
	}
	return x
}

// custom switches the fixture to a custom dictionary with facets.
func (x *phenotypeFixture) custom(t *testing.T) {
	dir := t.TempDir()
	x.opts.Dictionary = DictionaryCustom
	x.opts.Datasets = writeDemoFile(t, filepath.Join(dir, "datasets.csv"), []byte("ref,full_name,abbreviation,description\nsyn,S,S,s\n"))
	x.opts.Concepts = writeZip(t, dir, "concepts.zip", map[string]string{
		"concepts_1.csv": "dataset_ref,name,display,concept_type,concept_path,parent_concept_path,values\nsyn,a,a,categorical,\\a\\,,\n",
	}, "concepts_1.csv")
	x.opts.Facets = writeFacets(t, dir)
	x.curl = []docker.Result{{Stdout: []byte("ok")}}
}

func (x *phenotypeFixture) run() (string, error) {
	return DataLoadPhenotype(context.Background(), x.d, x.st, x.cfg, x.sec, x.state, x.opts)
}

// requests lists the curl calls as "METHOD path".
func (x *phenotypeFixture) requests() []string {
	var got []string
	for _, c := range x.curlCalls() {
		var method string
		for i, a := range c.Argv {
			if a == "-X" {
				method = c.Argv[i+1]
			}
		}
		got = append(got, method+" "+strings.TrimPrefix(c.Argv[len(c.Argv)-1], dictionaryETLURL))
	}
	return got
}

func TestDataLoadPhenotypeAutoHydratesThenWeighs(t *testing.T) {
	x := newPhenotypeFixture(t)
	x.opts.Load.HeapMB = 1024
	dataset, err := x.run()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dataset, "phenotype:") {
		t.Errorf("dataset = %q", dataset)
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
	if got := strings.Join(x.requests(), "; "); got != "POST /load/initialize" {
		t.Errorf("requests = %s", got)
	}
	if body := string(x.curlCalls()[0].Stdin); body != `{"clearDatabase":true,"includeDefaultFacets":false}` {
		t.Errorf("hydrate request = %s", body)
	}
	cm := x.f.CallsMatching(fakerunner.Glob("docker run * --name demo-columnmeta-* *"))[0]
	if !hasArgs(cm.Argv, "-e", "HEAPSIZE") {
		t.Errorf("columnmeta argv %q", cm.Argv)
	}
	x.noSecretInArgv()
}

func TestDataLoadPhenotypeCustomLoadsCSVsAndFacets(t *testing.T) {
	x := newPhenotypeFixture(t)
	x.custom(t)
	if _, err := x.run(); err != nil {
		t.Fatal(err)
	}
	want := "DELETE /clear/all; PUT /api/dataset/csv; PUT /api/concept/csv?datasetRef=syn; " +
		"PUT /api/facet/category/csv; PUT /api/facet/csv; PUT /api/facet/concept/csv"
	if got := strings.Join(x.requests(), "; "); got != want {
		t.Errorf("requests = %s\nwant %s", got, want)
	}
	x.f.AssertNotCalled(fakerunner.Glob("docker run * --name demo-columnmeta-* *"))
	x.f.AssertOrder(
		fakerunner.Glob("docker compose * up -d --wait --wait-timeout 900 hpds"),
		fakerunner.Glob("docker run * --name demo-dictionary-curl-* *"),
		fakerunner.Glob("docker run * --name demo-dictionary-weights-* *"),
		fakerunner.Glob("docker compose * restart dictionary-api"),
	)
}

func TestDataLoadPhenotypeChecksBeforeTouchingHPDS(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, x *phenotypeFixture)
		code  int
		want  string
	}{
		{"bad datasets", func(t *testing.T, x *phenotypeFixture) {
			x.custom(t)
			writeDemoFile(t, x.opts.Datasets, []byte("name\nsyn\n"))
		}, exitcode.CodeUsage, "ref"},
		{"old facets", func(t *testing.T, x *phenotypeFixture) {
			x.custom(t)
			writeDemoFile(t, x.opts.Facets.Facets, []byte("facet_category,facet_name,display_name,description,parent_name\nsyn,a,A,a,\n"))
		}, exitcode.CodeUsage, "facet_name(unique)"},
		{"no weights image", func(_ *testing.T, x *phenotypeFixture) {
			delete(x.state.Images, "dictionary-weights")
		}, exitcode.CodePrecondition, "no dictionary-weights image"},
		{"no etl image", func(_ *testing.T, x *phenotypeFixture) {
			x.opts.SkipWeights = true
			delete(x.state.Images, "dictionary-etl")
		}, exitcode.CodePrecondition, "no dictionary-etl image"},
		{"bad source", func(_ *testing.T, x *phenotypeFixture) {
			x.opts.Dictionary = "manual"
		}, exitcode.CodeUsage, `--dictionary must be auto or custom, not "manual"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			x := newPhenotypeFixture(t)
			c.setup(t, x)
			_, err := x.run()
			if exitcode.FromError(err) != c.code || err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v (exit %d), want exit %d and %q", err, exitcode.FromError(err), c.code, c.want)
			}
			x.f.AssertNotCalled(fakerunner.Glob("docker compose * stop hpds"))
		})
	}
}

func TestDataLoadPhenotypeSkipWeights(t *testing.T) {
	x := newPhenotypeFixture(t)
	x.opts.SkipWeights = true
	delete(x.state.Images, "dictionary-weights")
	src, err := x.opts.Cache.SourceDir("pic-sure", strings.Repeat("a", 40))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(src, filepath.FromSlash(defaultWeightsFile))); err != nil {
		t.Fatal(err)
	}
	if _, err := x.run(); err != nil {
		t.Fatal(err)
	}
	x.f.AssertNotCalled(fakerunner.Glob("docker run * --name demo-dictionary-weights-* *"))
	x.f.AssertCalled(fakerunner.Glob("docker compose * restart dictionary-api"))
}

func TestDataLoadPhenotypeDictionaryFailureKeepsHPDS(t *testing.T) {
	x := newPhenotypeFixture(t)
	x.curl = []docker.Result{{Stdout: []byte("Already running")}}
	dataset, err := x.run()
	var de *PhenotypeDictionaryError
	if !errors.As(err, &de) || de.Step != StepHydrate || de.Interrupted {
		t.Fatalf("err = %v", err)
	}
	if !strings.HasPrefix(dataset, "phenotype:") {
		t.Errorf("dataset = %q, want the load's marker", dataset)
	}
	x.f.AssertNotCalled(fakerunner.Glob("docker run * --name demo-dictionary-weights-* *"))
	x.f.AssertCalled(fakerunner.Glob("docker rm -v -f demo-dictionaryetl-*"))
}

func TestDataLoadPhenotypeRebuildsTheDictionaryWhenHPDSDoesntStart(t *testing.T) {
	x := newPhenotypeFixture(t)
	x.hpdsStartExit = 1
	dataset, err := x.run()
	var se *HPDSStartError
	if !errors.As(err, &se) || !strings.Contains(err.Error(), "`pic-sure up`") {
		t.Fatalf("err = %v", err)
	}
	var de *PhenotypeDictionaryError
	if errors.As(err, &de) {
		t.Errorf("err is a dictionary error: %v", err)
	}
	if !strings.HasPrefix(dataset, "phenotype:") {
		t.Errorf("dataset = %q", dataset)
	}
	x.f.AssertOrder(
		fakerunner.Glob("docker compose * up -d --wait --wait-timeout 900 hpds"),
		fakerunner.Glob("docker run * --name demo-columnmeta-* *"),
		fakerunner.Glob("docker run * --name demo-dictionary-weights-* *"),
		fakerunner.Glob("docker compose * restart dictionary-api"),
	)
}

func TestDataLoadPhenotypeDictionaryFailureAfterHPDSDidntStart(t *testing.T) {
	x := newPhenotypeFixture(t)
	x.hpdsStartExit = 1
	x.curl = []docker.Result{{Stdout: []byte("Already running")}}
	_, err := x.run()
	var de *PhenotypeDictionaryError
	if !errors.As(err, &de) || de.Step != StepHydrate || de.HPDSStart == nil || !strings.Contains(err.Error(), "`pic-sure up`") {
		t.Fatalf("err = %v", err)
	}
}
