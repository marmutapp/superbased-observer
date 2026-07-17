package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// TestGetOutputComposition_Captured exercises the tool end-to-end over the
// JSON-RPC path: an assistant turn with a fenced go block + file writes + a
// shell command rolls up to the expected code/explain split.
func TestGetOutputComposition_Captured(t *testing.T) {
	s, database, _ := testServer(t)
	st := store.New(database)
	ctx := context.Background()

	pid, _ := st.UpsertProject(ctx, "/tmp/oc", "")
	if err := st.UpsertSession(ctx, models.Session{
		ID: "oc", ProjectID: pid, Tool: models.ToolClaudeCode, Model: "opus", StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	mk := func(eid, atype, rawName, target, body string, cb int64) models.Action {
		return models.Action{
			SessionID: "oc", ProjectID: pid, Timestamp: time.Now().UTC(),
			ActionType: atype, Target: target, Success: true,
			Tool: models.ToolClaudeCode, RawToolName: rawName, RawToolOutput: body,
			ContentBytes: cb, SourceFile: "f.jsonl", SourceEventID: eid,
		}
	}
	assistantBody := "Here is the fix.\n```go\nfunc main() {}\n```\nThat should do it."
	if _, err := st.InsertActions(ctx, []models.Action{
		mk("e1", models.ActionTaskComplete, "claudecode.assistant_text", "preview", assistantBody, 0),
		mk("e2", models.ActionWriteFile, "Write", "internal/x/foo.go", "", 1200), // go code
		mk("e3", models.ActionWriteFile, "Write", "README.md", "", 500),          // docs (not code)
		mk("e4", models.ActionRunCommand, "Bash", "go test ./...", "", 42),       // shell = code
	}); err != nil {
		t.Fatal(err)
	}

	out := callTool(t, s, "get_output_composition", map[string]any{"session_id": "oc"})

	if out["session_id"] != "oc" {
		t.Errorf("session_id = %v, want oc", out["session_id"])
	}
	if got := out["authored_captured"]; got != true {
		t.Errorf("authored_captured = %v, want true", got)
	}
	// CodeBytes = go(1200) + bash(42) + the fenced go block. README is docs.
	if got := int64(out["code_bytes"].(float64)); got <= 1242 {
		t.Errorf("code_bytes = %d, want > 1242 (go+bash+fenced go)", got)
	}
	if got := int64(out["explain_bytes"].(float64)); got == 0 {
		t.Error("explain_bytes = 0, want > 0 (narrative)")
	}
	if out["code_explain_ratio"] == nil {
		t.Error("code_explain_ratio missing (explanation > 0)")
	}

	ch := out["channels"].(map[string]any)
	if got := int64(ch["written_bytes"].(float64)); got != 1700 {
		t.Errorf("channels.written_bytes = %d, want 1700 (go 1200 + markdown 500)", got)
	}
	if got := int64(ch["command_bytes"].(float64)); got != 42 {
		t.Errorf("channels.command_bytes = %d, want 42", got)
	}

	byCat := out["by_category"].(map[string]any)
	if got := int64(byCat["docs"].(float64)); got != 500 {
		t.Errorf("by_category.docs = %d, want 500 (README write)", got)
	}

	langs := map[string]int64{}
	for _, raw := range out["code_by_language"].([]any) {
		lb := raw.(map[string]any)
		langs[lb["language"].(string)] = int64(lb["bytes"].(float64))
	}
	if langs["go"] < 1200 {
		t.Errorf("code_by_language go = %d, want >= 1200", langs["go"])
	}
	if langs["bash"] != 42 {
		t.Errorf("code_by_language bash = %d, want 42", langs["bash"])
	}
}

// TestGetOutputComposition_NotCaptured: a session whose write action carries no
// content_bytes (pre-feature / daemon not on the build) reports
// authored_captured=false so the caller can prompt `observer backfill`.
func TestGetOutputComposition_NotCaptured(t *testing.T) {
	s, database, _ := testServer(t)
	st := store.New(database)
	ctx := context.Background()

	pid, _ := st.UpsertProject(ctx, "/tmp/ocn", "")
	if err := st.UpsertSession(ctx, models.Session{
		ID: "ocn", ProjectID: pid, Tool: models.ToolClaudeCode, StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertActions(ctx, []models.Action{{
		SessionID: "ocn", ProjectID: pid, Timestamp: time.Now().UTC(),
		ActionType: models.ActionWriteFile, Target: "main.go", Success: true,
		Tool: models.ToolClaudeCode, RawToolName: "Write",
		ContentBytes: 0, SourceFile: "f.jsonl", SourceEventID: "e1",
	}}); err != nil {
		t.Fatal(err)
	}

	out := callTool(t, s, "get_output_composition", map[string]any{"session_id": "ocn"})
	if got := out["authored_captured"]; got != false {
		t.Errorf("authored_captured = %v, want false (write present but unmeasured)", got)
	}
}

// TestGetOutputComposition_MissingSessionID: the tool rejects a call with no
// session_id rather than rolling up the whole DB.
func TestGetOutputComposition_MissingSessionID(t *testing.T) {
	s, _, _ := testServer(t)
	msg := callToolExpectError(t, s, "get_output_composition", map[string]any{})
	if msg == "" {
		t.Error("expected an error message for missing session_id")
	}
}
