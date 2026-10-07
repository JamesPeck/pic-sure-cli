package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"syscall"
	"testing"

	"github.com/spf13/cobra"

	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
)

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestSelectMode(t *testing.T) {
	tests := []struct {
		name     string
		g        GlobalOptions
		terminal bool
		env      map[string]string
		want     outputMode
	}{
		{"terminal", GlobalOptions{}, true, nil, modeTUI},
		{"no terminal", GlobalOptions{}, false, nil, modePlain},
		{"--plain on a terminal", GlobalOptions{Plain: true}, true, nil, modePlain},
		{"CI on a terminal", GlobalOptions{}, true, map[string]string{"CI": "true"}, modePlain},
		{"CI=1", GlobalOptions{}, true, map[string]string{"CI": "1"}, modePlain},
		{"CI=woodpecker", GlobalOptions{}, true, map[string]string{"CI": "woodpecker"}, modePlain},
		{"CI=false", GlobalOptions{}, true, map[string]string{"CI": "false"}, modeTUI},
		{"CI=0", GlobalOptions{}, true, map[string]string{"CI": "0"}, modeTUI},
		{"--json on a terminal", GlobalOptions{JSON: true}, true, nil, modeJSON},
		{"--json in CI", GlobalOptions{JSON: true}, false, map[string]string{"CI": "true"}, modeJSON},
	}
	for _, tt := range tests {
		if got := selectMode(tt.g, tt.terminal, env(tt.env)); got != tt.want {
			t.Errorf("%s: mode = %d, want %d", tt.name, got, tt.want)
		}
	}
}

func TestUseColor(t *testing.T) {
	tests := []struct {
		terminal bool
		env      map[string]string
		want     bool
	}{
		{true, nil, true},
		{false, nil, false},
		{true, map[string]string{"NO_COLOR": "1"}, false},
		{true, map[string]string{"NO_COLOR": "0"}, false}, // any non-empty value
		{true, map[string]string{"NO_COLOR": ""}, true},
		{true, map[string]string{"TERM": "dumb"}, false},
	}
	for _, tt := range tests {
		if got := useColor(tt.terminal, env(tt.env)); got != tt.want {
			t.Errorf("useColor(%v, %v) = %v, want %v", tt.terminal, tt.env, got, tt.want)
		}
	}
}

// jsonLines decodes stdout as NDJSON, failing on anything that isn't one
// JSON object per line.
func jsonLines(t *testing.T, stdout string) []map[string]any {
	t.Helper()
	if !strings.HasSuffix(stdout, "\n") {
		t.Fatalf("stdout does not end in a newline: %q", stdout)
	}
	var objs []map[string]any
	for _, line := range strings.Split(strings.TrimSuffix(stdout, "\n"), "\n") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil || obj == nil {
			t.Fatalf("stdout line %q is not a JSON object: %v", line, err)
		}
		objs = append(objs, obj)
	}
	return objs
}

func TestJSONFailureIsAResultLine(t *testing.T) {
	tests := []struct {
		args    []string
		code    int
		message string
	}{
		{[]string{"--stack", "/nowhere", "up", "--json"}, 3, "no pic-sure stack found in /nowhere: it has no pic-sure.yaml; create one with pic-sure init"},
		{[]string{"--json", "up", "extra-arg"}, 2, `unknown command "extra-arg" for "pic-sure up"`},
		{[]string{"--json", "--plain", "up"}, 2, "if any flags in the group [json plain] are set none of the others can be; [json plain] were all set"},
		{[]string{"up", "--bogus", "--json"}, 2, "unknown flag: --bogus"},
		{[]string{"up", "--bogus", "--json=false", "--json=1"}, 2, "unknown flag: --bogus"},
		{[]string{"help", "frobnicate", "--json"}, 2, `unknown help topic "frobnicate"`},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			a, stdout, stderr := testApp(t)
			if code := a.Run(context.Background(), tt.args); code != tt.code {
				t.Errorf("exit = %d, want %d", code, tt.code)
			}
			objs := jsonLines(t, stdout.String())
			want := map[string]any{"type": "result", "ok": false, "error": map[string]any{
				"exit_code": float64(tt.code), "message": tt.message,
			}}
			if len(objs) != 1 || !jsonEqual(objs[0], want) {
				t.Errorf("stdout = %s, want one line %v", stdout, want)
			}
			if !strings.HasPrefix(stderr.String(), "pic-sure: "+tt.message+"\n") {
				t.Errorf("stderr = %q", stderr)
			}
		})
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func TestPlainFailureLeavesStdoutAlone(t *testing.T) {
	a, stdout, stderr := testApp(t)
	if code := a.Run(context.Background(), []string{"--stack", "/nowhere", "up", "--plain"}); code != exitcode.CodePrecondition {
		t.Errorf("exit = %d", code)
	}
	if stdout.Len() != 0 || stderr.String() != "pic-sure: no pic-sure stack found in /nowhere: it has no pic-sure.yaml; create one with pic-sure init\n" {
		t.Errorf("stdout %q, stderr %q", stdout, stderr)
	}

	// The last --json wins even when cobra parsed an earlier one.
	a, stdout, _ = testApp(t)
	if code := a.Run(context.Background(), []string{"up", "--json", "--bogus", "--json=false"}); code != exitcode.CodeUsage {
		t.Errorf("exit = %d", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want plain output", stdout)
	}
}

// withRunE replaces the RunE of the command at path in a fresh root.
func withRunE(t *testing.T, a *App, path []string, run func(*cobra.Command, []string) error) *cobra.Command {
	t.Helper()
	root := newRootCmd(a)
	c, _, err := root.Find(path)
	if err != nil {
		t.Fatal(err)
	}
	c.RunE = run
	markRunning(a, c)
	return root
}

// streamingUp is an up that emits a step's events, then fails with err or
// finishes with data.
func streamingUp(a *App, failed bool, err error, data any) func(*cobra.Command, []string) error {
	return func(*cobra.Command, []string) error {
		sink := a.newDeps().Sink
		sink.Emit(events.StepStarted{ID: "db", Title: "Start the database"})
		sink.Emit(events.Log{ID: "db", Stream: events.StreamStdout, Line: "started"})
		if failed {
			sink.Emit(events.StepDone{ID: "db", Status: events.StepFailed})
			return err
		}
		sink.Emit(events.StepDone{ID: "db", Status: events.StepOK})
		return a.finish(data, nil)
	}
}

func TestJSONStreamEndsWithResult(t *testing.T) {
	t.Run("success carries the report", func(t *testing.T) {
		a, stdout, stderr := testApp(t)
		root := withRunE(t, a, []string{"up"}, streamingUp(a, false, nil, map[string]string{"url": "https://localhost:8443"}))
		if code := a.execute(context.Background(), root, []string{"up", "--json"}); code != 0 {
			t.Errorf("exit = %d", code)
		}
		objs := jsonLines(t, stdout.String())
		var types []string
		for _, o := range objs {
			types = append(types, o["type"].(string))
		}
		if strings.Join(types, " ") != "step_started log step_done result" {
			t.Errorf("types = %v", types)
		}
		if last := objs[len(objs)-1]; !jsonEqual(last, map[string]any{"type": "result", "ok": true, "data": map[string]any{"url": "https://localhost:8443"}}) {
			t.Errorf("result = %v", last)
		}
		if stderr.Len() != 0 {
			t.Errorf("stderr = %q", stderr)
		}
	})
	t.Run("a failed step names itself in the result", func(t *testing.T) {
		a, stdout, _ := testApp(t)
		err := exitcode.Precondition("docker daemon unreachable")
		root := withRunE(t, a, []string{"up"}, streamingUp(a, true, err, nil))
		if code := a.execute(context.Background(), root, []string{"up", "--json"}); code != exitcode.CodePrecondition {
			t.Errorf("exit = %d", code)
		}
		objs := jsonLines(t, stdout.String())
		want := map[string]any{"type": "result", "ok": false, "error": map[string]any{
			"exit_code": 3, "message": "docker daemon unreachable", "step": "db",
		}}
		if len(objs) != 4 || !jsonEqual(objs[3], want) {
			t.Errorf("stdout = %s", stdout)
		}
	})
	t.Run("a result that can't be encoded fails the command", func(t *testing.T) {
		a, stdout, _ := testApp(t)
		root := withRunE(t, a, []string{"up"}, streamingUp(a, false, nil, map[string]any{"ch": make(chan int)}))
		if code := a.execute(context.Background(), root, []string{"up", "--json"}); code != exitcode.CodeFailed {
			t.Errorf("exit = %d", code)
		}
		objs := jsonLines(t, stdout.String())
		if last := objs[len(objs)-1]; last["type"] != "result" || last["ok"] != false ||
			!strings.Contains(last["error"].(map[string]any)["message"].(string), "encoding the result") {
			t.Errorf("result = %v", last)
		}
	})
	t.Run("plain renders the events on stderr", func(t *testing.T) {
		a, stdout, stderr := testApp(t)
		root := withRunE(t, a, []string{"up"}, streamingUp(a, true, exitcode.Failed("boom"), nil))
		if code := a.execute(context.Background(), root, []string{"up"}); code != exitcode.CodeFailed {
			t.Errorf("exit = %d", code)
		}
		lines := strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n")
		if stdout.Len() != 0 || len(lines) != 4 ||
			!strings.HasSuffix(lines[0], " [ .. ] Start the database") ||
			!strings.HasSuffix(lines[1], " | started") ||
			!strings.HasSuffix(lines[2], " [FAIL] Start the database") ||
			lines[3] != "pic-sure: boom" {
			t.Errorf("stdout %q, stderr:\n%s", stdout, stderr)
		}
	})
}

func TestReports(t *testing.T) {
	t.Run("version --json is one object", func(t *testing.T) {
		a, stdout, _ := testApp(t)
		if code := a.Run(context.Background(), []string{"version", "--json"}); code != 0 {
			t.Errorf("exit = %d", code)
		}
		want := `{"schema_version":2,"version":"v2.0.0-test","commit":"abc1234","date":"2026-10-06"}` + "\n"
		if stdout.String() != want {
			t.Errorf("stdout = %q, want %q", stdout, want)
		}
	})
	t.Run("a report then a failure adds no result line", func(t *testing.T) {
		a, stdout, stderr := testApp(t)
		root := withRunE(t, a, []string{"doctor"}, func(*cobra.Command, []string) error {
			if err := a.printReport(map[string]bool{"ok": false}, nil); err != nil {
				return err
			}
			return exitcode.Failed("1 check failed")
		})
		if code := a.execute(context.Background(), root, []string{"doctor", "--json"}); code != exitcode.CodeFailed {
			t.Errorf("exit = %d", code)
		}
		if stdout.String() != `{"schema_version":2,"ok":false}`+"\n" || stderr.String() != "pic-sure: 1 check failed\n" {
			t.Errorf("stdout %q, stderr %q", stdout, stderr)
		}
	})
	t.Run("a report that can't be encoded is a result line", func(t *testing.T) {
		a, stdout, _ := testApp(t)
		root := withRunE(t, a, []string{"doctor"}, func(*cobra.Command, []string) error {
			return a.printReport([]string{"not an object"}, nil)
		})
		if code := a.execute(context.Background(), root, []string{"doctor", "--json"}); code != exitcode.CodeFailed {
			t.Errorf("exit = %d", code)
		}
		objs := jsonLines(t, stdout.String())
		if len(objs) != 1 || objs[0]["type"] != "result" {
			t.Errorf("stdout = %q", stdout)
		}
	})
}

func TestJSONSignalResult(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(exitcode.Signaled(syscall.SIGINT))
	a, stdout, _ := testApp(t)
	if code := a.Run(ctx, []string{"up", "--json"}); code != 130 {
		t.Errorf("exit = %d", code)
	}
	objs := jsonLines(t, stdout.String())
	if len(objs) != 1 || !jsonEqual(objs[0]["error"], map[string]any{"exit_code": 130, "message": "interrupted (interrupt)"}) {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestOutputIsPerRun(t *testing.T) {
	a, stdout, _ := testApp(t)
	if code := a.Run(context.Background(), []string{"--stack", "/nowhere", "up", "--json"}); code != exitcode.CodePrecondition || stdout.Len() == 0 {
		t.Fatalf("the --json run: exit = %d, stdout %q", code, stdout)
	}
	stdout.Reset()
	if code := a.Run(context.Background(), []string{"--stack", "/nowhere", "up"}); code != exitcode.CodePrecondition {
		t.Errorf("exit = %d", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("the second, plain run wrote %q to stdout", stdout)
	}
}

func TestTUIModeFallsBackToPlain(t *testing.T) {
	t.Setenv("CI", "")
	a, _, _ := testApp(t)
	a.IsTerminal = func() bool { return true }
	o := a.output()
	if o.mode != modeTUI {
		t.Fatalf("mode = %d, want TUI", o.mode)
	}
	if _, ok := o.sink.Sink.(*events.Plain); !ok {
		t.Errorf("TUI mode sink is %T, want the plain renderer", o.sink.Sink)
	}
}

func TestJSONRequested(t *testing.T) {
	tests := []struct {
		args []string
		want bool
	}{
		{[]string{"up", "--bogus", "--json"}, true},
		{[]string{"up", "--bogus", "--json=true"}, true},
		{[]string{"up", "--bogus", "--json", "--json=false"}, false},
		{[]string{"up", "--bogus", "--json=maybe"}, false},
		{[]string{"compose", "--bogus", "--", "--json"}, false},
		{[]string{"up", "--bogus"}, false},
	}
	for _, tt := range tests {
		if got := jsonRequested(tt.args); got != tt.want {
			t.Errorf("jsonRequested(%q) = %v, want %v", tt.args, got, tt.want)
		}
	}
}

func TestRunEndings(t *testing.T) {
	t.Run("a signal after finish fails the result", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(context.Background())
		defer cancel(nil)
		a, stdout, _ := testApp(t)
		root := withRunE(t, a, []string{"up"}, func(*cobra.Command, []string) error {
			err := a.finish(map[string]int{"n": 1}, nil)
			cancel(exitcode.Signaled(syscall.SIGINT))
			return err
		})
		if code := a.execute(ctx, root, []string{"up", "--json"}); code != 130 {
			t.Errorf("exit = %d", code)
		}
		objs := jsonLines(t, stdout.String())
		if len(objs) != 1 || objs[0]["ok"] != false || !jsonEqual(objs[0]["error"].(map[string]any)["exit_code"], 130) {
			t.Errorf("stdout = %s", stdout)
		}
	})
	t.Run("a command that never calls finish still gets a result", func(t *testing.T) {
		a, stdout, _ := testApp(t)
		root := withRunE(t, a, []string{"up"}, func(*cobra.Command, []string) error {
			a.newDeps().Sink.Emit(events.Warning{Text: "w"})
			return nil
		})
		if code := a.execute(context.Background(), root, []string{"up", "--json"}); code != 0 {
			t.Errorf("exit = %d", code)
		}
		if got, want := stdout.String(), `{"type":"warning","text":"w"}`+"\n"+`{"type":"result","ok":true}`+"\n"; got != want {
			t.Errorf("stdout = %q, want %q", got, want)
		}
	})
	t.Run("finish prints the text summary outside JSON mode", func(t *testing.T) {
		a, stdout, stderr := testApp(t)
		root := withRunE(t, a, []string{"up"}, func(*cobra.Command, []string) error {
			return a.finish(map[string]int{"n": 1}, func(w io.Writer) error {
				_, err := io.WriteString(w, "up at https://localhost:8443\n")
				return err
			})
		})
		if code := a.execute(context.Background(), root, []string{"up"}); code != 0 {
			t.Errorf("exit = %d", code)
		}
		if stdout.String() != "up at https://localhost:8443\n" || stderr.Len() != 0 {
			t.Errorf("stdout %q, stderr %q", stdout, stderr)
		}
	})
	for _, fail := range []int{1, 100} {
		t.Run(fmt.Sprintf("output that can't be written fails the run (%d failed writes)", fail), func(t *testing.T) {
			a, _, stderr := testApp(t)
			var stdout strings.Builder
			w := &failingWriter{fail: fail}
			a.Stdout = io.MultiWriter(w, &stdout)
			root := withRunE(t, a, []string{"up"}, streamingUp(a, false, nil, nil))
			if code := a.execute(context.Background(), root, []string{"up", "--json"}); code != exitcode.CodeFailed {
				t.Errorf("exit = %d", code)
			}
			if got := stderr.String(); got != "pic-sure: writing output: disk full\n" {
				t.Errorf("stderr = %q", got)
			}
			if strings.Contains(stdout.String(), `"ok":true`) {
				t.Errorf("a successful result reached stdout:\n%s", stdout.String())
			}
		})
	}
}

// failingWriter fails its first fail writes.
type failingWriter struct{ fail int }

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.fail > 0 {
		w.fail--
		return 0, errors.New("disk full")
	}
	return len(p), nil
}

func TestUsageHint(t *testing.T) {
	tests := []struct {
		name string
		err  error
		hint bool
	}{
		{"marked by the command", withUsageHint(exitcode.Usage("nope: unknown config key")), true},
		{"not marked", exitcode.Usage("pic-sure.yaml: line 4: bad port"), false},
		{"marked, not a usage error", withUsageHint(exitcode.Failed("boom")), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, _, stderr := testApp(t)
			root := withRunE(t, a, []string{"up"}, func(*cobra.Command, []string) error { return tt.err })
			a.execute(context.Background(), root, []string{"up"})
			if got := strings.Contains(stderr.String(), "Run 'pic-sure up --help' for usage."); got != tt.hint {
				t.Errorf("hint = %v, want %v; stderr %q", got, tt.hint, stderr)
			}
		})
	}
}
