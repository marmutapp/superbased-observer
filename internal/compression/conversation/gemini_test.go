package conversation

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/scrub"
	"github.com/marmutapp/superbased-observer/internal/stash"
)

// helperGeminiBody builds a generateContent request body from a contents[]
// slice and an optional tools[] slice.
func helperGeminiBody(t *testing.T, contents []map[string]any, tools []map[string]any) []byte {
	t.Helper()
	env := map[string]any{"contents": contents}
	if tools != nil {
		env["tools"] = tools
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("helperGeminiBody: marshal: %v", err)
	}
	return b
}

func newGeminiPipeline() *Pipeline {
	return NewPipeline(PipelineConfig{
		Enabled:       true,
		TargetRatio:   0.95,
		PreserveLastN: 0,
		CompressTypes: []string{"json", "logs", "code"},
	}, DefaultRegistry(), scrub.New())
}

// TestRunGoogle_CompressesFunctionResponseString pins the core win: a large
// JSON tool output inside functionResponse.response is compressed, the body
// shrinks, and the result is still valid generateContent JSON.
func TestRunGoogle_CompressesFunctionResponseString(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`{"items":[`)
	for i := 0; i < 80; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"id":` + itoa(i) + `,"name":"widget","price":12.5,"active":true}`)
	}
	sb.WriteString(`]}`)
	bigJSON := sb.String()

	body := helperGeminiBody(t, []map[string]any{
		{"role": "user", "parts": []any{map[string]any{"text": "list the widgets"}}},
		{"role": "model", "parts": []any{
			map[string]any{"functionCall": map[string]any{"name": "list_widgets", "args": map[string]any{}}},
		}},
		{"role": "user", "parts": []any{
			map[string]any{"functionResponse": map[string]any{
				"name":     "list_widgets",
				"response": map[string]any{"output": bigJSON},
			}},
		}},
	}, nil)

	got := newGeminiPipeline().Run("google", body)
	if got.Skipped {
		t.Fatalf("expected pipeline to run, got skipped")
	}
	if len(got.Events) == 0 {
		t.Fatalf("expected at least one compression event")
	}
	if got.CompressedBytes >= got.OriginalBytes {
		t.Errorf("expected body to shrink, original=%d compressed=%d", got.OriginalBytes, got.CompressedBytes)
	}
	if !json.Valid(got.Body) {
		t.Fatalf("compressed body is not valid JSON (would 400 upstream)")
	}
	// The compressed body must still parse as a generateContent envelope
	// with the same contents shape.
	var env struct {
		Contents []struct {
			Role  string            `json:"role"`
			Parts []json.RawMessage `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(got.Body, &env); err != nil {
		t.Fatalf("compressed body lost generateContent shape: %v", err)
	}
	if len(env.Contents) != 3 {
		t.Fatalf("expected 3 contents preserved, got %d", len(env.Contents))
	}
}

// TestRunGoogle_FastPathNoOpByteStable pins that a body with nothing to
// compress is forwarded byte-for-byte (modulo scrubbing), preserving Gemini's
// implicit prefix cache.
func TestRunGoogle_FastPathNoOpByteStable(t *testing.T) {
	body := helperGeminiBody(t, []map[string]any{
		{"role": "user", "parts": []any{map[string]any{"text": "hi there"}}},
		{"role": "model", "parts": []any{map[string]any{"text": "hello!"}}},
	}, nil)

	got := newGeminiPipeline().Run("google", body)
	if got.Skipped {
		t.Fatalf("expected non-skipped (fast-path), got skipped")
	}
	if len(got.Events) != 0 {
		t.Fatalf("expected no events on fast-path, got %+v", got.Events)
	}
	if string(got.Body) != string(body) {
		t.Errorf("fast-path must forward bytes unchanged:\n have %s\n want %s", got.Body, body)
	}
}

// TestRunGoogle_PreservesNumbersAndStructure pins that non-string values
// (numbers, bools) in a functionResponse are NEVER round-tripped through
// float64 — a small response with no compressible string is left untouched.
func TestRunGoogle_PreservesNumbersAndStructure(t *testing.T) {
	body := helperGeminiBody(t, []map[string]any{
		{"role": "user", "parts": []any{
			map[string]any{"functionResponse": map[string]any{
				"name": "get_count",
				// big integer + bool, but no compressible string value.
				"response": map[string]any{"count": json.RawMessage("99999999999999999"), "ok": true},
			}},
		}},
	}, nil)

	got := newGeminiPipeline().Run("google", body)
	if len(got.Events) != 0 {
		t.Fatalf("expected no compression (no large string value), got %+v", got.Events)
	}
	if !strings.Contains(string(got.Body), "99999999999999999") {
		t.Errorf("large integer was corrupted by a float64 round-trip: %s", got.Body)
	}
}

// TestRunGoogle_TrimsToolDefinitions pins functionDeclarations description/
// examples trimming.
func TestRunGoogle_TrimsToolDefinitions(t *testing.T) {
	longDesc := "Reads a file.\n\nParagraph two with detail.\n\nParagraph three that should be trimmed off the tail of the description so it shrinks."
	body := helperGeminiBody(t, []map[string]any{
		{"role": "user", "parts": []any{map[string]any{"text": "go"}}},
	}, []map[string]any{
		{"functionDeclarations": []any{
			map[string]any{
				"name":        "read_file",
				"description": longDesc,
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"path": map[string]any{"type": "string"}},
					"examples":   []any{map[string]any{"path": "a.go"}, map[string]any{"path": "b.go"}},
				},
			},
		}},
	})

	// Tool-def trim is gated behind compress_types containing "tools"
	// (mirrors the OpenAI/Anthropic gate).
	p := NewPipeline(PipelineConfig{
		Enabled:       true,
		TargetRatio:   0.95,
		CompressTypes: []string{"json", "logs", "code", "tools"},
	}, DefaultRegistry(), scrub.New())
	got := p.Run("google", body)
	if got.Skipped {
		t.Fatalf("expected pipeline to run")
	}
	sawTools := false
	for _, ev := range got.Events {
		if ev.Mechanism == "tools" {
			sawTools = true
		}
	}
	if !sawTools {
		t.Errorf("expected a tools trim event, got %+v", got.Events)
	}
	if !json.Valid(got.Body) {
		t.Fatalf("compressed body not valid JSON")
	}
	if strings.Contains(string(got.Body), "\"examples\"") {
		t.Errorf("examples should have been stripped from parameters")
	}
}

// TestRunGoogle_MalformedBodySkips pins that a non-generateContent body is
// skipped (forwarded untouched by the caller), never erroring.
func TestRunGoogle_MalformedBodySkips(t *testing.T) {
	got := newGeminiPipeline().Run("google", []byte(`{"messages":[{"role":"user"}]}`))
	if !got.Skipped {
		t.Errorf("expected skip for a non-generateContent body, got %+v", got)
	}
	got2 := newGeminiPipeline().Run("google", []byte(`not json`))
	if !got2.Skipped {
		t.Errorf("expected skip for non-JSON body")
	}
}

// TestRunGoogle_DisabledIsNoOp pins the global gate.
func TestRunGoogle_DisabledIsNoOp(t *testing.T) {
	p := NewPipeline(PipelineConfig{Enabled: false, CompressTypes: []string{"json"}}, DefaultRegistry(), scrub.New())
	body := helperGeminiBody(t, []map[string]any{
		{"role": "user", "parts": []any{map[string]any{"text": "hi"}}},
	}, nil)
	got := p.Run("google", body)
	if !got.Skipped {
		t.Errorf("disabled pipeline must skip")
	}
}

// bigGeminiJSON returns a compressible JSON blob large enough to clear the
// per-type shrink check.
func bigGeminiJSON() string {
	var sb strings.Builder
	sb.WriteString(`{"items":[`)
	for i := 0; i < 120; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"id":` + itoa(i) + `,"name":"widget","price":12.5,"active":true}`)
	}
	sb.WriteString(`]}`)
	return sb.String()
}

// TestRunGoogle_CompressesNestedResponseObject pins the v2 deep-recursion win:
// a large JSON string nested one level deeper than the top of `response` (a
// common structured tool-output shape) is reached and compressed — the v1
// top-level-only walk would have left it untouched.
func TestRunGoogle_CompressesNestedResponseObject(t *testing.T) {
	body := helperGeminiBody(t, []map[string]any{
		{"role": "user", "parts": []any{
			map[string]any{"functionResponse": map[string]any{
				"name":     "query",
				"response": map[string]any{"result": map[string]any{"data": bigGeminiJSON()}},
			}},
		}},
	}, nil)

	got := newGeminiPipeline().Run("google", body)
	if got.Skipped || len(got.Events) == 0 {
		t.Fatalf("expected nested string to be compressed, got skipped=%v events=%d", got.Skipped, len(got.Events))
	}
	if got.CompressedBytes >= got.OriginalBytes {
		t.Errorf("expected body to shrink, original=%d compressed=%d", got.OriginalBytes, got.CompressedBytes)
	}
	if !json.Valid(got.Body) {
		t.Fatalf("compressed body not valid JSON")
	}
	// Structure preserved: still result.data under response.
	if !strings.Contains(string(got.Body), `"result"`) || !strings.Contains(string(got.Body), `"data"`) {
		t.Errorf("nested keys result/data were not preserved: %s", got.Body)
	}
}

// TestRunGoogle_CompressesMCPArrayResponse pins compression of the MCP-style
// response:{content:[{type:"text",text:…}]} shape — the big text leaf lives
// inside an array element, which the v1 walk never reached.
func TestRunGoogle_CompressesMCPArrayResponse(t *testing.T) {
	body := helperGeminiBody(t, []map[string]any{
		{"role": "user", "parts": []any{
			map[string]any{"functionResponse": map[string]any{
				"name": "mcp_tool",
				"response": map[string]any{
					"content": []any{
						map[string]any{"type": "text", "text": bigGeminiJSON()},
					},
				},
			}},
		}},
	}, nil)

	got := newGeminiPipeline().Run("google", body)
	if got.Skipped || len(got.Events) == 0 {
		t.Fatalf("expected MCP array text to be compressed, got skipped=%v events=%d", got.Skipped, len(got.Events))
	}
	if got.CompressedBytes >= got.OriginalBytes {
		t.Errorf("expected body to shrink, original=%d compressed=%d", got.OriginalBytes, got.CompressedBytes)
	}
	if !json.Valid(got.Body) {
		t.Fatalf("compressed body not valid JSON")
	}
}

// TestRunGoogle_PreservesHTMLInUntouchedArraySibling pins that when one array
// element is compressed, an untouched sibling string containing LITERAL HTML
// chars keeps its exact bytes (marshalGeminiArray with HTML-escape OFF) rather
// than being rewritten to < etc. The body is assembled by hand (not via
// json.Marshal, which would pre-escape `<`) because real generateContent
// bodies come from a JS client whose JSON.stringify leaves `<` literal — the
// case where default Go marshaling would corrupt the cache prefix.
func TestRunGoogle_PreservesHTMLInUntouchedArraySibling(t *testing.T) {
	const sibling = "keep <b>this</b> & that raw"
	textVal, err := json.Marshal(bigGeminiJSON()) // no `<` in it — safe to encode
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := []byte(`{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"mcp_tool","response":{"content":[{"type":"text","text":` +
		string(textVal) + `},"` + sibling + `"]}}}]}]}`)
	if !strings.Contains(string(body), "<b>") {
		t.Fatalf("test setup: input should carry a literal <b>")
	}

	got := newGeminiPipeline().Run("google", body)
	if got.Skipped || len(got.Events) == 0 {
		t.Fatalf("expected compression to fire")
	}
	if !json.Valid(got.Body) {
		t.Fatalf("compressed body not valid JSON")
	}
	if !strings.Contains(string(got.Body), sibling) {
		t.Errorf("untouched HTML sibling was escaped/altered; want literal %q in:\n%s", sibling, got.Body)
	}
}

// TestRunGoogle_StashesOversizeLeaf pins CCR stash parity: a functionResponse
// string leaf still over the stash threshold after per-type compression is
// written to the stash and replaced with a retrieve_stashed marker that
// round-trips back to the original bytes.
func TestRunGoogle_StashesOversizeLeaf(t *testing.T) {
	st, err := stash.New(stash.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("stash.New: %v", err)
	}
	// CompressTypes WITHOUT "text" so the plain-text leaf is not per-type
	// compressed — it stays oversized and the stash pass is what fires.
	p := NewPipeline(PipelineConfig{
		Enabled:       true,
		TargetRatio:   0.95,
		CompressTypes: []string{"json", "logs", "code"},
	}, DefaultRegistry(), scrub.New()).WithStash(st, 256)

	bigText := strings.Repeat("a plain english sentence that does not compress as a known type. ", 30)
	body := helperGeminiBody(t, []map[string]any{
		{"role": "user", "parts": []any{
			map[string]any{"functionResponse": map[string]any{
				"name":     "read_doc",
				"response": map[string]any{"output": bigText},
			}},
		}},
	}, nil)

	got := p.Run("google", body)
	if got.Skipped {
		t.Fatalf("expected stash to fire, got skipped")
	}
	var sha string
	sawStash := false
	for _, ev := range got.Events {
		if ev.Mechanism == "stash" {
			sawStash = true
		}
	}
	if !sawStash {
		t.Fatalf("expected a stash event, got %+v", got.Events)
	}
	if !strings.Contains(string(got.Body), "retrieve_stashed") {
		t.Fatalf("expected a stash marker in the body: %s", got.Body)
	}
	// Recover the sha from the marker and confirm the stash round-trips.
	const shaTag = `sha=\"`
	if i := strings.Index(string(got.Body), shaTag); i >= 0 {
		rest := string(got.Body)[i+len(shaTag):]
		if j := strings.Index(rest, `\"`); j > 0 {
			sha = rest[:j]
		}
	}
	if sha == "" {
		t.Fatalf("could not parse sha from marker: %s", got.Body)
	}
	stored, err := st.Read(sha)
	if err != nil {
		t.Fatalf("stash.Read(%q): %v", sha, err)
	}
	if string(stored) != bigText {
		t.Errorf("stash round-trip mismatch: got %d bytes, want %d", len(stored), len(bigText))
	}
}

// --- Budget drop pass (v3) tests ------------------------------------------
//
// These pin the PAIRING + ALTERNATION safety of enforceGeminiBudget: the only
// drop unit is a complete (model-call, user-response) round-trip, dropped as a
// pair, and the surviving sequence must stay strictly user/model-alternating
// with a leading user turn. The 400-risk shapes (orphaned call/response,
// broken alternation, dropped leading/tail turn) are explicit cases.

// geminiResultRoles parses a generateContent body and returns the role of each
// surviving content, in order.
func geminiResultRoles(t *testing.T, body []byte) []string {
	t.Helper()
	var env struct {
		Contents []struct {
			Role string `json:"role"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("geminiResultRoles: %v", err)
	}
	roles := make([]string, len(env.Contents))
	for i, c := range env.Contents {
		roles[i] = c.Role
	}
	return roles
}

// assertGeminiAlternating fails when roles aren't a valid Gemini sequence:
// non-empty, leading "user", and no two consecutive entries sharing a role.
func assertGeminiAlternating(t *testing.T, roles []string) {
	t.Helper()
	if len(roles) == 0 {
		t.Fatalf("empty contents would 400 upstream")
	}
	if roles[0] != "user" {
		t.Errorf("leading role must be user, got %q (roles=%v)", roles[0], roles)
	}
	for i := 1; i < len(roles); i++ {
		if roles[i] == roles[i-1] {
			t.Errorf("alternation broken at %d: %v", i, roles)
		}
	}
}

// geminiDropPipeline compresses nothing by type (CompressTypes=json only, so
// plain-text tool outputs are never per-type compressed) and targets an
// aggressive ratio so the budget DROP pass is the only thing that fires —
// isolating the drop behaviour under test.
func geminiDropPipeline(ratio float64, preserveLastN int) *Pipeline {
	return NewPipeline(PipelineConfig{
		Enabled:       true,
		TargetRatio:   ratio,
		PreserveLastN: preserveLastN,
		CompressTypes: []string{"json"},
	}, DefaultRegistry(), scrub.New())
}

// sevenTurnBody is a u/m/u/m/u/m/u conversation with two complete tool round-
// trips (alpha, beta) followed by a final model answer + a fresh user prompt.
func sevenTurnBody(t *testing.T) []byte {
	t.Helper()
	big := strings.Repeat("the quick brown fox jumped over the lazy dog. ", 40)
	return helperGeminiBody(t, []map[string]any{
		{"role": "user", "parts": []any{map[string]any{"text": "investigate the repo"}}},
		{"role": "model", "parts": []any{map[string]any{"functionCall": map[string]any{"name": "alpha", "args": map[string]any{}}}}},
		{"role": "user", "parts": []any{map[string]any{"functionResponse": map[string]any{"name": "alpha", "response": map[string]any{"output": big}}}}},
		{"role": "model", "parts": []any{map[string]any{"functionCall": map[string]any{"name": "beta", "args": map[string]any{}}}}},
		{"role": "user", "parts": []any{map[string]any{"functionResponse": map[string]any{"name": "beta", "response": map[string]any{"output": big}}}}},
		{"role": "model", "parts": []any{map[string]any{"text": "the final answer here"}}},
		{"role": "user", "parts": []any{map[string]any{"text": "now do the follow up question"}}},
	}, nil)
}

// TestRunGoogle_DropsRoundTripsPairwise pins that an over-budget conversation
// drops its OLD tool round-trips as whole (call+response) pairs: both the
// functionCall and its matching functionResponse leave together (no orphan),
// the survivors still alternate with a leading user turn, and the leading user
// prompt + the live tail (final answer + new prompt) are preserved.
func TestRunGoogle_DropsRoundTripsPairwise(t *testing.T) {
	body := sevenTurnBody(t)
	got := geminiDropPipeline(0.1, 1).Run("google", body)
	if got.Skipped {
		t.Fatalf("expected drop pass to fire, got skipped")
	}
	if !json.Valid(got.Body) {
		t.Fatalf("dropped body is not valid JSON (would 400): %s", got.Body)
	}
	if got.DroppedCount == 0 {
		t.Fatalf("expected DroppedCount > 0, got events=%+v", got.Events)
	}
	s := string(got.Body)
	// Both sides of every dropped round-trip must vanish together. alpha and
	// beta were the oldest two round-trips; under ratio 0.1 both drop.
	if strings.Contains(s, "alpha") {
		t.Errorf("functionCall/functionResponse 'alpha' should be fully dropped (orphan risk): %s", s)
	}
	if strings.Contains(s, "beta") {
		t.Errorf("functionCall/functionResponse 'beta' should be fully dropped (orphan risk): %s", s)
	}
	// Leading prompt + live tail survive.
	if !strings.Contains(s, "investigate the repo") {
		t.Errorf("leading user prompt was dropped: %s", s)
	}
	if !strings.Contains(s, "the final answer here") {
		t.Errorf("final model answer was dropped: %s", s)
	}
	if !strings.Contains(s, "now do the follow up question") {
		t.Errorf("live tail user prompt was dropped: %s", s)
	}
	assertGeminiAlternating(t, geminiResultRoles(t, got.Body))
}

// TestRunGoogle_RefusesOrphanOnMismatch pins that a model-call whose following
// user-turn responds to a DIFFERENT name is NOT a droppable round-trip:
// dropping it would orphan a call or a response. With nothing else to do the
// pass no-ops and the body is forwarded byte-for-byte.
func TestRunGoogle_RefusesOrphanOnMismatch(t *testing.T) {
	big := strings.Repeat("plain english output that does not compress as json. ", 40)
	body := helperGeminiBody(t, []map[string]any{
		{"role": "user", "parts": []any{map[string]any{"text": "go"}}},
		{"role": "model", "parts": []any{map[string]any{"functionCall": map[string]any{"name": "alpha", "args": map[string]any{}}}}},
		// response name DIFFERS from the call → not a self-contained pair.
		{"role": "user", "parts": []any{map[string]any{"functionResponse": map[string]any{"name": "MISMATCH", "response": map[string]any{"output": big}}}}},
		{"role": "model", "parts": []any{map[string]any{"text": "answer"}}},
		{"role": "user", "parts": []any{map[string]any{"text": "next"}}},
	}, nil)

	got := geminiDropPipeline(0.1, 1).Run("google", body)
	if got.Skipped {
		t.Fatalf("unexpected skip; want byte-stable no-op")
	}
	if got.DroppedCount != 0 {
		t.Fatalf("must NOT drop a mismatched (orphan-risk) pair, dropped=%d", got.DroppedCount)
	}
	if string(got.Body) != string(body) {
		t.Errorf("expected byte-stable forward when nothing safely droppable")
	}
}

// TestRunGoogle_KeepsModelTurnCarryingText pins that a model turn mixing a
// functionCall with prose/thinking text is NOT treated as a pure tool-call
// turn — dropping it could discard model output we must keep — so the round-
// trip is refused even though the response name matches.
func TestRunGoogle_KeepsModelTurnCarryingText(t *testing.T) {
	big := strings.Repeat("plain english output that does not compress as json. ", 40)
	body := helperGeminiBody(t, []map[string]any{
		{"role": "user", "parts": []any{map[string]any{"text": "go"}}},
		{"role": "model", "parts": []any{
			map[string]any{"functionCall": map[string]any{"name": "alpha", "args": map[string]any{}}},
			map[string]any{"text": "let me check that for you"},
		}},
		{"role": "user", "parts": []any{map[string]any{"functionResponse": map[string]any{"name": "alpha", "response": map[string]any{"output": big}}}}},
		{"role": "model", "parts": []any{map[string]any{"text": "answer"}}},
		{"role": "user", "parts": []any{map[string]any{"text": "next"}}},
	}, nil)

	got := geminiDropPipeline(0.1, 1).Run("google", body)
	if got.DroppedCount != 0 {
		t.Fatalf("must keep a model turn carrying text, dropped=%d", got.DroppedCount)
	}
	if string(got.Body) != string(body) {
		t.Errorf("expected byte-stable forward; model-with-text round-trip must be kept")
	}
}

// TestRunGoogle_PreserveLastNProtectsTail pins that PreserveLastN keeps the
// most-recent round-trips even when over budget: with PreserveLastN large
// enough to cover both round-trips, nothing is dropped.
func TestRunGoogle_PreserveLastNProtectsTail(t *testing.T) {
	body := sevenTurnBody(t)
	// 7 contents; PreserveLastN=6 protects everything except the leading
	// user(0), which is never a pair start → no droppable pair survives.
	got := geminiDropPipeline(0.1, 6).Run("google", body)
	if got.DroppedCount != 0 {
		t.Fatalf("PreserveLastN must protect the tail round-trips, dropped=%d", got.DroppedCount)
	}
	if string(got.Body) != string(body) {
		t.Errorf("expected byte-stable forward when the tail is fully preserved")
	}
}

// TestRunGoogle_UnderTargetDoesNotDrop pins that a body already under target is
// never touched by the drop pass (byte-stable no-op), even with droppable
// round-trips present.
func TestRunGoogle_UnderTargetDoesNotDrop(t *testing.T) {
	body := sevenTurnBody(t)
	got := geminiDropPipeline(0.99, 1).Run("google", body)
	if got.DroppedCount != 0 {
		t.Fatalf("under-target body must not drop, dropped=%d", got.DroppedCount)
	}
	if string(got.Body) != string(body) {
		t.Errorf("expected byte-stable forward when already under target")
	}
}

// TestEnforceGeminiBudget_AlternationBackstop directly exercises the final
// validation: geminiAlternationValid rejects a surviving sequence that does
// not alternate or does not lead with user.
func TestGeminiAlternationValid_Backstop(t *testing.T) {
	cases := []struct {
		name    string
		roles   []string
		dropped []bool
		want    bool
	}{
		{"clean alternation", []string{"user", "model", "user"}, []bool{false, false, false}, true},
		{"leading model invalid", []string{"model", "user"}, []bool{false, false}, false},
		{"single-drop collapses to user,user", []string{"user", "model", "user"}, []bool{false, true, false}, false},
		{"pair-drop stays valid", []string{"user", "model", "user", "model"}, []bool{false, true, true, false}, true},
		{"all dropped is empty -> invalid", []string{"user", "model"}, []bool{true, true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			contents := make([]geminiContent, len(tc.roles))
			for i := range tc.roles {
				contents[i] = geminiContent{role: tc.roles[i], dropped: tc.dropped[i]}
			}
			if got := geminiAlternationValid(contents); got != tc.want {
				t.Errorf("geminiAlternationValid=%v want %v", got, tc.want)
			}
		})
	}
}

// TestGeminiIsRoundTrip pins the pair-eligibility predicate that gates every
// drop: only a pure model-call + pure user-response with exactly-matching
// names qualifies.
func TestGeminiIsRoundTrip(t *testing.T) {
	mkContent := func(role string, parts ...map[string]any) geminiContent {
		gc := geminiContent{role: role}
		for _, p := range parts {
			raw, _ := json.Marshal(p)
			gc.parts = append(gc.parts, geminiPart{raw: raw})
		}
		return gc
	}
	call := func(name string) map[string]any {
		return map[string]any{"functionCall": map[string]any{"name": name}}
	}
	resp := func(name string) map[string]any {
		return map[string]any{"functionResponse": map[string]any{"name": name, "response": map[string]any{}}}
	}
	text := map[string]any{"text": "hi"}

	cases := []struct {
		name        string
		model, user geminiContent
		want        bool
	}{
		{"single match", mkContent("model", call("a")), mkContent("user", resp("a")), true},
		{"parallel match", mkContent("model", call("a"), call("b")), mkContent("user", resp("a"), resp("b")), true},
		{"name mismatch", mkContent("model", call("a")), mkContent("user", resp("b")), false},
		{"count mismatch", mkContent("model", call("a"), call("a")), mkContent("user", resp("a")), false},
		{"model carries text", mkContent("model", call("a"), text), mkContent("user", resp("a")), false},
		{"user carries text", mkContent("model", call("a")), mkContent("user", resp("a"), text), false},
		{"wrong roles", mkContent("user", call("a")), mkContent("model", resp("a")), false},
		{"no calls", mkContent("model", text), mkContent("user", resp("a")), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, u := tc.model, tc.user
			if got := geminiIsRoundTrip(&m, &u); got != tc.want {
				t.Errorf("geminiIsRoundTrip=%v want %v", got, tc.want)
			}
		})
	}
}
