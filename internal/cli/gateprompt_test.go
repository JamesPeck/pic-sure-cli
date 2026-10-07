package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/release"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

// recordingUpdater stands in for self-update: it records the version and
// returns as a failed re-exec would.
type recordingUpdater struct{ version string }

func (u *recordingUpdater) SelfUpdate(_ context.Context, version string) error {
	u.version = version
	return nil
}

// newerCLIRelease is a release whose build-spec needs pic-sure v2.1.0.
func newerCLIRelease(t *testing.T) *release.Release {
	t.Helper()
	spec, err := release.ParseBuildSpec([]byte(`{"application":[{"project_job_git_key":"PSCLI","git_hash":"v2.1.0"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	return &release.Release{Commit: strings.Repeat("ab", 20), Spec: spec}
}

type gatePromptCase struct {
	name                       string
	terminal                   bool
	json, nonInteractive, yes  bool
	selfUpdate                 bool
	stdin                      string
	wantAsked, wantSelfUpdated bool
}

var gatePromptCases = []gatePromptCase{
	{name: "yes on a terminal", terminal: true, stdin: "y\n", wantAsked: true, wantSelfUpdated: true},
	{name: "no on a terminal", terminal: true, stdin: "n\n", wantAsked: true},
	{name: "no answer on a terminal", terminal: true, stdin: "", wantAsked: true},
	{name: "no terminal", stdin: "y\n"},
	{name: "no terminal, --self-update", selfUpdate: true, wantSelfUpdated: true},
	{name: "--json", terminal: true, json: true, stdin: "y\n"},
	{name: "--non-interactive", terminal: true, nonInteractive: true, stdin: "y\n"},
	{name: "--yes", terminal: true, yes: true, stdin: "y\n"},
}

// checkGate runs the gate with opts and checks the prompt and its outcome.
func checkGate(t *testing.T, tc gatePromptCase, opts release.GateOptions, stderr *bytes.Buffer) {
	t.Helper()
	u := &recordingUpdater{}
	opts.Updater = u
	opts.CLIVersion = "v2.0.0"
	err := newerCLIRelease(t).Gate(context.Background(), opts)
	if code := exitcode.FromError(err); code != exitcode.CodeIncompatible {
		t.Errorf("exit = %d (%v), want 5", code, err)
	}
	if got := u.version != ""; got != tc.wantSelfUpdated {
		t.Errorf("self-updated = %v (%q), want %v", got, u.version, tc.wantSelfUpdated)
	}
	asked := strings.Contains(stderr.String(), "Update pic-sure now? [y/N] ")
	if asked != tc.wantAsked {
		t.Errorf("asked = %v, stderr = %q", asked, stderr)
	}
}

// update offers the gate's self-update on a terminal; elsewhere only
// --self-update replaces the binary, and --yes never does (D12).
func TestUpdateGatePrompt(t *testing.T) {
	for _, tc := range gatePromptCases {
		t.Run(tc.name, func(t *testing.T) {
			a, _, stderr := testApp(t)
			a.IsTerminal = func() bool { return tc.terminal }
			a.Global.JSON, a.Global.NonInteractive, a.Global.Yes = tc.json, tc.nonInteractive, tc.yes
			a.Stdin = strings.NewReader(tc.stdin)
			r := &updateRun{a: a, cfg: &stack.Config{}, selfUpdate: tc.selfUpdate}
			checkGate(t, tc, r.gateOptions(&events.Recorder{}), stderr)
		})
	}
}

func TestInitGatePrompt(t *testing.T) {
	for _, tc := range gatePromptCases {
		t.Run(tc.name, func(t *testing.T) {
			var args []string
			if tc.selfUpdate {
				args = append(args, "--self-update")
			}
			args = append(args, "--name", "demo", "--admin-email", "admin@example.com", "--auth-mode", "open")
			r, err := newInitRun(t, "", tc.stdin, args...)
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			r.a.Stderr = &stderr
			r.a.IsTerminal = func() bool { return tc.terminal }
			r.a.Global.JSON, r.a.Global.NonInteractive, r.a.Global.Yes = tc.json, tc.nonInteractive, tc.yes
			r.readGateFlags()
			checkGate(t, tc, r.gateOptions(&events.Recorder{}), &stderr)
		})
	}
}

// With a --*-stdin secret, stdin isn't the user's to answer with.
func TestInitGateNoPromptWithStdinSecret(t *testing.T) {
	r, err := newInitRun(t, "", "synthetic-client-secret-0123456789abcdef\n", "--name", "demo",
		"--admin-email", "admin@example.com", "--auth-mode", "open", "--auth0-client-secret-stdin")
	if err != nil {
		t.Fatal(err)
	}
	r.a.IsTerminal = func() bool { return true }
	r.readGateFlags()
	if r.confirm != nil {
		t.Error("init prompts with stdin holding a secret")
	}
}

func TestGateConfirmAnswers(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"y\n", true}, {"Yes\n", true}, {" y ", true},
		{"n\n", false}, {"\n", false}, {"", false}, {"yep\n", false},
	} {
		a, _, stderr := testApp(t)
		a.Stdin = strings.NewReader(tc.in)
		got, err := a.gateConfirm(context.Background(), "Update?")
		if err != nil || got != tc.want {
			t.Errorf("answer %q: got %v, %v; want %v", tc.in, got, err, tc.want)
		}
		// The terminal echoes the user's newline; without one the prompt adds it.
		wantNL := !strings.HasSuffix(tc.in, "\n")
		if !strings.HasPrefix(stderr.String(), "Update? [y/N] ") || strings.HasSuffix(stderr.String(), "\n") != wantNL {
			t.Errorf("answer %q: stderr = %q", tc.in, stderr)
		}
	}
}

func TestGateConfirmStopsOnCancel(t *testing.T) {
	a, _, _ := testApp(t)
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	a.Stdin = pr
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.gateConfirm(ctx, "Update?"); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want canceled", err)
	}
}

// lockedBuffer collects a pty's output.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// On a real terminal the progress renderer is drawing the release step, and
// reading keys, when the gate asks. The prompt must end it, so the answer
// reaches the prompt rather than the renderer, and the next event redraws.
func TestGateConfirmOnATerminal(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("CI", "")
	ptm, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	defer func() { _ = ptm.Close() }()
	defer func() { _ = tty.Close() }()
	if err := pty.Setsize(ptm, &pty.Winsize{Rows: 24, Cols: 100}); err != nil {
		t.Fatal(err)
	}
	out := &lockedBuffer{}
	go func() { _, _ = io.Copy(out, ptm) }()

	a, _, _ := testApp(t)
	a.Stdin, a.Stdout, a.Stderr = tty, tty, tty
	a.IsTerminal = func() bool { return true }
	if a.output().tui == nil {
		t.Fatal("no TUI renderer on a terminal")
	}
	sink := a.newSink()
	sink.Emit(events.StepStarted{ID: "release", Title: "Fetch the release"})

	ask := func(question, answer string) bool {
		t.Helper()
		type result struct {
			yes bool
			err error
		}
		got := make(chan result, 1)
		go func() {
			yes, err := a.gateConfirm(context.Background(), question)
			got <- result{yes, err}
		}()
		waitFor(t, out, question+" [y/N] ")
		if _, err := ptm.Write([]byte(answer)); err != nil {
			t.Fatal(err)
		}
		select {
		case r := <-got:
			if r.err != nil {
				t.Fatal(r.err)
			}
			return r.yes
		case <-time.After(10 * time.Second):
			t.Fatalf("the prompt never got %q; output %q", answer, out)
		}
		return false
	}
	if !ask("Update pic-sure now?", "y\n") {
		t.Error("yes read as no")
	}
	sink.Emit(events.StepStarted{ID: "next", Title: "Next step"})
	waitFor(t, out, "Next step")
	if ask("Update it again?", "n\n") {
		t.Error("no read as yes")
	}
	a.output().endTUI()
}

func waitFor(t *testing.T, out *lockedBuffer, s string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(out.String(), s) {
		if time.Now().After(deadline) {
			t.Fatalf("no %q in %q", s, out)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
