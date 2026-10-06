// Package fakerunner is a docker.Runner for unit tests. Tests register rules
// that match argv and give canned output, run the code under test, then
// assert on the recorded calls. A call no rule matches fails the test.
//
//	f := fakerunner.New(t)
//	f.On(fakerunner.Exact("docker", "volume", "inspect", "demo_hpds-data")).Exit(1)
//	f.On(fakerunner.Glob("docker volume create *"))
//	// ... run the code under test with f as its Runner ...
//	f.AssertOrder(
//		fakerunner.Glob("docker volume inspect *"),
//		fakerunner.Glob("docker volume create *"),
//	)
//
// Calls record env names only, never values, so assertions can show that a
// secret was passed by environment without the test handling the value.
package fakerunner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
)

// ErrUnmatched is returned (wrapped) for a call that no rule matches.
var ErrUnmatched = errors.New("fakerunner: no rule matches")

// Call is one recorded invocation.
type Call struct {
	Argv  []string
	Env   []string // the names from Cmd.Env, in order; never the values
	Stdin []byte
	Dir   string
}

func (c Call) String() string { return docker.FormatArgv(c.Argv) }

// HasEnv reports whether the call was given the environment variable name.
func (c Call) HasEnv(name string) bool {
	for _, n := range c.Env {
		if n == name {
			return true
		}
	}
	return false
}

// Rule is the canned response for calls that match it. Configure it before
// the code under test runs; its methods return the rule for chaining.
type Rule struct {
	match  Matcher
	stdout string
	stderr string
	exit   int
	err    error
	do     func(context.Context, Call) (docker.Result, error)
	times  int // 0 means unlimited
	used   int
}

// Stdout sets the call's standard output.
func (r *Rule) Stdout(s string) *Rule { r.stdout = s; return r }

// Stderr sets the call's standard error.
func (r *Rule) Stderr(s string) *Rule { r.stderr = s; return r }

// Exit sets the call's exit code (default 0).
func (r *Rule) Exit(code int) *Rule { r.exit = code; return r }

// Err makes the call fail to run, as if the program could not be started.
func (r *Rule) Err(err error) *Rule { r.err = err; return r }

// Times limits the rule to its first n matches; later calls fall through to
// the next matching rule. Use it for responses that change over time, such as
// a container that is "starting" twice and then "healthy".
func (r *Rule) Times(n int) *Rule { r.times = n; return r }

// Do computes the response instead. It overrides Stdout, Stderr, Exit and
// Err, and can block on ctx to simulate a long-running process.
func (r *Rule) Do(fn func(ctx context.Context, c Call) (docker.Result, error)) *Rule {
	r.do = fn
	return r
}

// Runner is the fake. It is safe for concurrent use.
type Runner struct {
	t testing.TB

	mu    sync.Mutex
	rules []*Rule
	calls []Call
}

var _ docker.Runner = (*Runner)(nil)

// New returns a Runner that reports unmatched calls and failed assertions
// through t.
func New(t testing.TB) *Runner { return &Runner{t: t} }

// On adds a rule. Rules are tried in the order they were added, and the
// first one that matches (and has uses left) answers the call. A rule with
// no response set exits 0 with no output.
func (f *Runner) On(m Matcher) *Rule {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := &Rule{match: m}
	f.rules = append(f.rules, r)
	return r
}

// Run implements docker.Runner.
func (f *Runner) Run(ctx context.Context, c docker.Cmd) (docker.Result, error) {
	return f.call(ctx, c)
}

// Stream implements docker.Runner; it writes the canned output to the
// writers in one piece each.
func (f *Runner) Stream(ctx context.Context, c docker.Cmd, stdout, stderr io.Writer) (int, error) {
	res, err := f.call(ctx, c)
	if err != nil {
		return res.ExitCode, err
	}
	for _, out := range []struct {
		w    io.Writer
		data []byte
	}{{stdout, res.Stdout}, {stderr, res.Stderr}} {
		if out.w != nil && len(out.data) > 0 {
			if _, err := out.w.Write(out.data); err != nil {
				return res.ExitCode, err
			}
		}
	}
	return res.ExitCode, nil
}

func (f *Runner) call(ctx context.Context, c docker.Cmd) (docker.Result, error) {
	if err := ctx.Err(); err != nil {
		return docker.Result{}, err
	}
	call := Call{Argv: append([]string(nil), c.Argv...), Dir: c.Dir}
	for _, kv := range c.Env {
		name, _, _ := strings.Cut(kv, "=")
		call.Env = append(call.Env, name)
	}
	if c.Stdin != nil {
		in, err := io.ReadAll(c.Stdin)
		if err != nil {
			return docker.Result{}, fmt.Errorf("fakerunner: reading stdin: %w", err)
		}
		call.Stdin = in
	}

	f.mu.Lock()
	f.calls = append(f.calls, call)
	rule := f.claimRule(call.Argv)
	f.mu.Unlock()

	if rule == nil {
		f.t.Errorf("fakerunner: unexpected call: %s", call)
		return docker.Result{}, fmt.Errorf("%w: %s", ErrUnmatched, call)
	}
	if rule.do != nil {
		return rule.do(ctx, call)
	}
	if rule.err != nil {
		return docker.Result{}, rule.err
	}
	return docker.Result{Stdout: []byte(rule.stdout), Stderr: []byte(rule.stderr), ExitCode: rule.exit}, nil
}

// claimRule finds the rule for argv and uses it up once. f.mu must be held.
func (f *Runner) claimRule(argv []string) *Rule {
	for _, r := range f.rules {
		if (r.times == 0 || r.used < r.times) && r.match.Match(argv) {
			r.used++
			return r
		}
	}
	return nil
}

// Calls returns every call so far, in order.
func (f *Runner) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

// CallsMatching returns the calls that m matches, in order.
func (f *Runner) CallsMatching(m Matcher) []Call {
	var out []Call
	for _, c := range f.Calls() {
		if m.Match(c.Argv) {
			out = append(out, c)
		}
	}
	return out
}

// AssertCalled fails the test unless some call matches m.
func (f *Runner) AssertCalled(m Matcher) {
	f.t.Helper()
	if len(f.CallsMatching(m)) == 0 {
		f.t.Errorf("fakerunner: no call matches %s; calls:\n%s", m, f.callList())
	}
}

// AssertNotCalled fails the test if any call matches m.
func (f *Runner) AssertNotCalled(m Matcher) {
	f.t.Helper()
	if got := f.CallsMatching(m); len(got) > 0 {
		f.t.Errorf("fakerunner: %s was called %d times; calls:\n%s", m, len(got), f.callList())
	}
}

// AssertOrder fails the test unless the calls include, in this order, one
// matching each matcher. Other calls may come before, between and after.
func (f *Runner) AssertOrder(ms ...Matcher) {
	f.t.Helper()
	next := 0
	for _, c := range f.Calls() {
		if next < len(ms) && ms[next].Match(c.Argv) {
			next++
		}
	}
	if next < len(ms) {
		f.t.Errorf("fakerunner: no call matches %s after the calls matching %s; calls:\n%s",
			ms[next], matcherList(ms[:next]), f.callList())
	}
}

func (f *Runner) callList() string {
	calls := f.Calls()
	if len(calls) == 0 {
		return "  (none)"
	}
	lines := make([]string, len(calls))
	for i, c := range calls {
		lines[i] = fmt.Sprintf("  %d: %s", i, c)
	}
	return strings.Join(lines, "\n")
}

func matcherList(ms []Matcher) string {
	if len(ms) == 0 {
		return "(nothing)"
	}
	parts := make([]string, len(ms))
	for i, m := range ms {
		parts[i] = m.String()
	}
	return strings.Join(parts, ", ")
}
