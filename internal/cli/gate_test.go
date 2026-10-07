package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

// findCmd returns the command at path, such as "config get", from a tree
// of its own: building one resets the global flags of the App it is for.
func findCmd(t *testing.T, path string) *cobra.Command {
	t.Helper()
	cmd, rest, err := newRootCmd(&App{}).Find(strings.Fields(path))
	if err != nil || len(rest) > 0 {
		t.Fatalf("no command %q: %v", path, err)
	}
	return cmd
}

func TestEveryCommandHasAClass(t *testing.T) {
	a, _, _ := testApp(t)
	var leaves, classified, readOnly []string
	for path := range leafCommands(newRootCmd(a)) {
		leaves = append(leaves, path)
	}
	for path, class := range commandClasses {
		classified = append(classified, path)
		if class == stack.ReadOnly {
			readOnly = append(readOnly, path)
		}
	}
	slices.Sort(leaves)
	slices.Sort(classified)
	slices.Sort(readOnly)
	if !slices.Equal(leaves, classified) {
		t.Errorf("classified commands:\n got  %q\n want %q", classified, leaves)
	}
	// §10.6's read-only list.
	want := []string{"cache list", "config get", "config show", "dev list", "doctor", "logs", "ps", "shared-data list", "status", "support-bundle", "version"}
	if !slices.Equal(readOnly, want) {
		t.Errorf("read-only commands = %q, want %q", readOnly, want)
	}
	if c := commandClass(findCmd(t, "update")); c != stack.Migrating {
		t.Errorf("update's class = %d, want Migrating", c)
	}
}

// gateStack makes a stack with a valid pic-sure.yaml of the given schema
// and, unless state is "", state.json.
func gateStack(t *testing.T, schema int, state string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".pic-sure"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := "schema: " + strconv.Itoa(schema) + "\nname: demo\nnetwork: {http_port: 8080}\nauth: {admin_email: admin@example.org, auth0: {client_id: abc}}\n"
	if err := os.WriteFile(filepath.Join(dir, "pic-sure.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	if state != "" {
		if err := os.WriteFile(filepath.Join(dir, ".pic-sure", "state.json"), []byte(state), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// toSchema1 is a registry whose one test migration brings schema 0 to the
// schema this pic-sure reads, so a schema-0 stack has migrations pending.
func toSchema1() *stack.Registry {
	return &stack.Registry{Target: stack.ConfigSchema, Steps: []stack.Migration{{
		From: 0, Summary: "nothing to change",
		Apply: func(*yaml.Node) error { return nil },
	}}}
}

func TestOpenStackGates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		schema int
		state  string
		reg    *stack.Registry
		// What config get (read-only), config set (mutating) and update
		// (migrating) do: "runs", "warns" (runs with a warning) or "exit N".
		want [3]string
		msg  string
	}{{
		name: "current", schema: 1, state: `{"cli_version": "v2.0.0-test", "schema_version": 1}`,
		want: [3]string{"runs", "runs", "runs"},
	}, {
		name: "rendered by an older pic-sure", schema: 1, state: `{"cli_version": "v1.9.0", "schema_version": 1}`,
		want: [3]string{"runs", "runs", "runs"},
	}, {
		name: "rendered by a newer pic-sure", schema: 1, state: `{"cli_version": "v2.1.0", "schema_version": 1}`,
		want: [3]string{"warns", "exit 5", "exit 5"},
		msg:  "this stack was last rendered by pic-sure v2.1.0, which is newer than this pic-sure (v2.0.0-test)",
	}, {
		name: "newer schema", schema: 2, state: `{"cli_version": "v3.0.0", "schema_version": 2}`,
		want: [3]string{"warns", "exit 5", "exit 5"},
		msg:  "schema 2",
	}, {
		name: "migrations pending", schema: 0, state: `{"cli_version": "v1.0.0", "schema_version": 0}`, reg: toSchema1(),
		want: [3]string{"runs", "exit 5", "runs"},
		msg:  "run pic-sure update",
	}, {
		name: "corrupt state.json", schema: 1, state: `{`,
		want: [3]string{"warns", "exit 1", "exit 1"},
		msg:  "state.json",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := gateStack(t, tc.schema, tc.state)
			for i, path := range []string{"config get", "config set", "update"} {
				a, _, stderr := testApp(t)
				a.Global.Stack = dir
				a.migrations = tc.reg
				st, err := a.openStack(findCmd(t, path))
				if err == nil {
					_ = st.Close()
				}
				got, msg := "runs", stderr.String()
				switch {
				case err != nil:
					got, msg = fmt.Sprintf("exit %d", exitcode.FromError(err)), err.Error()
				case msg != "":
					got = "warns"
				}
				if got != tc.want[i] {
					t.Errorf("%s %s (err %v, stderr %q), want %s", path, got, err, stderr, tc.want[i])
				}
				if got != "runs" && !strings.Contains(msg, tc.msg) {
					t.Errorf("%s: %q doesn't contain %q", path, msg, tc.msg)
				}
			}
		})
	}
}

func TestConfigOnANewerSchemaShowsTheFile(t *testing.T) {
	dir := gateStack(t, 2, `{"cli_version": "v3.0.0", "schema_version": 2}`)
	for _, tc := range []struct {
		args []string
		out  string
	}{
		{[]string{"config", "get", "network.http_port"}, "8080\n"},
		{[]string{"config", "get", "auth"}, "admin_email: admin@example.org\nauth0:\n  client_id: abc\n"},
		{[]string{"config", "show"}, "schema: 2\nname: demo\nnetwork: {http_port: 8080}\n"},
		{[]string{"--json", "config", "show"}, `{"auth":{"admin_email":"admin@example.org","auth0":{"client_id":"abc"}},"name":"demo","network":{"http_port":8080},"schema":2}` + "\n"},
	} {
		a, outBuf, errBuf := testApp(t)
		code := a.Run(context.Background(), append([]string{"--stack", dir}, tc.args...))
		if code != 0 || !strings.HasPrefix(outBuf.String(), tc.out) {
			t.Errorf("%q: exit %d, stdout %q; want 0 and %q", tc.args, code, outBuf, tc.out)
		}
		if !strings.Contains(errBuf.String(), "pic-sure: warning: this stack uses pic-sure.yaml schema 2") ||
			!strings.Contains(errBuf.String(), "pic-sure: warning: this pic-sure can't decode pic-sure.yaml schema 2, so this is the file as written, without defaults") {
			t.Errorf("%q: stderr %q, want both warnings", tc.args, errBuf)
		}
	}

	a, _, errBuf := testApp(t)
	if code := a.Run(context.Background(), []string{"--stack", dir, "config", "get", "network.hostname"}); code != exitcode.CodeUsage ||
		!strings.Contains(errBuf.String(), "network.hostname: not set in pic-sure.yaml") {
		t.Errorf("get of a key the file doesn't set: exit %d, stderr %q", code, errBuf)
	}
}

func TestConfigOnAnOlderSchemaMigratesInMemory(t *testing.T) {
	dir := gateStack(t, 0, `{"cli_version": "v1.0.0", "schema_version": 0}`)
	before := readFile(t, filepath.Join(dir, "pic-sure.yaml"))

	a, outBuf, errBuf := testApp(t)
	a.migrations = toSchema1()
	if code := a.Run(context.Background(), []string{"--stack", dir, "config", "get", "schema"}); code != 0 || outBuf.String() != "1\n" || errBuf.Len() > 0 {
		t.Errorf("config get schema: exit %d, stdout %q, stderr %q; want the migrated 1", code, outBuf, errBuf)
	}

	a, _, errBuf = testApp(t)
	a.migrations = toSchema1()
	if code := a.Run(context.Background(), []string{"--stack", dir, "config", "set", "network.http_port", "80"}); code != exitcode.CodeIncompatible ||
		!strings.Contains(errBuf.String(), "run pic-sure update") {
		t.Errorf("config set: exit %d, stderr %q; want exit 5 directing to update", code, errBuf)
	}
	if after := readFile(t, filepath.Join(dir, "pic-sure.yaml")); after != before {
		t.Errorf("pic-sure.yaml changed:\n%s", after)
	}
}

// A schema older than every migration can't be read either, but read-only
// commands still run.
func TestConfigOnAnUnmigratableSchemaShowsTheFile(t *testing.T) {
	dir := gateStack(t, 0, "")
	a, outBuf, errBuf := testApp(t)
	if code := a.Run(context.Background(), []string{"--stack", dir, "config", "get", "name"}); code != 0 || outBuf.String() != "demo\n" ||
		!strings.Contains(errBuf.String(), "can't decode pic-sure.yaml schema 0") {
		t.Errorf("config get: exit %d, stdout %q, stderr %q", code, outBuf, errBuf)
	}
	a, _, errBuf = testApp(t)
	if code := a.Run(context.Background(), []string{"--stack", dir, "config", "set", "name", "x"}); code != exitcode.CodeIncompatible ||
		!strings.Contains(errBuf.String(), "pic-sure.yaml is schema 0, but this pic-sure reads schema 1") {
		t.Errorf("config set: exit %d, stderr %q; want exit 5", code, errBuf)
	}
}
