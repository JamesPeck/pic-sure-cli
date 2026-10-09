package ops_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/pki"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

// TestTLSStepAgainstDocker fills a real certs volume, in generated mode and
// then in provided mode, and checks what httpd's uid 2 would find there.
func TestTLSStepAgainstDocker(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	// Everything the test creates carries this name, and cleanup removes
	// only that.
	name := "picsuretest-024-" + hex.EncodeToString(b)
	volume := name + "_certs"
	cleanupDocker(t, func(ctx context.Context) {
		out, _ := exec.CommandContext(ctx, "docker", "ps", "-aq", "--filter", "label="+stack.LabelStack+"="+name).Output()
		for _, id := range strings.Fields(string(out)) {
			_ = exec.CommandContext(ctx, "docker", "rm", "-f", id).Run()
		}
		_ = exec.CommandContext(ctx, "docker", "volume", "rm", "-f", volume).Run()
	})

	st, err := stack.Create(filepath.Join(t.TempDir(), "stack"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := stack.DefaultConfig()
	cfg.Name = name
	d := &ops.Deps{Docker: newLabelEngine(), Rand: rand.Reader, Clock: ops.SystemClock{}, Sink: events.Discard}
	applyAndCheck := func() {
		t.Helper()
		s := ops.TLSStep(d, st, &cfg)
		if done, err := s.Check(ctx); err != nil || done {
			t.Fatalf("Check before Apply = %v, %v", done, err)
		}
		if err := s.Apply(ctx, events.Discard); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if done, err := s.Check(ctx); err != nil || !done {
			t.Fatalf("Check after Apply = %v, %v", done, err)
		}
	}
	alpine, _ := catalog.LookupImage("alpine")
	inVolume := func(user string, args ...string) string {
		t.Helper()
		argv := []string{"run", "--rm", "--network", "none", "--label", stack.LabelStack + "=" + name, "--label", testLabel + "=1",
			"--user", user, "-v", volume + ":/certs:ro", alpine.Ref}
		out, err := exec.Command("docker", append(argv, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	checkVolume := func(key []byte) {
		t.Helper()
		got := inVolume("0:0", "sh", "-c", "cd /certs && ls -A && stat -c '%n %u:%g %a' server.key server.crt server.chain")
		want := "server.chain\nserver.crt\nserver.key\nserver.key 2:2 640\nserver.crt 2:2 644\nserver.chain 2:2 644\n"
		if got != want {
			t.Errorf("volume listing:\n%s\nwant:\n%s", got, want)
		}
		if got := inVolume("2:2", "cat", "/certs/server.key"); got != string(key) {
			t.Error("uid 2 doesn't read the installed key")
		}
	}

	applyAndCheck()
	key, err := st.ReadFile(ops.TLSDir + "/server.key")
	if err != nil {
		t.Fatal(err)
	}
	checkVolume(key)
	labels, err := exec.Command("docker", "volume", "inspect", "--format", "{{json .Labels}}", volume).Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range []string{`"com.docker.compose.project":"` + name + `"`, `"com.docker.compose.volume":"certs"`, `"` + stack.LabelStack + `":"` + name + `"`} {
		if !strings.Contains(string(labels), l) {
			t.Errorf("volume labels %s lack %s", labels, l)
		}
	}

	// Switching to the operator's files replaces the installed ones.
	f, err := pki.Generate(rand.Reader, "picsure.example.org", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	dir := st.Path("certs/tls")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for file, data := range map[string][]byte{"server.key": f.Key, "server.crt": f.Cert} {
		if err := os.WriteFile(filepath.Join(dir, file), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg.TLS.Mode, cfg.Network.Hostname = stack.TLSProvided, "picsure.example.org"
	applyAndCheck()
	checkVolume(f.Key)
	if got := inVolume("2:2", "cat", "/certs/server.chain"); got != string(f.Cert) {
		t.Error("the chain isn't the operator's certificate")
	}
}
