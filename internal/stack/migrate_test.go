package stack

import (
	"errors"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

var t0 = time.Date(2026, 10, 6, 15, 4, 5, 0, time.UTC)

// renameKey returns a migration step that renames a top-level key, keeping
// its comments and value.
func renameKey(from, to string) func(*yaml.Node) error {
	return func(top *yaml.Node) error {
		for i := 0; i+1 < len(top.Content); i += 2 {
			if top.Content[i].Value == from {
				top.Content[i].Value = to
			}
		}
		return nil
	}
}

// fakeRegistry goes from schema 1 to a schema 2 that only tests know,
// renaming frontend to ui.
func fakeRegistry() Registry {
	return Registry{Target: 2, Steps: []Migration{{From: 1, Summary: "rename frontend to ui", Apply: renameKey("frontend", "ui")}}}
}

const schema1YAML = `# My stack.
schema: 1 # do not edit
name: demo
# The UI.
frontend:
  theme: picsure # the default
`

const schema2YAML = `# My stack.
schema: 2 # do not edit
name: demo
# The UI.
ui:
  theme: picsure # the default
`

func parseDoc(t *testing.T, s string) *ConfigDoc {
	t.Helper()
	doc, err := ParseConfigDoc([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func docString(t *testing.T, doc *ConfigDoc) string {
	t.Helper()
	b, err := doc.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPlan(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		reg        Registry
		want       []int // From of each step
		err        any   // *SchemaVersionError or *ConfigError
	}{
		{name: "current schema, no migrations", yaml: "schema: 1", reg: ConfigMigrations()},
		{name: "one step pending", yaml: "schema: 1", reg: fakeRegistry(), want: []int{1}},
		{name: "already migrated", yaml: "schema: 2", reg: fakeRegistry()},
		{name: "newer than the target", yaml: "schema: 3", reg: fakeRegistry(), err: new(*SchemaVersionError)},
		{name: "older than every step", yaml: "schema: 0", reg: fakeRegistry(), err: new(*SchemaVersionError)},
		{name: "no schema", yaml: "name: demo", reg: fakeRegistry(), err: new(*ConfigError)},
		{name: "two steps", yaml: "schema: 1", reg: Registry{Target: 3, Steps: []Migration{
			{From: 2, Apply: renameKey("a", "b")}, {From: 1, Apply: renameKey("b", "c")},
		}}, want: []int{1, 2}},
		{name: "a gap in the chain", yaml: "schema: 1", reg: Registry{Target: 3, Steps: []Migration{
			{From: 1, Apply: renameKey("a", "b")},
		}}, err: new(*SchemaVersionError)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps, err := tc.reg.Plan(parseDoc(t, tc.yaml))
			if tc.err != nil {
				if !errors.As(err, tc.err) {
					t.Fatalf("err = %v, want a %T", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var got []int
			for _, s := range steps {
				got = append(got, s.From)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("steps from %v, want %v", got, tc.want)
			}
		})
	}
}

func TestConfigMigrationsLeadToConfigSchema(t *testing.T) {
	r := ConfigMigrations()
	if r.Target != ConfigSchema {
		t.Fatalf("Target = %d, want ConfigSchema %d", r.Target, ConfigSchema)
	}
	for i, m := range r.Steps {
		if m.Apply == nil || m.Summary == "" {
			t.Errorf("step %d (from %d) needs an Apply and a Summary", i, m.From)
		}
		next := r.Target
		if i+1 < len(r.Steps) {
			next = r.Steps[i+1].From
		}
		if m.From+1 != next {
			t.Errorf("step %d goes from %d to %d, but the next starts at %d", i, m.From, m.From+1, next)
		}
	}
}

func TestMigrateKeepsComments(t *testing.T) {
	doc := parseDoc(t, schema1YAML)
	steps, err := fakeRegistry().Migrate(doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 {
		t.Errorf("ran %d steps, want 1", len(steps))
	}
	if got := docString(t, doc); got != schema2YAML {
		t.Errorf("migrated:\n%s\nwant:\n%s", got, schema2YAML)
	}
}

// writeStack makes a stack with pic-sure.yaml and, unless state is "",
// state.json.
func writeStack(t *testing.T, config, state string) *Stack {
	t.Helper()
	s := newStack(t)
	if err := s.WriteConfig([]byte(config)); err != nil {
		t.Fatal(err)
	}
	if state != "" {
		if err := s.WriteFile(StateFile, []byte(state), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestApplyBacksUpThenMigrates(t *testing.T) {
	state := `{"cli_version": "v2.0.0", "schema_version": 1}`
	s := writeStack(t, schema1YAML, state)

	got, err := fakeRegistry().Apply(s, t0.In(time.FixedZone("EST", -5*3600)))
	if err != nil {
		t.Fatal(err)
	}
	const dir = ".pic-sure/backups/20261006T150405Z"
	if got.BackupDir != dir || len(got.Steps) != 1 || got.Steps[0].From != 1 {
		t.Errorf("Apply = %+v", got)
	}
	wantContent(t, s.Path(dir+"/pic-sure.yaml"), schema1YAML)
	wantContent(t, s.Path(dir+"/state.json"), state)
	wantContent(t, s.Path(ConfigFile), schema2YAML)
	wantContent(t, s.Path(StateFile), state) // rendering updates it, not Apply
	if m, _ := s.Manifest(); !m.Has(BackupsDir) || !m.Has(dir) || !m.Has(dir+"/pic-sure.yaml") || !m.Has(dir+"/state.json") {
		t.Errorf("backups not all recorded in the manifest: %+v", m)
	}

	// Nothing is pending now: no second backup.
	got, err = fakeRegistry().Apply(s, t0)
	if err != nil || got.BackupDir != "" || len(got.Steps) != 0 {
		t.Errorf("second Apply = %+v, %v; want nothing done", got, err)
	}

	// A second migration in the same second gets its own directory.
	if err := s.WriteConfig([]byte(schema1YAML)); err != nil {
		t.Fatal(err)
	}
	got, err = fakeRegistry().Apply(s, t0)
	if err != nil {
		t.Fatal(err)
	}
	if got.BackupDir != dir+"-2" {
		t.Errorf("BackupDir = %q, want %q", got.BackupDir, dir+"-2")
	}
	wantContent(t, s.Path(dir+"/pic-sure.yaml"), schema1YAML)
}

func TestApplyWithoutState(t *testing.T) {
	s := writeStack(t, schema1YAML, "")
	got, err := fakeRegistry().Apply(s, t0)
	if err != nil {
		t.Fatal(err)
	}
	wantContent(t, s.Path(got.BackupDir+"/pic-sure.yaml"), schema1YAML)
	if _, err := os.Stat(s.Path(got.BackupDir + "/state.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("state.json backed up although there is none: %v", err)
	}
}

// The registry this pic-sure runs has nothing to do on a current stack.
func TestApplyCurrentSchemaIsANoOp(t *testing.T) {
	config := "schema: 1\nname: demo\nauth: {admin_email: admin@example.org, auth0: {client_id: abc}}\n"
	s := writeStack(t, config, `{"schema_version": 1}`)
	got, err := ConfigMigrations().Apply(s, t0)
	if err != nil || got.BackupDir != "" || len(got.Steps) != 0 {
		t.Errorf("Apply = %+v, %v; want nothing done", got, err)
	}
	wantContent(t, s.Path(ConfigFile), config)
	if _, err := os.Stat(s.Path(BackupsDir)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("backups dir made: %v", err)
	}
}

func TestApplyChangesNothingOnFailure(t *testing.T) {
	valid := "auth: {admin_email: admin@example.org, auth0: {client_id: abc}}\n"
	for _, tc := range []struct {
		name, config string
		reg          Registry
		want         string
	}{{
		name:   "a step fails",
		config: schema1YAML,
		reg: Registry{Target: 2, Steps: []Migration{{From: 1, Apply: func(*yaml.Node) error {
			return errors.New("boom")
		}}}},
		want: "migrating pic-sure.yaml from schema 1 to 2: boom",
	}, {
		name:   "the result is invalid",
		config: "schema: 0\nname: demo\n" + valid,
		reg: Registry{Target: ConfigSchema, Steps: []Migration{{From: 0, Apply: func(top *yaml.Node) error {
			top.Content = append(top.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "bogus"}, &yaml.Node{Kind: yaml.ScalarNode, Value: "1"})
			return nil
		}}}},
		want: "pic-sure.yaml migrated to schema 1: invalid pic-sure.yaml: bogus",
	}, {
		name:   "the schema is newer",
		config: "schema: 3\n",
		reg:    fakeRegistry(),
		want:   "pic-sure.yaml is schema 3",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			s := writeStack(t, tc.config, `{"schema_version": 1}`)
			_, err := tc.reg.Apply(s, t0)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
			wantContent(t, s.Path(ConfigFile), tc.config)
			if _, err := os.Stat(s.Path(BackupsDir)); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("backups dir made: %v", err)
			}
		})
	}
}

// A migration that produces a valid config at ConfigSchema is validated and
// written, keeping the file's mode.
func TestApplyToConfigSchema(t *testing.T) {
	s := writeStack(t, "schema: 0\nname: demo\nadmin: admin@example.org\nauth: {auth0: {client_id: abc}}\n", "")
	if err := os.Chmod(s.Path(ConfigFile), 0o640); err != nil {
		t.Fatal(err)
	}
	reg := Registry{Target: ConfigSchema, Steps: []Migration{{From: 0, Summary: "move admin under auth", Apply: func(top *yaml.Node) error {
		var admin *yaml.Node
		for i := 0; i+1 < len(top.Content); i += 2 {
			if top.Content[i].Value == "admin" {
				admin = top.Content[i+1]
				top.Content = append(top.Content[:i], top.Content[i+2:]...)
				break
			}
		}
		auth := lookupNode(top, []string{"auth"})
		auth.Content = append(auth.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "admin_email"}, admin)
		return nil
	}}}}
	if _, err := reg.Apply(s, t0); err != nil {
		t.Fatal(err)
	}
	cfg, err := s.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.AdminEmail != "admin@example.org" {
		t.Errorf("admin_email = %q", cfg.Auth.AdminEmail)
	}
	wantMode(t, s.Path(ConfigFile), 0o640)
}
