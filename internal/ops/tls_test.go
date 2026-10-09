package ops_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/pki"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

var t0 = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

// certsVolume fakes the docker side of the TLS step: the certs volume and
// the helper container that fills it from the tar on its stdin.
type certsVolume struct {
	f *fakerunner.Runner

	mu        sync.Mutex
	createdAt string // "" when the volume doesn't exist
	labels    map[string]string
	files     map[string][]byte // what the last helper run installed
	runs      int
	runExit   int
	runStderr string
}

const helperRun = `^docker run -i --rm --name demo-tls-[0-9a-f]{8} --network none ` +
	`--label org\.hms-dbmi\.picsure\.stack=demo --label org\.hms-dbmi\.picsure\.stack-dir=\S+ ` +
	`-v demo_certs:/certs alpine:3\.23 sh -c set -eu`

func newCertsVolume(t *testing.T) *certsVolume {
	v := &certsVolume{f: fakerunner.New(t)}
	v.f.On(fakerunner.Exact("docker", "volume", "inspect", "demo_certs")).Do(func(context.Context, fakerunner.Call) (docker.Result, error) {
		v.mu.Lock()
		defer v.mu.Unlock()
		if v.createdAt == "" {
			return docker.Result{Stderr: []byte("Error response from daemon: get demo_certs: no such volume\n"), ExitCode: 1}, nil
		}
		out, err := json.Marshal([]map[string]any{{"Name": "demo_certs", "Driver": "local", "CreatedAt": v.createdAt, "Labels": v.labels}})
		return docker.Result{Stdout: out}, err
	})
	v.f.On(fakerunner.Glob("docker volume create * demo_certs")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		v.mu.Lock()
		defer v.mu.Unlock()
		v.createdAt = "2026-10-06T12:00:00Z"
		v.labels = map[string]string{}
		for i, a := range c.Argv {
			if a == "--label" {
				k, val, _ := strings.Cut(c.Argv[i+1], "=")
				v.labels[k] = val
			}
		}
		return docker.Result{}, nil
	})
	v.f.On(fakerunner.Regex(helperRun)).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		v.mu.Lock()
		defer v.mu.Unlock()
		v.runs++
		if v.runExit != 0 {
			return docker.Result{Stderr: []byte(v.runStderr), ExitCode: v.runExit}, nil
		}
		files, err := untar(c.Stdin)
		if err != nil {
			return docker.Result{}, err
		}
		v.files = files
		return docker.Result{}, nil
	})
	return v
}

// recreate simulates the volume being removed and created again, empty.
func (v *certsVolume) recreate(createdAt string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.createdAt, v.files = createdAt, nil
}

func untar(data []byte) (map[string][]byte, error) {
	files := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg || hdr.Mode != 0o600 {
			return nil, fmt.Errorf("tar entry %s: type %c mode %o, want a 0600 regular file", hdr.Name, hdr.Typeflag, hdr.Mode)
		}
		if files[hdr.Name], err = io.ReadAll(tr); err != nil {
			return nil, err
		}
	}
}

type tlsFixture struct {
	st  *stack.Stack
	cfg *stack.Config
	vol *certsVolume
	rec *events.Recorder
	d   *ops.Deps
}

func newTLSFixture(t *testing.T) *tlsFixture {
	st, err := stack.Create(filepath.Join(t.TempDir(), "demo"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SaveState(&stack.State{CLIVersion: "2.0.0"}); err != nil {
		t.Fatal(err)
	}
	cfg := stack.DefaultConfig()
	cfg.Name = "demo"
	vol := newCertsVolume(t)
	rec := &events.Recorder{}
	return &tlsFixture{st: st, cfg: &cfg, vol: vol, rec: rec, d: &ops.Deps{
		Docker: docker.NewEngine(vol.f),
		Rand:   rand.Reader,
		Clock:  ops.FixedClock(t0),
		Sink:   rec,
	}}
}

func (fx *tlsFixture) check(t *testing.T) bool {
	t.Helper()
	done, err := ops.TLSStep(fx.d, fx.st, fx.cfg).Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return done
}

func (fx *tlsFixture) apply(t *testing.T) {
	t.Helper()
	if err := ops.TLSStep(fx.d, fx.st, fx.cfg).Apply(context.Background(), fx.rec); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

// generated reads the files generated mode keeps in .pic-sure/tls.
func (fx *tlsFixture) generated(t *testing.T) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	for _, name := range []string{"server.key", "server.crt", "server.chain"} {
		data, err := fx.st.ReadFile(ops.TLSDir + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		files[name] = data
	}
	return files
}

func (fx *tlsFixture) progress() []string {
	var texts []string
	for _, e := range fx.rec.Events() {
		if p, ok := e.(events.Progress); ok && p.ID == ops.TLSStepID {
			texts = append(texts, p.Text)
		}
	}
	return texts
}

func TestTLSStepGeneratesACertificateAndFillsTheVolume(t *testing.T) {
	fx := newTLSFixture(t)
	if fx.check(t) {
		t.Fatal("Check on a new stack = done")
	}
	fx.apply(t)

	files := fx.generated(t)
	if !maps.EqualFunc(fx.vol.files, files, bytes.Equal) {
		t.Errorf("the helper got %v, want the files in %s", slices.Sorted(maps.Keys(fx.vol.files)), ops.TLSDir)
	}
	f := pki.Files{Key: files["server.key"], Cert: files["server.crt"], Chain: files["server.chain"]}
	if r, err := pki.Validate(f, "localhost", t0); err != nil || len(r.Warnings) > 0 {
		t.Errorf("generated files: Validate = %v, %q", err, r.Warnings)
	}
	for rel, want := range map[string]fs.FileMode{ops.TLSDir: 0o700 | fs.ModeDir, ops.TLSDir + "/server.key": 0o600, ops.TLSDir + "/server.crt": 0o600, ops.TLSDir + "/server.chain": 0o600} {
		if fi, err := os.Stat(fx.st.Path(rel)); err != nil || fi.Mode() != want {
			t.Errorf("%s: mode %v (%v), want %v", rel, fi.Mode(), err, want)
		}
	}
	m, err := fx.st.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(m.Entries, func(e stack.Entry) bool { return e.Path == ops.TLSDir+"/server.key" }) {
		t.Errorf("manifest doesn't record the generated key: %v", m.Entries)
	}

	wantLabels := map[string]string{
		"com.docker.compose.project":     "demo",
		"com.docker.compose.volume":      "certs",
		"org.hms-dbmi.picsure.stack":     "demo",
		"org.hms-dbmi.picsure.stack-dir": fx.st.Dir,
	}
	if !maps.Equal(fx.vol.labels, wantLabels) {
		t.Errorf("volume labels = %v, want %v", fx.vol.labels, wantLabels)
	}
	state, err := fx.st.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if state.TLS == nil || len(state.TLS.Hash) != 64 || state.TLS.VolumeCreatedAt != "2026-10-06T12:00:00Z" || state.CLIVersion != "2.0.0" {
		t.Errorf("state = %+v, TLS %+v", state, state.TLS)
	}
	if got := fx.progress(); len(got) != 1 || got[0] != "Generating a self-signed certificate for localhost" {
		t.Errorf("progress = %q", got)
	}

	if !fx.check(t) {
		t.Error("Check after Apply = not done")
	}
}

func TestTLSStepReinstallsIntoARecreatedVolume(t *testing.T) {
	fx := newTLSFixture(t)
	fx.apply(t)
	before := fx.generated(t)

	fx.vol.recreate("2026-10-07T09:30:00Z")
	if fx.check(t) {
		t.Fatal("Check after the volume was re-created = done")
	}
	fx.apply(t)
	if !maps.EqualFunc(fx.generated(t), before, bytes.Equal) {
		t.Error("the certificate was regenerated, want the valid one reused")
	}
	if !maps.EqualFunc(fx.vol.files, before, bytes.Equal) || fx.vol.runs != 2 {
		t.Errorf("after %d helper runs the volume holds %v", fx.vol.runs, slices.Sorted(maps.Keys(fx.vol.files)))
	}
	if !fx.check(t) {
		t.Error("Check after reinstalling = not done")
	}
}

func TestTLSStepRegeneratesACertificateThatWontDo(t *testing.T) {
	tests := []struct {
		name   string
		change func(*tlsFixture)
		reason string
	}{
		{"new hostname", func(fx *tlsFixture) { fx.cfg.Network.Hostname = "picsure.example.org" }, "which doesn't name picsure.example.org"},
		{"near expiry", func(fx *tlsFixture) { fx.d.Clock = ops.FixedClock(t0.Add(pki.Validity - 29*24*time.Hour)) }, "which expires on 2027-10-06"},
		{"key replaced", func(fx *tlsFixture) {
			other, err := pki.Generate(rand.Reader, "localhost", t0)
			if err != nil {
				panic(err)
			}
			if err := fx.st.WriteFile(ops.TLSDir+"/server.key", other.Key, 0o600); err != nil {
				panic(err)
			}
		}, "which isn't valid (the private key doesn't match the certificate)"},
		{"chain removed", func(fx *tlsFixture) {
			if err := os.Remove(fx.st.Path(ops.TLSDir + "/server.chain")); err != nil {
				panic(err)
			}
		}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newTLSFixture(t)
			fx.apply(t)
			before := fx.generated(t)

			tt.change(fx)
			if fx.check(t) {
				t.Fatal("Check = done")
			}
			fx.apply(t)
			after := fx.generated(t)
			if bytes.Equal(after["server.crt"], before["server.crt"]) {
				t.Fatal("the certificate wasn't regenerated")
			}
			if !maps.EqualFunc(fx.vol.files, after, bytes.Equal) {
				t.Error("the volume doesn't hold the new files")
			}
			want := "Generating a self-signed certificate for " + fx.cfg.Network.Hostname
			if tt.reason != "" {
				want += " to replace the one in .pic-sure/tls, " + tt.reason
			}
			if got := fx.progress(); len(got) != 2 || got[1] != want {
				t.Errorf("progress = %q, want %q last", got, want)
			}
			if !fx.check(t) {
				t.Error("Check after Apply = not done")
			}
		})
	}
}

// writeOperatorFiles puts a certificate for picsure.example.org where the
// default tls block looks for the operator's files.
func writeOperatorFiles(t *testing.T, fx *tlsFixture) pki.Files {
	t.Helper()
	f, err := pki.Generate(rand.Reader, "picsure.example.org", t0.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	dir := fx.st.Path("certs/tls")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"server.key": f.Key, "server.crt": f.Cert} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fx.cfg.TLS.Mode = stack.TLSProvided
	fx.cfg.Network.Hostname = "picsure.example.org"
	return f
}

func TestTLSStepCopiesTheOperatorsFiles(t *testing.T) {
	fx := newTLSFixture(t)
	f := writeOperatorFiles(t, fx)
	if fx.check(t) {
		t.Fatal("Check = done before Apply")
	}
	fx.apply(t)

	want := map[string][]byte{"server.key": f.Key, "server.crt": f.Cert, "server.chain": f.Cert}
	if !maps.EqualFunc(fx.vol.files, want, bytes.Equal) {
		t.Error("the volume doesn't hold the operator's key and certificate, with the certificate as the chain")
	}
	if _, err := os.Stat(fx.st.Path(ops.TLSDir)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("provided mode created %s: %v", ops.TLSDir, err)
	}
	if len(fx.rec.Events()) != 0 {
		t.Errorf("events = %v, want none", fx.rec.Events())
	}
	if !fx.check(t) {
		t.Fatal("Check after Apply = not done")
	}

	// An installed certificate isn't re-validated, so its expiry doesn't
	// stop the stack.
	fx.d.Clock = ops.FixedClock(t0.Add(2 * pki.Validity))
	if !fx.check(t) {
		t.Error("Check once the installed certificate expired = not done")
	}

	// A chain file replaces the certificate as the chain.
	fx.d.Clock = ops.FixedClock(t0)
	if err := os.WriteFile(fx.st.Path("certs/tls/chain.pem"), f.Cert, 0o644); err != nil {
		t.Fatal(err)
	}
	fx.cfg.TLS.ChainFile = fx.st.Path("certs/tls/chain.pem") // absolute paths work too
	if !fx.check(t) {
		t.Error("Check with an identical chain file = not done")
	}
	chain := append(bytes.Clone(f.Cert), f.Cert...)
	if err := os.WriteFile(fx.cfg.TLS.ChainFile, chain, 0o644); err != nil {
		t.Fatal(err)
	}
	if fx.check(t) {
		t.Fatal("Check after the chain file changed = done")
	}
	fx.apply(t)
	if !bytes.Equal(fx.vol.files["server.chain"], chain) {
		t.Error("the volume's chain isn't the chain file")
	}
}

func TestTLSStepWarnsWhenTheOperatorsCertificateDoesntNameTheHostname(t *testing.T) {
	fx := newTLSFixture(t)
	writeOperatorFiles(t, fx)
	fx.cfg.Network.Hostname = "other.example.org"
	fx.apply(t)
	evs := fx.rec.Events()
	if len(evs) != 1 {
		t.Fatalf("events = %v, want one warning", evs)
	}
	if w, ok := evs[0].(events.Warning); !ok || w.ID != ops.TLSStepID || !strings.Contains(w.Text, `doesn't name "other.example.org"`) {
		t.Errorf("event = %#v", evs[0])
	}
}

func TestTLSStepRefusesUnusableOperatorFiles(t *testing.T) {
	tests := []struct {
		name   string
		change func(*testing.T, *tlsFixture)
		want   string
	}{
		{"missing key", func(t *testing.T, fx *tlsFixture) { fx.cfg.TLS.KeyFile = "certs/tls/nope.key" }, "tls.key_file: open "},
		{"missing chain", func(t *testing.T, fx *tlsFixture) { fx.cfg.TLS.ChainFile = "nope.pem" }, "tls.chain_file: open "},
		{"empty chain", func(t *testing.T, fx *tlsFixture) {
			if err := os.WriteFile(fx.st.Path("empty.pem"), []byte("\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			fx.cfg.TLS.ChainFile = "empty.pem"
		}, "tls.chain_file: empty.pem is empty"},
		{"key mismatch", func(t *testing.T, fx *tlsFixture) {
			other, err := pki.Generate(rand.Reader, "picsure.example.org", t0)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(fx.st.Path("certs/tls/server.key"), other.Key, 0o600); err != nil {
				t.Fatal(err)
			}
		}, "tls.mode is provided, but the files won't do: the private key doesn't match the certificate"},
		{"expired", func(t *testing.T, fx *tlsFixture) { fx.d.Clock = ops.FixedClock(t0.Add(pki.Validity)) }, "the certificate has expired"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newTLSFixture(t)
			writeOperatorFiles(t, fx)
			tt.change(t, fx)
			if fx.check(t) {
				t.Error("Check = done")
			}
			err := ops.TLSStep(fx.d, fx.st, fx.cfg).Apply(context.Background(), fx.rec)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Apply = %v, want %q", err, tt.want)
			}
			if fx.vol.runs != 0 || fx.vol.createdAt != "" {
				t.Error("Apply touched docker")
			}
		})
	}
}

func TestTLSStepRefusesAVolumeItDoesntOwn(t *testing.T) {
	for _, labels := range []map[string]string{nil, {"org.hms-dbmi.picsure.stack": "other"}} {
		fx := newTLSFixture(t)
		fx.vol.createdAt, fx.vol.labels = "2026-01-01T00:00:00Z", labels
		err := ops.TLSStep(fx.d, fx.st, fx.cfg).Apply(context.Background(), fx.rec)
		if err == nil || !strings.Contains(err.Error(), "volume demo_certs belongs to ") {
			t.Errorf("labels %v: Apply = %v", labels, err)
		}
		if fx.vol.runs != 0 {
			t.Errorf("labels %v: the helper ran", labels)
		}
	}
}

func TestTLSStepReportsAFailedCopy(t *testing.T) {
	fx := newTLSFixture(t)
	fx.vol.runExit, fx.vol.runStderr = 1, "tar: short read\nchown: server.key: Operation not permitted\n"
	err := ops.TLSStep(fx.d, fx.st, fx.cfg).Apply(context.Background(), fx.rec)
	want := "copying the TLS files into volume demo_certs: the helper container exited 1: chown: server.key: Operation not permitted"
	if err == nil || err.Error() != want {
		t.Errorf("Apply = %v, want %q", err, want)
	}
	if state, _ := fx.st.LoadState(); state.TLS != nil {
		t.Errorf("a failed copy was recorded: %+v", state.TLS)
	}
	if fx.check(t) {
		t.Error("Check after a failed copy = done")
	}
}

// A copy that fails while replacing an install leaves it unrecorded, so
// restoring the old files doesn't make Check skip the repair.
func TestTLSStepForgetsAnInstallItFailsToReplace(t *testing.T) {
	fx := newTLSFixture(t)
	if err := ops.TLSStep(fx.d, fx.st, fx.cfg).Apply(context.Background(), fx.rec); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(fx.st.Path(ops.TLSDir + "/server.chain")); err != nil {
		t.Fatal(err)
	}
	fx.vol.runExit = 1
	if err := ops.TLSStep(fx.d, fx.st, fx.cfg).Apply(context.Background(), fx.rec); err == nil {
		t.Fatal("Apply succeeded")
	}
	if state, _ := fx.st.LoadState(); state.TLS != nil {
		t.Errorf("the replaced install is still recorded: %+v", state.TLS)
	}
}

// The helper container gets the key only on stdin, never in argv or env.
func TestTLSStepKeepsTheKeyOutOfArgv(t *testing.T) {
	fx := newTLSFixture(t)
	fx.apply(t)
	key := fx.generated(t)["server.key"]
	calls := fx.vol.f.CallsMatching(fakerunner.Regex(helperRun))
	if len(calls) != 1 {
		t.Fatalf("%d helper runs", len(calls))
	}
	if len(calls[0].Env) != 0 || regexp.MustCompile(`PRIVATE KEY`).MatchString(strings.Join(calls[0].Argv, " ")) {
		t.Errorf("helper call = %s, env %v", calls[0], calls[0].Env)
	}
	if !bytes.Contains(calls[0].Stdin, key) {
		t.Error("the key isn't on the helper's stdin")
	}
}
