package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

const editStartYAML = `# Demo stack.
schema: 1
name: demo
network:
  http_port: 8080 # off 80
auth: {admin_email: admin@example.org, auth0: {client_id: abc}}
`

// editStack makes a stack with editStartYAML, and an editor that runs
// scripts[N-1] on its Nth run and saves what it was shown in shown-N.yaml.
// EDITOR passes a flag before the file, so the file is $2.
func editStack(t *testing.T, scripts ...string) (dir string) {
	t.Helper()
	dir = t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".pic-sure"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pic-sure.yaml"), []byte(editStartYAML), 0o640); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	editor := "#!/bin/sh\nn=$(($(cat " + bin + "/count 2>/dev/null || echo 0) + 1))\necho $n > " + bin + "/count\ncp \"$2\" " + bin + "/shown-$n.yaml\ncase $n in\n"
	for i, s := range scripts {
		editor += "  " + string(rune('1'+i)) + ") " + s + " ;;\n"
	}
	editor += "  *) echo too many edits >&2; exit 1 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "editor"), []byte(editor), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", filepath.Join(bin, "editor")+" --flag-is-passed-through")
	t.Setenv("EDIT_BIN", bin)
	return dir
}

func runEdit(t *testing.T, dir string) (code int, stderr string) {
	t.Helper()
	a, _, errBuf := testApp(t)
	a.IsTerminal = func() bool { return true }
	code = a.Run(context.Background(), []string{"--stack", dir, "config", "edit"})
	return code, errBuf.String()
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// replace is a script that edits the file ($2, after the pass-through flag)
// with sed, portably.
func replace(from, to string) string {
	return `sed 's/` + from + `/` + to + `/' "$2" > "$2.new" && mv "$2.new" "$2"`
}

func TestConfigEditSavesTheEditVerbatim(t *testing.T) {
	dir := editStack(t, replace("8080", "9090"))
	if code, stderr := runEdit(t, dir); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	want := strings.Replace(editStartYAML, "8080", "9090", 1)
	if got := readFile(t, filepath.Join(dir, "pic-sure.yaml")); got != want {
		t.Errorf("saved:\n%s\nwant:\n%s", got, want)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "pic-sure.yaml")); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640 kept", fi.Mode().Perm())
	}
}

func TestConfigEditWithoutChanges(t *testing.T) {
	dir := editStack(t, "true")
	code, stderr := runEdit(t, dir)
	if code != 0 || stderr != "pic-sure.yaml is unchanged\n" {
		t.Errorf("exit %d, stderr %q", code, stderr)
	}
}

func TestConfigEditReopensUntilValid(t *testing.T) {
	dir := editStack(t, replace("8080", "0"), replace("http_port: 0", "http_port: 8081"))
	if code, stderr := runEdit(t, dir); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	shown := readFile(t, filepath.Join(os.Getenv("EDIT_BIN"), "shown-2.yaml"))
	header := "# pic-sure: this config is invalid, so it wasn't saved:\n" +
		"#   network.http_port (line 9): must be a port from 1 to 65535, got 0\n" +
		"# Fix it and save, or exit without saving to give up.\n#\n"
	if !strings.HasPrefix(shown, header) {
		t.Fatalf("second edit was shown:\n%s\nwant the header:\n%s", shown, header)
	}
	if line := strings.Split(shown, "\n")[8]; line != "  http_port: 0 # off 80" {
		t.Errorf("line 9 of the second edit is %q, not the bad port", line)
	}
	want := strings.Replace(editStartYAML, "8080", "8081", 1)
	if got := readFile(t, filepath.Join(dir, "pic-sure.yaml")); got != want {
		t.Errorf("saved:\n%s\nwant:\n%s", got, want)
	}
}

func TestConfigEditHeaderShiftsSyntaxErrorLines(t *testing.T) {
	dir := editStack(t, replace("name: demo", "name: [demo"), replace("name: \\[demo", "name: demo"))
	if code, stderr := runEdit(t, dir); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	_, err := stack.ParseConfigDoc([]byte(strings.Replace(editStartYAML, "name: demo", "name: [demo", 1)))
	var ce *stack.ConfigError
	if !errors.As(err, &ce) || ce.Problems[0].Line == 0 {
		t.Fatalf("parse error %v has no line", err)
	}
	// One problem line plus three more header lines.
	want := fmt.Sprintf("#   line %d: %s", ce.Problems[0].Line+4, ce.Problems[0].Msg)
	shown := strings.Split(readFile(t, filepath.Join(os.Getenv("EDIT_BIN"), "shown-2.yaml")), "\n")
	if shown[1] != want {
		t.Errorf("header line = %q, want %q", shown[1], want)
	}
}

func TestConfigEditKeepsNameWhenTheFileWasInvalid(t *testing.T) {
	dir := editStack(t, replace("name: demo", "name: other"), "true")
	invalid := strings.Replace(editStartYAML, "8080", "0", 1)
	if err := os.WriteFile(filepath.Join(dir, "pic-sure.yaml"), []byte(invalid), 0o640); err != nil {
		t.Fatal(err)
	}
	code, stderr := runEdit(t, dir)
	if code != exitcode.CodeUsage || !strings.Contains(stderr, "name: is read-only; it was demo") {
		t.Errorf("exit %d, stderr %q", code, stderr)
	}
}

func TestConfigEditStripsAChangedHeader(t *testing.T) {
	// The second edit fixes the port and deletes a line of the header.
	dir := editStack(t, replace("8080", "0"), `sed -e '/must be a port/d' -e 's/http_port: 0/http_port: 8081/' "$2" > "$2.new" && mv "$2.new" "$2"`)
	if code, stderr := runEdit(t, dir); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	want := strings.Replace(editStartYAML, "8080", "8081", 1)
	if got := readFile(t, filepath.Join(dir, "pic-sure.yaml")); got != want {
		t.Errorf("saved:\n%s\nwant:\n%s", got, want)
	}
}

func TestConfigEditGivesUpOnAnUnchangedRetry(t *testing.T) {
	for name, edit := range map[string]string{
		"invalid value": replace("8080", "0"),
		"rename":        replace("name: demo", "name: other"),
		"other schema":  replace("schema: 1", "schema: 2"),
		"syntax":        replace("name: demo", "name: [demo"),
	} {
		t.Run(name, func(t *testing.T) {
			dir := editStack(t, edit, "true")
			code, stderr := runEdit(t, dir)
			if code != exitcode.CodeUsage || !strings.Contains(stderr, "pic-sure.yaml is unchanged") {
				t.Errorf("exit %d, stderr %q", code, stderr)
			}
			if got := readFile(t, filepath.Join(dir, "pic-sure.yaml")); got != editStartYAML {
				t.Errorf("the file changed:\n%s", got)
			}
		})
	}
}

func TestConfigEditFailingEditor(t *testing.T) {
	dir := editStack(t, "exit 3")
	code, stderr := runEdit(t, dir)
	if code != exitcode.CodeFailed || !strings.Contains(stderr, "pic-sure.yaml is unchanged") {
		t.Errorf("exit %d, stderr %q", code, stderr)
	}
}

func TestConfigEditNeedsATerminal(t *testing.T) {
	dir := editStack(t)
	for _, flags := range [][]string{{"--json"}, {"--non-interactive"}} {
		a, _, stderr := testApp(t)
		a.IsTerminal = func() bool { return true }
		args := append(append([]string{"--stack", dir}, flags...), "config", "edit")
		if code := a.Run(context.Background(), args); code != exitcode.CodeUsage {
			t.Errorf("%v: exit %d, stderr %q", flags, code, stderr)
		}
	}
}
