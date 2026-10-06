package stack

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// ConfigDoc is pic-sure.yaml as a YAML document. Edits go through Set and
// SetValue, which change only the key they name, so Bytes keeps the user's
// comments and key order.
type ConfigDoc struct {
	root *yaml.Node // a DocumentNode holding one MappingNode
}

// SchemaVersionError is a pic-sure.yaml whose schema isn't ConfigSchema.
// The version gate turns it into "run pic-sure update" or "this pic-sure is
// too old".
type SchemaVersionError struct {
	Found int
}

func (e *SchemaVersionError) Error() string {
	return fmt.Sprintf("%s is schema %d, but this pic-sure reads schema %d", ConfigFile, e.Found, ConfigSchema)
}

// ReadConfigDoc reads the pic-sure.yaml in the stack directory dir.
func ReadConfigDoc(dir string) (*ConfigDoc, error) {
	data, err := os.ReadFile(filepath.Join(dir, ConfigFile))
	if err != nil {
		return nil, err
	}
	return ParseConfigDoc(data)
}

// LoadConfig reads, decodes and validates the pic-sure.yaml in the stack
// directory dir.
func LoadConfig(dir string) (*Config, error) {
	doc, err := ReadConfigDoc(dir)
	if err != nil {
		return nil, err
	}
	return doc.Config()
}

// ParseConfig decodes and validates pic-sure.yaml content.
func ParseConfig(data []byte) (*Config, error) {
	doc, err := ParseConfigDoc(data)
	if err != nil {
		return nil, err
	}
	return doc.Config()
}

// ParseConfigDoc parses pic-sure.yaml content without decoding it, so a
// file with bad values can still be edited. A YAML syntax error is a
// *ConfigError.
func ParseConfigDoc(data []byte) (*ConfigDoc, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var root yaml.Node
	switch err := dec.Decode(&root); {
	case errors.Is(err, io.EOF):
		root = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	case err != nil:
		return nil, &ConfigError{Problems: []Problem{syntaxProblem(err)}}
	}
	var extra yaml.Node
	switch err := dec.Decode(&extra); {
	case errors.Is(err, io.EOF):
	case err != nil:
		return nil, &ConfigError{Problems: []Problem{syntaxProblem(err)}}
	default:
		return nil, &ConfigError{Problems: []Problem{{Line: extra.Line, Msg: "only one YAML document is allowed"}}}
	}
	if len(root.Content) == 0 { // only comments
		root.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	}
	if top := root.Content[0]; top.Kind != yaml.MappingNode {
		return nil, &ConfigError{Problems: []Problem{{Line: top.Line, Msg: "want a mapping of keys at the top level"}}}
	}
	if problems := findAnchors(&root, nil); len(problems) > 0 {
		return nil, &ConfigError{Problems: problems}
	}
	return &ConfigDoc{root: &root}, nil
}

// findAnchors reports every anchor and alias under n. They aren't
// supported: an alias can refer to itself, and editing an anchored value
// would change, or orphan, its aliases.
func findAnchors(n *yaml.Node, problems []Problem) []Problem {
	if (n.Anchor != "" || n.Kind == yaml.AliasNode) && (len(problems) == 0 || problems[len(problems)-1].Line != n.Line) {
		problems = append(problems, Problem{Line: n.Line, Msg: "YAML anchors and aliases (&name, *name) aren't supported"})
	}
	for _, c := range n.Content {
		problems = findAnchors(c, problems)
	}
	return problems
}

var yamlLineRE = regexp.MustCompile(`^yaml: line (\d+): (.*)$`)

// syntaxProblem turns a yaml.v3 syntax error into a Problem, moving its
// line number into Problem.Line.
func syntaxProblem(err error) Problem {
	m := yamlLineRE.FindStringSubmatch(err.Error())
	if m == nil {
		return Problem{Msg: strings.TrimPrefix(err.Error(), "yaml: ")}
	}
	line, _ := strconv.Atoi(m[1])
	return Problem{Line: line, Msg: m[2]}
}

// NewConfigDoc returns a new pic-sure.yaml holding c, with a header, the
// allowed values of each enum as line comments, and lists in flow style.
func NewConfigDoc(c *Config) (*ConfigDoc, error) {
	var top yaml.Node
	if err := top.Encode(c); err != nil {
		return nil, err
	}
	top.HeadComment = "pic-sure stack config. Edit it with `pic-sure config set` or `pic-sure config edit`,\n" +
		"which validate before saving. Secrets are in .pic-sure/secrets.yaml."
	for _, f := range Fields {
		n := lookupNode(&top, splitKey(f.Key))
		switch {
		case n == nil:
		case len(f.Options) > 0:
			n.LineComment = strings.Join(f.Options, " | ")
		case f.Kind == KindList:
			n.Style |= yaml.FlowStyle
		}
	}
	return &ConfigDoc{root: &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{&top}}}, nil
}

// Node returns the document's root node, for config migrations, which
// rewrite it in place.
func (d *ConfigDoc) Node() *yaml.Node { return d.root }

// Bytes encodes the document.
func (d *ConfigDoc) Bytes() ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(d.root); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Config decodes the document over DefaultConfig and validates the result.
// A key missing from the file keeps its default, and so does one set to
// null. Every problem found comes back in one *ConfigError, with the line it
// is on when the file has the key. A schema other than ConfigSchema is a
// *SchemaVersionError instead, because the rest of the file follows another
// schema.
func (d *ConfigDoc) Config() (*Config, error) {
	top := d.root.Content[0]
	if err := checkSchema(top); err != nil {
		return nil, err
	}
	c := DefaultConfig()
	dec := decoder{lines: map[string]int{}}
	dec.decode(top, reflect.ValueOf(&c).Elem(), "")
	if len(dec.problems) > 0 {
		return nil, &ConfigError{Problems: dec.problems}
	}
	if err := c.Validate(); err != nil {
		var ce *ConfigError
		if errors.As(err, &ce) {
			for i := range ce.Problems {
				ce.Problems[i].Line = dec.lines[ce.Problems[i].Path]
			}
		}
		return nil, err
	}
	return &c, nil
}

// checkSchema reports a missing or non-integer schema as a problem, and a
// different schema as a *SchemaVersionError.
func checkSchema(top *yaml.Node) error {
	n := lookupNode(top, []string{"schema"})
	if n == nil {
		return &ConfigError{Problems: []Problem{{Path: "schema", Msg: fmt.Sprintf("required; this pic-sure writes schema %d", ConfigSchema)}}}
	}
	var v int
	if !scalarFits(n, reflect.Int) || n.Decode(&v) != nil {
		return &ConfigError{Problems: []Problem{{Path: "schema", Line: n.Line, Msg: "want a whole number, got " + describeNode(n)}}}
	}
	if v != ConfigSchema {
		return &SchemaVersionError{Found: v}
	}
	return nil
}

// ReadOnlyChanges returns a *ConfigError naming each read-only key whose
// value in d differs from its value in before, or nil. It compares the
// documents, so it works even when before is invalid.
func (d *ConfigDoc) ReadOnlyChanges(before *ConfigDoc) error {
	var problems []Problem
	for _, f := range Fields {
		if !f.ReadOnly {
			continue
		}
		was, ok := before.readOnlyValue(f)
		if !ok {
			continue
		}
		if now, _ := d.readOnlyValue(f); now != was {
			problems = append(problems, Problem{Path: f.Key, Msg: fmt.Sprintf("is read-only; it was %v", was)})
		}
	}
	if len(problems) > 0 {
		return &ConfigError{Problems: problems}
	}
	return nil
}

// readOnlyValue decodes read-only field f in d. ok is false when the value
// is missing, empty, or one no stack could have been created with, since
// then there's nothing to protect.
func (d *ConfigDoc) readOnlyValue(f Field) (v any, ok bool) {
	n := lookupNode(d.root.Content[0], splitKey(f.Key))
	if n == nil || n.Kind != yaml.ScalarNode {
		return nil, false
	}
	if f.Kind == KindInt {
		var i int
		if !scalarFits(n, reflect.Int) || n.Decode(&i) != nil {
			return nil, false
		}
		return i, true
	}
	if n.Value == "" || (f.Key == "name" && !nameRE.MatchString(n.Value)) {
		return nil, false
	}
	return n.Value, true
}

// Set parses value for the field at key and stores it, like
// `pic-sure config set`. It doesn't validate the whole config; call Config
// for that.
func (d *ConfigDoc) Set(key, value string) error {
	f, err := settableField(key)
	if err != nil {
		return err
	}
	v, err := parseValue(f, key, value)
	if err != nil {
		return err
	}
	return d.put(key, v)
}

// SetValue stores v, which must have the field's Go type (string, int,
// bool or []string), at key. Like Set, it doesn't validate.
func (d *ConfigDoc) SetValue(key string, v any) error {
	f, err := settableField(key)
	if err != nil {
		return err
	}
	if !kindAccepts(f.Kind, v) {
		return fmt.Errorf("%s: a %s field can't hold a %T", key, f.Kind, v)
	}
	return d.put(key, v)
}

func settableField(key string) (Field, error) {
	f, ok := LookupField(key)
	switch {
	case !ok:
		return Field{}, &KeyError{Key: key, Reason: "unknown config key"}
	case f.Secret:
		return Field{}, secretKeyError(key)
	case f.ReadOnly:
		return Field{}, &KeyError{Key: key, Reason: "is read-only"}
	}
	return f, nil
}

func kindAccepts(k FieldKind, v any) bool {
	switch v.(type) {
	case string:
		return k == KindString
	case int:
		return k == KindInt
	case bool:
		return k == KindBool
	case []string:
		return k == KindList
	}
	return false
}

// put stores v at key, creating the mappings on the way. A replaced value
// keeps its comments, and a replaced flow-style list stays flow style.
func (d *ConfigDoc) put(key string, v any) error {
	var val yaml.Node
	if err := val.Encode(v); err != nil {
		return err
	}
	n := d.root.Content[0]
	segs := splitKey(key)
	for i, seg := range segs {
		last := i == len(segs)-1
		child := lookupNode(n, []string{seg})
		if child == nil {
			child = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			if last {
				child = &val
			}
			k := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: seg}
			if len(n.Content) == 0 {
				n.Style &^= yaml.FlowStyle // a {} that gains keys becomes a block
			} else {
				// A comment after the last key stays last.
				prev := n.Content[len(n.Content)-2]
				k.FootComment, prev.FootComment = prev.FootComment, ""
			}
			n.Content = append(n.Content, k, child)
		} else if last {
			replaceNode(child, &val)
		} else if child.Kind != yaml.MappingNode {
			replaceNode(child, &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"})
		}
		n = child
	}
	return nil
}

// replaceNode makes old hold new's value, keeping old's comments, and its
// flow style when both are lists.
func replaceNode(old, new *yaml.Node) {
	flow := old.Kind == yaml.SequenceNode && new.Kind == yaml.SequenceNode && old.Style&yaml.FlowStyle != 0
	head, line, foot := old.HeadComment, old.LineComment, old.FootComment
	*old = *new
	old.HeadComment, old.LineComment, old.FootComment = head, line, foot
	if flow {
		old.Style |= yaml.FlowStyle
	}
}

// lookupNode follows segs through mappings, returning nil if a key is
// missing.
func lookupNode(n *yaml.Node, segs []string) *yaml.Node {
	for _, seg := range segs {
		var next *yaml.Node
		if n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				if n.Content[i].Value == seg {
					next = n.Content[i+1]
					break
				}
			}
		}
		if next == nil {
			return nil
		}
		n = next
	}
	return n
}

// decoder decodes a YAML node tree into a Config strictly: an unknown key,
// a duplicate key or a value of the wrong type is a problem at its key path.
type decoder struct {
	problems []Problem
	lines    map[string]int // key path -> line, for annotating validation problems
}

func (d *decoder) fail(path string, line int, format string, args ...any) {
	d.problems = append(d.problems, Problem{Path: path, Line: line, Msg: fmt.Sprintf(format, args...)})
}

func (d *decoder) decode(n *yaml.Node, v reflect.Value, path string) {
	if n.Kind == yaml.ScalarNode && n.ShortTag() == "!!null" {
		return // keep the default
	}
	switch v.Kind() {
	case reflect.Struct, reflect.Map:
		if n.Kind != yaml.MappingNode {
			d.fail(path, n.Line, "want a mapping of keys, got %s", describeNode(n))
			return
		}
		if v.Kind() == reflect.Map && v.IsNil() {
			v.Set(reflect.MakeMap(v.Type()))
		}
		seen := map[string]bool{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, val := n.Content[i], n.Content[i+1]
			if k.Kind != yaml.ScalarNode {
				d.fail(path, k.Line, "keys must be plain names")
				continue
			}
			p := joinKey(path, k.Value)
			if seen[k.Value] {
				d.fail(p, k.Line, "duplicate key")
				continue
			}
			seen[k.Value] = true
			d.lines[p] = k.Line
			if v.Kind() == reflect.Map {
				d.decodeMapEntry(val, v, k.Value, p)
				continue
			}
			f, ok := fieldByYAMLName(v, k.Value)
			if !ok {
				d.fail(p, k.Line, "unknown key")
				continue
			}
			d.decode(val, f, p)
		}
	default:
		if !scalarFits(n, v.Kind()) || n.Decode(v.Addr().Interface()) != nil {
			d.fail(path, n.Line, "want %s, got %s", describeType(v.Type()), describeNode(n))
		}
	}
}

// scalarFits reports whether n's tag suits a field of kind k. yaml.v3 alone
// would truncate 8080.5 into an int, and take yes or on as a bool.
func scalarFits(n *yaml.Node, k reflect.Kind) bool {
	switch k {
	case reflect.Int:
		return n.ShortTag() == "!!int"
	case reflect.Bool:
		return n.ShortTag() == "!!bool"
	}
	return true
}

func (d *decoder) decodeMapEntry(n *yaml.Node, m reflect.Value, key, path string) {
	k := reflect.ValueOf(key).Convert(m.Type().Key())
	elem := reflect.New(m.Type().Elem()).Elem()
	if cur := m.MapIndex(k); cur.IsValid() {
		elem.Set(cur)
	}
	d.decode(n, elem, path)
	m.SetMapIndex(k, elem)
}

func joinKey(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func describeNode(n *yaml.Node) string {
	switch n.Kind {
	case yaml.MappingNode:
		return "a mapping"
	case yaml.SequenceNode:
		return "a list"
	default:
		return fmt.Sprintf("%q", n.Value)
	}
}

func describeType(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Int:
		return "a whole number"
	case reflect.Bool:
		return "true or false"
	case reflect.Slice:
		return "a list of strings"
	default:
		return "a string"
	}
}
