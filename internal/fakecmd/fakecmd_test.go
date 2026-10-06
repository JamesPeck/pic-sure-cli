package fakecmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRunReplaysScenarioAndLogsArgv(t *testing.T) {
	home := t.TempDir()
	writeFiles(t, home, map[string]string{
		"docker.scenario": `# health goes starting -> healthy
docker inspect * => 0 stdout=starting.txt times=2
docker inspect * => 0 stdout=healthy.txt
re:^docker volume rm => 1 stderr=in-use.txt
docker version => 0
`,
		"starting.txt": "starting\n",
		"healthy.txt":  "healthy\n",
		"in-use.txt":   "Error: volume is in use\n",
	})

	call := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := run("docker", args, home, &out, &errOut)
		return code, out.String(), errOut.String()
	}

	for i, want := range []string{"starting\n", "starting\n", "healthy\n", "healthy\n"} {
		if code, out, _ := call("inspect", "db"); code != 0 || out != want {
			t.Errorf("inspect #%d = %d %q, want 0 %q", i, code, out, want)
		}
	}
	if code, out, errOut := call("volume", "rm", "x"); code != 1 || out != "" || errOut != "Error: volume is in use\n" {
		t.Errorf("volume rm = %d %q %q", code, out, errOut)
	}
	if code, _, errOut := call("pull", "alpine"); code != ExitMisuse || !strings.Contains(errOut, "no rule") || !strings.Contains(errOut, "docker pull alpine") {
		t.Errorf("unmatched = %d %q", code, errOut)
	}
	if code, _, _ := call("version"); code != 0 {
		t.Errorf("version = %d", code)
	}

	log, err := os.ReadFile(filepath.Join(home, "docker.log"))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"docker inspect db", "docker inspect db", "docker inspect db", "docker inspect db",
		"docker volume rm x", "docker pull alpine", "docker version",
	}, "\n") + "\n"
	if string(log) != want {
		t.Errorf("log =\n%s\nwant\n%s", log, want)
	}
}

func TestConcurrentCallsKeepExactCounts(t *testing.T) {
	home := t.TempDir()
	writeFiles(t, home, map[string]string{
		"docker.scenario": "docker ps => 0 stdout=first.txt times=10\ndocker ps => 0 stdout=rest.txt\n",
		"first.txt":       "first",
		"rest.txt":        "rest",
	})

	const calls = 50
	outputs := make(chan string, calls)
	var wg sync.WaitGroup
	for range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var out, errOut bytes.Buffer
			if code := run("docker", []string{"ps"}, home, &out, &errOut); code != 0 {
				t.Errorf("exit %d: %s", code, errOut.String())
			}
			outputs <- out.String()
		}()
	}
	wg.Wait()
	close(outputs)

	got := map[string]int{}
	for out := range outputs {
		got[out]++
	}
	if got["first"] != 10 || got["rest"] != calls-10 {
		t.Errorf("responses = %v, want 10 first and %d rest", got, calls-10)
	}
	log, err := os.ReadFile(filepath.Join(home, "docker.log"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(log), "docker ps\n"); n != calls {
		t.Errorf("log has %d calls, want %d", n, calls)
	}
}

func TestRunWithoutScenarioRejectsEveryCall(t *testing.T) {
	var errOut bytes.Buffer
	if code := run("git", []string{"status"}, t.TempDir(), &bytes.Buffer{}, &errOut); code != ExitMisuse {
		t.Errorf("code = %d, want %d (%s)", code, ExitMisuse, errOut.String())
	}
}

func TestParseScenarioErrors(t *testing.T) {
	for _, bad := range []string{
		"docker version",
		"docker version =>",
		"docker version => zero",
		"docker version => 0 stdout",
		"docker version => 0 colour=red",
		"docker version => 0 times=0",
		"re:docker ( => 0",
	} {
		if _, err := ParseScenario(strings.NewReader("# ok\n\n" + bad + "\n")); err == nil || !strings.Contains(err.Error(), "line 3") {
			t.Errorf("%q: err = %v, want a line 3 error", bad, err)
		}
	}
}
