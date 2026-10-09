package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/JamesPeck/pic-sure-cli/internal/events"
)

// pumpRun feeds the run screen what its operation sends until pred holds.
func pumpRun(t *testing.T, s *runScreen, pred func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for !pred() {
		ch := make(chan tea.Msg, 1)
		go func() { ch <- s.listen() }()
		select {
		case m := <-ch:
			s.update(m)
		case <-deadline:
			t.Fatalf("timed out; view:\n%s", s.view())
		}
	}
}

// feedRun feeds msgs to s, following the commands it returns.
func feedRun(s *runScreen, msgs ...tea.Msg) {
	for i := 0; i < 200 && len(msgs) > 0; i++ {
		_, cmd := s.update(msgs[0])
		msgs = append(runCmd(cmd), msgs[1:]...)
	}
}

func plainView(s *runScreen) string { return wizardANSI.ReplaceAllString(s.view(), "") }

func TestRunScreenShowsStepsAndSummary(t *testing.T) {
	var got InitRequest
	run := func(ctx context.Context, req InitRequest) (InitResult, error) {
		got = req
		req.Sink.Emit(events.StepStarted{ID: "release", Title: "Fetch the release"})
		req.Sink.Emit(events.StepDone{ID: "release", Status: events.StepOK})
		return InitResult{Summary: "Stack demo is up: https://localhost:8443/\n"}, nil
	}
	s := newRunScreen(context.Background(), "Setting up PIC-SURE", run, InitRequest{Dir: "/tmp/x"}, false)
	s.setSize(100, 40)
	defer s.close()
	pumpRun(t, s, func() bool { return s.finished })
	if got.Dir != "/tmp/x" {
		t.Errorf("init ran on %q", got.Dir)
	}
	view := plainView(s)
	for _, want := range []string{"Fetch the release", "Setup finished", "https://localhost:8443/", "enter to go back"} {
		if !strings.Contains(view, want) {
			t.Errorf("view lacks %q:\n%s", want, view)
		}
	}
	_, cmd := s.update(enter)
	if _, ok := cmd().(runClosedMsg); !ok {
		t.Error("enter after the run didn't close the screen")
	}
}

func TestRunScreenShowsTheFailure(t *testing.T) {
	run := func(ctx context.Context, req InitRequest) (InitResult, error) {
		req.Sink.Emit(events.StepStarted{ID: "preconditions", Title: "Check the host"})
		req.Sink.Emit(events.StepDone{ID: "preconditions", Status: events.StepFailed})
		return InitResult{LogPath: "/tmp/x/.pic-sure/logs/cli.log"}, errors.New("port 8443 (--https-port) is in use")
	}
	s := newRunScreen(context.Background(), "Setting up PIC-SURE", run, InitRequest{}, false)
	s.setSize(100, 40)
	defer s.close()
	pumpRun(t, s, func() bool { return s.finished })
	view := plainView(s)
	for _, want := range []string{"✗ port 8443", "Log file: /tmp/x/.pic-sure/logs/cli.log"} {
		if !strings.Contains(view, want) {
			t.Errorf("view lacks %q:\n%s", want, view)
		}
	}
}

// The gate's self-update offer opens a dialog, and the answer goes back to
// the operation.
func TestRunScreenAsksTheGateQuestion(t *testing.T) {
	answer := make(chan bool, 1)
	run := func(ctx context.Context, req InitRequest) (InitResult, error) {
		yes, err := req.Confirm(ctx, "release-control abc needs pic-sure 2.1.0; this is pic-sure 2.0.0. Update pic-sure now?")
		answer <- yes
		return InitResult{}, err
	}
	s := newRunScreen(context.Background(), "Setting up PIC-SURE", run, InitRequest{}, false)
	s.setSize(100, 40)
	defer s.close()
	pumpRun(t, s, func() bool { return s.askDlg != nil })
	if view := plainView(s); !strings.Contains(view, "needs pic-sure 2.1.0") {
		t.Fatalf("the question isn't shown:\n%s", view)
	}
	feedRun(s, runCmd(s.askDlg.Init())...)
	feedRun(s, left, enter)
	select {
	case yes := <-answer:
		if !yes {
			t.Error("Update answered no")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no answer reached the operation; view:\n%s", plainView(s))
	}
}

func TestRunScreenCtrlCTwiceCancels(t *testing.T) {
	run := func(ctx context.Context, req InitRequest) (InitResult, error) {
		req.Sink.Emit(events.StepStarted{ID: "images", Title: "Build the images"})
		<-ctx.Done()
		return InitResult{}, context.Cause(ctx)
	}
	s := newRunScreen(context.Background(), "Setting up PIC-SURE", run, InitRequest{}, false)
	s.setSize(100, 40)
	defer s.close()
	pumpRun(t, s, func() bool { return strings.Contains(plainView(s), "Build the images") })
	ctrlC := tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	s.update(ctrlC)
	if !strings.Contains(plainView(s), "Press Ctrl-C again") {
		t.Fatalf("first Ctrl-C didn't ask:\n%s", plainView(s))
	}
	s.update(ctrlC)
	pumpRun(t, s, func() bool { return s.finished })
	if !strings.Contains(plainView(s), "✗") || strings.Contains(plainView(s), "Cancelling") {
		t.Errorf("a cancelled run isn't shown as failed, or still as stopping:\n%s", plainView(s))
	}
}

// close stops a running operation and waits for it, as Run does when a
// signal ends the TUI mid-init.
func TestRunScreenCloseStopsTheOperation(t *testing.T) {
	stopped := make(chan struct{})
	run := func(ctx context.Context, req InitRequest) (InitResult, error) {
		defer close(stopped)
		for ctx.Err() == nil {
			req.Sink.Emit(events.Progress{Text: "working"}) // blocks once nobody listens
		}
		return InitResult{}, ctx.Err()
	}
	s := newRunScreen(context.Background(), "Setting up PIC-SURE", run, InitRequest{}, false)
	s.close()
	select {
	case <-stopped:
	default:
		t.Fatal("close returned before the operation did")
	}
}

// A failed command's summary (doctor's report) is shown, and one taller
// than the screen scrolls.
func TestRunScreenScrollsAFailedCommandsSummary(t *testing.T) {
	var report []string
	for i := range 40 {
		report = append(report, fmt.Sprintf("check-%02d", i))
	}
	run := func(context.Context, InitRequest) (InitResult, error) {
		return InitResult{Summary: strings.Join(report, "\n") + "\n"}, errors.New("doctor: 1 check(s) failed")
	}
	s := newRunScreen(context.Background(), "Preflight check", run, InitRequest{}, false)
	s.setSize(80, 24)
	defer s.close()
	pumpRun(t, s, func() bool { return s.finished })
	view := plainView(s)
	for _, want := range []string{"✗ doctor: 1 check(s) failed", "check-00", "pgup/pgdn scroll"} {
		if !strings.Contains(view, want) {
			t.Errorf("view lacks %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "check-39") || lipgloss.Height(view) > 24 {
		t.Errorf("the summary isn't cut to the screen:\n%s", view)
	}
	s.update(tea.KeyPressMsg{Code: tea.KeyEnd})
	if view := plainView(s); !strings.Contains(view, "check-39") || strings.Contains(view, "check-00") {
		t.Errorf("end didn't scroll to the last line:\n%s", view)
	}
}

// On a narrow terminal the footer stays on one line, inside the terminal
// and the frame, shortened when the long one doesn't fit.
func TestRunScreenFooterFitsANarrowTerminal(t *testing.T) {
	var report []string
	for i := range 40 {
		report = append(report, fmt.Sprintf("check-%02d", i))
	}
	run := func(context.Context, InitRequest) (InitResult, error) {
		return InitResult{Summary: strings.Join(report, "\n") + "\n"}, errors.New("doctor: 1 check(s) failed")
	}
	for _, width := range []int{30, 40, 43, 80} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			s := newRunScreen(context.Background(), "Preflight check", run, InitRequest{}, false)
			s.setSize(width, 24)
			defer s.close()
			pumpRun(t, s, func() bool { return s.finished })
			view := plainView(s)
			lines := strings.Split(view, "\n")
			if len(lines) > 24 {
				t.Fatalf("the view is %d lines tall:\n%s", len(lines), view)
			}
			for _, l := range lines {
				if lipgloss.Width(l) > width {
					t.Fatalf("a line is %d wide: %q", lipgloss.Width(l), l)
				}
			}
			footer := ""
			for _, l := range lines {
				if strings.Contains(l, "scroll") {
					footer = l
				}
			}
			if !strings.Contains(footer, "enter") || !strings.Contains(footer, "back") {
				t.Errorf("no one-line footer at %d columns:\n%s", width, view)
			}
		})
	}
}

// A long error after a full step list and a failed step's log tail stays
// readable on a small terminal: the footer and the log file line stay on
// screen, and scrolling reaches every line of the error.
func TestRunScreenKeepsALongErrorReadable(t *testing.T) {
	var words []string
	for i := range 78 {
		words = append(words, fmt.Sprintf("w%03d", i))
	}
	msg := strings.Join(words, " ")                                        // 389 characters, plus the mark
	logPath := "/tmp/" + strings.Repeat("deep-stack-dir/", 12) + "run.log" // wraps to three lines
	run := func(_ context.Context, req InitRequest) (InitResult, error) {
		for i := range 15 {
			id := fmt.Sprint(i)
			req.Sink.Emit(events.StepStarted{ID: id, Title: "Step " + id})
			req.Sink.Emit(events.StepDone{ID: id, Status: events.StepOK})
		}
		req.Sink.Emit(events.StepStarted{ID: "x", Title: "Failing step"})
		for i := range 30 {
			req.Sink.Emit(events.Log{ID: "x", Line: fmt.Sprintf("log line %02d", i)})
		}
		req.Sink.Emit(events.StepDone{ID: "x", Status: events.StepFailed})
		return InitResult{LogPath: logPath}, errors.New(msg)
	}
	for _, height := range []int{24, 30} {
		t.Run(fmt.Sprint(height), func(t *testing.T) {
			s := newRunScreen(context.Background(), "Setting up PIC-SURE", run, InitRequest{}, false)
			s.setSize(80, height)
			defer s.close()
			pumpRun(t, s, func() bool { return s.finished })
			seen := map[string]bool{}
			look := func() {
				t.Helper()
				view := plainView(s)
				if lipgloss.Height(view) > height {
					t.Fatalf("the view is %d lines tall:\n%s", lipgloss.Height(view), view)
				}
				for _, want := range []string{"enter to go back", "Log file: /tmp/deep-stack-dir/", "run.log"} {
					if !strings.Contains(view, want) {
						t.Fatalf("view lacks %q:\n%s", want, view)
					}
				}
				for _, f := range strings.Fields(view) {
					seen[f] = true
				}
			}
			look()
			if !seen["✗"] || !seen["w000"] {
				t.Errorf("the error doesn't start on screen:\n%s", plainView(s))
			}
			s.update(tea.KeyPressMsg{Code: tea.KeyHome})
			look()
			if !seen["Step"] {
				t.Errorf("home doesn't show the first steps:\n%s", plainView(s))
			}
			for range 60 {
				s.update(tea.KeyPressMsg{Code: tea.KeyDown})
				look()
			}
			// The failure tail keeps the last 20 of the 30 log lines.
			for _, w := range append(words, "10", "29") {
				if !seen[w] {
					t.Errorf("%q never came on screen", w)
				}
			}
		})
	}
}

// While the operation stops, the footer offers the force quit.
func TestRunScreenOffersTheForceQuit(t *testing.T) {
	release := make(chan struct{})
	run := func(_ context.Context, req InitRequest) (InitResult, error) {
		req.Sink.Emit(events.StepStarted{ID: "images", Title: "Build the images"})
		<-release // ignores cancellation
		return InitResult{}, nil
	}
	s := newRunScreen(context.Background(), "Setting up PIC-SURE", run, InitRequest{}, false)
	s.setSize(100, 40)
	defer s.close()
	defer close(release)
	pumpRun(t, s, func() bool { return strings.Contains(plainView(s), "Build the images") })
	ctrlC := tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	s.update(ctrlC)
	s.update(ctrlC)
	if !strings.Contains(plainView(s), "ctrl+c again to quit now") {
		t.Errorf("the footer doesn't offer the force quit:\n%s", plainView(s))
	}
	if _, cmd := s.update(ctrlC); !s.prog.Forced || cmd == nil {
		t.Fatal("a third Ctrl-C didn't force the quit")
	} else if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("a forced quit doesn't quit the program")
	}
}
