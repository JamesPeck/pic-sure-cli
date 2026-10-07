package ops_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"maps"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

const tsImage = "hms-dbmi/pic-sure-psama:0123456789ab"

// newCACert returns a self-signed CA certificate, DER-encoded.
func newCACert(t testing.TB, cn string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func pemCerts(ders ...[]byte) []byte {
	var b bytes.Buffer
	for _, der := range ders {
		_ = pem.Encode(&b, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	}
	return b.Bytes()
}

func writeTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// newTrustStack creates a stack named demo whose certs directory is the
// default, certs/trust, and doesn't exist yet.
func newTrustStack(t *testing.T) (*stack.Stack, *stack.Config) {
	t.Helper()
	st, err := stack.Create(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := stack.DefaultConfig()
	cfg.Name = "demo"
	return st, &cfg
}

func TestCustomCerts(t *testing.T) {
	st, cfg := newTrustStack(t)
	if certs, err := ops.CustomCerts(st, cfg); err != nil || certs != nil {
		t.Fatalf("missing directory: certs = %v, err = %v; want none", certs, err)
	}

	dir := st.Path("certs/trust")
	a, b1, b2, c, linked := newCACert(t, "a"), newCACert(t, "b1"), newCACert(t, "b2"), newCACert(t, "c"), newCACert(t, "linked")
	writeTestFile(t, filepath.Join(dir, "a.crt"), pemCerts(a))
	// A bundle in CRLF with a byte order mark and a header line.
	bundle := append([]byte("\ufeffsubject=CN = b1\n"), pemCerts(b1, b2)...)
	bundle = bytes.ReplaceAll(bundle, []byte("\n"), []byte("\r\n"))
	writeTestFile(t, filepath.Join(dir, "b.pem"), bundle)
	writeTestFile(t, filepath.Join(dir, "My CA (2).DER"), c)
	outside := filepath.Join(t.TempDir(), "elsewhere.pem")
	writeTestFile(t, outside, pemCerts(linked))
	if err := os.Symlink(outside, filepath.Join(dir, "link.cer")); err != nil {
		t.Fatal(err)
	}
	// Ignored: hidden files, other extensions, directories.
	writeTestFile(t, filepath.Join(dir, "._a.crt"), []byte("AppleDouble junk"))
	writeTestFile(t, filepath.Join(dir, "README.txt"), []byte("not a cert"))
	writeTestFile(t, filepath.Join(dir, "a.key"), []byte("not a cert"))
	if err := os.Mkdir(filepath.Join(dir, "sub.crt"), 0o755); err != nil {
		t.Fatal(err)
	}

	certs, err := ops.CustomCerts(st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := []ops.CustomCert{
		{File: "My CA (2).DER", Alias: "custom-1-myca2.der", DER: c},
		{File: "a.crt", Alias: "custom-2-a.crt", DER: a},
		{File: "b.pem", Alias: "custom-3-b.pem", DER: b1},
		{File: "b.pem", Alias: "custom-4-b.pem", DER: b2},
		{File: "link.cer", Alias: "custom-5-link.cer", DER: linked},
	}
	if !slices.EqualFunc(certs, want, func(x, y ops.CustomCert) bool {
		return x.File == y.File && x.Alias == y.Alias && bytes.Equal(x.DER, y.DER)
	}) {
		t.Errorf("certs:\n%v\nwant:\n%v", aliases(certs), aliases(want))
	}

	// An absolute directory is used as it is.
	cfg.Trust.CustomCertsDir = filepath.Dir(outside)
	if certs, err := ops.CustomCerts(st, cfg); err != nil || len(certs) != 1 || certs[0].Alias != "custom-1-elsewhere.pem" {
		t.Errorf("absolute directory: certs = %v, err = %v", aliases(certs), err)
	}
	cfg.Trust.CustomCertsDir = ""
	if certs, err := ops.CustomCerts(st, cfg); err != nil || certs != nil {
		t.Errorf("no directory configured: certs = %v, err = %v", aliases(certs), err)
	}
}

func TestCustomCertsRejectsAFileItCantFullyRead(t *testing.T) {
	good := pemCerts(newCACert(t, "good"))
	corrupt := bytes.Replace(pemCerts(newCACert(t, "corrupt")), []byte("\n"), []byte("\n!"), 2)
	for name, data := range map[string][]byte{
		"key.pem":       pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte{1}}),
		"empty.der":     []byte("\n"),
		"corrupt.pem":   slices.Concat(good, corrupt, good),
		"truncated.pem": slices.Concat(good, good[:len(good)-30]),
		"trusted.pem":   slices.Concat(good, pem.EncodeToMemory(&pem.Block{Type: "TRUSTED CERTIFICATE", Bytes: []byte{1}})),
	} {
		t.Run(name, func(t *testing.T) {
			st, cfg := newTrustStack(t)
			writeTestFile(t, st.Path("certs/trust/"+name), data)
			_, err := ops.CustomCerts(st, cfg)
			if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "trust.custom_certs_dir") {
				t.Errorf("err = %v, want one naming trust.custom_certs_dir and %s", err, name)
			}
		})
	}

	st, cfg := newTrustStack(t)
	writeTestFile(t, st.Path("certs/trust"), []byte("a file"))
	if _, err := ops.CustomCerts(st, cfg); err == nil {
		t.Error("certs directory is a file: no error")
	}
}

func aliases(certs []ops.CustomCert) []string {
	var out []string
	for _, c := range certs {
		out = append(out, c.Alias)
	}
	return out
}

// trustVolume fakes the docker side of the truststore step: the psama
// image, whose ID the test can change, and the demo_truststore volume, which
// exists once created.
type trustVolume struct {
	f         *fakerunner.Runner
	imageID   string
	exists    bool
	labels    map[string]string
	createdAt string
}

func newTrustVolume(t *testing.T) *trustVolume {
	v := &trustVolume{f: fakerunner.New(t), imageID: "sha256:aaaa", createdAt: "2026-10-06T12:00:00Z"}
	v.f.On(fakerunner.Exact("docker", "image", "inspect", tsImage)).Do(func(context.Context, fakerunner.Call) (docker.Result, error) {
		return docker.Result{Stdout: []byte(`[{"Id":"` + v.imageID + `"}]`)}, nil
	})
	v.f.On(fakerunner.Exact("docker", "volume", "inspect", "demo_truststore")).Do(func(context.Context, fakerunner.Call) (docker.Result, error) {
		if !v.exists {
			return docker.Result{ExitCode: 1, Stderr: []byte("Error response from daemon: get demo_truststore: no such volume\n")}, nil
		}
		out, _ := json.Marshal([]docker.Volume{{Name: "demo_truststore", Labels: v.labels, CreatedAt: v.createdAt}})
		return docker.Result{Stdout: out}, nil
	})
	v.f.On(fakerunner.Glob("docker volume create * demo_truststore")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		v.exists, v.labels = true, map[string]string{}
		for i, a := range c.Argv {
			if a == "--label" {
				k, val, _ := strings.Cut(c.Argv[i+1], "=")
				v.labels[k] = val
			}
		}
		return docker.Result{Stdout: []byte("demo_truststore\n")}, nil
	})
	return v
}

func newTrustDeps(f *fakerunner.Runner, rec *events.Recorder) *ops.Deps {
	return &ops.Deps{
		Runner: f,
		Docker: docker.NewEngine(f),
		Rand:   bytes.NewReader(bytes.Repeat([]byte{0xab}, 64)),
		Sink:   rec,
	}
}

func runTrustStep(t *testing.T, d *ops.Deps, st *stack.Stack, cfg *stack.Config) error {
	t.Helper()
	return steps.Run(context.Background(), d.Sink, []steps.Step{ops.TruststoreStep(d, st, cfg, tsImage)}, steps.Options{})
}

func trustDone(t *testing.T, d *ops.Deps, st *stack.Stack, cfg *stack.Config) bool {
	t.Helper()
	done, err := ops.TruststoreStep(d, st, cfg, tsImage).Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return done
}

func TestTruststoreStepDoesNothingWithoutCustomCerts(t *testing.T) {
	st, cfg := newTrustStack(t)
	writeTestFile(t, st.Path("certs/trust/README.txt"), []byte("put CA certs here"))
	f := fakerunner.New(t) // no rules: any docker call fails the test
	var rec events.Recorder
	if err := runTrustStep(t, newTrustDeps(f, &rec), st, cfg); err != nil {
		t.Fatal(err)
	}
	if got := rec.Events()[len(rec.Events())-1]; got != (events.StepDone{ID: ops.TruststoreStepID, Status: events.StepSkipped}) {
		t.Errorf("last event = %#v, want the step skipped", got)
	}
}

func TestTruststoreStepImportsTheCertsIntoANewVolume(t *testing.T) {
	st, cfg := newTrustStack(t)
	a, b1, b2 := newCACert(t, "a"), newCACert(t, "b1"), newCACert(t, "b2")
	writeTestFile(t, st.Path("certs/trust/a.crt"), pemCerts(a))
	writeTestFile(t, st.Path("certs/trust/b.der"), b1)
	writeTestFile(t, st.Path("certs/trust/c.pem"), pemCerts(b2))
	v := newTrustVolume(t)
	v.f.On(fakerunner.Glob("docker run *"))
	var rec events.Recorder
	d := newTrustDeps(v.f, &rec)

	if trustDone(t, d, st, cfg) {
		t.Fatal("Check says done before anything ran")
	}
	if err := runTrustStep(t, d, st, cfg); err != nil {
		t.Fatal(err)
	}

	wantLabels := st.VolumeLabels("demo", "truststore")
	if !v.exists || !maps.Equal(v.labels, wantLabels) {
		t.Errorf("volume labels = %v, want %v", v.labels, wantLabels)
	}
	runs := v.f.CallsMatching(fakerunner.Glob("docker run *"))
	if len(runs) != 1 {
		t.Fatalf("docker run calls = %d, want 1", len(runs))
	}
	argv := runs[0].Argv
	wantArgv := []string{"docker", "run", "-i", "--rm", "--name", "demo-truststore-abababab",
		"--user", "0", "--entrypoint", "sh", "--network", "none",
		"--label", stack.LabelStack + "=demo", "--label", stack.LabelStackDir + "=" + st.Dir,
		"-v", "demo_truststore:/truststore", tsImage,
		"-c", "SCRIPT", "sh", "custom-1-a.crt", "custom-2-b.der", "custom-3-c.pem"}
	if len(argv) == len(wantArgv) {
		argv = slices.Clone(argv)
		argv[slices.Index(wantArgv, "SCRIPT")] = "SCRIPT"
	}
	if !slices.Equal(argv, wantArgv) {
		t.Errorf("docker run argv:\n%q\nwant:\n%q", argv, wantArgv)
	}
	if got, want := runs[0].Stdin, pemCerts(a, b1, b2); !bytes.Equal(got, want) {
		t.Errorf("helper stdin:\n%s\nwant the certs in alias order as PEM:\n%s", got, want)
	}
	v.f.AssertOrder(
		fakerunner.Glob("docker volume create *"),
		fakerunner.Glob("docker run *"),
	)

	state, err := st.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if b := state.Truststore; b == nil || !strings.HasPrefix(b.Hash, "sha256:") || b.VolumeCreatedAt != v.createdAt {
		t.Errorf("state.json truststore = %+v", b)
	}
	if !trustDone(t, d, st, cfg) {
		t.Error("Check says not done right after Apply")
	}
}

func TestTruststoreStepRebuilds(t *testing.T) {
	for name, change := range map[string]func(t *testing.T, st *stack.Stack, v *trustVolume){
		"when the image changes": func(_ *testing.T, _ *stack.Stack, v *trustVolume) { v.imageID = "sha256:bbbb" },
		"when the volume was recreated": func(_ *testing.T, _ *stack.Stack, v *trustVolume) {
			v.createdAt = "2026-10-07T09:00:00Z"
		},
		"when the volume is gone": func(_ *testing.T, _ *stack.Stack, v *trustVolume) { v.exists = false },
		"when a cert changes": func(t *testing.T, st *stack.Stack, _ *trustVolume) {
			writeTestFile(t, st.Path("certs/trust/a.crt"), pemCerts(newCACert(t, "a2")))
		},
		"when a cert is added": func(t *testing.T, st *stack.Stack, _ *trustVolume) {
			writeTestFile(t, st.Path("certs/trust/b.crt"), pemCerts(newCACert(t, "b")))
		},
		"when a cert is renamed": func(t *testing.T, st *stack.Stack, _ *trustVolume) {
			if err := os.Rename(st.Path("certs/trust/a.crt"), st.Path("certs/trust/z.crt")); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			st, cfg := newTrustStack(t)
			writeTestFile(t, st.Path("certs/trust/a.crt"), pemCerts(newCACert(t, "a")))
			v := newTrustVolume(t)
			v.f.On(fakerunner.Glob("docker run *"))
			var rec events.Recorder
			d := newTrustDeps(v.f, &rec)
			if err := runTrustStep(t, d, st, cfg); err != nil {
				t.Fatal(err)
			}
			change(t, st, v)
			if trustDone(t, d, st, cfg) {
				t.Error("Check says done")
			}
		})
	}
}

func TestTruststoreStepRefusesAVolumeThatIsntTheStacks(t *testing.T) {
	for owner, labels := range map[string]map[string]string{
		`"other"`: {stack.LabelStack: "other"},
		`""`:      {},
	} {
		t.Run(owner, func(t *testing.T) {
			st, cfg := newTrustStack(t)
			writeTestFile(t, st.Path("certs/trust/a.crt"), pemCerts(newCACert(t, "a")))
			v := newTrustVolume(t)
			v.exists, v.labels = true, labels
			var rec events.Recorder
			err := runTrustStep(t, newTrustDeps(v.f, &rec), st, cfg)
			if exitcode.FromError(err) != exitcode.CodePrecondition || !strings.Contains(err.Error(), owner) {
				t.Errorf("err = %v (exit %d), want exit 3 naming owner %s", err, exitcode.FromError(err), owner)
			}
			v.f.AssertNotCalled(fakerunner.Glob("docker run *"))
		})
	}
}

func TestTruststoreStepIsNotDoneWithoutState(t *testing.T) {
	st, cfg := newTrustStack(t)
	writeTestFile(t, st.Path("certs/trust/a.crt"), pemCerts(newCACert(t, "a")))
	v := newTrustVolume(t)
	v.exists, v.labels = true, st.VolumeLabels("demo", "truststore")
	var rec events.Recorder
	if trustDone(t, newTrustDeps(v.f, &rec), st, cfg) {
		t.Error("Check says done with no state.json")
	}
}

// A rebuild that fails after the helper may have replaced the truststore
// must not leave the old record, or going back to the old certs would skip
// the rebuild.
func TestTruststoreStepFailedRebuildForgetsTheOldOne(t *testing.T) {
	st, cfg := newTrustStack(t)
	old := pemCerts(newCACert(t, "a"))
	writeTestFile(t, st.Path("certs/trust/a.crt"), old)
	v := newTrustVolume(t)
	v.f.On(fakerunner.Glob("docker run *")).Times(1)
	v.f.On(fakerunner.Glob("docker run *")).Exit(1)
	var rec events.Recorder
	d := newTrustDeps(v.f, &rec)
	if err := runTrustStep(t, d, st, cfg); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, st.Path("certs/trust/a.crt"), pemCerts(newCACert(t, "b")))
	if err := runTrustStep(t, d, st, cfg); err == nil {
		t.Fatal("the failed rebuild returned no error")
	}
	writeTestFile(t, st.Path("certs/trust/a.crt"), old)
	if trustDone(t, d, st, cfg) {
		t.Error("Check says done for the old certs after a failed rebuild")
	}
}

func TestTruststoreStepReportsAFailedImport(t *testing.T) {
	st, cfg := newTrustStack(t)
	writeTestFile(t, st.Path("certs/trust/a.crt"), pemCerts(newCACert(t, "a")))
	v := newTrustVolume(t)
	v.f.On(fakerunner.Glob("docker run *")).Exit(1).
		Stderr("custom-1-a.crt: keytool error: java.lang.Exception: Input not an X.509 certificate\n")
	var rec events.Recorder
	err := runTrustStep(t, newTrustDeps(v.f, &rec), st, cfg)
	if err == nil || !strings.Contains(err.Error(), "exited 1: custom-1-a.crt: keytool error") {
		t.Errorf("err = %v, want the helper's last stderr line", err)
	}
	var se *steps.Error
	if !errors.As(err, &se) || se.Step != ops.TruststoreStepID {
		t.Errorf("err = %v, want a *steps.Error for the truststore step", err)
	}
	if state, err := st.LoadState(); err == nil && state.Truststore != nil {
		t.Errorf("a failed build was recorded: %+v", state.Truststore)
	}
}

func TestTruststoreStepRemovesAnInterruptedHelper(t *testing.T) {
	st, cfg := newTrustStack(t)
	writeTestFile(t, st.Path("certs/trust/a.crt"), pemCerts(newCACert(t, "a")))
	v := newTrustVolume(t)
	v.f.On(fakerunner.Glob("docker run *")).Err(context.Canceled)
	v.f.On(fakerunner.Exact("docker", "rm", "-v", "-f", "demo-truststore-abababab"))
	var rec events.Recorder
	if err := runTrustStep(t, newTrustDeps(v.f, &rec), st, cfg); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	v.f.AssertOrder(fakerunner.Glob("docker run *"), fakerunner.Glob("docker rm *"))
}
