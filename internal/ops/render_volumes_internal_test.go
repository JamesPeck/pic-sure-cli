package ops

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

// Render keeps the labels of the stack's existing volumes, including those
// it adopted after a move, and leaves out compose's own and other stacks'.
func TestExistingVolumeLabels(t *testing.T) {
	st, err := stack.Create(filepath.Join(t.TempDir(), "demo"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	const id = "0123456789abcdef0123456789abcdef"
	if err := st.SaveState(&stack.State{StackID: id}); err != nil {
		t.Fatal(err)
	}
	compose := func(key string, l map[string]string) map[string]string {
		l = maps.Clone(l)
		l[stack.LabelComposeProject], l[stack.LabelComposeVolume] = "demo", key
		l["com.docker.compose.config-hash"] = "abc"
		return l
	}
	moved := stack.StackLabels("demo", "/stacks/gone", id)
	// Made before the stack-id label.
	old := map[string]string{stack.LabelStack: "demo", stack.LabelStackDir: CanonicalDir(st.Dir)}
	own := stack.StackLabels("demo", CanonicalDir(st.Dir), id)
	vols := []docker.Volume{
		{Name: "demo_hpds-data", Labels: compose("hpds-data", moved)},
		{Name: "demo_hpds-csv", Labels: compose("hpds-csv", old)},
		{Name: "demo_certs", Labels: compose("certs", own)},
		{Name: "demo_other", Labels: compose("other", stack.StackLabels("demo", "/x", "fedcba9876543210fedcba9876543210"))},
		{Name: "demo_unkeyed", Labels: map[string]string{stack.LabelComposeProject: "demo"}},
	}
	inspect, err := json.Marshal(vols)
	if err != nil {
		t.Fatal(err)
	}
	f := fakerunner.New(t)
	f.On(fakerunner.Exact("docker", "volume", "ls", "-q", "--filter", "label=com.docker.compose.project=demo")).
		Stdout("demo_hpds-data\ndemo_hpds-csv\ndemo_certs\ndemo_other\ndemo_unkeyed\n")
	f.On(fakerunner.Glob("docker volume inspect *")).Stdout(string(inspect))
	d := &Deps{Runner: f, Docker: docker.NewEngine(f)}

	got, err := existingVolumeLabels(context.Background(), d, st, "demo")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]map[string]string{"hpds-data": moved, "hpds-csv": old, "certs": own}
	if !maps.EqualFunc(got, want, maps.Equal) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// A compose config that would relabel an existing volume is refused; one
// that matches, after compose's $$ escape, or names no existing volume runs.
func TestCheckVolumeLabels(t *testing.T) {
	st, err := stack.Create(filepath.Join(t.TempDir(), "demo"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	labels := map[string]string{
		stack.LabelComposeProject: "demo", stack.LabelComposeVolume: "hpds-data",
		stack.LabelStack: "demo", stack.LabelStackDir: CanonicalDir(st.Dir), "x": "a$b",
	}
	inspect, err := json.Marshal([]docker.Volume{{Name: "demo_hpds-data", Labels: labels}})
	if err != nil {
		t.Fatal(err)
	}
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker volume ls -q --filter label=com.docker.compose.project=demo")).Stdout("demo_hpds-data\n")
	f.On(fakerunner.Glob("docker volume inspect *")).Stdout(string(inspect))
	d := &Deps{Runner: f, Docker: docker.NewEngine(f)}
	config := func(dir string) []byte {
		// As compose config prints it.
		return []byte("volumes:\n  hpds-data:\n    labels:\n      - org.hms-dbmi.picsure.stack=demo\n" +
			"      - org.hms-dbmi.picsure.stack-dir=" + dir + "\n      - x=a$$b\n  hpds-csv:\n    labels: {a: b}\n")
	}

	if err := CheckVolumeLabels(context.Background(), d, st, "demo", config(CanonicalDir(st.Dir))); err != nil {
		t.Errorf("matching labels: %v", err)
	}
	err = CheckVolumeLabels(context.Background(), d, st, "demo", config("/elsewhere"))
	var ee *exitcode.Error
	if !errors.As(err, &ee) || ee.Code != exitcode.CodePrecondition || !strings.Contains(err.Error(), "demo_hpds-data") {
		t.Errorf("other labels: %v, want exit 3 naming demo_hpds-data", err)
	}
}
