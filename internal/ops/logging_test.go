package ops_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

func TestLoggingVolumeStep(t *testing.T) {
	for _, tc := range []struct {
		name, user string
		chown      bool
	}{
		{"a root image has nothing to do", "", false},
		{"an explicit root user has nothing to do", "0:0", false},
		{"a non-root image gets the volume", "10001:10001", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := stack.Create(filepath.Join(t.TempDir(), "demo"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			cfg := stack.DefaultConfig()
			cfg.Name = "demo"
			state := &stack.State{Images: map[string]string{"pic-sure-logging": "v1"}}
			f := fakerunner.New(t)
			inspect, err := json.Marshal([]map[string]any{{"Id": "sha256:1", "Config": map[string]any{"User": tc.user}}})
			if err != nil {
				t.Fatal(err)
			}
			f.On(fakerunner.Exact("docker", "image", "inspect", "hms-dbmi/pic-sure-logging:v1")).Stdout(string(inspect))
			f.On(fakerunner.Exact("docker", "volume", "inspect", "demo_logging-logs")).Do(func(context.Context, fakerunner.Call) (docker.Result, error) {
				out, err := json.Marshal([]map[string]any{{"Name": "demo_logging-logs", "Labels": st.Labels("demo")}})
				return docker.Result{Stdout: out}, err
			})
			chown := fakerunner.Glob("docker run --rm --pull never --name demo-logging-chown-* --user 0 --entrypoint chown --network none * -v demo_logging-logs:/app/logs hms-dbmi/pic-sure-logging:v1 -R " + tc.user + " /app/logs")
			f.On(chown)
			d := &ops.Deps{Runner: f, Docker: docker.NewEngine(f), Rand: rand.Reader, Sink: &events.Recorder{}}

			step := ops.LoggingVolumeStep(d, st, &cfg, state)
			done, err := step.Check(context.Background())
			if err != nil || done == tc.chown {
				t.Fatalf("Check = %v, %v, want %v", done, err, !tc.chown)
			}
			if !tc.chown {
				return
			}
			if err := step.Apply(context.Background(), d.Sink); err != nil {
				t.Fatal(err)
			}
			f.AssertCalled(chown)
		})
	}
}
