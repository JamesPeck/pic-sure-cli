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
// finds its stack in dir, logs, and returns err. It returns the run logs
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
		a.openRunLog(dir)
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
			if logs, _ := runWithStack(t, t.TempDir(), path, nil, args...); len(logs) != 0 {
				t.Errorf("at info: run logs %q, want none", logs)
			}
			if logs, _ := runWithStack(t, t.TempDir(), path, nil, append(args, "--log-level", "debug")...); len(logs) != 1 {
				t.Errorf("at debug: run logs %q, want one", logs)
			}
		})
	}
	for _, path := range []string{"up", "down", "data demo"} {
		t.Run(path, func(t *testing.T) {
			if logs, _ := runWithStack(t, t.TempDir(), path, nil, "--log-level", "error"); len(logs) != 1 {
				t.Errorf("at error: run logs %q, want one", logs)
			}
		})
	}
}

func TestRunLogRecordsTheRun(t *testing.T) {
	const secret = "Kx7Qm2Wv9Lp4Rt8Zb3Nc6Hd"
	log.RegisterSecrets(secret)
	logs, stderr := runWithStack(t, t.TempDir(), "up", errors.New("compose up failed: password "+secret),
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
	missing := filepath.Join(t.TempDir(), "missing")
	_, stderr := runWithStack(t, missing, "up", nil)
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
	for name, out := range map[string]string{"stderr": stderr.String(), "run log": string(file)} {
		if !strings.Contains(out, "connecting") || !strings.Contains(out, "proxy.example") {
			t.Errorf("%s lacks the record:\n%s", name, out)
		}
		if strings.Contains(out, secret) || strings.Contains(out, proxyPassword) {
			t.Errorf("%s has a secret:\n%s", name, out)
		}
	}
}
