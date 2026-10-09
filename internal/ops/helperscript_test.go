package ops_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
)

// helperScripts runs the alpine helper containers' scripts for real, so a
// test checks what they do rather than their text. Locally, the script runs
// under sh with each mount target in its arguments replaced by the host
// directory standing in for the volume, so it works only for a script that
// takes every path it touches as an argument. In Docker, it runs in the
// alpine container the argv names, with each mount's source swapped for
// the test's own volume.
type helperScripts struct {
	t *testing.T
	// vols maps the stack's volume names to a host directory, or in Docker
	// to a test volume.
	vols     map[string]string
	inDocker bool
	// prefix starts the name of every container and volume the Docker mode
	// creates, and names keeps the containers' names for the cleanup.
	prefix string
	names  []string
}

func newLocalHelperScripts(t *testing.T, vols ...string) *helperScripts {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	h := &helperScripts{t: t, vols: map[string]string{}}
	for _, v := range vols {
		h.vols[v] = t.TempDir()
	}
	return h
}

// newDockerHelperScripts skips the test without a Docker daemon. Every
// container and volume it makes is removed when the test ends, however it
// ends.
func newDockerHelperScripts(t *testing.T, vols ...string) *helperScripts {
	t.Helper()
	if testing.Short() {
		t.Skip("-short")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker CLI")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		t.Skip("docker daemon unavailable")
	}
	var suffix [4]byte
	_, _ = rand.Read(suffix[:])
	h := &helperScripts{t: t, vols: map[string]string{}, inDocker: true, prefix: "picsure-test-094-" + hex.EncodeToString(suffix[:])}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if len(h.names) > 0 {
			_ = exec.CommandContext(ctx, "docker", append([]string{"rm", "-f"}, h.names...)...).Run()
		}
		for _, v := range h.vols {
			_ = exec.CommandContext(ctx, "docker", "volume", "rm", "-f", v).Run()
		}
	})
	for i, v := range vols {
		h.vols[v] = h.prefix + "-" + string(rune('a'+i))
		if out, err := exec.CommandContext(ctx, "docker", "volume", "create", h.vols[v]).CombinedOutput(); err != nil {
			t.Fatalf("creating a test volume: %v: %s", err, out)
		}
	}
	return h
}

// run answers a fakerunner call of a helper container by running its
// script. env is added to a local run's environment.
func (h *helperScripts) run(ctx context.Context, c fakerunner.Call, env ...string) (docker.Result, error) {
	var cmd *exec.Cmd
	if h.inDocker {
		var argv []string
		for i := 1; i < len(c.Argv); i++ {
			switch a := c.Argv[i]; a {
			case "--label":
				i++
			case "--name":
				i++
				h.names = append(h.names, h.prefix+"-"+c.Argv[i])
				argv = append(argv, a, h.names[len(h.names)-1])
			case "-v":
				i++
				src, rest, _ := strings.Cut(c.Argv[i], ":")
				v, ok := h.vols[src]
				if !ok {
					return docker.Result{}, fmt.Errorf("the test has no volume for mount %s", c.Argv[i])
				}
				argv = append(argv, a, v+":"+rest)
			default:
				argv = append(argv, a)
			}
		}
		cmd = exec.CommandContext(ctx, "docker", argv...)
	} else {
		dirs := map[string]string{}
		for i, a := range c.Argv {
			if a == "-v" {
				src, rest, _ := strings.Cut(c.Argv[i+1], ":")
				target, _, _ := strings.Cut(rest, ":")
				if dir, ok := h.vols[src]; ok {
					dirs[target] = dir
				}
			}
		}
		i := slices.Index(c.Argv, "-c")
		args := []string{"-c", c.Argv[i+1], "sh"}
		if len(c.Argv) > i+3 {
			for _, a := range c.Argv[i+3:] {
				for target, dir := range dirs {
					if a == target || strings.HasPrefix(a, target+"/") {
						a = dir + a[len(target):]
					}
				}
				args = append(args, a)
			}
		}
		cmd = exec.CommandContext(ctx, "sh", args...)
		cmd.Env = append(os.Environ(), env...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(c.Stdin), &stdout, &stderr
	err := cmd.Run()
	if ee := (*exec.ExitError)(nil); errors.As(err, &ee) && ctx.Err() == nil {
		code := ee.ExitCode()
		if code < 0 { // killed by a signal, as by docker rm -f
			code = 137
		}
		return docker.Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: code}, nil
	}
	return docker.Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, err
}

// seed writes files, each path relative to the volume's root, into vol.
func (h *helperScripts) seed(vol string, files map[string]string) {
	h.t.Helper()
	if !h.inDocker {
		for name, content := range files {
			p := filepath.Join(h.vols[vol], name)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				h.t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				h.t.Fatal(err)
			}
		}
		return
	}
	var script strings.Builder
	var args []string
	for name, content := range files {
		n := len(args)
		f, c := "${"+strconv.Itoa(n+1)+"}", "${"+strconv.Itoa(n+2)+"}"
		script.WriteString(`mkdir -p "$(dirname "` + f + `")"; printf %s "` + c + `" > "` + f + `"; `)
		args = append(args, "/d/"+name, content)
	}
	h.docker(vol, script.String()+"true", args...)
}

// tree returns vol's files, each relative path mapped to its content, and
// its empty directories, each as "path/".
func (h *helperScripts) tree(vol string) map[string]string {
	h.t.Helper()
	got := map[string]string{}
	if !h.inDocker {
		root := h.vols[vol]
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || p == root {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			if d.IsDir() {
				if entries, err := os.ReadDir(p); err == nil && len(entries) == 0 {
					got[rel+"/"] = ""
				}
				return nil
			}
			data, err := os.ReadFile(p)
			got[rel] = string(data)
			return err
		})
		if err != nil {
			h.t.Fatal(err)
		}
		return got
	}
	out := h.docker(vol, `cd /d; find . -mindepth 1 -type d -empty | while read -r d; do printf '%s/\n' "${d#./}"; done; `+
		`find . -type f | while read -r f; do printf '%s=%s\n' "${f#./}" "$(cat "$f")"; done`)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		if name, content, ok := strings.Cut(line, "="); ok {
			got[name] = content
		} else {
			got[line] = ""
		}
	}
	return got
}

// docker runs script in an alpine container with vol at /d.
func (h *helperScripts) docker(vol, script string, args ...string) string {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	name := h.prefix + "-seed-" + strconv.Itoa(len(h.names))
	h.names = append(h.names, name)
	alpine, _ := catalog.LookupImage("alpine")
	argv := append([]string{"run", "--rm", "--name", name, "--network", "none", "-v", h.vols[vol] + ":/d", alpine.Ref, "sh", "-c", script, "sh"}, args...)
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", argv...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		h.t.Fatalf("docker %s: %v: %s", script, err, stderr.String())
	}
	return string(out)
}

// treeString formats a tree for failure messages.
func treeString(tree map[string]string) string {
	var lines []string
	for name, content := range tree {
		lines = append(lines, name+"="+content)
	}
	sort.Strings(lines)
	return strings.Join(lines, " ")
}
