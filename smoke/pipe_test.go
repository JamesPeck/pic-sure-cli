package smoke

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The built binary writing into a pipe whose reader has gone, as with
// `pic-sure up --json | head -n1`, cancels the run and exits 141 instead of
// dying of SIGPIPE mid-step.
func TestClosedPipeExits141(t *testing.T) {
	for _, tt := range []struct {
		name   string
		args   []string
		stderr bool // the closed pipe is stderr, not stdout
	}{
		{"stdout under --json", []string{"--json"}, false},
		{"stderr in plain mode", []string{"--plain"}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			var other bytes.Buffer
			cmd := exec.Command(bin, append(tt.args, "smoke-steps", "--wait")...)
			if tt.stderr {
				cmd.Stdout, cmd.Stderr = &other, w
			} else {
				cmd.Stdout, cmd.Stderr = w, &other
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			_ = w.Close()
			// Read one line, like head -n1, then go away while the
			// command still has output to write.
			if _, err := bufio.NewReader(r).ReadString('\n'); err != nil {
				t.Fatal(err)
			}
			_ = r.Close()

			err = cmd.Wait()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 141 {
				t.Fatalf("exit: %v (want status 141, not a signal); other stream:\n%s", err, other.String())
			}
			if tt.stderr {
				if other.Len() != 0 {
					t.Errorf("stdout = %q", other.String())
				}
			} else if got := other.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, "interrupted (broken pipe)") {
				t.Errorf("stderr = %q", got)
			}
		})
	}
}
