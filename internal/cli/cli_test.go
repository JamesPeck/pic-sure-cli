package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/spf13/cobra"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
	"github.com/JamesPeck/pic-sure-cli/internal/tui"
)

// testApp returns an App with captured output, no terminal, and a TUI that
// fails the test if started.
func testApp(t *testing.T) (*App, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	a := &App{
		Info:       BuildInfo{Version: "v2.0.0-test", Commit: "abc1234", Date: "2026-10-06"},
		Stdin:      strings.NewReader(""),
		Stdout:     &stdout,
		Stderr:     &stderr,
		IsTerminal: func() bool { return false },
		StartTUI: func(context.Context, tui.Options) error {
			t.Error("the TUI started")
			return nil
		},
	}
	return a, &stdout, &stderr
}

// specCommands is every command path in spec §5, with the ticket that
// implements it ("" for commands that already work).
var specCommands = map[string]string{
	"init": "", "up": "",
	"down": "", "restart": "", "ps": "", "logs": "", "compose": "",
	"status": "", "doctor": "", "update": "", "build": "", "migrate": "",
	"config show": "", "config get": "", "config set": "", "config edit": "",
	"secrets rotate":      "",
	"data demo":           "",
	"data load-phenotype": "",
	"data load-genomic":   "",
	"dictionary hydrate":  "", "dictionary load-csv": "", "dictionary load-facets": "", "dictionary weights": "",
	"shared-data publish": "", "shared-data list": "", "shared-data remove": "",
	"dev list": "052", "dev on": "052", "dev off": "052",
	"db bootstrap": "",
	"reset":        "", "destroy": "",
	"cache list": "", "cache prune": "",
	"self-update":    "",
	"support-bundle": "059",
	"version":        "",
}

// specFlags is every command flag in spec §5.
var specFlags = map[string][]string{
	"logs":                {"follow"},
	"status":              {"deep"},
	"doctor":              {"network"},
	"update":              {"dry-run", "release-commit", "no-build", "self-update", "ignore-cli-version"},
	"build":               {"force"},
	"migrate":             {"check", "repair"},
	"data demo":           {"heap"},
	"data load-phenotype": {"file", "entry", "input-dir", "heap", "dictionary", "skip-weights"},
	"data load-genomic":   {"partition", "vcf-index", "vcf-dir", "heap", "promote", "all-partitions", "backup", "enable-profile"},
	"db bootstrap":        {"check", "sync-passwords"},
	"reset":               {"keep-db"},
	"destroy":             {"prune-images"},
	"self-update":         {"to"},
	"support-bundle":      {"output"},
}

// leafCommands returns the runnable commands under root that have no
// subcommands, keyed by path without the root's name.
func leafCommands(root *cobra.Command) map[string]*cobra.Command {
	leaves := map[string]*cobra.Command{}
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		if !c.HasSubCommands() {
			leaves[strings.TrimPrefix(c.CommandPath(), "pic-sure ")] = c
			return
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
	delete(leaves, "help")
	for path := range leaves {
		if strings.HasPrefix(path, "completion ") {
			delete(leaves, path)
		}
	}
	return leaves
}

func TestCommandTreeMatchesSpec(t *testing.T) {
	a, _, _ := testApp(t)
	leaves := leafCommands(newRootCmd(a))

	var got, want []string
	for path := range leaves {
		got = append(got, path)
	}
	for path := range specCommands {
		want = append(want, path)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("command tree:\n got  %q\n want %q", got, want)
	}

	for path, flags := range specFlags {
		c, ok := leaves[path]
		if !ok {
			continue // reported above
		}
		for _, name := range flags {
			if c.Flags().Lookup(name) == nil {
				t.Errorf("%s has no --%s flag", path, name)
			}
		}
	}
}

func TestGlobalFlags(t *testing.T) {
	a, _, _ := testApp(t)
	root := newRootCmd(a)
	for _, name := range []string{"stack", "json", "plain", "yes", "non-interactive", "no-animations", "log-level", "skip-step"} {
		if root.PersistentFlags().Lookup(name) == nil {
			t.Errorf("missing global flag --%s", name)
		}
	}

	code := a.Run(context.Background(), []string{
		"--stack", "/srv/demo", "--yes", "--non-interactive", "--no-animations",
		"--log-level", "debug", "--skip-step", "db", "up", "--skip-step", "seed", "--json",
	})
	if code != exitcode.CodePrecondition {
		t.Errorf("exit = %d, want %d", code, exitcode.CodePrecondition)
	}
	want := GlobalOptions{
		Stack: "/srv/demo", JSON: true, Yes: true, NonInteractive: true, NoAnimations: true,
		LogLevel: "debug", SkipSteps: []string{"db", "seed"},
	}
	if g := a.Global; g.Stack != want.Stack || g.JSON != want.JSON || g.Plain || g.Yes != want.Yes ||
		g.NonInteractive != want.NonInteractive || g.NoAnimations != want.NoAnimations ||
		g.LogLevel != want.LogLevel || !slices.Equal(g.SkipSteps, want.SkipSteps) {
		t.Errorf("Global = %+v, want %+v", g, want)
	}
}

func TestEveryStubNamesItsTicket(t *testing.T) {
	a, _, _ := testApp(t)
	for path, c := range leafCommands(newRootCmd(a)) {
		ticket := specCommands[path]
		if ticket == "" {
			continue
		}
		err := c.RunE(c, nil)
		if exitcode.FromError(err) != exitcode.CodeFailed || !strings.Contains(err.Error(), "(ticket "+ticket+")") {
			t.Errorf("%s: RunE = %v, want exit 1 naming ticket %s", path, err, ticket)
		}
	}
}

func TestRunExitCodesAndErrors(t *testing.T) {
	tests := []struct {
		args       []string
		code       int
		stderr     string
		usageHint  string
		wantStdout string
	}{
		{args: []string{"version"}, code: 0, wantStdout: "pic-sure v2 (native)\nversion v2.0.0-test, commit abc1234, built 2026-10-06\n"},
		{args: []string{"--stack", "/nowhere", "up"}, code: 3, stderr: "pic-sure: no pic-sure stack found in /nowhere: it has no pic-sure.yaml; create one with pic-sure init\n"},
		{args: []string{"frobnicate"}, code: 2, usageHint: "pic-sure"},
		{args: []string{"up", "--frobnicate"}, code: 2, usageHint: "pic-sure up"},
		{args: []string{"config"}, code: 2, usageHint: "pic-sure config"},
		{args: []string{"config", "set", "name"}, code: 2, usageHint: "pic-sure config set"},
		{args: []string{"data", "load-genomic"}, code: 2, usageHint: "pic-sure data load-genomic"},
		{args: []string{"migrate", "--check", "--repair"}, code: 2, usageHint: "pic-sure migrate"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			a, stdout, stderr := testApp(t)
			if code := a.Run(context.Background(), tt.args); code != tt.code {
				t.Errorf("exit = %d, want %d (stderr %q)", code, tt.code, stderr)
			}
			if tt.stderr != "" && stderr.String() != tt.stderr {
				t.Errorf("stderr = %q, want %q", stderr, tt.stderr)
			}
			if tt.usageHint != "" && !strings.HasSuffix(stderr.String(), "Run '"+tt.usageHint+" --help' for usage.\n") {
				t.Errorf("stderr = %q, want the usage hint for %q", stderr, tt.usageHint)
			}
			if tt.wantStdout != "" && stdout.String() != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", stdout, tt.wantStdout)
			}
		})
	}
}

func TestPlainErrorFromRunEIsAFailure(t *testing.T) {
	a, _, stderr := testApp(t)
	root := newRootCmd(a)
	up, _, err := root.Find([]string{"up"})
	if err != nil {
		t.Fatal(err)
	}
	up.RunE = func(*cobra.Command, []string) error { return errors.New("compose up failed") }
	markRunning(a, up)

	if code := a.execute(context.Background(), root, []string{"up"}); code != exitcode.CodeFailed {
		t.Errorf("exit = %d, want %d", code, exitcode.CodeFailed)
	}
	if got, want := stderr.String(), "pic-sure: compose up failed\n"; got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
}

func TestSignalDecidesExitCode(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(exitcode.Signaled(syscall.SIGTERM))

	a, _, stderr := testApp(t)
	if code := a.Run(ctx, []string{"up"}); code != 143 {
		t.Errorf("failed command: exit = %d, want 143", code)
	}
	if !strings.Contains(stderr.String(), "interrupted") {
		t.Errorf("stderr = %q, want it to say interrupted", stderr)
	}

	// A command that returns cleanly after the signal still exits 128+N.
	a, _, _ = testApp(t)
	if code := a.Run(ctx, []string{"version"}); code != 143 {
		t.Errorf("clean return: exit = %d, want 143", code)
	}
}

func TestSignalKeepsTheStepToResumeFrom(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	a, stdout, stderr := testApp(t)
	root := newRootCmd(a)
	up, _, err := root.Find([]string{"up"})
	if err != nil {
		t.Fatal(err)
	}
	up.RunE = func(cmd *cobra.Command, _ []string) error {
		err := steps.Run(cmd.Context(), a.newSink(), []steps.Step{{
			ID: "db-migrate",
			Apply: func(ctx context.Context, _ events.Sink) error {
				cancel(exitcode.Signaled(syscall.SIGINT))
				return ctx.Err()
			},
		}}, steps.Options{})
		return exitcode.Failed("up: %w", err)
	}
	markRunning(a, up)

	if code := a.execute(ctx, root, []string{"up", "--json"}); code != exitcode.CodeInterrupted {
		t.Errorf("exit = %d, want %d", code, exitcode.CodeInterrupted)
	}
	msg := "up: stopped at step db-migrate: interrupted (interrupt); re-run the command to resume from it"
	if got, want := stderr.String(), "pic-sure: "+msg+"\n"; got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	var result struct {
		Error events.ErrorInfo `json:"error"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &result); err != nil {
		t.Fatalf("last stdout line %q: %v", lines[len(lines)-1], err)
	}
	want := events.ErrorInfo{ExitCode: exitcode.CodeInterrupted, Message: msg, Step: "db-migrate"}
	if result.Error != want {
		t.Errorf("result error = %+v, want %+v", result.Error, want)
	}
}

func TestRunIsRepeatable(t *testing.T) {
	a, _, _ := testApp(t)
	if code := a.Run(context.Background(), []string{"--stack", "/nowhere", "up"}); code != exitcode.CodePrecondition {
		t.Fatalf("up: exit = %d", code)
	}
	// The first run reached RunE; the second must still classify cobra's
	// rejection of its command line as a usage error.
	if code := a.Run(context.Background(), []string{"up", "--frobnicate"}); code != exitcode.CodeUsage {
		t.Errorf("second run: exit = %d, want %d", code, exitcode.CodeUsage)
	}
}

func TestHelpCommand(t *testing.T) {
	// help TOPIC prints the same as TOPIC --help.
	for _, topic := range [][]string{nil, {"config", "set"}} {
		a, helpOut, _ := testApp(t)
		if code := a.Run(context.Background(), append([]string{"help"}, topic...)); code != 0 {
			t.Errorf("help %v: exit = %d", topic, code)
		}
		b, flagOut, _ := testApp(t)
		if code := b.Run(context.Background(), append(topic, "--help")); code != 0 {
			t.Errorf("%v --help: exit = %d", topic, code)
		}
		if helpOut.String() != flagOut.String() || !strings.Contains(helpOut.String(), "-h, --help") {
			t.Errorf("help %v:\n%s\n%v --help:\n%s", topic, helpOut, topic, flagOut)
		}
	}

	// An unknown topic is a usage error and prints no help.
	for _, args := range [][]string{{"help", "frobnicate"}, {"help", "config", "frobnicate"}} {
		a, stdout, stderr := testApp(t)
		if code := a.Run(context.Background(), args); code != exitcode.CodeUsage {
			t.Errorf("%v: exit = %d, want %d", args, code, exitcode.CodeUsage)
		}
		if stdout.Len() != 0 || !strings.Contains(stderr.String(), "unknown help topic") {
			t.Errorf("%v: stdout %q, stderr %q", args, stdout, stderr)
		}
	}
}

func TestBareInvocation(t *testing.T) {
	t.Run("no terminal prints help", func(t *testing.T) {
		a, stdout, _ := testApp(t)
		if code := a.Run(context.Background(), nil); code != 0 {
			t.Errorf("exit = %d", code)
		}
		if !strings.Contains(stdout.String(), "Usage:") {
			t.Errorf("stdout = %q, want help", stdout)
		}
	})
	t.Run("terminal starts the TUI on --stack", func(t *testing.T) {
		a, _, _ := testApp(t)
		a.IsTerminal = func() bool { return true }
		var got *tui.Options
		a.StartTUI = func(_ context.Context, o tui.Options) error { got = &o; return nil }
		if code := a.Run(context.Background(), []string{"--stack", "/srv/demo", "--no-animations"}); code != 0 {
			t.Errorf("exit = %d", code)
		}
		if got == nil || got.Root != "/srv/demo" || got.Start != tui.ScreenLanding || got.Animations || got.Init == nil || got.Defaults == nil {
			t.Errorf("TUI options = %+v", got)
		}
	})
	for _, flag := range []string{"--json", "--plain", "--yes", "--non-interactive"} {
		t.Run("terminal with "+flag+" prints help", func(t *testing.T) {
			a, stdout, _ := testApp(t)
			a.IsTerminal = func() bool { return true }
			if code := a.Run(context.Background(), []string{flag}); code != 0 {
				t.Errorf("exit = %d", code)
			}
			if !strings.Contains(stdout.String(), "Usage:") {
				t.Errorf("stdout = %q, want help", stdout)
			}
		})
	}
}

func TestNewDeps(t *testing.T) {
	a, _, _ := testApp(t)
	d := a.newDeps()
	if d.Runner == nil || d.Clock == nil || d.Rand == nil || d.Sink == nil || d.Log == nil {
		t.Fatalf("newDeps left a required field nil: %+v", d)
	}
	if r, ok := d.Runner.(*docker.ExecRunner); !ok || r.Log != d.Log {
		t.Errorf("Runner = %#v, want an ExecRunner logging to Deps.Log", d.Runner)
	}
}
