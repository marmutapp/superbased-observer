package compile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// jsonObj is an ORDER-PRESERVING JSON object: the native config files
// this package rewrites are user-owned, so a pass must not reorder
// the user's keys (and OpenCode's permission.bash map is
// order-SENSITIVE — last match wins). encoding/json's map round-trip
// alphabetizes; this minimal member list does not.
type jsonObj struct {
	members []jsonMember
}

// jsonMember is one key/value pair; the value stays raw so untouched
// subtrees round-trip byte-comparable (modulo re-indentation).
type jsonMember struct {
	key string
	val json.RawMessage
}

// parseJSONObj decodes one JSON object preserving member order. Empty
// (or whitespace-only) input parses as an empty object — the
// missing-file case. Anything other than a single object is an error;
// callers surface it and refuse to write (never corrupt a file we
// cannot understand — JSONC with comments lands here too).
func parseJSONObj(raw []byte) (*jsonObj, error) {
	obj := &jsonObj{}
	if len(bytes.TrimSpace(raw)) == 0 {
		return obj, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("compile.parseJSONObj: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("compile.parseJSONObj: content is not a JSON object")
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("compile.parseJSONObj: key: %w", err)
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, errors.New("compile.parseJSONObj: non-string key")
		}
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, fmt.Errorf("compile.parseJSONObj: value of %q: %w", key, err)
		}
		obj.set(key, val)
	}
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("compile.parseJSONObj: close: %w", err)
	}
	return obj, nil
}

// get returns the raw value for key, false when absent.
func (o *jsonObj) get(key string) (json.RawMessage, bool) {
	for _, m := range o.members {
		if m.key == key {
			return m.val, true
		}
	}
	return nil, false
}

// set replaces key's value in place, appending the member when new
// (append-at-end is load-bearing for last-match-wins dialects).
func (o *jsonObj) set(key string, val json.RawMessage) {
	for i := range o.members {
		if o.members[i].key == key {
			o.members[i].val = val
			return
		}
	}
	o.members = append(o.members, jsonMember{key: key, val: val})
}

// remove deletes key, preserving the order of the rest.
func (o *jsonObj) remove(key string) {
	for i := range o.members {
		if o.members[i].key == key {
			o.members = append(o.members[:i], o.members[i+1:]...)
			return
		}
	}
}

// keys returns the member keys in order.
func (o *jsonObj) keys() []string {
	out := make([]string, 0, len(o.members))
	for _, m := range o.members {
		out = append(out, m.key)
	}
	return out
}

// encode renders the object with two-space indentation, member order
// preserved, nested raw values re-indented. prefix is the indentation
// of the line the object starts on ("" at top level).
func (o *jsonObj) encode(prefix string) json.RawMessage {
	if len(o.members) == 0 {
		return json.RawMessage("{}")
	}
	var buf bytes.Buffer
	buf.WriteString("{\n")
	inner := prefix + "  "
	for i, m := range o.members {
		buf.WriteString(inner)
		kb, _ := json.Marshal(m.key)
		buf.Write(kb)
		buf.WriteString(": ")
		var vb bytes.Buffer
		if err := json.Indent(&vb, m.val, inner, "  "); err == nil {
			buf.Write(vb.Bytes())
		} else {
			buf.Write(m.val)
		}
		if i < len(o.members)-1 {
			buf.WriteString(",")
		}
		buf.WriteString("\n")
	}
	buf.WriteString(prefix + "}")
	return buf.Bytes()
}
