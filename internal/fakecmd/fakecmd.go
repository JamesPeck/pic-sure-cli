// Package fakecmd is the fake `docker` and `git` that CLI scenario tests put
// on PATH (spec §11). cmd/pic-sure's testscript harness registers them, so
// every script under cmd/pic-sure/testdata/script runs the real pic-sure
// binary against them.
//
// A fake reads its rules from $HOME/<name>.scenario (docker.scenario,
// git.scenario), answers each call from the first matching rule, and
// appends the call's argv to $HOME/<name>.log, one line per call, quoting
// arguments that contain spaces as docker.FormatArgv does. The harness sets
// HOME to the script's work directory, so a script supplies the scenario as
// a txtar file and checks the log with `cmp` or `grep`. HOME is used because
// the CLI's runner passes it through to subprocesses (ticket 003).
//
// Scenario syntax, one rule per line; blank lines and # comments are
// ignored:
//
//	PATTERN => EXIT [stdout=FILE] [stderr=FILE] [times=N]
//
// PATTERN is matched against the whole argv, program name included, joined
// with single spaces and unquoted: a glob in which * matches any run of
// characters and ? any one character, or, with a re: prefix, a regular
// expression. EXIT is the exit code. FILE names a file (relative paths are
// from the scenario's directory) whose contents the fake writes to that
// stream. times=N retires the rule after N matches, so later calls fall
// through to the next matching rule. For example:
//
//	docker version --format json => 0 stdout=version.json
//	docker compose * ps --format json => 0 stdout=starting.json times=1
//	docker compose * ps --format json => 0 stdout=healthy.json
//	re:^docker volume rm => 1 stderr=in-use.txt
//
// A call that no rule matches, or a broken scenario, exits ExitMisuse with
// the reason on stderr.
package fakecmd

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
)

// ExitMisuse is the exit code for an unmatched call or a bad scenario.
// Scenarios should not script it, so it always means the fake was misused.
const ExitMisuse = 97

// Rule is one scenario line.
type Rule struct {
	Match  fakerunner.Matcher
	Exit   int
	Stdout string // file path; relative paths are from the scenario's directory
	Stderr string
	Times  int // 0 means unlimited
}

// ParseScenario reads scenario rules from r.
func ParseScenario(r io.Reader) ([]Rule, error) {
	var rules []Rule
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rule, err := parseRule(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		rules = append(rules, rule)
	}
	return rules, sc.Err()
}

func parseRule(line string) (Rule, error) {
	pattern, response, ok := strings.Cut(line, "=>")
	if !ok {
		return Rule{}, errors.New(`want "PATTERN => EXIT [stdout=FILE] [stderr=FILE] [times=N]"`)
	}
	var rule Rule
	pattern = strings.TrimSpace(pattern)
	if expr, isRegex := strings.CutPrefix(pattern, "re:"); isRegex {
		if _, err := regexp.Compile(expr); err != nil {
			return Rule{}, fmt.Errorf("bad regular expression: %w", err)
		}
		rule.Match = fakerunner.Regex(expr)
	} else {
		rule.Match = fakerunner.Glob(pattern)
	}

	fields := strings.Fields(response)
	if len(fields) == 0 {
		return Rule{}, errors.New("missing exit code after =>")
	}
	code, err := strconv.Atoi(fields[0])
	if err != nil {
		return Rule{}, fmt.Errorf("exit code %q is not a number", fields[0])
	}
	rule.Exit = code
	for _, opt := range fields[1:] {
		key, value, ok := strings.Cut(opt, "=")
		if !ok || value == "" {
			return Rule{}, fmt.Errorf("option %q needs a value", opt)
		}
		switch key {
		case "stdout":
			rule.Stdout = value
		case "stderr":
			rule.Stderr = value
		case "times":
			if rule.Times, err = strconv.Atoi(value); err != nil || rule.Times < 1 {
				return Rule{}, fmt.Errorf("times=%s is not a positive number", value)
			}
		default:
			return Rule{}, fmt.Errorf("unknown option %q", opt)
		}
	}
	return rule, nil
}

// Main runs the fake called name with the process's arguments and returns
// its exit code.
func Main(name string) int {
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice == 0 {
		// Drain piped input so the caller never blocks writing to us.
		_, _ = io.Copy(io.Discard, os.Stdin)
	}
	return run(name, os.Args[1:], os.Getenv("HOME"), os.Stdout, os.Stderr)
}

func run(name string, args []string, home string, stdout, stderr io.Writer) int {
	argv := append([]string{name}, args...)
	fail := func(format string, a ...any) int {
		_, _ = fmt.Fprintf(stderr, "fake %s: "+format+"\n", append([]any{name}, a...)...)
		return ExitMisuse
	}
	if home == "" {
		return fail("HOME is not set")
	}
	rule, err := claim(name, argv, home)
	if err != nil {
		return fail("%v", err)
	}
	for _, out := range []struct {
		file string
		w    io.Writer
	}{{rule.Stdout, stdout}, {rule.Stderr, stderr}} {
		if out.file == "" {
			continue
		}
		path := out.file
		if !filepath.IsAbs(path) {
			path = filepath.Join(home, path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fail("%v", err)
		}
		if _, err := out.w.Write(data); err != nil {
			return fail("%v", err)
		}
	}
	return rule.Exit
}

// claim logs the call and picks the rule that answers it. The CLI may run
// several fakes at once, so claim holds a lock per fake name to keep the log
// in call order and the times=N counts exact. The lock is released before
// the caller writes output: a full pipe must not block other calls.
func claim(name string, argv []string, home string) (Rule, error) {
	unlock, err := lockFile(filepath.Join(home, name+".lock"))
	if err != nil {
		return Rule{}, err
	}
	defer unlock()
	if err := appendLine(filepath.Join(home, name+".log"), docker.FormatArgv(argv)); err != nil {
		return Rule{}, err
	}

	scenarioPath := filepath.Join(home, name+".scenario")
	rules, err := readScenario(scenarioPath)
	if err != nil {
		return Rule{}, err
	}
	countsPath := filepath.Join(home, name+".counts")
	counts, err := readCounts(countsPath)
	if err != nil {
		return Rule{}, err
	}
	for i, rule := range rules {
		if rule.Times > 0 && counts[i] >= rule.Times {
			continue
		}
		if !rule.Match.Match(argv) {
			continue
		}
		if rule.Times > 0 {
			counts[i]++
			if err := writeCounts(countsPath, counts); err != nil {
				return Rule{}, err
			}
		}
		return rule, nil
	}
	return Rule{}, fmt.Errorf("no rule in %s matches: %s", scenarioPath, docker.FormatArgv(argv))
}

func readScenario(path string) ([]Rule, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	rules, err := ParseScenario(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return rules, nil
}

// readCounts loads how often each times=N rule (by index) has matched in
// earlier calls of this script.
func readCounts(path string) (map[int]int, error) {
	counts := map[int]int{}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return counts, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &counts); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return counts, nil
}

func writeCounts(path string, counts map[int]int) error {
	data, err := json.Marshal(counts)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// lockFile takes an exclusive flock on path and returns the function that
// releases it.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	return func() { _ = f.Close() }, nil
}

func appendLine(path, line string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(f, line)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
