package fakerunner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
)

// spyTB records failures instead of failing the real test, so the fake's own
// failure reporting can be asserted.
type spyTB struct {
	testing.TB
	errs []string
}

func (s *spyTB) Helper() {}

func (s *spyTB) Errorf(format string, args ...any) {
	s.errs = append(s.errs, fmt.Sprintf(format, args...))
}

func TestMatchers(t *testing.T) {
	argv := []string{"docker", "compose", "-f", "/s/render/compose.yaml", "up", "-d", "--wait", "hpds"}
	tests := []struct {
		m    Matcher
		want bool
	}{
		{Exact("docker", "compose", "-f", "/s/render/compose.yaml", "up", "-d", "--wait", "hpds"), true},
		{Exact("docker", "compose"), false},
		{Glob("docker compose * up -d *"), true},
		{Glob("docker compose * down*"), false},
		{Glob("docker compose -f /s/render/compose.yaml up -d --wait hpd?"), true},
		// Globs are anchored at both ends, and . is literal.
		{Glob("docker compose"), false},
		{Glob("docker compose -f /s/render/compose.yaml up -d --wait hpds.*"), false},
		{Regex(`\bup -d\b`), true},
		{Regex(`^docker volume`), false},
	}
	for _, tt := range tests {
		if got := tt.m.Match(argv); got != tt.want {
			t.Errorf("%s.Match = %v, want %v", tt.m, got, tt.want)
		}
	}
}

func TestRunRecordsCallsAndReturnsCannedOutput(t *testing.T) {
	f := New(t)
	f.On(Exact("docker", "exec", "-i", "-e", "MYSQL_PWD", "db", "mysql")).Stdout("ok\n").Stderr("warn\n").Exit(3)

	res, err := f.Run(context.Background(), docker.Cmd{
		Argv:  []string{"docker", "exec", "-i", "-e", "MYSQL_PWD", "db", "mysql"},
		Env:   []string{"MYSQL_PWD=s3cret-value"},
		Stdin: strings.NewReader("SELECT 1;"),
		Dir:   "/stack",
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Stdout) != "ok\n" || string(res.Stderr) != "warn\n" || res.ExitCode != 3 {
		t.Errorf("result = %+v", res)
	}

	calls := f.Calls()
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	c := calls[0]
	if !c.HasEnv("MYSQL_PWD") || c.HasEnv("OTHER") {
		t.Errorf("Env = %v, want just MYSQL_PWD", c.Env)
	}
	if strings.Contains(fmt.Sprintf("%+v", c), "s3cret-value") {
		t.Errorf("recorded call holds the env value: %+v", c)
	}
	if string(c.Stdin) != "SELECT 1;" || c.Dir != "/stack" {
		t.Errorf("Stdin = %q, Dir = %q", c.Stdin, c.Dir)
	}
}

func TestTimesSequencesResponses(t *testing.T) {
	f := New(t)
	ps := Glob("docker inspect *")
	f.On(ps).Stdout("starting").Times(2)
	f.On(ps).Stdout("healthy")

	var got []string
	for i := 0; i < 4; i++ {
		res, err := f.Run(context.Background(), docker.Cmd{Argv: []string{"docker", "inspect", "db"}})
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, string(res.Stdout))
	}
	if want := "starting starting healthy healthy"; strings.Join(got, " ") != want {
		t.Errorf("responses = %q, want %q", got, want)
	}
}

func TestUnmatchedCallFailsTheTest(t *testing.T) {
	spy := &spyTB{TB: t}
	f := New(spy)
	_, err := f.Run(context.Background(), docker.Cmd{Argv: []string{"docker", "rm", "x"}})
	if !errors.Is(err, ErrUnmatched) {
		t.Errorf("err = %v, want ErrUnmatched", err)
	}
	if len(spy.errs) != 1 || !strings.Contains(spy.errs[0], "docker rm x") {
		t.Errorf("reported %q, want one error naming the call", spy.errs)
	}
	if len(f.Calls()) != 1 {
		t.Error("an unmatched call should still be recorded")
	}
}

func TestStream(t *testing.T) {
	f := New(t)
	f.On(Glob("docker build *")).Stdout("step 1\nstep 2\n").Stderr("warning\n").Exit(1)

	var out, errOut bytes.Buffer
	code, err := f.Stream(context.Background(), docker.Cmd{Argv: []string{"docker", "build", "."}}, &out, &errOut)
	if err != nil || code != 1 {
		t.Fatalf("Stream = %d, %v", code, err)
	}
	if out.String() != "step 1\nstep 2\n" || errOut.String() != "warning\n" {
		t.Errorf("stdout %q, stderr %q", out.String(), errOut.String())
	}
	if _, err := f.Stream(context.Background(), docker.Cmd{Argv: []string{"docker", "build", "."}}, nil, nil); err != nil {
		t.Errorf("nil writers: %v", err)
	}
}

func TestErrAndDo(t *testing.T) {
	f := New(t)
	notFound := errors.New("exec: docker: not found")
	f.On(Exact("docker", "version")).Err(notFound)
	f.On(Glob("docker logs -f *")).Do(func(ctx context.Context, c Call) (docker.Result, error) {
		<-ctx.Done()
		return docker.Result{ExitCode: 143}, ctx.Err()
	})

	if _, err := f.Run(context.Background(), docker.Cmd{Argv: []string{"docker", "version"}}); !errors.Is(err, notFound) {
		t.Errorf("Err rule: got %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := f.Stream(ctx, docker.Cmd{Argv: []string{"docker", "logs", "-f", "hpds"}}, nil, nil)
		done <- err
	}()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Do rule after cancel: got %v", err)
	}
}

func TestDoneContextIsNotACall(t *testing.T) {
	f := New(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.Run(ctx, docker.Cmd{Argv: []string{"docker", "ps"}}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if n := len(f.Calls()); n != 0 {
		t.Errorf("recorded %d calls, want 0", n)
	}
}

func TestAssertions(t *testing.T) {
	spy := &spyTB{TB: t}
	f := New(spy)
	f.On(Glob("*"))
	for _, argv := range [][]string{
		{"docker", "compose", "stop", "hpds"},
		{"docker", "run", "--rm", "hpds-etl"},
		{"docker", "compose", "up", "-d", "hpds"},
	} {
		if _, err := f.Run(context.Background(), docker.Cmd{Argv: argv}); err != nil {
			t.Fatal(err)
		}
	}

	f.AssertCalled(Glob("docker run *"))
	f.AssertNotCalled(Glob("docker volume rm *"))
	f.AssertOrder(Glob("* stop hpds"), Glob("docker run *"), Glob("* up -d hpds"))
	if len(spy.errs) != 0 {
		t.Fatalf("passing assertions reported %q", spy.errs)
	}

	f.AssertCalled(Glob("docker volume rm *"))
	f.AssertNotCalled(Glob("docker run *"))
	f.AssertOrder(Glob("* up -d hpds"), Glob("* stop hpds"))
	if len(spy.errs) != 3 {
		t.Fatalf("failing assertions reported %d errors, want 3: %q", len(spy.errs), spy.errs)
	}
	if !strings.Contains(spy.errs[2], "no call matches * stop hpds after the calls matching * up -d hpds") {
		t.Errorf("AssertOrder message = %q", spy.errs[2])
	}
}
