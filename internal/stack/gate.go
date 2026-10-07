package stack

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strconv"
	"strings"

	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
)

// CommandClass is how the version gate treats a command (§10.6).
type CommandClass int

const (
	// ReadOnly commands run on any stack, with a warning on one that a
	// newer pic-sure rendered.
	ReadOnly CommandClass = iota + 1
	// Mutating commands are refused on a stack that a newer pic-sure
	// rendered. While config migrations are pending, they direct the user
	// to pic-sure update instead of running.
	Mutating
	// Migrating is update's class: refused on a newer stack like Mutating,
	// but it runs the pending config migrations itself.
	Migrating
)

// VersionCheck compares the versions a stack records with this pic-sure's
// (D13).
type VersionCheck struct {
	// StackCLI and StackSchema are state.json's cli_version and
	// schema_version: the pic-sure that last rendered the stack, and the
	// schema it rendered. They are empty and 0 before the first render.
	StackCLI    string
	StackSchema int
	// ConfigSchema is pic-sure.yaml's schema. It is 0 when the file has
	// no schema that can be read; the command reports why when it loads
	// the config.
	ConfigSchema int
	// CLI and Schema are this pic-sure's version and the schema it reads.
	CLI    string
	Schema int
	// Pending lists the config migrations update would run.
	Pending []Migration

	configOK bool  // whether ConfigSchema was read from the file
	planErr  error // why an older ConfigSchema can't be migrated
}

// CheckVersions reads what the version gate compares: state.json's
// versions and pic-sure.yaml's schema. cli is this pic-sure's version and
// reg the migrations it runs (ConfigMigrations()). Before the first render
// there is no state.json, and nothing to compare but the config's schema;
// a state.json that can't be read is an error. Only its two version fields
// are decoded, so a newer pic-sure may change the rest of State.
func (s *Stack) CheckVersions(cli string, reg Registry) (*VersionCheck, error) {
	var st struct {
		CLIVersion    string `json:"cli_version"`
		SchemaVersion int    `json:"schema_version"`
	}
	data, err := s.ReadFile(StateFile)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(data, &st); err != nil {
			return nil, fmt.Errorf("reading %s: %w", s.Path(StateFile), err)
		}
	}
	schema, configOK := 0, false
	if doc, err := s.ReadConfigDoc(); err == nil {
		schema, err = doc.Schema()
		configOK = err == nil
	}
	return newVersionCheck(st.CLIVersion, st.SchemaVersion, schema, configOK, cli, reg), nil
}

func newVersionCheck(stackCLI string, stackSchema, configSchema int, configOK bool, cli string, reg Registry) *VersionCheck {
	v := &VersionCheck{
		StackCLI:     stackCLI,
		StackSchema:  stackSchema,
		ConfigSchema: configSchema,
		CLI:          cli,
		Schema:       reg.Target,
		configOK:     configOK,
	}
	if configOK && configSchema < v.Schema {
		v.Pending, v.planErr = reg.plan(configSchema)
	}
	return v
}

// MigrationErr is why the config's schema can't be migrated to this
// pic-sure's (it is older than every migration), or nil.
func (v *VersionCheck) MigrationErr() error { return v.planErr }

// Newer reports whether a newer pic-sure than this one rendered the stack:
// state.json or pic-sure.yaml has a schema above Schema, or cli_version is
// a later version than CLI. Versions that CompareVersions can't order,
// such as dev, don't count.
func (v *VersionCheck) Newer() bool { return v.newer() != "" }

// newer explains why the stack is newer than this pic-sure, or returns "".
func (v *VersionCheck) newer() string {
	if schema := max(v.StackSchema, v.ConfigSchema); schema > v.Schema {
		by := ""
		if v.StackCLI != "" {
			by = ", last rendered by pic-sure " + v.StackCLI
		}
		return fmt.Sprintf("this stack uses %s schema %d%s, but this pic-sure (%s) reads schema %d", ConfigFile, schema, by, v.CLI, v.Schema)
	}
	if c, ok := CompareVersions(v.StackCLI, v.CLI); ok && c > 0 {
		return fmt.Sprintf("this stack was last rendered by pic-sure %s, which is newer than this pic-sure (%s)", v.StackCLI, v.CLI)
	}
	return ""
}

// Gate applies the version gate to a command of class (§10.6). A read-only
// command always runs; on a stack that a newer pic-sure rendered, warning
// says so. A mutating command gets an exit-5 error on such a stack, and
// while config migrations are pending, unless it is Migrating.
func (v *VersionCheck) Gate(class CommandClass) (warning string, err error) {
	if reason := v.newer(); reason != "" {
		if class == ReadOnly {
			return reason + "; upgrade pic-sure before changing the stack", nil
		}
		return "", exitcode.Incompatible("%s; upgrade pic-sure (see pic-sure self-update) to change the stack", reason)
	}
	if class == ReadOnly || !v.configOK || v.ConfigSchema >= v.Schema {
		return "", nil
	}
	if v.planErr != nil {
		return "", exitcode.Incompatible("%w", v.planErr)
	}
	if class == Migrating {
		return "", nil
	}
	return "", exitcode.Incompatible("this stack's %s is schema %d, and this pic-sure (%s) uses schema %d; run pic-sure update to migrate it, which backs it up first",
		ConfigFile, v.ConfigSchema, v.CLI, v.Schema)
}

// Raw returns the value at a key path as written in the file, with no
// defaults: a scalar, a []any or a map[string]any, or the whole document
// for "". It is for read-only commands on a stack whose schema this
// pic-sure can't decode.
func (d *ConfigDoc) Raw(key string) (any, error) {
	n := lookupNode(d.root.Content[0], splitKey(key))
	if n == nil {
		return nil, &KeyError{Key: key, Reason: "not set in " + ConfigFile}
	}
	var v any
	if err := n.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// CompareVersions orders two pic-sure versions as semantic versions,
// returning -1, 0 or 1 as a is older than, the same as, or newer than b. A
// leading v and +build metadata are ignored. So is a git describe suffix
// (-N-gSHA, -dirty), so a development build compares as the release it
// was built from. ok is false when either isn't a version, such as dev or
// a bare commit.
func CompareVersions(a, b string) (order int, ok bool) {
	va, okA := parseVersion(a)
	vb, okB := parseVersion(b)
	if !okA || !okB {
		return 0, false
	}
	for i := range va.core {
		if c := cmp.Compare(va.core[i], vb.core[i]); c != 0 {
			return c, true
		}
	}
	return comparePre(va.pre, vb.pre), true
}

type version struct {
	core [3]int
	pre  []string // dot-separated prerelease identifiers; none for a release
}

var (
	describeSuffix = regexp.MustCompile(`(-[0-9]+-g[0-9a-f]{4,})?(-dirty)?$`)
	preIdent       = regexp.MustCompile(`^[0-9A-Za-z-]+$`)
)

func parseVersion(s string) (version, bool) {
	s = strings.TrimPrefix(s, "v")
	s, _, _ = strings.Cut(s, "+")
	s = describeSuffix.ReplaceAllString(s, "")
	core, pre, hasPre := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return version{}, false
	}
	var v version
	for i, p := range parts {
		n, ok := numericIdent(p)
		if !ok {
			return version{}, false
		}
		v.core[i] = n
	}
	if hasPre {
		v.pre = strings.Split(pre, ".")
		for _, id := range v.pre {
			if !preIdent.MatchString(id) {
				return version{}, false
			}
		}
	}
	return v, true
}

// numericIdent parses a semver numeric identifier: digits, without a
// leading zero.
func numericIdent(s string) (int, bool) {
	if s == "" || (len(s) > 1 && s[0] == '0') || strings.TrimLeft(s, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// comparePre orders prerelease identifiers as semver does: a release
// beats any prerelease, numeric identifiers compare as numbers and sort
// before alphanumeric ones, and a longer list wins a tie.
func comparePre(a, b []string) int {
	switch {
	case len(a) == 0 && len(b) == 0:
		return 0
	case len(a) == 0:
		return 1
	case len(b) == 0:
		return -1
	}
	for i := 0; i < len(a) && i < len(b); i++ {
		na, numA := numericIdent(a[i])
		nb, numB := numericIdent(b[i])
		var c int
		switch {
		case numA && numB:
			c = cmp.Compare(na, nb)
		case numA:
			c = -1
		case numB:
			c = 1
		default:
			c = strings.Compare(a[i], b[i])
		}
		if c != 0 {
			return c
		}
	}
	return cmp.Compare(len(a), len(b))
}
