package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/tui"
)

// fakeDocker puts a docker on PATH that logs its arguments to the returned
// file and answers compose's ps, logs and restart.
func fakeDocker(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	logf := filepath.Join(bin, "docker.log")
	script := `#!/bin/sh
echo "$@" >> ` + logf + `
case "$*" in
*" ps "*) echo '{"Service":"hpds","Name":"demo-hpds-1","State":"running","Health":"healthy","Status":"Up 2 minutes"}' ;;
*" logs "*) echo "hpds-1  | started"; echo "hpds-1  | ready" ;;
*" restart nosuch") echo "no such service: nosuch" >&2; exit 1 ;;
*" restart "*) echo " Container demo-hpds-1  Restarting" >&2 ;;
*) echo "unexpected: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logf
}

// renderedStack writes a stack the compose verbs can run on.
func renderedStack(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, data := range map[string]string{
		"pic-sure.yaml":                 "schema: 1\nname: demo\nauth: {mode: open, admin_email: admin@example.com}\n",
		".pic-sure/state.json":          `{"cli_version": "dev", "schema_version": 1}`,
		".pic-sure/secrets.yaml":        "db_root_password: rootpw-for-the-test\n",
		".pic-sure/render/compose.yaml": "services: {}\n",
	} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestDashBackendReadsTheStack(t *testing.T) {
	logf := fakeDocker(t)
	dir := renderedStack(t)
	a, stdout, stderr := testApp(t)
	b := dashBackend{a: a, dir: dir}
	ctx := context.Background()

	services, err := b.Services(ctx)
	if err != nil || len(services) != 1 || services[0].Service != "hpds" || services[0].Health != "healthy" {
		t.Fatalf("Services = %+v, %v", services, err)
	}

	report, err := b.Status(ctx, false)
	if err != nil || report.Stack.Name != "demo" || report.Deep != nil {
		t.Fatalf("Status = %+v, %v", report, err)
	}

	var logs strings.Builder
	if err := b.FollowLogs(ctx, "hpds", &logs); err != nil {
		t.Fatal(err)
	}
	if logs.String() != "hpds-1  | started\nhpds-1  | ready\n" {
		t.Errorf("logs = %q", logs.String())
	}
	data, _ := os.ReadFile(logf)
	if !strings.Contains(string(data), "logs --follow --tail 200 hpds") {
		t.Errorf("docker calls:\n%s", data)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Errorf("reads wrote over the TUI: %q %q", stdout, stderr)
	}
	// Read-only commands leave no run log.
	if logs, _ := filepath.Glob(filepath.Join(dir, ".pic-sure/logs/*")); len(logs) != 0 {
		t.Errorf("run logs written: %v", logs)
	}
}

func TestDashBackendWithoutAStack(t *testing.T) {
	a, _, _ := testApp(t)
	b := dashBackend{a: a, dir: t.TempDir()}
	if _, err := b.Services(context.Background()); err == nil || !strings.Contains(err.Error(), "no pic-sure stack") {
		t.Errorf("err = %v", err)
	}
}

func TestCommandFromTUIRunsTheCommand(t *testing.T) {
	logf := fakeDocker(t)
	dir := renderedStack(t)
	a, stdout, stderr := testApp(t)
	var rec events.Recorder
	res, err := a.commandFromTUI(context.Background(), tui.CommandRequest{Dir: dir, Args: []string{"restart", "hpds"}, Sink: &rec})
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Types(); strings.Join(got, " ") != "step_started log step_done" {
		t.Errorf("events = %v, want the restart step and no result", got)
	}
	data, _ := os.ReadFile(logf)
	if !strings.Contains(string(data), "restart hpds") {
		t.Errorf("docker calls:\n%s", data)
	}
	if res.LogPath == "" {
		t.Error("no run log for a mutating command")
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Errorf("the command wrote over the TUI: %q %q", stdout, stderr)
	}
}

func TestCommandFromTUIFailure(t *testing.T) {
	fakeDocker(t)
	dir := renderedStack(t)
	a, _, _ := testApp(t)
	var rec events.Recorder
	_, err := a.commandFromTUI(context.Background(), tui.CommandRequest{Dir: dir, Args: []string{"restart", "nosuch"}, Sink: &rec})
	var coded *exitcode.Error
	if !errors.As(err, &coded) || coded.Code != 1 || !strings.Contains(err.Error(), "no such service: nosuch") {
		t.Errorf("err = %v", err)
	}

	_, err = a.commandFromTUI(context.Background(), tui.CommandRequest{Dir: t.TempDir(), Args: []string{"update"}, Sink: &rec})
	if !errors.As(err, &coded) || coded.Code != 3 || !strings.Contains(err.Error(), "no pic-sure stack") {
		t.Errorf("no stack: err = %v", err)
	}
}

// The summary the command prints comes back as the result's.
func TestCommandFromTUISummary(t *testing.T) {
	a, _, _ := testApp(t)
	res, err := a.commandFromTUI(context.Background(), tui.CommandRequest{Dir: t.TempDir(), Args: []string{"version"}, Sink: events.Discard})
	if err != nil || !strings.Contains(res.Summary, "v2.0.0-test") {
		t.Errorf("summary %q, err %v", res.Summary, err)
	}
}
