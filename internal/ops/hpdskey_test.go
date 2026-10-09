package ops_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

// hpdsVolume fakes the hpds-data volume and the helper that writes the key.
type hpdsVolume struct {
	mu        sync.Mutex
	createdAt string
	owner     string
	key       []byte
	runs      int
	runExit   int
}

type hpdsKeyFixture struct {
	st  *stack.Stack
	cfg *stack.Config
	vol *hpdsVolume
	f   *fakerunner.Runner
	d   *ops.Deps
}

// ownerLabels are the stack labels of a volume of stack owner: st's, when
// owner is st's name "demo".
func ownerLabels(st *stack.Stack, owner string) map[string]string {
	if owner == "demo" {
		return st.Labels("demo")
	}
	return map[string]string{stack.LabelStack: owner, stack.LabelStackDir: "/stacks/" + owner}
}

// labelsJSON is st's labels for stack demo, as docker inspect has them.
func labelsJSON(st *stack.Stack) string {
	b, _ := json.Marshal(st.Labels("demo"))
	return string(b)
}

func newHPDSKeyFixture(t *testing.T) *hpdsKeyFixture {
	st, err := stack.Create(filepath.Join(t.TempDir(), "demo"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.EnsureSecrets(rand.Reader, stack.EnsureOptions{OpenAuth: true}); err != nil {
		t.Fatal(err)
	}
	cfg := stack.DefaultConfig()
	cfg.Name = "demo"
	v := &hpdsVolume{}
	f := fakerunner.New(t)
	f.On(fakerunner.Exact("docker", "volume", "inspect", "demo_hpds-data")).Do(func(context.Context, fakerunner.Call) (docker.Result, error) {
		v.mu.Lock()
		defer v.mu.Unlock()
		if v.createdAt == "" {
			return docker.Result{Stderr: []byte("Error response from daemon: get demo_hpds-data: no such volume\n"), ExitCode: 1}, nil
		}
		out, err := json.Marshal([]map[string]any{{"Name": "demo_hpds-data", "CreatedAt": v.createdAt,
			"Labels": ownerLabels(st, v.owner)}})
		return docker.Result{Stdout: out}, err
	})
	f.On(fakerunner.Glob("docker volume create * demo_hpds-data")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		v.mu.Lock()
		defer v.mu.Unlock()
		v.createdAt, v.owner = "2026-10-07T12:00:00Z", "demo"
		return docker.Result{}, nil
	})
	f.On(fakerunner.Glob("docker run -i --rm --name demo-hpds-key-* --network none * -v demo_hpds-data:/data alpine:* sh -c *")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		v.mu.Lock()
		defer v.mu.Unlock()
		v.runs++
		if v.runExit != 0 {
			return docker.Result{Stderr: []byte("sh: can't create /data/.encryption_key.incoming\n"), ExitCode: v.runExit}, nil
		}
		v.key = c.Stdin
		return docker.Result{}, nil
	})
	f.On(fakerunner.Glob("docker rm -v -f demo-hpds-key-*"))
	return &hpdsKeyFixture{st: st, cfg: &cfg, vol: v, f: f, d: &ops.Deps{
		Runner: f, Docker: docker.NewEngine(f), Rand: rand.Reader, Clock: ops.FixedClock(t0), Sink: &events.Recorder{},
	}}
}

func (fx *hpdsKeyFixture) check(t *testing.T) bool {
	t.Helper()
	done, err := ops.HPDSKeyStep(fx.d, fx.st, fx.cfg).Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return done
}

func (fx *hpdsKeyFixture) apply(t *testing.T) error {
	t.Helper()
	return ops.HPDSKeyStep(fx.d, fx.st, fx.cfg).Apply(context.Background(), fx.d.Sink)
}

func TestHPDSKeyStepCopiesTheKeyIntoTheVolume(t *testing.T) {
	fx := newHPDSKeyFixture(t)
	if fx.check(t) {
		t.Fatal("Check is done before the key was copied")
	}
	if err := fx.apply(t); err != nil {
		t.Fatal(err)
	}
	key, err := fx.st.LoadHPDSKey()
	if err != nil {
		t.Fatal(err)
	}
	if string(fx.vol.key) != string(key)+"\n" {
		t.Errorf("the helper got %q on stdin, want the key file", fx.vol.key)
	}
	for _, c := range fx.f.Calls() {
		if strings.Contains(strings.Join(c.Argv, " "), string(key)) {
			t.Errorf("the key is in argv: %s", c)
		}
	}
	if !fx.check(t) {
		t.Error("Check isn't done after Apply")
	}

	// A volume re-created since (by reset) is keyed again.
	fx.vol.createdAt = "2026-10-08T12:00:00Z"
	if fx.check(t) {
		t.Error("Check is done for a re-created volume")
	}
}

func TestHPDSKeyStepRefusesAnotherStacksVolume(t *testing.T) {
	fx := newHPDSKeyFixture(t)
	fx.vol.createdAt, fx.vol.owner = "2026-10-07T12:00:00Z", "other"
	err := fx.apply(t)
	if err == nil || !strings.Contains(err.Error(), "belongs to stack other in /stacks/other") {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if fx.vol.runs != 0 {
		t.Error("the helper ran")
	}
}

func TestHPDSKeyStepReportsAFailedCopy(t *testing.T) {
	fx := newHPDSKeyFixture(t)
	fx.vol.runExit = 1
	err := fx.apply(t)
	if err == nil || !strings.Contains(err.Error(), "exited 1: sh: can't create") {
		t.Fatalf("err = %v", err)
	}
	if fx.check(t) {
		t.Error("Check is done after a failed copy")
	}
}

func TestHPDSKeyStepDoesNothingForSharedData(t *testing.T) {
	fx := newHPDSKeyFixture(t)
	fx.cfg.HPDS.Data, fx.cfg.HPDS.SharedName = stack.HPDSShared, "demo-set"
	if !fx.check(t) {
		t.Error("Check isn't done with shared data")
	}
	if err := fx.apply(t); err != nil || len(fx.f.Calls()) != 0 {
		t.Errorf("Apply with shared data: err %v, calls %v", err, fx.f.Calls())
	}
}
