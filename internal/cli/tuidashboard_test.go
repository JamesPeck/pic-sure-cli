package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/tui"
)

// fakeDocker puts a docker on PATH that logs its arguments to the returned
// file and answers compose's ps, logs and restart, and the ownership
// check's listings with nothing.
func fakeDocker(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	logf := filepath.Join(bin, "docker.log")
	script := `#!/bin/sh
echo "$@" >> ` + logf + `
case "$*" in
"ps --all --no-trunc "*|"volume ls "*|"network ls "*) ;;
*" ps "*) echo '{"Service":"hpds","Name":"demo-hpds-1","State":"running","Health":"healthy","Status":"Up 2 minutes"}' ;;
*" logs "*) echo "hpds-1  | started"; echo "compose noise" >&2; echo "hpds-1  | ready" ;;
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

// Without a Dir the command finds no stack, not even the one it runs in:
// the landing's preflight check in a directory that holds no stack checks
// only the host.
func TestCommandFromTUIWithoutAStack(t *testing.T) {
	fakeDocker(t)
	t.Chdir(renderedStack(t))
	a, _, _ := testApp(t)
	res, err := a.commandFromTUI(context.Background(), tui.CommandRequest{Args: []string{"doctor"}, Sink: events.Discard})
	if err != nil && strings.Contains(err.Error(), "no pic-sure stack") {
		t.Fatalf("doctor looked for a stack: %v", err)
	}
	if !strings.Contains(res.Summary, "docker") || strings.Contains(res.Summary, "Stack:") {
		t.Errorf("summary:\n%s", res.Summary)
	}
}

// --wait-lock reaches the dashboard's actions: with it, a held lock is
// waited for until the action is cancelled, instead of refused.
func TestCommandFromTUIKeepsWaitLock(t *testing.T) {
	fakeDocker(t)
	dir := renderedStack(t)
	st, err := stack.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	lock, err := st.Lock(context.Background(), stack.LockOptions{Command: "pic-sure test"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Unlock() }()

	a, _, _ := testApp(t)
	req := tui.CommandRequest{Dir: dir, Args: []string{"restart", "hpds"}, Sink: events.Discard}
	if _, err := a.commandFromTUI(context.Background(), req); err == nil || !strings.Contains(err.Error(), "--wait-lock") {
		t.Fatalf("without --wait-lock: err = %v", err)
	}
	a.Global.WaitLock = true
	var rec events.Recorder
	req.Sink = &rec
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := a.commandFromTUI(ctx, req); err == nil || strings.Contains(err.Error(), "pass --wait-lock") {
		t.Errorf("with --wait-lock: err = %v", err)
	}
	if !slices.Contains(rec.Types(), "warning") {
		t.Errorf("no waiting warning: %v", rec.Types())
	}
}

func TestWarnEvents(t *testing.T) {
	var rec events.Recorder
	var rest strings.Builder
	w := &warnEvents{sink: &rec, rest: &rest}
	_, _ = io.WriteString(w, "pic-sure: warning: can't open the cache\n")
	_, _ = io.WriteString(w, "pic-sure: the stack is locked\n")
	got := rec.Events()
	if len(got) != 1 || got[0].(events.Warning).Text != "can't open the cache" {
		t.Errorf("events = %#v", got)
	}
	if rest.String() != "pic-sure: the stack is locked\n" {
		t.Errorf("rest = %q", rest.String())
	}
}

// update on the dashboard's child App (actionApp) offers the gate's
// self-update through the run screen's Confirm, and installs without
// re-running; with no Confirm, or a no, it refuses with exit 5 as before.
func TestActionAppGateAsksThroughConfirm(t *testing.T) {
	for _, tc := range []struct {
		name                string
		confirm, yes        bool
		wantUpdated         bool
		wantMsg, notWantMsg string
	}{
		{name: "yes", confirm: true, yes: true, wantUpdated: true, wantMsg: "run pic-sure again"},
		{name: "no", confirm: true, wantMsg: "pic-sure self-update --to v2.1.0", notWantMsg: "--self-update to update"},
		{name: "no dialog", wantMsg: "pic-sure update --self-update"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, _ := testApp(t)
			var asked string
			req := tui.CommandRequest{Args: []string{"update"}, Sink: events.Discard}
			if tc.confirm {
				req.Confirm = func(_ context.Context, q string) (bool, error) { asked = q; return tc.yes, nil }
			}
			c := a.actionApp(req, io.Discard, io.Discard, func(events.Result) {})
			r := &updateRun{a: c, cfg: &stack.Config{}}
			o := r.gateOptions(&events.Recorder{})
			if _, ok := o.Updater.(installOnly); ok != tc.confirm {
				t.Errorf("updater = %T, want installOnly: %v", o.Updater, tc.confirm)
			}
			u := &recordingUpdater{}
			o.Updater, o.CLIVersion = u, "v2.0.0"
			err := newerCLIRelease(t).Gate(context.Background(), o)
			if code := exitcode.FromError(err); code != exitcode.CodeIncompatible {
				t.Fatalf("exit = %d (%v), want 5", code, err)
			}
			if tc.confirm != strings.Contains(asked, "Update pic-sure now?") {
				t.Errorf("asked %q", asked)
			}
			if got := u.version == "v2.1.0"; got != tc.wantUpdated {
				t.Errorf("self-updated to %q", u.version)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error = %q, want %q", err, tc.wantMsg)
			}
			if tc.notWantMsg != "" && strings.Contains(err.Error(), tc.notWantMsg) {
				t.Errorf("error = %q names %q", err, tc.notWantMsg)
			}
		})
	}
}
