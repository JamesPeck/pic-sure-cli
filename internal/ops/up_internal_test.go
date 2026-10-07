package ops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/render"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

// upFixture is a stack with a render, a fake runner whose compose ps
// reports running, and the restart tracker over them.
func upFixture(t *testing.T, running ...string) (*upRestarts, *fakerunner.Runner, *events.Recorder) {
	t.Helper()
	st, err := stack.Create(filepath.Join(t.TempDir(), "demo"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	writeFile(t, st.Path(render.ComposeFile), "name: demo\n")
	writeFile(t, st.Path(render.FilesDir+"/httpd/httpd-vhosts.conf"), "old\n")
	writeFile(t, st.Path(render.FilesDir+"/flyway/run-migrations.sh"), "same\n")

	f := fakerunner.New(t)
	var ps strings.Builder
	for _, s := range running {
		ps.WriteString(`{"Service":"` + s + `","State":"running"}` + "\n")
	}
	f.On(fakerunner.Glob("docker compose * ps *")).Stdout(ps.String())
	rec := &events.Recorder{}
	d := &Deps{Runner: f, Sink: rec}
	opts := ConvergeOptions{Compose: func() (docker.Composer, error) { return docker.NewCompose(f, st.Dir, nil) }}
	return &upRestarts{d: d, st: st, opts: opts}, f, rec
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

func applyAll(t *testing.T, r *upRestarts, list ...steps.Step) error {
	t.Helper()
	list = append(list, withCompose(r.d, r.opts, r.step()))
	return steps.Run(context.Background(), r.d.Sink, list, steps.Options{})
}

func noop(id string) steps.Step {
	return steps.Step{ID: id, Apply: func(context.Context, events.Sink) error { return nil }}
}

func TestUpRestartsARunningServiceWhoseVolumeAStepRefilled(t *testing.T) {
	r, f, _ := upFixture(t, "httpd", "psama")
	restart := fakerunner.Glob("docker compose * restart httpd")
	f.On(restart)
	if err := applyAll(t, r, r.restartAfter(noop("tls"), httpd)); err != nil {
		t.Fatal(err)
	}
	f.AssertCalled(restart)
}

func TestUpLeavesAServiceThatWasntRunningToComposeUp(t *testing.T) {
	r, f, rec := upFixture(t, "psama")
	if err := applyAll(t, r, r.restartAfter(noop("tls"), httpd)); err != nil {
		t.Fatal(err)
	}
	f.AssertNotCalled(fakerunner.Glob("docker compose * restart *"))
	if got := stepStatus(rec, RestartStepID); got != events.StepSkipped {
		t.Errorf("restart step = %s, want skipped", got)
	}
}

func TestUpRestartsTheRunningServicesThatMountAChangedRenderedFile(t *testing.T) {
	r, f, _ := upFixture(t, "httpd", "flyway-init", "dictionary-db")
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
`)
	restart := fakerunner.Glob("docker compose * restart dictionary-db httpd")
	f.On(restart)
	rerender := steps.Step{ID: "render", Apply: func(context.Context, events.Sink) error {
		writeFile(t, filepath.Join(dir, "httpd/httpd-vhosts.conf"), "new\n")
		writeFile(t, filepath.Join(dir, "flyway/run-migrations.sh"), "same\n")
		return nil
	}}
	if err := applyAll(t, r, r.watchRender(rerender)); err != nil {
		t.Fatal(err)
	}
	f.AssertCalled(restart)
}

func TestUpSkipsTheRestartWhenTheRenderChangedNothing(t *testing.T) {
	r, f, rec := upFixture(t, "httpd")
	if err := applyAll(t, r, r.watchRender(noop("render"))); err != nil {
		t.Fatal(err)
	}
	f.AssertNotCalled(fakerunner.Glob("docker compose * config *"))
	if got := stepStatus(rec, RestartStepID); got != events.StepSkipped {
		t.Errorf("restart step = %s, want skipped", got)
	}
}

func TestUpWarnsWhenTheRestartFails(t *testing.T) {
	r, f, rec := upFixture(t, "psama")
	f.On(fakerunner.Glob("docker compose * restart psama")).Stderr("boom\n").Exit(1)
	if err := applyAll(t, r, r.restartAfter(noop("truststore"), psama)); err != nil {
		t.Fatalf("a failed restart failed up: %v", err)
	}
	if !strings.Contains(warnings(rec), "run `pic-sure restart psama`") {
		t.Errorf("warnings = %q, want the restart command", warnings(rec))
	}
}

func stepStatus(rec *events.Recorder, id string) events.StepStatus {
	for _, e := range rec.Events() {
		if done, ok := e.(events.StepDone); ok && done.ID == id {
			return done.Status
		}
	}
	return ""
}

func warnings(rec *events.Recorder) string {
	var b strings.Builder
	for _, e := range rec.Events() {
		if w, ok := e.(events.Warning); ok {
			b.WriteString(w.Text + "\n")
		}
	}
	return b.String()
}
