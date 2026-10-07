package ops_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

var keystoreEntries = regexp.MustCompile(`Your keystore contains (\d+) entr`)

// keytoolList runs keytool -list on a cacerts file in a container from image.
func keytoolList(t *testing.T, e docker.Engine, image, cacerts string, mounts ...docker.Mount) (out string, entries int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code, err := e.Run(context.Background(), docker.RunOpts{
		Image: image, Remove: true, Network: "none", Mounts: mounts,
		Entrypoint: "sh",
		Args:       []string{"-c", `keytool -list -keystore "` + cacerts + `" -storepass changeit`},
		Stdout:     &stdout, Stderr: &stderr,
	})
	if err != nil || code != 0 {
		t.Fatalf("keytool -list: exit %d, %v: %s", code, err, stderr.String())
	}
	m := keystoreEntries.FindStringSubmatch(stdout.String())
	if m == nil {
		t.Fatalf("keytool -list printed no entry count:\n%s", stdout.String())
	}
	n, _ := strconv.Atoi(m[1])
	return stdout.String(), n
}

// TestTruststoreStepWithDocker builds the truststore for real from a stand-in
// for the psama image: amazoncorretto:25-alpine with a certificate imported
// as aws_cert, the way psama's Dockerfile does.
func TestTruststoreStepWithDocker(t *testing.T) {
	if testing.Short() {
		t.Skip("-short")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker CLI")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		t.Skip("docker daemon unavailable")
	}
	r := execRunner{}
	e := docker.NewEngine(r)

	// Everything the test creates is named with this prefix and removed.
	var suffix [4]byte
	_, _ = rand.Read(suffix[:])
	prefix := "picsure-test-023-" + hex.EncodeToString(suffix[:])
	image := prefix + ":psama"

	build := t.TempDir()
	writeTestFile(t, filepath.Join(build, "certificate.der"), newCACert(t, "aws stand-in"))
	writeTestFile(t, filepath.Join(build, "Dockerfile"), []byte(`FROM amazoncorretto:25-alpine
COPY certificate.der /certificate.der
RUN keytool -noprompt -import -alias aws_cert -keystore $JAVA_HOME/lib/security/cacerts -storepass changeit -file /certificate.der
`))
	var buildLog bytes.Buffer
	t.Cleanup(func() { _ = e.RemoveImage(context.Background(), image) })
	if err := e.Build(ctx, docker.BuildOpts{Context: build, Tag: image, Stdout: &buildLog, Stderr: &buildLog}); err != nil {
		t.Fatalf("building the stand-in psama image: %v\n%s", err, buildLog.String())
	}
	imageList, imageEntries := keytoolList(t, e, image, "$JAVA_HOME/lib/security/cacerts")
	if !strings.Contains(imageList, "aws_cert,") {
		t.Fatalf("the stand-in image has no aws_cert:\n%s", imageList)
	}

	st, cfg := newTrustStack(t)
	cfg.Name = prefix
	volume := prefix + "_truststore"
	t.Cleanup(func() { _ = e.VolumeRemove(context.Background(), volume) })
	writeTestFile(t, st.Path("certs/trust/a.crt"), pemCerts(newCACert(t, "a1"), newCACert(t, "a2")))
	writeTestFile(t, st.Path("certs/trust/b.der"), newCACert(t, "b"))

	var rec events.Recorder
	d := &ops.Deps{Runner: r, Docker: e, Rand: rand.Reader, Sink: &rec}
	step := ops.TruststoreStep(d, st, cfg, image)
	if err := steps.Run(ctx, d.Sink, []steps.Step{step}, steps.Options{}); err != nil {
		t.Fatalf("truststore step: %v\nevents: %+v", err, rec.Events())
	}

	list, entries := keytoolList(t, e, image, "/truststore/cacerts", docker.Mount{Source: volume, Target: "/truststore", ReadOnly: true})
	for _, alias := range []string{"aws_cert", "custom-1-a.crt", "custom-2-a.crt", "custom-3-b.der"} {
		if !strings.Contains(list, alias+",") {
			t.Errorf("keytool -list has no %s", alias)
		}
	}
	if entries != imageEntries+3 {
		t.Errorf("truststore has %d entries, want the image's %d plus 3", entries, imageEntries)
	}

	if done, err := step.Check(ctx); err != nil || !done {
		t.Errorf("Check after Apply = %v, %v; want done", done, err)
	}
}
