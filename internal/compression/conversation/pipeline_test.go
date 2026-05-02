package conversation

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/scrub"
)

func TestPipelineDisabledIsNoop(t *testing.T) {
	p := NewPipeline(PipelineConfig{Enabled: false}, DefaultRegistry(), scrub.New())
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	got := p.Run("anthropic", body)
	if !got.Skipped {
		t.Error("Skipped should be true when Enabled=false")
	}
	if string(got.Body) != string(body) {
		t.Errorf("body mutated: %q", got.Body)
	}
}

func TestPipelineSkipsUnknownProviders(t *testing.T) {
	p := NewPipeline(PipelineConfig{Enabled: true, TargetRatio: 0.5}, DefaultRegistry(), scrub.New())
	body := []byte(`{"messages":[]}`)
	got := p.Run("bedrock", body)
	if !got.Skipped {
		t.Error("unknown provider should Skip")
	}
	if got.Provider != "bedrock" {
		t.Errorf("Provider = %q, want bedrock", got.Provider)
	}
}

func TestPipelineSkipsEmptyOpenAIMessages(t *testing.T) {
	p := NewPipeline(PipelineConfig{Enabled: true, TargetRatio: 0.5}, DefaultRegistry(), scrub.New())
	got := p.Run("openai", []byte(`{"messages":[]}`))
	if !got.Skipped {
		t.Error("empty messages array should Skip")
	}
}

func TestPipelineSkipsMalformedBody(t *testing.T) {
	p := NewPipeline(PipelineConfig{Enabled: true, TargetRatio: 0.5}, DefaultRegistry(), scrub.New())
	got := p.Run("anthropic", []byte("not json"))
	if !got.Skipped {
		t.Error("malformed body should Skip")
	}
}

func TestPipelineCompressesToolResultBody(t *testing.T) {
	bigJSON := `{"records":[`
	for i := 0; i < 80; i++ {
		if i > 0 {
			bigJSON += ","
		}
		bigJSON += `{"id":` + itoa(i) + `,"payload":"`
		for j := 0; j < 30; j++ {
			bigJSON += "x"
		}
		bigJSON += `"}`
	}
	bigJSON += `]}`

	body := mustMarshal(t, map[string]any{
		"model": "claude-opus-4-6",
		"messages": []any{
			map[string]any{"role": "user", "content": "please analyze"},
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type":        "tool_result",
						"tool_use_id": "toolu_01",
						"content":     bigJSON,
					},
				},
			},
		},
	})

	p := NewPipeline(PipelineConfig{
		Enabled:       true,
		TargetRatio:   0.9,
		PreserveLastN: 0,
		CompressTypes: []string{"json"},
	}, DefaultRegistry(), scrub.New())
	got := p.Run("anthropic", body)

	if got.Skipped {
		t.Fatalf("pipeline skipped: %+v", got)
	}
	if got.CompressedCount == 0 {
		t.Errorf("expected CompressedCount > 0: %+v", got)
	}
	if got.CompressedBytes >= got.OriginalBytes {
		t.Errorf("CompressedBytes %d should be < OriginalBytes %d", got.CompressedBytes, got.OriginalBytes)
	}

	// Output must still be valid JSON with the right shape.
	var env map[string]any
	if err := json.Unmarshal(got.Body, &env); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	msgs, ok := env["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("messages malformed: %v", env["messages"])
	}
	// The tool_result message's block content should now be a schema,
	// not the original records.
	second, _ := msgs[1].(map[string]any)
	content, _ := second["content"].([]any)
	block, _ := content[0].(map[string]any)
	result, _ := block["content"].(string)
	if !strings.Contains(result, "<string>") && !strings.Contains(result, "<number>") {
		t.Errorf("tool_result content not compressed: %q", result)
	}
	if got.MessagePrefixHash == "" {
		t.Errorf("expected non-empty message_prefix_hash")
	}
}

func TestPipelineDropsLowestScoredMessages(t *testing.T) {
	// Many oldest tool_result messages, one big user prompt at the end.
	msgs := []any{
		map[string]any{"role": "user", "content": "initial prompt"},
	}
	for i := 0; i < 10; i++ {
		msgs = append(msgs, map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": "toolu_" + itoa(i),
					"content":     strings.Repeat("A", 500),
				},
			},
		})
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": "final question"})

	body := mustMarshal(t, map[string]any{"model": "claude-opus-4-6", "messages": msgs})

	p := NewPipeline(PipelineConfig{
		Enabled:       true,
		TargetRatio:   0.25,
		PreserveLastN: 2,
		CompressTypes: []string{},
	}, DefaultRegistry(), scrub.New())
	got := p.Run("anthropic", body)

	if got.Skipped {
		t.Fatalf("unexpected skip: %+v", got)
	}
	if got.DroppedCount == 0 {
		t.Errorf("expected drops: %+v", got)
	}
	if got.MarkerCount == 0 {
		t.Errorf("expected at least 1 marker: %+v", got)
	}

	var env map[string]any
	if err := json.Unmarshal(got.Body, &env); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	final, _ := env["messages"].([]any)
	lastTwo := final[len(final)-2:]
	last, _ := lastTwo[1].(map[string]any)
	if last["content"] != "final question" {
		t.Errorf("last message not preserved: %v", last)
	}
	// Marker should be present somewhere in the middle.
	markerFound := false
	for _, m := range final {
		mm, _ := m.(map[string]any)
		if c, ok := mm["content"].(string); ok && strings.Contains(c, "messages compressed") {
			markerFound = true
			break
		}
	}
	if !markerFound {
		t.Errorf("marker missing from final messages: %v", final)
	}
}

func TestPipelineScrubsOutput(t *testing.T) {
	body := mustMarshal(t, map[string]any{
		"model": "claude-opus-4-6",
		"messages": []any{
			map[string]any{"role": "user", "content": "here is my token ghp_aabbccddeeff1122334455667788"},
		},
	})
	p := NewPipeline(PipelineConfig{Enabled: true, TargetRatio: 0.9}, DefaultRegistry(), scrub.New())
	got := p.Run("anthropic", body)
	if strings.Contains(string(got.Body), "ghp_aabbccddeeff1122334455667788") {
		t.Errorf("token leaked through pipeline: %s", got.Body)
	}
}

// findBreakpointIndex returns the single index i for which messages[i]
// carries a cache_control annotation on its last block, or -1 when
// none do. Fails the test when more than one index matches (the
// pipeline must set exactly one breakpoint).
func findBreakpointIndex(t *testing.T, body []byte) int {
	t.Helper()
	var env map[string]json.RawMessage
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(env["messages"], &msgs); err != nil {
		t.Fatalf("unmarshal messages: %v", err)
	}
	found := -1
	for i, mRaw := range msgs {
		var msg map[string]json.RawMessage
		if err := json.Unmarshal(mRaw, &msg); err != nil {
			continue
		}
		var blocks []map[string]any
		if err := json.Unmarshal(msg["content"], &blocks); err != nil {
			continue
		}
		for _, b := range blocks {
			if _, has := b["cache_control"]; has {
				if found >= 0 && found != i {
					t.Fatalf("more than one message carries cache_control (indices %d and %d)", found, i)
				}
				found = i
			}
		}
	}
	return found
}

// messageContentIsArray reports whether the i-th message's content field
// decodes as an array (vs a bare string).
func messageContentIsArray(t *testing.T, body []byte, i int) bool {
	t.Helper()
	var env map[string]json.RawMessage
	_ = json.Unmarshal(body, &env)
	var msgs []json.RawMessage
	_ = json.Unmarshal(env["messages"], &msgs)
	var msg map[string]json.RawMessage
	_ = json.Unmarshal(msgs[i], &msg)
	var arr []any
	return json.Unmarshal(msg["content"], &arr) == nil
}

func TestPipelineCacheMode_InjectsCacheControlAtPrefixBoundary(t *testing.T) {
	body := mustMarshal(t, map[string]any{
		"model": "claude-opus-4-6",
		"messages": []any{
			map[string]any{"role": "user", "content": "alpha"},
			map[string]any{"role": "assistant", "content": "beta"},
			map[string]any{"role": "user", "content": "gamma"},
		},
	})
	p := NewPipeline(PipelineConfig{
		Enabled:     true,
		Mode:        ModeCache,
		TargetRatio: 1.0,
	}, DefaultRegistry(), nil)
	got := p.Run("anthropic", body)
	if got.Skipped {
		t.Fatalf("want pipeline to engage in cache mode; Body=%s", got.Body)
	}
	idx := findBreakpointIndex(t, got.Body)
	if idx < 0 {
		t.Fatalf("no cache_control in body: %s", got.Body)
	}
	// Whichever message carries the breakpoint, its content must be in
	// array form — injection upgrades bare strings into a one-element
	// text block so the annotation has a valid home.
	if !messageContentIsArray(t, got.Body, idx) {
		t.Errorf("messages[%d] content not array-shaped; body=%s", idx, got.Body)
	}
}

func TestPipelineTokenMode_DoesNotInjectCacheControl(t *testing.T) {
	body := mustMarshal(t, map[string]any{
		"model": "claude-opus-4-6",
		"messages": []any{
			map[string]any{"role": "user", "content": "alpha"},
			map[string]any{"role": "assistant", "content": "beta"},
		},
	})
	p := NewPipeline(PipelineConfig{
		Enabled:     true,
		Mode:        ModeToken,
		TargetRatio: 1.0,
	}, DefaultRegistry(), nil)
	got := p.Run("anthropic", body)
	if got.Skipped {
		t.Fatalf("want pipeline to engage; Body=%s", got.Body)
	}
	if idx := findBreakpointIndex(t, got.Body); idx >= 0 {
		t.Fatalf("token mode injected cache_control on messages[%d]; body=%s", idx, got.Body)
	}
}

func TestPipelineCacheMode_PreservesExistingCacheControl(t *testing.T) {
	// Single-message body: SplitIndex → 1, breakpoint lands on the
	// same message whose only block already carries cache_control.
	// The pipeline must not duplicate the annotation on that block.
	body := mustMarshal(t, map[string]any{
		"model": "claude-opus-4-6",
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type":          "text",
						"text":          "alpha",
						"cache_control": map[string]string{"type": "ephemeral"},
					},
				},
			},
		},
	})
	p := NewPipeline(PipelineConfig{
		Enabled:     true,
		Mode:        ModeCache,
		TargetRatio: 1.0,
	}, DefaultRegistry(), nil)
	got := p.Run("anthropic", body)

	// Exactly one cache_control on exactly one block — the original
	// one. No second copy, no overwrite.
	idx := findBreakpointIndex(t, got.Body)
	if idx != 0 {
		t.Fatalf("want breakpoint at 0, got %d; body=%s", idx, got.Body)
	}
	// Drill in: the single block on messages[0] has a single
	// cache_control field.
	var env map[string]json.RawMessage
	_ = json.Unmarshal(got.Body, &env)
	var msgs []json.RawMessage
	_ = json.Unmarshal(env["messages"], &msgs)
	var msg map[string]json.RawMessage
	_ = json.Unmarshal(msgs[0], &msg)
	var blocks []map[string]any
	if err := json.Unmarshal(msg["content"], &blocks); err != nil {
		t.Fatalf("content not array: %v; body=%s", err, got.Body)
	}
	if len(blocks) != 1 {
		t.Fatalf("block count duplicated: got %d want 1; body=%s", len(blocks), got.Body)
	}
}

// buildBigJSON returns a JSON string with `n` small records, suitable
// as a fake tool-output body that the JSON compressor will shrink.
func buildBigJSON(n int) string {
	out := `{"records":[`
	for i := 0; i < n; i++ {
		if i > 0 {
			out += ","
		}
		out += `{"id":` + itoa(i) + `,"payload":"`
		for j := 0; j < 30; j++ {
			out += "x"
		}
		out += `"}`
	}
	out += `]}`
	return out
}

func TestPipelineOpenAI_CompressesToolMessageContent(t *testing.T) {
	// Chat Completions: a tool message carries the function-call output
	// as a bare string in `content`. The pipeline should shrink it
	// via the JSON compressor, rewrite the message, and forward.
	bigJSON := buildBigJSON(80)
	body := mustMarshal(t, map[string]any{
		"model": "gpt-4o",
		"messages": []any{
			map[string]any{"role": "system", "content": "helpful"},
			map[string]any{"role": "user", "content": "please analyze"},
			map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
				map[string]any{
					"id":       "call_1",
					"type":     "function",
					"function": map[string]any{"name": "fetch", "arguments": "{}"},
				},
			}},
			map[string]any{
				"role":         "tool",
				"tool_call_id": "call_1",
				"content":      bigJSON,
			},
			map[string]any{"role": "user", "content": "summarize"},
		},
	})
	p := NewPipeline(PipelineConfig{
		Enabled:       true,
		Mode:          ModeToken,
		TargetRatio:   0.5,
		CompressTypes: []string{"json"},
	}, DefaultRegistry(), nil)
	got := p.Run("openai", body)
	if got.Skipped {
		t.Fatalf("want pipeline to engage; body=%s", got.Body)
	}
	if got.CompressedCount < 1 {
		t.Fatalf("expected >=1 compression, got %d", got.CompressedCount)
	}
	if got.CompressedBytes >= got.OriginalBytes {
		t.Fatalf("compressed not smaller: %d >= %d", got.CompressedBytes, got.OriginalBytes)
	}

	// Round-trip the output and confirm the tool message's content
	// shrunk relative to the original.
	var env map[string]json.RawMessage
	if err := json.Unmarshal(got.Body, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v; body=%s", err, got.Body)
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(env["messages"], &msgs); err != nil {
		t.Fatalf("unmarshal messages: %v", err)
	}
	// Find the tool message.
	var toolRaw json.RawMessage
	for _, mRaw := range msgs {
		var hdr struct {
			Role string `json:"role"`
		}
		_ = json.Unmarshal(mRaw, &hdr)
		if hdr.Role == "tool" {
			toolRaw = mRaw
			break
		}
	}
	if toolRaw == nil {
		t.Fatalf("no tool message in output: %s", got.Body)
	}
	if len(toolRaw) >= len(bigJSON) {
		t.Errorf("tool message not compressed: len=%d bigJSON=%d", len(toolRaw), len(bigJSON))
	}
}

func TestPipelineOpenAI_CompressesToolMessageWithPartsArray(t *testing.T) {
	// Variant where the tool content is an array of text parts (newer
	// Chat Completions shape) — the rewrite should preserve the array
	// form with one collapsed text part.
	bigJSON := buildBigJSON(80)
	body := mustMarshal(t, map[string]any{
		"model": "gpt-4o",
		"messages": []any{
			map[string]any{"role": "user", "content": "analyze"},
			map[string]any{
				"role":         "tool",
				"tool_call_id": "call_1",
				"content": []any{
					map[string]any{"type": "text", "text": bigJSON},
				},
			},
			map[string]any{"role": "user", "content": "summarize"},
		},
	})
	p := NewPipeline(PipelineConfig{
		Enabled:       true,
		Mode:          ModeToken,
		TargetRatio:   0.5,
		CompressTypes: []string{"json"},
	}, DefaultRegistry(), nil)
	got := p.Run("openai", body)
	if got.Skipped {
		t.Fatalf("want engage; body=%s", got.Body)
	}
	if got.CompressedCount < 1 {
		t.Fatalf("expected compression; got=%+v", got)
	}
	// Confirm the array shape survived the rewrite.
	var env map[string]json.RawMessage
	_ = json.Unmarshal(got.Body, &env)
	var msgs []json.RawMessage
	_ = json.Unmarshal(env["messages"], &msgs)
	for _, mRaw := range msgs {
		var hdr struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(mRaw, &hdr)
		if hdr.Role != "tool" {
			continue
		}
		var arr []any
		if err := json.Unmarshal(hdr.Content, &arr); err != nil {
			t.Errorf("tool content not array after rewrite: %s", hdr.Content)
		}
	}
}

func TestPipelineOpenAI_CacheModeDoesNotInjectCacheControl(t *testing.T) {
	// Chat Completions has no standardized cache marker today, so
	// cache mode must NOT annotate any OpenAI message even when the
	// split boundary is computed.
	body := mustMarshal(t, map[string]any{
		"model": "gpt-4o",
		"messages": []any{
			map[string]any{"role": "user", "content": "alpha"},
			map[string]any{"role": "assistant", "content": "beta"},
		},
	})
	p := NewPipeline(PipelineConfig{
		Enabled:     true,
		Mode:        ModeCache,
		TargetRatio: 1.0,
	}, DefaultRegistry(), nil)
	got := p.Run("openai", body)
	if got.Skipped {
		t.Fatalf("want engage; body=%s", got.Body)
	}
	// findBreakpointIndex asserts at most one hit AND fails on
	// duplicates; no hits == -1 and passes here.
	if idx := findBreakpointIndex(t, got.Body); idx >= 0 {
		t.Errorf("OpenAI body should not carry cache_control; found at idx=%d body=%s", idx, got.Body)
	}
}

func TestPipelineOpenAI_PreservesToolCallsOnAssistantMessages(t *testing.T) {
	// Assistant messages that carry tool_calls must pass through
	// untouched (no content to compress). The serializer should not
	// mangle them.
	body := mustMarshal(t, map[string]any{
		"model": "gpt-4o",
		"messages": []any{
			map[string]any{"role": "user", "content": "run it"},
			map[string]any{
				"role":    "assistant",
				"content": nil,
				"tool_calls": []any{
					map[string]any{
						"id":       "call_xyz",
						"type":     "function",
						"function": map[string]any{"name": "f", "arguments": "{}"},
					},
				},
			},
		},
	})
	p := NewPipeline(PipelineConfig{
		Enabled:     true,
		Mode:        ModeToken,
		TargetRatio: 1.0,
	}, DefaultRegistry(), nil)
	got := p.Run("openai", body)
	if got.Skipped {
		t.Fatalf("want engage; body=%s", got.Body)
	}
	if !strings.Contains(string(got.Body), `"call_xyz"`) {
		t.Errorf("tool_calls id dropped from assistant message: %s", got.Body)
	}
}

func TestPipelineCacheMode_SingleMessageAnnotated(t *testing.T) {
	body := mustMarshal(t, map[string]any{
		"model": "claude-opus-4-6",
		"messages": []any{
			map[string]any{"role": "user", "content": "alpha"},
		},
	})
	p := NewPipeline(PipelineConfig{
		Enabled:     true,
		Mode:        ModeCache,
		TargetRatio: 1.0,
	}, DefaultRegistry(), nil)
	got := p.Run("anthropic", body)
	if got.Skipped {
		t.Fatalf("want engage; body=%s", got.Body)
	}
	idx := findBreakpointIndex(t, got.Body)
	if idx != 0 {
		t.Fatalf("want breakpoint at index 0, got %d; body=%s", idx, got.Body)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return b
}
