package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/log"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

// runWithStack runs args with the command at path replaced by one that
// opens the stack in dir, logs, and returns err. It returns the run logs
// left in dir and stderr.
func runWithStack(t *testing.T, dir, path string, err error, args ...string) (logs []string, stderr string) {
	t.Helper()
	a, _, errOut := testApp(t)
	root := newRootCmd(a)
	c, _, findErr := root.Find(strings.Fields(path))
	if findErr != nil || c.CommandPath() != "pic-sure "+path {
		t.Fatalf("no command %q: %v", path, findErr)
	}
	c.RunE = func(*cobra.Command, []string) error {
		st, openErr := stack.Open(dir)
		if openErr != nil {
			t.Fatal(openErr)
		}
		defer func() { _ = st.Close() }()
		a.openRunLog(st)
		a.newDeps().Log.Debug("working")
		return err
	}
	markRunning(a, c)
	a.execute(context.Background(), root, append(strings.Fields(path), args...))

	entries, _ := os.ReadDir(filepath.Join(dir, log.Dir))
	for _, e := range entries {
		logs = append(logs, filepath.Join(dir, log.Dir, e.Name()))
	}
	return logs, errOut.String()
}

func TestReadOnlyCommandsWriteARunLogOnlyAtDebug(t *testing.T) {
	for path := range readOnlyCommands {
		t.Run(path, func(t *testing.T) {
			var args []string
			if path == "config get" {
				args = []string{"name"}
			}
			if logs, _ := runWithStack(t, newTestStack(t), path, nil, args...); len(logs) != 0 {
				t.Errorf("at info: run logs %q, want none", logs)
			}
			if logs, _ := runWithStack(t, newTestStack(t), path, nil, append(args, "--log-level", "debug")...); len(logs) != 1 {
				t.Errorf("at debug: run logs %q, want one", logs)
			}
		})
	}
	for _, path := range []string{"up", "down", "data demo"} {
		t.Run(path, func(t *testing.T) {
			if logs, _ := runWithStack(t, newTestStack(t), path, nil, "--log-level", "error"); len(logs) != 1 {
				t.Errorf("at error: run logs %q, want one", logs)
			}
		})
	}
}

func TestRunLogRecordsTheRun(t *testing.T) {
	const secret = "Kx7Qm2Wv9Lp4Rt8Zb3Nc6Hd"
	log.RegisterSecrets(secret)
	logs, stderr := runWithStack(t, newTestStack(t), "up", errors.New("compose up failed: password "+secret),
		"--skip-step", "db")
	if len(logs) != 1 {
		t.Fatalf("run logs %q, want one", logs)
	}
	b, err := os.ReadFile(logs[0])
	if err != nil {
		t.Fatal(err)
	}
	var msgs []string
	var first, last map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("%v: %s", err, line)
		}
		if first == nil {
			first = rec
		}
		last = rec
		msgs = append(msgs, rec["msg"].(string))
	}
	if got := strings.Join(msgs, ","); got != "pic-sure run,working,pic-sure exit" {
		t.Errorf("records = %s", got)
	}
	if first["command"] != "pic-sure up" || first["version"] != "v2.0.0-test" ||
		fmt.Sprint(first["flags"]) != "[--skip-step=[db]]" {
		t.Errorf("first record = %v", first)
	}
	if last["code"] != float64(exitcode.CodeFailed) || last["err"] != "compose up failed: password [REDACTED]" {
		t.Errorf("last record = %v", last)
	}
	if strings.Contains(string(b), secret) || strings.Contains(stderr, "working") {
		t.Errorf("the log has the secret or stderr has a debug record:\n%s\nstderr: %s", b, stderr)
	}
}

func TestRunLogFailureIsAWarning(t *testing.T) {
	dir := newTestStack(t)
	if err := os.WriteFile(filepath.Join(dir, log.Dir), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr := runWithStack(t, dir, "up", nil)
	if !strings.Contains(stderr, `level=WARN msg="can't write the run log"`) {
		t.Errorf("stderr = %q, want a warning", stderr)
	}
}

func TestNewLoggerOutsideARun(t *testing.T) {
	a, _, stderr := testApp(t)
	a.newLogger().Error("nobody hears this")
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q", stderr)
	}
}

// A secret loaded through the stack package is redacted from stderr and the
// run log, however it is logged, since the cli registers stack secrets with
// the log redactor.
func TestStackSecretsNeverReachTheLogs(t *testing.T) {
	const secret = "Vq8Rn3Ty6Lm1Pz5Kd9Wc2Hf"
	const proxyPassword = "Gj4Xs7Bn2Qe"
	dir := newTestStack(t)
	if err := os.WriteFile(filepath.Join(dir, stack.SecretsFile),
		[]byte("db_root_password: "+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, _, stderr := testApp(t)
	root := newRootCmd(a)
	c, _, _ := root.Find([]string{"up"})
	c.RunE = func(cmd *cobra.Command, _ []string) error {
		st, err := a.openStack(cmd)
		if err != nil {
			return err
		}
		defer func() { _ = st.Close() }()
		sec, err := st.LoadSecrets()
		if err != nil {
			return err
		}
		pw := string(sec.DBRootPassword)
		a.newDeps().Log.Info("connecting", "dsn", "root:"+pw+"@tcp(db:3306)/",
			"proxy", "http://alice:"+proxyPassword+"@proxy.example:3128")
		return fmt.Errorf("mysql refused %s", pw)
	}
	markRunning(a, c)
	if code := a.execute(context.Background(), root, []string{"up", "--stack", dir, "--log-level", "debug"}); code != exitcode.CodeFailed {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	entries, err := os.ReadDir(filepath.Join(dir, log.Dir))
	if err != nil || len(entries) != 1 {
		t.Fatalf("run logs %v, %v; want one", entries, err)
	}
	file, err := os.ReadFile(filepath.Join(dir, log.Dir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	st, err := stack.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if m, err := st.Manifest(); err != nil || !m.Has(log.Dir+"/"+entries[0].Name()) {
		t.Errorf("the manifest doesn't list the run log: %v, %v", m, err)
	}
	for name, out := range map[string]string{"stderr": stderr.String(), "run log": string(file)} {
		if !strings.Contains(out, "connecting") || !strings.Contains(out, "proxy.example") {
			t.Errorf("%s lacks the record:\n%s", name, out)
		}
		if strings.Contains(out, secret) || strings.Contains(out, proxyPassword) {
			t.Errorf("%s has a secret:\n%s", name, out)
		}
	}
}

// Pruning through the stack keeps the newest 50 of the CLI's run logs,
// drops the pruned ones from the manifest, and ignores a log the manifest
// doesn't list.
func TestRunLogPruningUsesTheManifest(t *testing.T) {
	dir := newTestStack(t)
	st, err := stack.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.MkdirAll(log.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var ours []string
	for i := range 50 {
		name := fmt.Sprintf("%s/cli-20260101T0000%02d.000Z-1.log", log.Dir, i)
		f, err := st.CreateFile(name, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
		ours = append(ours, name)
	}
	// Newer than every log of ours, so it would push one out if it counted.
	foreign := log.Dir + "/cli-29990101T000000.000Z-1.log"
	if err := os.WriteFile(st.Path(foreign), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	logs, stderr := runWithStack(t, dir, "up", nil)
	if len(logs) != 51 || strings.Contains(stderr, "WARN") {
		t.Fatalf("left %d logs, want 51 (49 old, the new one, the foreign one); stderr:\n%s", len(logs), stderr)
	}
	m, err := st.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(st.Path(ours[0])); !errors.Is(err, os.ErrNotExist) || m.Has(ours[0]) {
		t.Errorf("the oldest log wasn't pruned and forgotten: %v, listed %v", err, m.Has(ours[0]))
	}
	if !m.Has(ours[1]) || m.Has(foreign) {
		t.Errorf("manifest = %v", m)
	}
}

// A rejected `config set` of a secret key never logs the value, in the run
// log or on stderr; the key stays, and other keys' values stay too.
func TestRunLogRedactsConfigSetValues(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"auth.auth0.client_secret", "SYNTHsecretVALUE123"},
		{"auth.admin_email", "someone@example.org"},
		{"services.psama.env.SMTP_PASSWORD", "SYNTHsmtpVALUE456"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			dir := newTestStack(t)
			a, stdout, stderr := testApp(t)
			a.Run(context.Background(), []string{"--stack", dir, "--log-level", "debug", "config", "set", tc.key, tc.value})
			logs, _ := filepath.Glob(filepath.Join(dir, log.Dir, "*.log"))
			if len(logs) != 1 {
				t.Fatalf("run logs %q, want one", logs)
			}
			b, err := os.ReadFile(logs[0])
			if err != nil {
				t.Fatal(err)
			}
			for name, out := range map[string]string{"run log": string(b), "stderr": stderr.String(), "stdout": stdout.String()} {
				if strings.Contains(out, tc.value) {
					t.Errorf("%s has the value:\n%s", name, out)
				}
			}
			if !strings.Contains(string(b), `"args":["`+tc.key+`","[REDACTED]"]`) {
				t.Errorf("run log doesn't keep the key:\n%s", b)
			}
		})
	}
	if got := logArgs("config set", []string{"network.http_port", "8081"}); got[1] != "8081" {
		t.Errorf("a non-secret value: %q", got)
	}
	if got := logArgs("config set", []string{"auth.consent_authorization", "true"}); got[1] != "true" {
		t.Errorf("a field that is named like a secret but isn't one: %q", got)
	}
	// A boolean is redacted from the record but not registered.
	if got := logArgs("config set", []string{"services.psama.env.API_TOKEN", "false"}); got[1] != log.Redacted || log.Redact("ok: false") != "ok: false" {
		t.Errorf("a secret-named key's boolean: %q, %q", got, log.Redact("ok: false"))
	}
}
