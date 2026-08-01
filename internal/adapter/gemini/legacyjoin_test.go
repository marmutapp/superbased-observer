package gemini

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/adapter"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/scrub"
)

// writeLegacySession writes a legacy single-object session JSON under a
// throwaway watch root and returns its path.
func writeLegacySession(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, ".gemini", "tmp", "abcd", "chats", "session-2026-07-31T10-00-legacy.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// legacyFailedInBatch: the functionCall and its failing functionResponse
// are in the SAME session file, so joinResponse finds the row in memory.
const legacyFailedInBatch = `{
  "sessionId": "legacy-fail-1",
  "projectHash": "abcd",
  "startTime": "2026-07-31T10:00:00.000Z",
  "messages": [
    {"id":"u1","role":"user","timestamp":"2026-07-31T10:00:01.000Z","cwd":"/tmp/g","content":[{"type":"text","text":"read it"}]},
    {"id":"m1","role":"gemini","timestamp":"2026-07-31T10:00:02.000Z","model":"gemini-2.5-pro","content":[
      {"type":"functionCall","functionCall":{"id":"call-1","name":"read_file","args":{"absolute_path":"/tmp/g/missing.txt"}}}
    ]},
    {"id":"t1","role":"tool","timestamp":"2026-07-31T10:00:03.000Z","content":[
      {"type":"functionResponse","functionResponse":{"id":"call-1","name":"read_file","response":{"error":"File not found: /tmp/g/missing.txt"}}}
    ]}
  ]
}`

// legacySucceededInBatch is the same shape with a response carrying NO
// error key — the invention-refusal control.
const legacySucceededInBatch = `{
  "sessionId": "legacy-ok-1",
  "projectHash": "abcd",
  "startTime": "2026-07-31T10:00:00.000Z",
  "messages": [
    {"id":"u1","role":"user","timestamp":"2026-07-31T10:00:01.000Z","cwd":"/tmp/g","content":[{"type":"text","text":"read it"}]},
    {"id":"m1","role":"gemini","timestamp":"2026-07-31T10:00:02.000Z","model":"gemini-2.5-pro","content":[
      {"type":"functionCall","functionCall":{"id":"call-1","name":"read_file","args":{"absolute_path":"/tmp/g/there.txt"}}}
    ]},
    {"id":"t1","role":"tool","timestamp":"2026-07-31T10:00:03.000Z","content":[
      {"type":"functionResponse","functionResponse":{"id":"call-1","name":"read_file","response":{"output":"hello"}}}
    ]}
  ]
}`

// TestLegacyJoin_FailedCallInBatch pins the in-batch half of the B3
// fold-in: a legacy shape carries NO status anywhere, so the `error` key
// inside the functionResponse is the only failure signal. Pre-fix the
// row stayed optimistically successful forever.
func TestLegacyJoin_FailedCallInBatch(t *testing.T) {
	path := writeLegacySession(t, legacyFailedInBatch)
	res, err := New().ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	var call *models.ToolEvent
	for i := range res.ToolEvents {
		if res.ToolEvents[i].SourceEventID == "call-1" {
			call = &res.ToolEvents[i]
		}
	}
	if call == nil {
		t.Fatalf("no row for call-1; events=%v", summary(res.ToolEvents))
	}
	if call.Success {
		t.Errorf("Success = true, want false (the response reported an error)")
	}
	if call.ErrorMessage == "" {
		t.Fatalf("ErrorMessage empty — the store's evidence-gated success 1→0 self-heal can never fire without it")
	}
	if want := "File not found: /tmp/g/missing.txt"; call.ErrorMessage != want {
		t.Errorf("ErrorMessage = %q, want %q", call.ErrorMessage, want)
	}
	// The output join is unaffected — both facts land.
	if call.ToolOutput == "" {
		t.Errorf("ToolOutput empty — the error path must not swallow the output join")
	}
}

// TestLegacyJoin_SucceededCallInBatch pins the invention-refusal
// control: a response with no `error` key must leave the row exactly as
// the call emitted it.
func TestLegacyJoin_SucceededCallInBatch(t *testing.T) {
	path := writeLegacySession(t, legacySucceededInBatch)
	res, err := New().ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	for _, ev := range res.ToolEvents {
		if ev.SourceEventID != "call-1" {
			continue
		}
		if !ev.Success {
			t.Errorf("Success = false, want true (nothing reported a failure)")
		}
		if ev.ErrorMessage != "" {
			t.Errorf("ErrorMessage = %q, want empty (invented verdict)", ev.ErrorMessage)
		}
		return
	}
	t.Fatalf("no row for call-1; events=%v", summary(res.ToolEvents))
}

// writeLegacyJSONL writes a JSONL session whose functionCall and
// functionResponse are separate RECORDS, and returns the path plus the
// byte offset that splits them — so a parse can start after the call
// exactly the way an incremental poll tick does.
func writeLegacyJSONL(t *testing.T, errorKey bool) (string, int64) {
	t.Helper()
	header := `{"sessionId":"legacy-xbatch","projectHash":"abcd","startTime":"2026-07-31T10:00:00.000Z","kind":"main"}` + "\n"
	call := `{"id":"m1","type":"gemini","timestamp":"2026-07-31T10:00:02.000Z","model":"gemini-2.5-pro","content":[{"type":"functionCall","functionCall":{"id":"call-1","name":"read_file","args":{"absolute_path":"/tmp/g/missing.txt"}}}]}` + "\n"
	resp := `{"id":"t1","type":"tool","timestamp":"2026-07-31T10:00:03.000Z","content":[{"type":"functionResponse","functionResponse":{"id":"call-1","name":"read_file","response":{"error":"File not found: /tmp/g/missing.txt"}}}]}` + "\n"
	if !errorKey {
		resp = `{"id":"t1","type":"tool","timestamp":"2026-07-31T10:00:03.000Z","content":[{"type":"functionResponse","functionResponse":{"id":"call-1","name":"read_file","response":{"output":"hello"}}}]}` + "\n"
	}
	dir := t.TempDir()
	path := filepath.Join(dir, ".gemini", "tmp", "abcd", "chats", "session-2026-07-31T10-00-xbatch.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(header+call+resp), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path, int64(len(header) + len(call))
}

// TestLegacyJoin_FailedCallCrossBatch pins the cross-batch half: the
// call landed in an EARLIER parse window (the offset starts after its
// record), so no in-memory row matches and the verdict has to ride the
// ActionOutcomeUpdate. store.UpdateActionOutcome moves success only
// under SuccessKnown and error_message only when non-empty, so both
// must be set here or the update is a no-op on the outcome columns.
func TestLegacyJoin_FailedCallCrossBatch(t *testing.T) {
	path, split := writeLegacyJSONL(t, true)
	res, err := New().ParseSessionFile(context.Background(), path, split)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	for _, ev := range res.ToolEvents {
		if ev.SourceEventID == "call-1" {
			t.Fatalf("the call row is supposed to be OUTSIDE this parse window: %+v", ev)
		}
	}
	if len(res.OutcomeUpdates) != 1 {
		t.Fatalf("OutcomeUpdates = %d, want 1: %+v", len(res.OutcomeUpdates), res.OutcomeUpdates)
	}
	u := res.OutcomeUpdates[0]
	if u.SourceEventID != "call-1" {
		t.Errorf("SourceEventID = %q, want call-1", u.SourceEventID)
	}
	if !u.SuccessKnown {
		t.Errorf("SuccessKnown = false — store.UpdateActionOutcome would leave success untouched")
	}
	if u.Success {
		t.Errorf("Success = true, want false")
	}
	if u.ErrorMessage != "File not found: /tmp/g/missing.txt" {
		t.Errorf("ErrorMessage = %q", u.ErrorMessage)
	}
	if u.ToolOutput == "" {
		t.Errorf("ToolOutput empty — the pre-existing cross-batch output rescue must survive")
	}
}

// TestLegacyJoin_SucceededCallCrossBatch pins the cross-batch
// invention-refusal control: no error key ⇒ SuccessKnown stays false,
// so the already-persisted success column is left exactly as it is.
func TestLegacyJoin_SucceededCallCrossBatch(t *testing.T) {
	path, split := writeLegacyJSONL(t, false)
	res, err := New().ParseSessionFile(context.Background(), path, split)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.OutcomeUpdates) != 1 {
		t.Fatalf("OutcomeUpdates = %d, want 1: %+v", len(res.OutcomeUpdates), res.OutcomeUpdates)
	}
	u := res.OutcomeUpdates[0]
	if u.SuccessKnown {
		t.Errorf("SuccessKnown = true on a response that reported no verdict")
	}
	if u.ErrorMessage != "" {
		t.Errorf("ErrorMessage = %q, want empty", u.ErrorMessage)
	}
	if u.ToolOutput == "" {
		t.Errorf("ToolOutput empty — the cross-batch output rescue regressed")
	}
}

// TestLegacyJoin_ErrorAppliesEvenWhenOutputAlreadyPresent pins that the
// failure verdict does NOT ride the `ToolOutput != ""` early return:
// output richness and outcome evidence are independent facts.
func TestLegacyJoin_ErrorAppliesEvenWhenOutputAlreadyPresent(t *testing.T) {
	res := &adapter.ParseResult{}
	res.ToolEvents = append(res.ToolEvents, models.ToolEvent{
		SourceEventID: "call-1",
		Success:       true,
		ToolOutput:    "already-rich-output",
	})
	joinResponse(res, "/tmp/session.json", &legacyFnResp{
		ID:       "call-1",
		Name:     "read_file",
		Response: map[string]any{"error": "boom"},
	}, scrub.New())
	ev := res.ToolEvents[0]
	if ev.Success {
		t.Errorf("Success = true; the early return swallowed the failure verdict")
	}
	if ev.ErrorMessage != "boom" {
		t.Errorf("ErrorMessage = %q, want boom", ev.ErrorMessage)
	}
	if ev.ToolOutput != "already-rich-output" {
		t.Errorf("ToolOutput = %q — the richer embedded output must not be overwritten", ev.ToolOutput)
	}
}

// TestResponseErrorText pins the shared helper both branches read, with
// the "meaningful" flag separate from the string. The first three rows
// are the shapes liveToolCall.errorText handled before the extraction —
// its behaviour is unchanged by construction.
func TestResponseErrorText(t *testing.T) {
	cases := []struct {
		name       string
		resp       map[string]any
		wantText   string
		wantIsFail bool
	}{
		{"no body", nil, "", false},
		{"empty body", map[string]any{}, "", false},
		{"no error key", map[string]any{"output": "fine"}, "", false},
		{"explicit null", map[string]any{"error": nil}, "", false},
		{"blank error string", map[string]any{"error": "   "}, "", false},
		{"string error", map[string]any{"error": "File not found"}, "File not found", true},
		{"structured error", map[string]any{"error": map[string]any{"code": "ENOENT"}}, `{"code":"ENOENT"}`, true},
		// The real legacy failure shape must keep working.
		{"structured error with status", map[string]any{"error": map[string]any{"code": float64(403), "message": "denied"}}, `{"code":403,"message":"denied"}`, true},
		{"non-empty array error", map[string]any{"error": []any{"a"}}, `["a"]`, true},
		// FALSEY / CONTENT-FREE values are NEVER a failure report. Each
		// of these used to render as an error string ("false", "0",
		// "[]", "{}") and file success=0 the store then honoured
		// forever (codex round finding F4).
		{"false", map[string]any{"error": false}, "", false},
		{"zero", map[string]any{"error": float64(0)}, "", false},
		{"empty array", map[string]any{"error": []any{}}, "", false},
		{"empty object", map[string]any{"error": map[string]any{}}, "", false},
		// A bare true/number reports no reason at all — filing it as a
		// measured failure would be the invention SuccessKnown exists to
		// prevent.
		{"bare true", map[string]any{"error": true}, "", false},
		{"bare number", map[string]any{"error": float64(42)}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := responseErrorText(tc.resp)
			if got != tc.wantText || ok != tc.wantIsFail {
				t.Errorf("responseErrorText = (%q, %v), want (%q, %v)", got, ok, tc.wantText, tc.wantIsFail)
			}
		})
	}
}

// TestLiveShapeUnaffectedByLegacyErrorJoin is the byte-identity gate for
// the LIVE shape. The live CLI embeds every result on the call itself
// (17/17 calls across the live corpus) and writes no functionResponse
// record for them, so joinResponse never runs and nothing about the live
// parse may move: zero outcome updates, every call's verdict still
// sourced from its own `status`.
func TestLiveShapeUnaffectedByLegacyErrorJoin(t *testing.T) {
	dst := writeFixture(t, "gemini/chats/session-live.jsonl", "../../../testdata/gemini/session-live-toolcalls.jsonl")
	res, err := New().ParseSessionFile(context.Background(), dst, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.OutcomeUpdates) != 0 {
		t.Errorf("live shape produced %d OutcomeUpdates, want 0: %+v", len(res.OutcomeUpdates), res.OutcomeUpdates)
	}
	for _, ev := range res.ToolEvents {
		if ev.ActionType == models.ActionUserPrompt || ev.RawToolName == "gemini.assistant_text" {
			continue
		}
		if !ev.Success || ev.ErrorMessage != "" {
			t.Errorf("live call %q: Success=%v ErrorMessage=%q — the legacy error join leaked into the live shape",
				ev.SourceEventID, ev.Success, ev.ErrorMessage)
		}
	}
}

// TestLegacyJoin_FalseyErrorLeavesVerdictUntouched pins finding F4 at
// the JOIN level, not just in the helper: a response body whose `error`
// key holds a falsey/content-free value must leave the row exactly as
// the call emitted it on BOTH branches.
func TestLegacyJoin_FalseyErrorLeavesVerdictUntouched(t *testing.T) {
	for _, falsey := range []any{false, float64(0), []any{}, map[string]any{}, true, float64(42)} {
		// in-batch
		res := &adapter.ParseResult{}
		res.ToolEvents = append(res.ToolEvents, models.ToolEvent{SourceEventID: "call-1", Success: true})
		joinResponse(res, "/tmp/s.json", &legacyFnResp{
			ID: "call-1", Name: "read_file",
			Response: map[string]any{"error": falsey, "output": "fine"},
		}, scrub.New())
		if !res.ToolEvents[0].Success || res.ToolEvents[0].ErrorMessage != "" {
			t.Errorf("in-batch %#v: Success=%v ErrorMessage=%q — invented a failure",
				falsey, res.ToolEvents[0].Success, res.ToolEvents[0].ErrorMessage)
		}
		// cross-batch
		out := &adapter.ParseResult{}
		joinResponse(out, "/tmp/s.json", &legacyFnResp{
			ID: "call-1", Name: "read_file",
			Response: map[string]any{"error": falsey, "output": "fine"},
		}, scrub.New())
		if len(out.OutcomeUpdates) != 1 {
			t.Fatalf("cross-batch %#v: OutcomeUpdates=%d", falsey, len(out.OutcomeUpdates))
		}
		if out.OutcomeUpdates[0].SuccessKnown || out.OutcomeUpdates[0].ErrorMessage != "" {
			t.Errorf("cross-batch %#v: SuccessKnown=%v ErrorMessage=%q — invented a verdict",
				falsey, out.OutcomeUpdates[0].SuccessKnown, out.OutcomeUpdates[0].ErrorMessage)
		}
	}
}

// TestLegacyJoin_StructuredErrorStillFiles is the counter-pin: the real
// legacy failure shape must survive the F4 tightening.
func TestLegacyJoin_StructuredErrorStillFiles(t *testing.T) {
	res := &adapter.ParseResult{}
	res.ToolEvents = append(res.ToolEvents, models.ToolEvent{SourceEventID: "call-1", Success: true})
	joinResponse(res, "/tmp/s.json", &legacyFnResp{
		ID: "call-1", Name: "read_file",
		Response: map[string]any{"error": map[string]any{"code": float64(403), "message": "permission denied"}},
	}, scrub.New())
	ev := res.ToolEvents[0]
	if ev.Success {
		t.Errorf("Success = true on a structured error body")
	}
	if ev.ErrorMessage != `{"code":403,"message":"permission denied"}` {
		t.Errorf("ErrorMessage = %q", ev.ErrorMessage)
	}
}
