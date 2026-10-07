package stack

import (
	"reflect"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
)

func TestCompareVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b  string
		order int
		ok    bool
	}{
		{"v2.0.0", "v2.0.0", 0, true},
		{"2.0.0", "v2.0.0", 0, true},
		{"v2.0.0", "v2.0.1", -1, true},
		{"v2.1.0", "v2.0.9", 1, true},
		{"v10.0.0", "v9.9.9", 1, true},
		{"v2.0.0-rc.1", "v2.0.0", -1, true},
		{"v2.0.0-rc.2", "v2.0.0-rc.10", -1, true},
		{"v2.0.0-alpha", "v2.0.0-alpha.1", -1, true},
		{"v2.0.0-1", "v2.0.0-alpha", -1, true},
		{"v2.0.0-beta", "v2.0.0-alpha", 1, true},
		{"v2.0.0+build.5", "v2.0.0", 0, true},
		// git describe: a build after a tag compares as that tag.
		{"v2.0.0-5-gabc1234", "v2.0.0", 0, true},
		{"v2.0.0-5-gabc1234-dirty", "v2.0.1", -1, true},
		{"v2.0.0-dirty", "v2.0.0", 0, true},
		{"v2.0.0-rc.1-3-gdeadbeef", "v2.0.0-rc.1", 0, true},
		{"dev", "v2.0.0", 0, false},
		{"v2.0.0", "", 0, false},
		{"abc1234", "v2.0.0", 0, false},
		{"v2.0", "v2.0.0", 0, false},
		{"v2.01.0", "v2.0.0", 0, false},
		{"v2.0.0-", "v2.0.0", 0, false},
		{"v2.0.0-a..b", "v2.0.0", 0, false},
	} {
		order, ok := CompareVersions(tc.a, tc.b)
		if order != tc.order || ok != tc.ok {
			t.Errorf("CompareVersions(%q, %q) = %d, %v; want %d, %v", tc.a, tc.b, order, ok, tc.order, tc.ok)
		}
		if tc.ok {
			if back, _ := CompareVersions(tc.b, tc.a); back != -tc.order {
				t.Errorf("CompareVersions(%q, %q) = %d, want %d", tc.b, tc.a, back, -tc.order)
			}
		}
	}
}

// outcome is what the gate does with one command class.
type outcome string

const (
	runs    outcome = "runs"
	warns   outcome = "runs with a warning"
	refused outcome = "exit 5"
)

func TestGate(t *testing.T) {
	type side struct {
		cli    string
		schema int // this pic-sure's schema: 1, or 2 with fakeRegistry
	}
	type stackSide struct {
		cli          string // state.json
		schema       int    // state.json
		configSchema int    // pic-sure.yaml
	}
	for _, tc := range []struct {
		name  string
		stack stackSide
		cli   side
		// Outcomes for ReadOnly, Mutating and Migrating.
		want [3]outcome
		// Part of the warning or error.
		msg string
	}{{
		name:  "fresh stack, not rendered yet",
		stack: stackSide{configSchema: 1},
		cli:   side{"v2.0.0", 1},
		want:  [3]outcome{runs, runs, runs},
	}, {
		name:  "same version",
		stack: stackSide{"v2.0.0", 1, 1},
		cli:   side{"v2.0.0", 1},
		want:  [3]outcome{runs, runs, runs},
	}, {
		name:  "older CLI rendered it, same schema",
		stack: stackSide{"v2.0.0", 1, 1},
		cli:   side{"v2.1.0", 1},
		want:  [3]outcome{runs, runs, runs},
	}, {
		name:  "newer CLI rendered it, same schema",
		stack: stackSide{"v2.1.0", 1, 1},
		cli:   side{"v2.0.0", 1},
		want:  [3]outcome{warns, refused, refused},
		msg:   "this stack was last rendered by pic-sure v2.1.0, which is newer than this pic-sure (v2.0.0)",
	}, {
		name:  "newer prerelease rendered it",
		stack: stackSide{"v2.1.0-rc.1", 1, 1},
		cli:   side{"v2.0.0", 1},
		want:  [3]outcome{warns, refused, refused},
	}, {
		name:  "rendered with a newer schema",
		stack: stackSide{"v3.0.0", 2, 2},
		cli:   side{"v2.0.0", 1},
		want:  [3]outcome{warns, refused, refused},
		msg:   "this stack uses pic-sure.yaml schema 2, last rendered by pic-sure v3.0.0, but this pic-sure (v2.0.0) reads schema 1",
	}, {
		name:  "state.json alone is newer",
		stack: stackSide{"dev", 2, 1},
		cli:   side{"v2.0.0", 1},
		want:  [3]outcome{warns, refused, refused},
	}, {
		name:  "pic-sure.yaml alone is newer, no state",
		stack: stackSide{configSchema: 2},
		cli:   side{"v2.0.0", 1},
		want:  [3]outcome{warns, refused, refused},
		msg:   "this stack uses pic-sure.yaml schema 2, but this pic-sure (v2.0.0) reads schema 1",
	}, {
		name:  "dev build on a release's stack",
		stack: stackSide{"v2.1.0", 1, 1},
		cli:   side{"dev", 1},
		want:  [3]outcome{runs, runs, runs},
	}, {
		name:  "release on a dev build's stack",
		stack: stackSide{"dev", 1, 1},
		cli:   side{"v2.0.0", 1},
		want:  [3]outcome{runs, runs, runs},
	}, {
		name:  "older stack, migrations pending",
		stack: stackSide{"v2.0.0", 1, 1},
		cli:   side{"v2.1.0", 2},
		want:  [3]outcome{runs, refused, runs},
		msg:   "this stack's pic-sure.yaml is schema 1, and this pic-sure (v2.1.0) uses schema 2; run pic-sure update",
	}, {
		name:  "older stack, migrated but not yet rendered",
		stack: stackSide{"v2.0.0", 1, 2},
		cli:   side{"v2.1.0", 2},
		want:  [3]outcome{runs, runs, runs},
	}, {
		name:  "dev build, migrations pending",
		stack: stackSide{"dev", 1, 1},
		cli:   side{"dev", 2},
		want:  [3]outcome{runs, refused, runs},
	}, {
		name:  "pic-sure.yaml older than any migration",
		stack: stackSide{configSchema: 0},
		cli:   side{"v2.1.0", 2},
		want:  [3]outcome{runs, refused, refused},
		msg:   "pic-sure.yaml is schema 0",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			reg := ConfigMigrations()
			if tc.cli.schema == 2 {
				reg = fakeRegistry()
			}
			v := newVersionCheck(tc.stack.cli, tc.stack.schema, tc.stack.configSchema, true, tc.cli.cli, reg)
			for i, class := range []CommandClass{ReadOnly, Mutating, Migrating} {
				warning, err := v.Gate(class)
				got := runs
				switch {
				case err != nil && exitcode.FromError(err) == exitcode.CodeIncompatible:
					got = refused
				case err != nil:
					t.Fatalf("class %d: err = %v, want exit 5 or nil", class, err)
				case warning != "":
					got = warns
				}
				if got != tc.want[i] {
					t.Errorf("class %d %s (%q, %v), want it to be %s", class, got, warning, err, tc.want[i])
				}
				msg := warning
				if err != nil {
					msg = err.Error()
				}
				if tc.msg != "" && got != runs && !strings.Contains(msg, tc.msg) {
					t.Errorf("class %d: %q doesn't contain %q", class, msg, tc.msg)
				}
			}
		})
	}
}

func TestGateUnreadableConfig(t *testing.T) {
	v := newVersionCheck("", 0, 0, false, "v2.1.0", fakeRegistry())
	for _, class := range []CommandClass{ReadOnly, Mutating, Migrating} {
		if w, err := v.Gate(class); w != "" || err != nil {
			t.Errorf("class %d: %q, %v; want it to run, so the command can report the config problem", class, w, err)
		}
	}
}

func TestCheckVersions(t *testing.T) {
	t.Run("rendered", func(t *testing.T) {
		s := writeStack(t, schema1YAML, `{"cli_version": "v2.0.0", "schema_version": 1, "future": true}`)
		v, err := s.CheckVersions("v2.1.0", fakeRegistry())
		if err != nil {
			t.Fatal(err)
		}
		want := &VersionCheck{
			StackCLI: "v2.0.0", StackSchema: 1, ConfigSchema: 1, CLI: "v2.1.0", Schema: 2,
			Pending: fakeRegistry().Steps, configOK: true,
		}
		if v.Pending[0].Summary != want.Pending[0].Summary {
			t.Errorf("Pending = %+v", v.Pending)
		}
		v.Pending, want.Pending = nil, nil // funcs don't compare
		if !reflect.DeepEqual(v, want) {
			t.Errorf("CheckVersions = %+v, want %+v", v, want)
		}
	})
	t.Run("no state yet", func(t *testing.T) {
		s := writeStack(t, "schema: 2\n", "")
		v, err := s.CheckVersions("v2.0.0", ConfigMigrations())
		if err != nil {
			t.Fatal(err)
		}
		if v.StackCLI != "" || v.StackSchema != 0 || v.ConfigSchema != 2 || !v.Newer() {
			t.Errorf("CheckVersions = %+v, want a newer config schema and no state", v)
		}
	})
	t.Run("config without a usable schema", func(t *testing.T) {
		for _, config := range []string{"name: [unclosed\n", "schema: one\n", "name: demo\n"} {
			s := writeStack(t, config, `{"cli_version": "v2.0.0", "schema_version": 1}`)
			v, err := s.CheckVersions("v2.0.0", ConfigMigrations())
			if err != nil || v.configOK || v.ConfigSchema != 0 {
				t.Errorf("%q: CheckVersions = %+v, %v; want no config schema", config, v, err)
			}
		}
	})
	t.Run("a newer pic-sure changed the rest of state.json", func(t *testing.T) {
		s := writeStack(t, schema1YAML, `{"cli_version": "v9.0.0", "schema_version": 1, "images": {"hpds": {"tag": "abc"}}, "release": "main"}`)
		v, err := s.CheckVersions("v2.0.0", ConfigMigrations())
		if err != nil || v.StackCLI != "v9.0.0" || !v.Newer() {
			t.Errorf("CheckVersions = %+v, %v; want it newer", v, err)
		}
	})
	t.Run("corrupt state", func(t *testing.T) {
		s := writeStack(t, schema1YAML, `{"cli_version": `)
		if _, err := s.CheckVersions("v2.0.0", ConfigMigrations()); err == nil || !strings.Contains(err.Error(), "state.json") {
			t.Errorf("err = %v, want one naming state.json", err)
		}
	})
}

func TestRaw(t *testing.T) {
	doc := parseDoc(t, schema2YAML+"list: [a, b]\nn: 3\n")
	for key, want := range map[string]any{
		"name":     "demo",
		"ui.theme": "picsure",
		"ui":       map[string]any{"theme": "picsure"},
		"list":     []any{"a", "b"},
		"n":        3,
	} {
		got, err := doc.Raw(key)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("Raw(%q) = %#v, %v; want %#v", key, got, err, want)
		}
	}
	all, err := doc.Raw("")
	if m, ok := all.(map[string]any); err != nil || !ok || m["schema"] != 2 {
		t.Errorf(`Raw("") = %#v, %v`, all, err)
	}
	if _, err := doc.Raw("frontend.theme"); err == nil || err.Error() != "frontend.theme: not set in pic-sure.yaml" {
		t.Errorf("Raw of a missing key: err = %v", err)
	}
}
