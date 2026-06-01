package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestInjectAnthropicSystemBlock_NoSystemField pins that a body
// without a `system` field gets the new block as a one-element array.
func TestInjectAnthropicSystemBlock_NoSystemField(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hi"}]}`)
	out, err := injectAnthropicSystemBlock(body, "recovery context")
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	var env map[string]any
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, out)
	}
	sys, ok := env["system"].([]any)
	if !ok {
		t.Fatalf("system not array: %v", env["system"])
	}
	if len(sys) != 1 {
		t.Fatalf("system length %d, want 1", len(sys))
	}
	first := sys[0].(map[string]any)
	if first["text"] != "recovery context" {
		t.Errorf("system[0].text = %v, want %q", first["text"], "recovery context")
	}
}

// TestInjectAnthropicSystemBlock_StringSystem pins that a top-level
// string `system` is lifted into a two-element array (new block
// first, original second) so neither value is lost.
func TestInjectAnthropicSystemBlock_StringSystem(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4","system":"original system prompt","messages":[{"role":"user","content":"hi"}]}`)
	out, err := injectAnthropicSystemBlock(body, "recovery context")
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	var env map[string]any
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	sys := env["system"].([]any)
	if len(sys) != 2 {
		t.Fatalf("system length %d, want 2: %v", len(sys), sys)
	}
	if sys[0].(map[string]any)["text"] != "recovery context" {
		t.Errorf("first block lost recovery context: %v", sys[0])
	}
	if sys[1].(map[string]any)["text"] != "original system prompt" {
		t.Errorf("second block lost original prompt: %v", sys[1])
	}
}

// TestInjectAnthropicSystemBlock_ArraySystem pins that an existing
// array `system` is preserved with the new block prepended.
func TestInjectAnthropicSystemBlock_ArraySystem(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4","system":[{"type":"text","text":"existing block"}],"messages":[{"role":"user","content":"hi"}]}`)
	out, err := injectAnthropicSystemBlock(body, "recovery context")
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	var env map[string]any
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	sys := env["system"].([]any)
	if len(sys) != 2 {
		t.Fatalf("system length %d, want 2", len(sys))
	}
	if sys[0].(map[string]any)["text"] != "recovery context" {
		t.Errorf("first block: %v, want recovery", sys[0])
	}
	if sys[1].(map[string]any)["text"] != "existing block" {
		t.Errorf("second block: %v, want existing", sys[1])
	}
}

// TestInjectAnthropicSystemBlock_PreservesMessages pins that the
// `messages` field passes through bytewise — only `system` changes.
func TestInjectAnthropicSystemBlock_PreservesMessages(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}]}`)
	out, err := injectAnthropicSystemBlock(body, "x")
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	var env map[string]any
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatal(err)
	}
	msgs := env["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages length %d, want 2", len(msgs))
	}
	if msgs[0].(map[string]any)["content"] != "hi" {
		t.Errorf("messages[0] mutated: %v", msgs[0])
	}
}

// TestInjectAnthropicSystemBlock_EmptyContentNoOp pins that an empty
// `content` argument returns the body unchanged.
func TestInjectAnthropicSystemBlock_EmptyContentNoOp(t *testing.T) {
	body := []byte(`{"model":"claude","messages":[]}`)
	out, err := injectAnthropicSystemBlock(body, "")
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("body mutated when content empty:\n got %s\n in  %s", out, body)
	}
}

// TestInjectAnthropicSystemBlock_Determinism pins byte-identical
// output across repeated calls — the cross-turn invariance predicate.
func TestInjectAnthropicSystemBlock_Determinism(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4","system":"orig","messages":[{"role":"user","content":"hi"}]}`)
	first, err := injectAnthropicSystemBlock(body, "X")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 25; i++ {
		next, err := injectAnthropicSystemBlock(body, "X")
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		if string(next) != string(first) {
			t.Errorf("iter %d differs from first call", i)
		}
	}
}

// TestInjectAnthropicSystemBlock_BadJSON pins that a non-JSON body
// surfaces an error rather than silently passing through.
func TestInjectAnthropicSystemBlock_BadJSON(t *testing.T) {
	body := []byte(`not json`)
	_, err := injectAnthropicSystemBlock(body, "X")
	if err == nil {
		t.Errorf("expected error on non-JSON body")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("error should mention parse: %v", err)
	}
}
