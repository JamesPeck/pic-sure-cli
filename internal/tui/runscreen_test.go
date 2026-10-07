package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

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
	if !strings.Contains(plainView(s), "✗") {
		t.Errorf("a cancelled run isn't shown as failed:\n%s", plainView(s))
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
