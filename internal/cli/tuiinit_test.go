package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/tui"
)

func wizardDoc(t *testing.T) *stack.ConfigDoc {
	t.Helper()
	cfg := stack.DefaultConfig()
	cfg.Name = "demo"
	cfg.Auth.Mode = stack.AuthOpen
	cfg.Auth.AdminEmail = "wizard-admin@example.com"
	cfg.Network.HTTPPort, cfg.Network.HTTPSPort = 8083, 8447
	doc, err := stack.NewConfigDoc(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// The wizard's init runs the same steps as the command, with its events
// on the TUI's sink and nothing on stderr, and leaves the run's output as
// it found it. Without docker on PATH it stops at the preconditions,
// before writing anything.
func TestInitFromTUIRunsInitOnTheWizardConfig(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	a, stdout, stderr := testApp(t)
	dir := filepath.Join(t.TempDir(), "demo")
	var rec events.Recorder
	_, err := a.initFromTUI(context.Background(), tui.InitRequest{Dir: dir, Config: wizardDoc(t), Sink: &rec})
	if err == nil || !strings.Contains(err.Error(), "the host isn't ready") {
		t.Fatalf("err = %v, want the preconditions' refusal", err)
	}
	if strings.Contains(err.Error(), "wizard-admin@example.com") {
		t.Errorf("error carries the admin email: %v", err)
	}
	got := rec.Events()
	if len(got) == 0 {
		t.Fatal("no events reached the TUI's sink")
	}
	if s, ok := got[0].(events.StepStarted); !ok || s.ID != initPreconditions {
		t.Errorf("first event = %#v, want the preconditions step", got[0])
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Errorf("init wrote over the TUI: stdout %q, stderr %q", stdout, stderr)
	}
	if a.out != nil || a.tuiLog.Load() != nil {
		t.Error("the run's output wasn't restored")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the stack dir was created (%v)", err)
	}
}

// A stack init already finished gets its summary, as the command prints it.
func TestInitFromTUIOnAFinishedStack(t *testing.T) {
	dir := t.TempDir()
	st, err := stack.Create(dir)
	if err != nil {
		t.Fatal(err)
	}
	data, err := wizardDoc(t).Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteConfig(data); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveState(&stack.State{CLIVersion: "v2.0.0-test", SchemaVersion: stack.ConfigSchema, InitializedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	a, _, _ := testApp(t)
	res, err := a.initFromTUI(context.Background(), tui.InitRequest{Dir: dir, Sink: &events.Recorder{}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Summary, "Stack demo is already initialised") {
		t.Errorf("summary = %q", res.Summary)
	}
}

func TestLogRecordsBecomeLogEvents(t *testing.T) {
	var rec events.Recorder
	a, _, stderr := testApp(t)
	a.tuiLog.Store(&logEvents{&rec})
	if _, err := (logStderr{a}).Write([]byte("level=WARN msg=one\nlevel=WARN msg=two\n")); err != nil {
		t.Fatal(err)
	}
	got := rec.Events()
	if len(got) != 2 || got[1] != (events.Log{Stream: events.StreamStderr, Line: "level=WARN msg=two"}) {
		t.Errorf("events = %#v", got)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestSuggestName(t *testing.T) {
	for in, want := range map[string]string{
		"demo": "demo", "My Stack": "my-stack", "_x.y": "x-y", "...": "picsure", "Ünï": "n-",
	} {
		if got := suggestName(in); got != want {
			t.Errorf("suggestName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWizardDefaultsNameAndPorts(t *testing.T) {
	cfg := wizardDefaults("/srv/My Stack")
	if cfg.Name != "my-stack" {
		t.Errorf("name = %q", cfg.Name)
	}
	if cfg.Network.HTTPPort == cfg.Network.HTTPSPort || cfg.Network.HTTPPort == 0 {
		t.Errorf("ports = %d/%d", cfg.Network.HTTPPort, cfg.Network.HTTPSPort)
	}
}

// Building init's command for the TUI doesn't reset the global flags the
// TUI was started with.
func TestInitFromTUIKeepsTheGlobalFlags(t *testing.T) {
	a, _, _ := testApp(t)
	a.Global.WaitLock = true
	a.Global.SkipSteps = []string{"no-such-step"}
	_, err := a.initFromTUI(context.Background(), tui.InitRequest{Dir: t.TempDir(), Config: wizardDoc(t), Sink: &events.Recorder{}})
	if err == nil || !strings.Contains(err.Error(), "no-such-step") {
		t.Errorf("err = %v, want the --skip-step refusal", err)
	}
	if !a.Global.WaitLock {
		t.Error("--wait-lock was reset")
	}
}

// The TUI's init offers the gate's self-update through its Confirm, and
// installs without re-running.
func TestInitFromTUIGateOptions(t *testing.T) {
	a, _, _ := testApp(t)
	asked := false
	r := &initRun{a: a, cfg: &stack.Config{}, installOnly: true, gateCommand: "pic-sure",
		confirm: func(context.Context, string) (bool, error) { asked = true; return true, nil }}
	o := r.gateOptions(&events.Recorder{})
	if _, ok := o.Updater.(installOnly); !ok {
		t.Errorf("updater = %T, want installOnly", o.Updater)
	}
	if o.Confirm == nil || o.Command != "pic-sure" {
		t.Fatalf("gate options = %+v", o)
	}
	if _, _ = o.Confirm(context.Background(), "?"); !asked {
		t.Error("Confirm isn't the TUI's")
	}
}
