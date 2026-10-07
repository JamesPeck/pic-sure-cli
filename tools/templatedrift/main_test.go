package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRealReadme(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "internal", "render", "templates", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := ParseReadme(data)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Sources["docker-compose.yml"]; len(got) < 2 {
		t.Errorf("docker-compose.yml maps to %v, want several templates", got)
	}
	if _, ok := r.Sources["none"]; ok {
		t.Error(`"none" parsed as a source`)
	}
}

func TestParseReadmeErrors(t *testing.T) {
	for name, readme := range map[string]string{
		"no commit":  "| `a.tmpl` | `a.yml` |\n",
		"bad source": "AIO commit: `abcdef1`\n\n| `a.tmpl` | a.yml |\n",
		"no table":   "AIO commit: `abcdef1`\n",
		"unclosed":   "AIO commit: `abcdef1`\n\n| `a.tmpl | `a.yml` |\n",
	} {
		if _, err := ParseReadme([]byte(readme)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

const testReadme = "AIO commit: `%s`\n\n" +
	"| Template | AIO source |\n|---|---|\n" +
	"| `compose/base.yaml.tmpl` | `docker-compose.yml` |\n" +
	"| `compose/local-db.yaml.tmpl` | `docker-compose.yml`, `docker-compose.remote-db.yml` |\n" +
	"| `files/run.sh` | `config/run.sh` |\n" +
	"| `compose/service-env.yaml.tmpl` | none |\n"

// aioRepo makes a git repository with two commits and returns it with the
// first commit's ID.
func aioRepo(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	// Keep the user's git config (commit signing, say) and any enclosing
	// repository out of the fixture and the tool.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, content string) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "-b", "aio-compose")
	write("docker-compose.yml", "services:\n  httpd: {}\n")
	write("docker-compose.remote-db.yml", "services: {}\n")
	write("config/run.sh", "echo hi\n")
	write("README.md", "AIO\n")
	git("add", ".")
	git("commit", "-q", "-m", "one")
	first := git("rev-parse", "HEAD")
	write("docker-compose.yml", "services:\n  httpd:\n    image: \"```\"\n")
	write("docker-compose.dev-new.yml", "services: {}\n")
	write("README.md", "AIO v2\n")
	git("rm", "-q", "config/run.sh")
	git("add", ".")
	git("commit", "-q", "-m", "two")
	return dir, first
}

func runTool(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

func writeReadme(t *testing.T, commit string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "README.md")
	if err := os.WriteFile(p, []byte(strings.Replace(testReadme, "%s", commit, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDrift(t *testing.T) {
	dir, first := aioRepo(t)
	code, out, stderr := runTool(t, "-aio", dir, "-readme", writeReadme(t, first[:7]), "-to", "aio-compose", "-name", "o/aio@aio-compose")
	if code != 1 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	for _, want := range []string{
		"Compared o/aio@aio-compose at `",
		"with `" + first[:12] + "`, the commit",
		"2 of the 3 AIO files",
		"| `docker-compose.yml` | modified | `compose/base.yaml.tmpl`, `compose/local-db.yaml.tmpl` |",
		"| `config/run.sh` | deleted | `files/run.sh` |",
		"| `docker-compose.dev-new.yml` | added |",
		"````diff\n", // the diff holds a run of three backticks
		"+    image: \"```\"",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "README.md") {
		t.Errorf("report lists an unwatched file:\n%s", out)
	}
	if strings.Contains(out, "`docker-compose.remote-db.yml`") {
		t.Errorf("report lists an unchanged source:\n%s", out)
	}

	// The limit leaves diffs out but keeps the tables.
	code, short, _ := runTool(t, "-aio", dir, "-readme", writeReadme(t, first), "-max-bytes", "900")
	if code != 1 || len(short) > 900 || !strings.Contains(short, "diffs left out") || !strings.Contains(short, "| `config/run.sh` |") {
		t.Errorf("exit %d, %d bytes:\n%s", code, len(short), short)
	}
}

func TestNoDrift(t *testing.T) {
	dir, _ := aioRepo(t)
	code, out, stderr := runTool(t, "-aio", dir, "-readme", writeReadme(t, "0000000"), "-from", "aio-compose")
	if code != 0 || !strings.Contains(out, "None of its 3 template sources") {
		t.Errorf("exit %d, stderr %q:\n%s", code, stderr, out)
	}
}

func TestMissingRevision(t *testing.T) {
	dir, first := aioRepo(t)
	for _, args := range [][]string{
		{"-readme", writeReadme(t, "abcdef1")},            // recorded commit not in the repo
		{"-readme", writeReadme(t, first), "-to", "gone"}, // branch not fetched
	} {
		code, out, stderr := runTool(t, append([]string{"-aio", dir}, args...)...)
		if code != 2 || out != "" || !strings.Contains(stderr, "not found") {
			t.Errorf("%v: exit %d, stdout %q, stderr %q", args, code, out, stderr)
		}
	}
	if code, _, _ := runTool(t); code != 2 {
		t.Errorf("no -aio: exit %d", code)
	}
}
