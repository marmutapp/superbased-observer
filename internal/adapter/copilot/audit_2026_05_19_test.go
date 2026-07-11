package copilot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeModernSession writes a synthetic modern Copilot chatSessions file
// under a fake workspaceStorage tree so projectRootFromPath has something
// to walk over. Returns the full path to the audit.jsonl-equivalent file.
func writeModernSession(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "workspaceStorage", "ws-audit", "chatSessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "session-audit.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// TestParseModern_CapiResolvedModelFallsBackToModelId pins the v1.6.16
// audit F1 fix — when result.metadata.resolvedModel is a `capi-*`
// internal routing identifier, the emitted Model must be modelId
// (`copilot/auto` in the snapshot) rather than the unpriced capi-*
// string. Two turns: turn 1's resolvedModel is the public
// `grok-code-fast-1` (preserved); turn 2's is
// `capi-cus-ptuc-h100-oswe-vscode-prime` (replaced by modelId).
func TestParseModern_CapiResolvedModelFallsBackToModelId(t *testing.T) {
	// One kind=0 snapshot, no patches. Two requests inline.
	// NOTE: Copilot writes one JSON object per line; the bufio.Scanner
	// in parseModern reads line-by-line, so the snapshot must fit on
	// a single line (no embedded newlines).
	snapshot := `{"kind":0,"v":{"requests":[` +
		`{"requestId":"req-public","timestamp":1747600000000,"modelId":"copilot/auto",` +
		`"message":{"text":"hello"},` +
		`"result":{"metadata":{"resolvedModel":"grok-code-fast-1","promptTokens":100,"toolCallRounds":[]}},` +
		`"completionTokens":50,"isComplete":true,"elapsedMs":1000},` +
		`{"requestId":"req-capi","timestamp":1747600060000,"modelId":"copilot/auto",` +
		`"message":{"text":"second turn"},` +
		`"result":{"metadata":{"resolvedModel":"capi-cus-ptuc-h100-oswe-vscode-prime","promptTokens":200,"toolCallRounds":[]}},` +
		`"completionTokens":75,"isComplete":true,"elapsedMs":2000}` +
		`]}}`
	path := writeModernSession(t, snapshot+"\n")
	res, err := New().ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	models := map[string]string{}
	for _, ev := range res.ToolEvents {
		if i := strings.Index(ev.SourceEventID, ":"); i > 0 {
			models[ev.SourceEventID[:i]] = ev.Model
		}
	}
	if got := models["req-public"]; got != "grok-code-fast-1" {
		t.Errorf("public resolvedModel: got %q want grok-code-fast-1", got)
	}
	if got := models["req-capi"]; got != "copilot/auto" {
		t.Errorf("capi-* resolvedModel: got %q want copilot/auto (F1 fallback)", got)
	}
	// Token rows must reflect the same model attribution.
	tokenModels := map[string]string{}
	for _, te := range res.TokenEvents {
		if i := strings.Index(te.SourceEventID, ":"); i > 0 {
			tokenModels[te.SourceEventID[:i]] = te.Model
		}
	}
	if got := tokenModels["req-public"]; got != "grok-code-fast-1" {
		t.Errorf("public token model: got %q", got)
	}
	if got := tokenModels["req-capi"]; got != "copilot/auto" {
		t.Errorf("capi-* token model: got %q want copilot/auto", got)
	}
}

// TestParseModern_ThinkingTextStampedOnToolRows pins the v1.6.16 audit
// F2 fix — `response[*].kind="thinking"` entries hold reasoning prose
// that should land on each subsequent tool-call row's
// PrecedingReasoning field. Pairs the first thinking entry with the
// first round of tool calls; subsequent rounds use later thinking
// entries (sticking at the last entry if rounds exceed thinking blocks).
// Also pins that thinking text does NOT leak into the task_complete
// row's ToolOutput (which should hold only the assistant prose).
func TestParseModern_ThinkingTextStampedOnToolRows(t *testing.T) {
	snapshot := `{"kind":0,"v":{"requests":[` +
		`{"requestId":"req-thinking","timestamp":1747600000000,"modelId":"copilot/auto",` +
		`"message":{"text":"do the thing"},` +
		`"result":{"metadata":{` +
		`"resolvedModel":"claude-haiku-4-5-20251001",` +
		`"promptTokens":500,` +
		`"toolCallRounds":[` +
		`{"id":"round-0","timestamp":1747600001000,"toolCalls":[{"id":"call-aaa","name":"read_file","arguments":"{\"path\":\"/a/b.go\"}"}]},` +
		`{"id":"round-1","timestamp":1747600002000,"toolCalls":[{"id":"call-bbb","name":"list_dir","arguments":"{\"path\":\"/a\"}"}]}` +
		`]}},` +
		`"response":[` +
		`{"kind":"thinking","value":"REASONING_BEFORE_ROUND_ZERO"},` +
		`{"kind":"toolInvocationSerialized","toolId":"call-aaa"},` +
		`{"kind":"thinking","value":"REASONING_BEFORE_ROUND_ONE"},` +
		`{"kind":"toolInvocationSerialized","toolId":"call-bbb"},` +
		`{"value":"VISIBLE_ASSISTANT_PROSE"}` +
		`],` +
		`"completionTokens":100,"isComplete":true,"elapsedMs":3000}` +
		`]}}`
	path := writeModernSession(t, snapshot+"\n")
	res, err := New().ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Locate the two tool rows + the task_complete row.
	var toolRows []string
	var completeText string
	for _, ev := range res.ToolEvents {
		switch ev.RawToolName {
		case "read_file", "list_dir":
			toolRows = append(toolRows, ev.PrecedingReasoning)
		case "agent_response":
			completeText = ev.ToolOutput
		}
	}
	if len(toolRows) != 2 {
		t.Fatalf("tool rows: got %d want 2", len(toolRows))
	}
	if !strings.Contains(toolRows[0], "REASONING_BEFORE_ROUND_ZERO") {
		t.Errorf("tool row 0 PrecedingReasoning: got %q want substring REASONING_BEFORE_ROUND_ZERO", toolRows[0])
	}
	if !strings.Contains(toolRows[1], "REASONING_BEFORE_ROUND_ONE") {
		t.Errorf("tool row 1 PrecedingReasoning: got %q want substring REASONING_BEFORE_ROUND_ONE", toolRows[1])
	}
	if !strings.Contains(completeText, "VISIBLE_ASSISTANT_PROSE") {
		t.Errorf("task_complete output missing assistant prose: %q", completeText)
	}
	if strings.Contains(completeText, "REASONING_BEFORE_ROUND") {
		t.Errorf("task_complete output should NOT contain thinking text (F2): %q", completeText)
	}
}

// TestParseModern_NoThinkingTextWhenAbsent verifies the F2 helper
// gracefully handles requests without response[].kind="thinking"
// entries — the tool rows just get empty PrecedingReasoning.
func TestParseModern_NoThinkingTextWhenAbsent(t *testing.T) {
	snapshot := `{"kind":0,"v":{"requests":[` +
		`{"requestId":"req-nothink","timestamp":1747600000000,"modelId":"copilot/auto",` +
		`"message":{"text":"go"},` +
		`"result":{"metadata":{` +
		`"resolvedModel":"claude-haiku-4-5-20251001","promptTokens":50,` +
		`"toolCallRounds":[{"id":"r0","timestamp":1747600001000,"toolCalls":[{"id":"c0","name":"read_file","arguments":"{\"path\":\"/x.go\"}"}]}]` +
		`}},` +
		`"response":[{"value":"done"}],` +
		`"completionTokens":10,"isComplete":true,"elapsedMs":1000}` +
		`]}}`
	path := writeModernSession(t, snapshot+"\n")
	res, err := New().ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, ev := range res.ToolEvents {
		if ev.RawToolName == "read_file" && ev.PrecedingReasoning != "" {
			t.Errorf("tool row PrecedingReasoning should be empty when no thinking blocks present, got %q", ev.PrecedingReasoning)
		}
	}
}
