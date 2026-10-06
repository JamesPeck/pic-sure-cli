package stack

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"go.yaml.in/yaml/v3"
)

// ConfigDoc is pic-sure.yaml as a YAML document. Edits go through Set and
// SetValue, which change only the key they name, so Bytes keeps the user's
// comments, key order and formatting everywhere else.
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
		return nil, &ConfigError{Problems: []Problem{{Msg: strings.TrimPrefix(err.Error(), "yaml: ")}}}
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, &ConfigError{Problems: []Problem{{Line: extra.Line, Msg: "only one YAML document is allowed"}}}
	}
	if len(root.Content) == 0 { // only comments
		root.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	}
	if top := root.Content[0]; top.Kind != yaml.MappingNode {
		return nil, &ConfigError{Problems: []Problem{{Line: top.Line, Msg: "want a mapping of keys at the top level"}}}
	}
	return &ConfigDoc{root: &root}, nil
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
	if err := n.Decode(&v); err != nil {
		return &ConfigError{Problems: []Problem{{Path: "schema", Line: n.Line, Msg: "want a whole number, got " + describeNode(n)}}}
	}
	if v != ConfigSchema {
		return &SchemaVersionError{Found: v}
	}
	return nil
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
		var child *yaml.Node
		if j := mappingIndex(n, seg); j >= 0 {
			if child = n.Content[j]; child.Kind == yaml.AliasNode {
				// Edit a copy, so the anchor's other users don't change.
				child = deepCopy(child.Alias)
				child.Anchor = ""
				n.Content[j] = child
			}
		}
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

func deepCopy(n *yaml.Node) *yaml.Node {
	cp := *n
	cp.Content = make([]*yaml.Node, len(n.Content))
	for i, c := range n.Content {
		cp.Content[i] = deepCopy(c)
	}
	return &cp
}

// mappingIndex returns the index in n.Content of the value under key in
// mapping n, or -1.
func mappingIndex(n *yaml.Node, key string) int {
	if n.Kind != yaml.MappingNode {
		return -1
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return i + 1
		}
	}
	return -1
}

// lookupNode follows segs through mappings and aliases, returning nil if
// a key is missing.
func lookupNode(n *yaml.Node, segs []string) *yaml.Node {
	for _, seg := range segs {
		i := mappingIndex(n, seg)
		if i < 0 {
			return nil
		}
		if n = n.Content[i]; n.Kind == yaml.AliasNode {
			n = n.Alias
		}
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
	if n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	if n.Kind == yaml.ScalarNode && n.Tag == "!!null" {
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
		seen := map[string]int{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, val := n.Content[i], n.Content[i+1]
			if k.Kind != yaml.ScalarNode {
				d.fail(path, k.Line, "keys must be plain names")
				continue
			}
			p := joinKey(path, k.Value)
			if first, dup := seen[k.Value]; dup {
				d.fail(p, k.Line, "duplicate key (first on line %d)", first)
				continue
			}
			seen[k.Value] = k.Line
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
		if err := n.Decode(v.Addr().Interface()); err != nil {
			d.fail(path, n.Line, "want %s, got %s", describeType(v.Type()), describeNode(n))
		}
	}
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
