package ops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/render"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

// upFixture is a stack with a render and state.json, a fake runner whose
// compose ps reports running, and the restart tracker over them.
func upFixture(t *testing.T, running ...string) (*upRestarts, *fakerunner.Runner) {
	t.Helper()
	st, err := stack.Create(filepath.Join(t.TempDir(), "demo"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SaveState(&stack.State{}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, st.Path(render.ComposeFile), "name: demo\n")
	writeFile(t, st.Path(render.FilesDir+"/httpd/httpd-vhosts.conf"), "old\n")
	writeFile(t, st.Path(render.FilesDir+"/flyway/run-migrations.sh"), "same\n")

	f := fakerunner.New(t)
	var ps strings.Builder
	for _, s := range running {
		ps.WriteString(`{"Service":"` + s + `","State":"running"}` + "\n")
	}
	f.On(fakerunner.Glob("docker compose * ps *")).Stdout(ps.String())
	d := &Deps{Runner: f, Sink: &events.Recorder{}}
	opts := ConvergeOptions{Compose: func() (docker.Composer, error) { return docker.NewCompose(f, st.Dir, nil) }}
	cfg := stack.DefaultConfig()
	return &upRestarts{d: d, st: st, cfg: &cfg, opts: opts}, f
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// run runs list, then the restart step, the way UpSteps orders them.
func run(t *testing.T, r *upRestarts, list ...steps.Step) error {
	t.Helper()
	list = append(list, withCompose(r.d, r.opts, r.step()))
	return steps.Run(context.Background(), r.d.Sink, list, steps.Options{})
}

func noop(id string) steps.Step {
	return steps.Step{ID: id, Apply: func(context.Context, events.Sink) error { return nil }}
}

func fail(id string) steps.Step {
	return steps.Step{ID: id, Apply: func(context.Context, events.Sink) error { return errors.New("boom") }}
}

func pendingRestarts(t *testing.T, r *upRestarts) []string {
	t.Helper()
	state, err := r.st.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	return state.PendingRestarts
}

func TestUpRestartsARunningServiceWhoseVolumeAStepRefilled(t *testing.T) {
	r, f := upFixture(t, "httpd", "psama")
	restart := fakerunner.Glob("docker compose * restart httpd")
	f.On(restart)
	if err := run(t, r, r.restartAfter(noop("tls"), httpd)); err != nil {
		t.Fatal(err)
	}
	f.AssertCalled(restart)
	if got := pendingRestarts(t, r); len(got) != 0 {
		t.Errorf("PendingRestarts = %v after the restart", got)
	}
}

func TestUpLeavesAStoppedServiceToComposeUp(t *testing.T) {
	r, f := upFixture(t, "psama")
	if err := run(t, r, r.restartAfter(noop("tls"), httpd)); err != nil {
		t.Fatal(err)
	}
	f.AssertNotCalled(fakerunner.Glob("docker compose * restart *"))
	if got := pendingRestarts(t, r); len(got) != 0 {
		t.Errorf("PendingRestarts = %v, want it cleared", got)
	}
}

func TestUpKeepsARestartAcrossAFailedRun(t *testing.T) {
	r, f := upFixture(t, "psama")
	if err := run(t, r, r.restartAfter(noop("truststore"), psama), fail("db")); err == nil {
		t.Fatal("the run didn't fail")
	}
	if got := pendingRestarts(t, r); !slices.Equal(got, []string{psama}) {
		t.Fatalf("PendingRestarts = %v after the failed run, want [psama]", got)
	}

	// The re-run's truststore step is done, but psama still restarts.
	restart := fakerunner.Glob("docker compose * restart psama")
	f.On(restart)
	if err := run(t, r, noop("truststore")); err != nil {
		t.Fatal(err)
	}
	f.AssertCalled(restart)
}

func TestUpKeepsTheRestartWhenItFails(t *testing.T) {
	r, f := upFixture(t, "psama")
	f.On(fakerunner.Glob("docker compose * restart psama")).Stderr("boom\n").Exit(1)
	if err := run(t, r, r.restartAfter(noop("truststore"), psama)); err == nil {
		t.Fatal("a failed restart didn't fail the run")
	}
	if got := pendingRestarts(t, r); !slices.Equal(got, []string{psama}) {
		t.Errorf("PendingRestarts = %v, want [psama] kept for the re-run", got)
	}
}

func TestUpRestartsTheServicesThatMountAChangedRenderedFile(t *testing.T) {
	r, f := upFixture(t, "httpd", "dictionary-db", "gateway")
	dir := r.st.Path(render.FilesDir)
	f.On(fakerunner.Glob("docker compose * config --no-interpolate")).Stdout(`services:
  httpd:
    volumes:
      - {type: bind, source: ` + dir + `/httpd/httpd-vhosts.conf, target: /usr/local/apache2/conf/extra/httpd-vhosts.conf}
      - {type: volume, source: certs, target: /certs}
  flyway-init:
    volumes:
      - {type: bind, source: ` + dir + `/flyway/run-migrations.sh, target: /run.sh}
  dictionary-db:
    volumes:
      - {type: bind, source: ` + dir + `, target: /files}
  gateway: {}
`)
	restart := fakerunner.Glob("docker compose * restart dictionary-db httpd")
	f.On(restart)
	rerender := steps.Step{ID: "render", Apply: func(context.Context, events.Sink) error {
		writeFile(t, filepath.Join(dir, "httpd/httpd-vhosts.conf"), "new\n")
		writeFile(t, filepath.Join(dir, "flyway/run-migrations.sh"), "same\n")
		return nil
	}}
	if err := run(t, r, r.watchRender(rerender)); err != nil {
		t.Fatal(err)
	}
	f.AssertCalled(restart)
}

func TestUpSkipsTheRestartWhenTheRenderChangedNothing(t *testing.T) {
	r, f := upFixture(t, "httpd")
	if err := run(t, r, r.watchRender(noop("render"))); err != nil {
		t.Fatal(err)
	}
	f.AssertNotCalled(fakerunner.Glob("docker compose * config *"))
	f.AssertNotCalled(fakerunner.Glob("docker compose * restart *"))
}

func TestUpMarksTheRestartEvenWhenTheStepFails(t *testing.T) {
	r, _ := upFixture(t, "httpd")
	if err := run(t, r, r.restartAfter(fail("tls"), httpd)); err == nil {
		t.Fatal("the run didn't fail")
	}
	if got := pendingRestarts(t, r); !slices.Equal(got, []string{httpd}) {
		t.Errorf("PendingRestarts = %v, want [httpd]", got)
	}
}

func TestUpMarksTheReadersOfFilesARenderWroteBeforeFailing(t *testing.T) {
	r, f := upFixture(t, "httpd")
	dir := r.st.Path(render.FilesDir)
	f.On(fakerunner.Glob("docker compose * config --no-interpolate")).Stdout(`services:
  httpd:
    volumes:
      - {type: bind, source: ` + dir + `/httpd/httpd-vhosts.conf, target: /conf}
`)
	partial := steps.Step{ID: "render", Apply: func(context.Context, events.Sink) error {
		writeFile(t, filepath.Join(dir, "httpd/httpd-vhosts.conf"), "new\n")
		return errors.New("disk full")
	}}
	if err := run(t, r, r.watchRender(partial)); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v, want the render's", err)
	}
	if got := pendingRestarts(t, r); !slices.Equal(got, []string{httpd}) {
		t.Errorf("PendingRestarts = %v, want [httpd]", got)
	}
}
