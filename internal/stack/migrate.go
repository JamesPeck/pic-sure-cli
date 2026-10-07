package stack

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"strconv"
	"time"

	"go.yaml.in/yaml/v3"
)

// BackupsDir holds the config backups Apply takes, one directory per run.
const BackupsDir = CLIDir + "/backups"

// Migration rewrites pic-sure.yaml from schema From to From+1 (§6.2).
type Migration struct {
	From int `json:"from"`
	// Summary says what the migration changes, for update's plan.
	Summary string `json:"summary"`
	// Apply rewrites the document's top-level mapping in place. Keep the
	// comments of the nodes it moves. It doesn't touch schema: Migrate
	// sets that after each step.
	Apply func(top *yaml.Node) error `json:"-"`
}

// Registry is an ordered list of migrations that ends at Target: each step
// goes from schema From to From+1, the next starts where it ends, and the
// last ends at Target.
type Registry struct {
	// Target is the schema the migrations lead to.
	Target int
	Steps  []Migration
}

// ConfigMigrations is the registry this pic-sure runs: it leads to
// ConfigSchema. Add a migration here when ConfigSchema goes up.
func ConfigMigrations() Registry {
	return Registry{Target: ConfigSchema}
}

// Schema returns the document's schema, or a *ConfigError when it is
// missing or isn't a whole number.
func (d *ConfigDoc) Schema() (int, error) {
	return schemaOf(d.root.Content[0])
}

// Plan returns the migrations that take doc to r.Target, in order: none
// when doc is already there. A schema above Target, or below every step,
// is a *SchemaVersionError.
func (r Registry) Plan(doc *ConfigDoc) ([]Migration, error) {
	from, err := doc.Schema()
	if err != nil {
		return nil, err
	}
	return r.plan(from)
}

func (r Registry) plan(from int) ([]Migration, error) {
	if from > r.Target {
		return nil, &SchemaVersionError{Found: from}
	}
	var steps []Migration
	for v := from; v < r.Target; v++ {
		i := -1
		for j, m := range r.Steps {
			if m.From == v {
				i = j
				break
			}
		}
		if i < 0 {
			return nil, &SchemaVersionError{Found: from}
		}
		steps = append(steps, r.Steps[i])
	}
	return steps, nil
}

// Migrate runs the pending migrations on doc in memory and returns them.
// After an error doc is partly migrated; discard it.
func (r Registry) Migrate(doc *ConfigDoc) ([]Migration, error) {
	steps, err := r.Plan(doc)
	if err != nil {
		return nil, err
	}
	for _, m := range steps {
		if err := applyStep(doc.root.Content[0], m); err != nil {
			return nil, fmt.Errorf("migrating %s from schema %d to %d: %w", ConfigFile, m.From, m.From+1, err)
		}
	}
	return steps, nil
}

// applyStep runs m on the top-level mapping, then sets schema to m.From+1,
// keeping its comments.
func applyStep(top *yaml.Node, m Migration) error {
	if err := m.Apply(top); err != nil {
		return err
	}
	n := lookupNode(top, []string{"schema"})
	if n == nil {
		return errors.New("the step removed schema")
	}
	n.Kind, n.Tag, n.Style, n.Value, n.Content = yaml.ScalarNode, "!!int", 0, strconv.Itoa(m.From+1), nil
	return nil
}

// Applied is what Apply did.
type Applied struct {
	// Steps are the migrations it ran, in order.
	Steps []Migration
	// BackupDir is the slash-separated stack-relative directory holding
	// the files as they were before, such as .pic-sure/backups/20261006T150405Z.
	// It is empty when nothing was pending.
	BackupDir string
}

// Apply migrates the stack's pic-sure.yaml to r.Target. It runs the steps
// on a copy in memory and, when Target is ConfigSchema, validates the
// result; only then does it back up pic-sure.yaml and state.json (when
// there is one) to a new directory under BackupsDir, named for now in UTC,
// and write the migrated config. It changes nothing when no migration is
// pending. It doesn't touch state.json: its schema_version changes when
// the stack is next rendered. The caller holds the stack lock.
func (r Registry) Apply(s *Stack, now time.Time) (*Applied, error) {
	orig, err := s.ReadFile(ConfigFile)
	if err != nil {
		return nil, err
	}
	doc, err := ParseConfigDoc(orig)
	if err != nil {
		return nil, err
	}
	steps, err := r.Migrate(doc)
	if err != nil || len(steps) == 0 {
		return &Applied{}, err
	}
	if r.Target == ConfigSchema {
		if _, err := doc.Config(); err != nil {
			return nil, fmt.Errorf("%s migrated to schema %d: %w", ConfigFile, r.Target, err)
		}
	}
	migrated, err := doc.Bytes()
	if err != nil {
		return nil, err
	}

	dir, err := s.backup(orig, now)
	if err != nil {
		return nil, fmt.Errorf("backing up before migrating %s: %w", ConfigFile, err)
	}
	if err := s.WriteConfig(migrated); err != nil {
		return nil, err
	}
	return &Applied{Steps: steps, BackupDir: dir}, nil
}

// backup copies config (pic-sure.yaml as read) and state.json into a new
// directory under BackupsDir and returns that directory.
func (s *Stack) backup(config []byte, now time.Time) (string, error) {
	state, err := s.ReadFile(StateFile)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	// The stack lock makes check-then-create safe here.
	name := now.UTC().Format("20060102T150405Z")
	dir := path.Join(BackupsDir, name)
	for i := 2; ; i++ {
		_, err := s.root.Lstat(filepath.FromSlash(dir))
		if errors.Is(err, fs.ErrNotExist) {
			break
		}
		if err != nil {
			return "", err
		}
		dir = path.Join(BackupsDir, fmt.Sprintf("%s-%d", name, i))
	}
	if err := s.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := s.backupFile(dir, ConfigFile, config); err != nil {
		return "", err
	}
	if state != nil {
		if err := s.backupFile(dir, StateFile, state); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// backupFile writes data, the content of rel, into dir with rel's mode, so
// a copy of a file the operator restricted stays restricted.
func (s *Stack) backupFile(dir, rel string, data []byte) error {
	fi, err := s.root.Stat(filepath.FromSlash(rel))
	if err != nil {
		return err
	}
	return s.WriteFile(path.Join(dir, path.Base(rel)), data, fi.Mode().Perm())
}
