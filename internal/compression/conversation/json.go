package conversation

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/marmutapp/superbased-observer/internal/compression/conversation/types"
)

// JSONCompressor replaces every scalar value in a JSON body with a string
// naming its type ("<string>", "<number>", "<bool>", "<null>"). Structure
// (keys, arrays, nesting) is preserved so the model can still navigate the
// shape. Arrays of length > 1 keep only the first element's schema with a
// sibling `"_len": N` field on the parent when possible — falling back to a
// suffix comment when the parent isn't an object.
//
// When body is not valid JSON, the original body is returned unchanged so
// the budget enforcer can still ship it upstream.
type JSONCompressor struct{}

// NewJSONCompressor constructs a JSONCompressor.
func NewJSONCompressor() *JSONCompressor { return &JSONCompressor{} }

// Type implements Compressor.
func (JSONCompressor) Type() types.ContentType { return types.JSON }

// Compress implements Compressor. See [JSONCompressor] for semantics.
func (c JSONCompressor) Compress(body []byte) []byte {
	if len(bytes.TrimSpace(body)) == 0 {
		return body
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return body
	}
	schema := toSchema(v)
	// json.Marshal escapes "<" and ">" to \u003c / \u003e for HTML safety,
	// which triples the byte cost of every <string> / <number> sentinel.
	// The conversation compressor's output is forwarded to the LLM as the
	// body of a tool_result, not embedded in HTML — use an Encoder with
	// escape-html disabled to keep the schema compact.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(schema); err != nil {
		return body
	}
	// Encoder appends a trailing newline that Marshal does not; trim it.
	out := bytes.TrimRight(buf.Bytes(), "\n")
	// Fall back to the original body when compression didn't actually shrink
	// it — preserves the invariant that compressed output never grows.
	if len(out) >= len(body) {
		return body
	}
	return out
}

// toSchema recursively rewrites a decoded JSON value, replacing scalars with
// their type name and collapsing uniform arrays to a single-element schema
// annotated with their original length.
func toSchema(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, vv := range x {
			if arr, ok := vv.([]any); ok {
				out[k] = toSchema(arr)
				if len(arr) > 1 {
					out[k+"#len"] = len(arr)
				}
				continue
			}
			out[k] = toSchema(vv)
		}
		return out
	case []any:
		if len(x) == 0 {
			return []any{}
		}
		return []any{toSchema(x[0])}
	case string:
		return "<string>"
	case float64:
		return "<number>"
	case json.Number:
		return "<number>"
	case bool:
		return "<bool>"
	case nil:
		return "<null>"
	default:
		return fmt.Sprintf("<%T>", v)
	}
}
