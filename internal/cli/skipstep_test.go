package cli

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/log"
)

// runnableCommands returns every runnable command under root, root
// included.
func runnableCommands(root *cobra.Command) []*cobra.Command {
	var cmds []*cobra.Command
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		if c.Runnable() {
			cmds = append(cmds, c)
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
	return cmds
}

// commandLine is the arguments that reach c's RunE apart from --skip-step:
// its path, the fewest positional arguments it accepts (its first valid
// argument, or "x"), and an existing file for each required flag.
func commandLine(t *testing.T, c *cobra.Command, file string) []string {
	t.Helper()
	args := strings.Fields(commandName(c))
	if !c.HasParent() {
		args = nil
	}
	arg := "x"
	if len(c.ValidArgs) > 0 {
		arg = c.ValidArgs[0]
	}
	var pos []string
	for c.Args != nil && c.Args(c, pos) != nil {
		if len(pos) == 3 {
			t.Fatalf("%s: no positional arguments it accepts", c.CommandPath())
		}
		pos = append(pos, arg)
	}
	args = append(args, pos...)
	c.Flags().VisitAll(func(f *pflag.Flag) {
		if _, ok := f.Annotations[cobra.BashCompOneRequiredFlag]; ok {
			args = append(args, "--"+f.Name, file)
		}
	})
	return args
}

// skipStepEnv is a stack the commands can run on, with a docker that logs
// its arguments, and a cache directory. It returns the stack and the
// docker log.
func skipStepEnv(t *testing.T) (dir, dockerLog string) {
	t.Helper()
	bin := t.TempDir()
	dockerLog = filepath.Join(bin, "docker.log")
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\necho \"$@\" >> "+dockerLog+"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(t.TempDir(), "cache"))
	// A command that ran would write here: support-bundle writes its
	// archive to the current directory.
	t.Chdir(t.TempDir())
	dir = renderedStack(t)
	if err := os.WriteFile(filepath.Join(dir, "input.csv"), []byte("a,b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, dockerLog
}

// TestSkipStepRefusedUnlessSupported runs every command that doesn't
// declare --skip-step with it: each is exit 2 before it asks docker
// anything or writes to the stack.
func TestSkipStepRefusedUnlessSupported(t *testing.T) {
	a, _, _ := testApp(t)
	var skippable []string
	for _, c := range runnableCommands(newRootCmd(a)) {
		if c.Annotations[skipStepAnnotation] != "" {
			if !c.Hidden {
				skippable = append(skippable, commandName(c))
			}
			continue
		}
		t.Run(commandName(c), func(t *testing.T) {
			dir, dockerLog := skipStepEnv(t)
			a, _, stderr := testApp(t)
			// Before the command, since config set takes everything after
			// its name as arguments.
			args := append([]string{"--stack", dir, "--yes", "--skip-step", "x"}, commandLine(t, c, filepath.Join(dir, "input.csv"))...)
			if code := a.Run(context.Background(), args); code != exitcode.CodeUsage {
				t.Fatalf("%v: exit %d, want %d; stderr:\n%s", args, code, exitcode.CodeUsage, stderr)
			}
			if want := "--skip-step: " + commandName(c) + " has no steps to skip; only "; !strings.Contains(stderr.String(), want) {
				t.Errorf("%v: stderr %q, want %q", args, stderr, want)
			}
			if _, err := os.Stat(dockerLog); err == nil {
				t.Errorf("%v: docker was run", args)
			}
			if _, err := os.Stat(filepath.Join(dir, log.Dir)); err == nil {
				t.Errorf("%v: wrote a run log", args)
			}
		})
	}
	want := []string{"build", "db bootstrap", "dictionary hydrate", "dictionary load-csv", "dictionary load-facets", "dictionary weights", "init", "migrate", "up", "update"}
	if !slices.Equal(skippable, want) {
		t.Errorf("skippable commands %v, want %v", skippable, want)
	}
	// The flag's help names them by hand.
	usage := newRootCmd(a).PersistentFlags().Lookup("skip-step").Usage
	for _, name := range want {
		if first := strings.Fields(name)[0]; !slices.Contains(strings.FieldsFunc(usage, func(r rune) bool { return r == ' ' || r == ',' || r == ';' || r == ')' }), first) {
			t.Errorf("--skip-step's help %q doesn't name %s", usage, first)
		}
	}
}

// TestSkipStepUnknownIDIsRefusedEarly runs every command that declares
// --skip-step with an ID it has no step for: each is exit 2 before it
// takes the lock, records an operation or registers the stack.
func TestSkipStepUnknownIDIsRefusedEarly(t *testing.T) {
	a, _, _ := testApp(t)
	for _, c := range runnableCommands(newRootCmd(a)) {
		if c.Annotations[skipStepAnnotation] == "" || c.Hidden {
			continue
		}
		t.Run(commandName(c), func(t *testing.T) {
			dir, dockerLog := skipStepEnv(t)
			args := []string{"--stack", dir, "--yes"}
			args = append(args, commandLine(t, c, filepath.Join(dir, "input.csv"))...)
			if c.Name() == "init" {
				// init creates its stack, so it gets a directory that
				// doesn't exist yet.
				dir = filepath.Join(t.TempDir(), "new")
				args = []string{"--yes", "init", dir, "--name", "demo", "--admin-email", "admin@example.com", "--auth-mode", "open"}
			}
			args = append(args, "--skip-step", "bogus")
			state := filepath.Join(dir, ".pic-sure", "state.json")
			before, _ := os.ReadFile(state)
			a, _, stderr := testApp(t)
			if code := a.Run(context.Background(), args); code != exitcode.CodeUsage {
				t.Fatalf("%v: exit %d, want %d; stderr:\n%s", args, code, exitcode.CodeUsage, stderr)
			}
			if want := "--skip-step bogus: " + commandName(c) + " has no such step; it can skip "; !strings.Contains(stderr.String(), want) {
				t.Errorf("%v: stderr %q, want %q", args, stderr, want)
			}
			if after, _ := os.ReadFile(state); string(after) != string(before) {
				t.Errorf("%v: state.json changed:\n%s", args, after)
			}
			if _, err := os.Stat(filepath.Join(dir, log.Dir)); err == nil {
				t.Errorf("%v: wrote a run log", args)
			}
			if _, err := os.Stat(filepath.Join(dir, ".pic-sure", "lock")); err == nil {
				t.Errorf("%v: took the stack lock", args)
			}
			if _, err := os.Stat(os.Getenv("XDG_CACHE_HOME")); err == nil {
				t.Errorf("%v: opened the cache", args)
			}
			if _, err := os.Stat(dockerLog); err == nil {
				t.Errorf("%v: docker was run", args)
			}
		})
	}
}
