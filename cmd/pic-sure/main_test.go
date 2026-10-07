package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"

	"github.com/JamesPeck/pic-sure-cli/internal/fakecmd"
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
			"exitcode":  cmdExitCode,
			"jsonlines": cmdJSONLines,
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
