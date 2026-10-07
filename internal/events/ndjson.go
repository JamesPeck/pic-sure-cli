package events

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// NDJSON is the Sink for --json (spec §10.3): one JSON object per event per
// line, usually on stdout. Each object is the event's JSON form with its
// Type() as a leading "type" field:
//
//	{"type":"step_started","id":"db","title":"Start the database"}
//	{"type":"result","ok":true}
//
// The cli layer emits the Result, so it comes last.
type NDJSON struct {
	mu  sync.Mutex
	w   io.Writer
	err error
}

// NewNDJSON returns an NDJSON renderer writing to w.
func NewNDJSON(w io.Writer) *NDJSON { return &NDJSON{w: w} }

// Err returns the first error writing to w. Emit keeps going after one.
func (n *NDJSON) Err() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.err
}

// Emit writes e as one line. An event that can't be encoded (a Result whose
// Data holds a channel, say) becomes a warning line, so the output stays
// valid NDJSON.
func (n *NDJSON) Emit(e Event) {
	if p, ok := e.(Progress); ok {
		if _, finite := finitePct(p.Pct); !finite {
			p.Pct = nil
			e = p
		}
	}
	line, err := typedLine(e)
	if err != nil {
		line, _ = typedLine(Warning{Text: fmt.Sprintf("cannot encode a %s event: %v", e.Type(), err)})
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, err := n.w.Write(line); err != nil && n.err == nil {
		n.err = err
	}
}

// typedLine is e's NDJSON line: its JSON object with "type" first.
func typedLine(e Event) ([]byte, error) {
	obj, err := encodeObject(e)
	if err != nil {
		return nil, err
	}
	typ, err := json.Marshal(e.Type())
	if err != nil {
		return nil, err
	}
	return withLeadingField(`"type":`+string(typ), obj), nil
}

// ReportSchemaVersion is the schema_version of every single-object JSON
// report. Within v2, report changes are additive only (spec §10.3).
const ReportSchemaVersion = 2

// WriteReport writes a read-only command's report (status --json,
// doctor --json) to w as one JSON object with a leading
// "schema_version": 2, and a newline. report must encode as a JSON object
// without its own schema_version field.
func WriteReport(w io.Writer, report any) error {
	obj, err := encodeObject(report)
	if err == nil {
		var fields map[string]json.RawMessage
		err = json.Unmarshal(obj, &fields)
		if _, dup := fields["schema_version"]; dup {
			err = fmt.Errorf("%T has its own schema_version field", report)
		}
	}
	if err != nil {
		return fmt.Errorf("encoding the report: %w", err)
	}
	_, err = w.Write(withLeadingField(fmt.Sprintf(`"schema_version":%d`, ReportSchemaVersion), obj))
	return err
}

// encodeObject encodes v, which must encode as a JSON object, without a
// trailing newline. HTML characters are not escaped: the output is for
// terminals and programs, not web pages.
func encodeObject(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	obj := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	if len(obj) < 2 || obj[0] != '{' {
		return nil, fmt.Errorf("%T does not encode as a JSON object", v)
	}
	return obj, nil
}

// withLeadingField returns the JSON object obj with field ("key":value)
// inserted first, and a newline.
func withLeadingField(field string, obj []byte) []byte {
	line := make([]byte, 0, len(field)+len(obj)+3)
	line = append(line, '{')
	line = append(line, field...)
	if rest := obj[1:]; string(rest) != "}" {
		line = append(line, ',')
		line = append(line, rest...)
	} else {
		line = append(line, '}')
	}
	return append(line, '\n')
}
