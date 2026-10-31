package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"

	"github.com/JamesPeck/pic-sure-cli/internal/fakecmd"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
)

func TestMain(m *testing.M) {
	testscript.Main(m, map[string]func(){
		"pic-sure": func() { os.Exit(run(os.Args[1:])) },
		"docker":   func() { os.Exit(fakecmd.Main("docker")) },
		"git":      func() { os.Exit(fakecmd.Main("git")) },
	})
}

// TestScripts runs the CLI scenarios in testdata/script with the fake docker
// and git (internal/fakecmd) first on PATH. HOME is the script's work
// directory, which is where the fakes look for their scenarios.
func TestScripts(t *testing.T) {
	testscript.Run(t, testscript.Params{
		Dir:                 "testdata/script",
		RequireExplicitExec: true,
		RequireUniqueNames:  true,
		Setup: func(env *testscript.Env) error {
			env.Setenv("HOME", env.WorkDir)
			return nil
		},
		Cmds: map[string]func(*testscript.TestScript, bool, []string){
			"exitcode":      cmdExitCode,
			"jsonlines":     cmdJSONLines,
			"teardownmatch": cmdTeardownMatch,
		},
	})
}

// cmdExitCode is `exitcode N PROGRAM [ARGS...]`: run PROGRAM like exec does
// (keeping its stdout and stderr for later assertions) and require exit
// status N. It exists because `! exec` accepts any non-zero status, and the
// exit codes are part of the CLI's contract (spec §10.4).
func cmdExitCode(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("unsupported: ! exitcode")
	}
	if len(args) < 2 {
		ts.Fatalf("usage: exitcode N PROGRAM [ARGS...]")
	}
	want, err := strconv.Atoi(args[0])
	if err != nil {
		ts.Fatalf("exitcode: %q is not a number", args[0])
	}
	got := 0
	if err := ts.Exec(args[1], args[2:]...); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			ts.Fatalf("exitcode: running %s: %v", args[1], err)
		}
		got = exitErr.ExitCode()
	}
	if got != want {
		ts.Fatalf("%s exited %d, want %d", args[1], got, want)
	}
}

// cmdJSONLines is `jsonlines FILE`: FILE (usually stdout) must be one JSON
// object per line and nothing else, as --json promises (spec §10.3).
func cmdJSONLines(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("unsupported: ! jsonlines")
	}
	if len(args) != 1 {
		ts.Fatalf("usage: jsonlines FILE")
	}
	data := ts.ReadFile(args[0])
	if !strings.HasSuffix(data, "\n") {
		ts.Fatalf("%s is empty or doesn't end in a newline", args[0])
	}
	for i, line := range strings.Split(strings.TrimSuffix(data, "\n"), "\n") {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &obj); err != nil || obj == nil {
			ts.Fatalf("%s line %d is not a JSON object: %v\n%s", args[0], i+1, err, line)
		}
	}
}

// teardownmatch compares CLI reports while accounting for artifacts that only
// the real invocation creates: its own run log, and missing log/lock paths.
func cmdTeardownMatch(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 2 {
		ts.Fatalf("usage: teardownmatch PREVIEW REAL")
	}
	read := func(name string) ops.TeardownReport {
		lines := strings.Split(strings.TrimSpace(ts.ReadFile(name)), "\n")
		var result struct {
			Type string
			OK   bool
			Data ops.TeardownReport
		}
		if err := json.Unmarshal([]byte(lines[len(lines)-1]), &result); err != nil {
			ts.Fatalf("%s: invalid teardown JSON: %v", name, err)
		}
		if result.Type != "result" || !result.OK || result.Data.Files == nil {
			ts.Fatalf("%s: expected a successful teardown result with files, got %s", name, lines[len(lines)-1])
		}
		return result.Data
	}
	preview, real := read(args[0]), read(args[1])
	if !preview.DryRun || real.DryRun {
		ts.Fatalf("expected preview followed by real teardown")
	}
	runLog := regexp.MustCompile(`^\.pic-sure/logs/cli-[0-9]{8}T[0-9]{6}\.[0-9]{3}Z-[0-9]+(-[0-9]+)?\.log$`)
	newLogs := 0
	real.Files.Removed = slices.DeleteFunc(real.Files.Removed, func(path string) bool {
		if slices.Contains(preview.Files.Removed, path) {
			return false
		}
		if runLog.MatchString(path) {
			newLogs++
			return true
		}
		return path == ".pic-sure/lock" || path == ".pic-sure/logs"
	})
	if newLogs != 1 {
		ts.Fatalf("expected exactly one new run log in real report; got %d", newLogs)
	}
	preview.DryRun = false
	preview.Containers = nil // real reports retain their pre-preview wire format
	a, err := json.Marshal(preview)
	if err != nil {
		ts.Fatalf("encoding report: %v", err)
	}
	b, err := json.Marshal(real)
	if err != nil {
		ts.Fatalf("encoding report: %v", err)
	}
	if string(a) != string(b) {
		ts.Fatalf("preview and real removal sets differ:\npreview %s\nreal %s", a, b)
	}
}
