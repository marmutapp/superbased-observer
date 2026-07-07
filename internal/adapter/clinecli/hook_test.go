package clinecli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/scrub"
)

// TestParseHooksJSONL_FixtureCorpus exercises the JSONL tailer
// against testdata/clinecli/hooks-jsonl-sample.jsonl (the synthesised
// 9-event-type sample from plan §6).
func TestParseHooksJSONL_FixtureCorpus(t *testing.T) {
	t.Parallel()
	path := filepath.Join("..", "..", "..", "testdata", "clinecli", "hooks-jsonl-sample.jsonl")
	tools, tokens, newOffset, warnings, err := parseHooksJSONL(context.Background(), path, 0, scrub.New())
	if err != nil {
		t.Fatalf("parseHooksJSONL: %v", err)
	}
	if len(warnings) > 0 {
		t.Logf("warnings: %v", warnings)
	}
	if newOffset == 0 {
		t.Error("newOffset = 0; want > 0 (bytes consumed)")
	}

	// Count by ActionType.
	byType := map[string]int{}
	rawNames := map[string]int{}
	for _, ev := range tools {
		byType[ev.ActionType]++
		rawNames[ev.RawToolName]++
	}

	// Expected from the 9-event sample:
	// - agent_start         → session_start (Target=cli)
	// - agent_resume        → session_start (Target=resume)
	// - prompt_submit       → user_prompt
	// - tool_call           → read_file (read_files normalised)
	// - tool_result         → NO emit (lookup-and-fill the prior row)
	// - agent_end           → session_end
	// - agent_abort         → turn_aborted
	// - agent_error         → api_error
	// - session_shutdown    → session_end
	wants := map[string]int{
		models.ActionSessionStart: 2, // agent_start + agent_resume
		models.ActionUserPrompt:   1,
		models.ActionReadFile:     1,
		models.ActionSessionEnd:   2, // agent_end + session_shutdown
		models.ActionTurnAborted:  1,
		models.ActionAPIError:     1,
	}
	for k, want := range wants {
		if byType[k] != want {
			t.Errorf("byType[%s] = %d; want %d", k, byType[k], want)
		}
	}

	// Token events: hook-path TokenEvents not emitted today (the SQLite
	// path covers per-message metrics from messages.json; Tier 1 via
	// proxy is the v2 path for accurate per-API-call tokens).
	if len(tokens) != 0 {
		t.Errorf("token events: %d; want 0 (hook path is v1 metadata-only)", len(tokens))
	}

	// Spot-check: the read_files tool_call row has the expected
	// target extracted.
	for _, ev := range tools {
		if ev.ActionType == models.ActionReadFile {
			if !strings.Contains(ev.Target, "PROGRESS.md") {
				t.Errorf("read_files Target = %q; want substring PROGRESS.md", ev.Target)
			}
			if ev.RawToolName != "read_files" {
				t.Errorf("read_files RawToolName = %q; want read_files", ev.RawToolName)
			}
		}
	}

	// agent_error event becomes ActionAPIError with non-empty
	// ErrorMessage.
	for _, ev := range tools {
		if ev.ActionType == models.ActionAPIError {
			if ev.Target != "UpstreamProviderError" {
				t.Errorf("api_error Target = %q; want UpstreamProviderError", ev.Target)
			}
			if !strings.Contains(ev.ErrorMessage, "rate_limit_exceeded") {
				t.Errorf("api_error ErrorMessage = %q; want substring rate_limit_exceeded", ev.ErrorMessage)
			}
			if ev.Success {
				t.Error("api_error Success = true; want false")
			}
		}
	}

	// Every emitted row uses the hook source-file tag.
	for _, ev := range tools {
		if ev.SourceFile != hookSourceFile {
			t.Errorf("event SourceFile = %q; want %q", ev.SourceFile, hookSourceFile)
		}
	}
}

// TestParseHooksJSONL_Resume exercises the byte-offset resume:
// scan from 0, capture the offset, scan again from that offset →
// should return zero events.
func TestParseHooksJSONL_Resume(t *testing.T) {
	t.Parallel()
	path := filepath.Join("..", "..", "..", "testdata", "clinecli", "hooks-jsonl-sample.jsonl")
	_, _, off1, _, err := parseHooksJSONL(context.Background(), path, 0, scrub.New())
	if err != nil {
		t.Fatalf("first scan: %v", err)
	}
	if off1 == 0 {
		t.Fatal("first scan offset = 0; expected > 0")
	}
	tools, _, off2, _, err := parseHooksJSONL(context.Background(), path, off1, scrub.New())
	if err != nil {
		t.Fatalf("second scan: %v", err)
	}
	if len(tools) != 0 {
		t.Errorf("second scan events = %d; want 0 (already past EOF)", len(tools))
	}
	if off2 != off1 {
		t.Errorf("second scan offset = %d; want %d (no new bytes)", off2, off1)
	}
}

// TestParseHooksJSONL_PartialTrailingLine confirms the tailer
// doesn't consume a partial line at EOF — the writer may still be
// flushing it. The next call should pick it up.
func TestParseHooksJSONL_PartialTrailingLine(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.jsonl")

	// Write one complete event + a partial line at the end.
	completeEvent := `{"ts":"2026-06-05T23:21:51.520Z","hookName":"agent_start","agentId":"agt_x","sessionId":"sid-x"}`
	partial := `{"ts":"2026-06-05T23:30:00.000Z","hookName":"agent_e`
	body := completeEvent + "\n" + partial
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	tools, _, offset, _, err := parseHooksJSONL(context.Background(), path, 0, scrub.New())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(tools) != 1 {
		t.Errorf("tools = %d; want 1 (only the complete line)", len(tools))
	}
	completeLen := int64(len(completeEvent) + 1) // +1 for the newline
	if offset != completeLen {
		t.Errorf("offset = %d; want %d (one complete line consumed; partial unconsumed)", offset, completeLen)
	}

	// Now finish writing the partial line + add a third complete one.
	final := body + `nd","sessionId":"sid-x"}` + "\n" +
		`{"ts":"2026-06-05T23:31:00.000Z","hookName":"session_shutdown","sessionId":"sid-x"}` + "\n"
	if err := os.WriteFile(path, []byte(final), 0o600); err != nil {
		t.Fatal(err)
	}

	// Resume from the previous offset → should pick up the now-
	// complete agent_end + the session_shutdown.
	tools2, _, _, _, err := parseHooksJSONL(context.Background(), path, offset, scrub.New())
	if err != nil {
		t.Fatalf("resume scan: %v", err)
	}
	if len(tools2) != 2 {
		t.Errorf("resume tools = %d; want 2 (agent_end + session_shutdown)", len(tools2))
	}
}

// TestParseHooksJSONL_TolerantOfMalformedLines confirms a malformed
// JSON line surfaces as a warning + skip, not a fatal scan failure.
func TestParseHooksJSONL_TolerantOfMalformedLines(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.jsonl")
	body := strings.Join([]string{
		`{"ts":"2026-06-05T23:21:51.520Z","hookName":"agent_start","agentId":"a","sessionId":"s"}`,
		`{not json}`,
		`{"ts":"2026-06-05T23:35:00.000Z","hookName":"session_shutdown","sessionId":"s"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	tools, _, _, warnings, err := parseHooksJSONL(context.Background(), path, 0, scrub.New())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(tools) != 2 {
		t.Errorf("tools = %d; want 2 (skipped the malformed line)", len(tools))
	}
	if len(warnings) != 1 {
		t.Errorf("warnings = %d; want 1", len(warnings))
	}
}

// TestDispatchHookEvent_UnknownHookName warns and skips on an
// unknown hookName rather than panicking — Cline CLI may grow new
// event types in future versions and we don't want a new event to
// regress every adapter user.
func TestDispatchHookEvent_UnknownHookName(t *testing.T) {
	t.Parallel()
	ev := &hookEvent{
		Ts:        "2026-06-05T23:21:51.520Z",
		HookName:  "some_future_event_type",
		SessionID: "s",
	}
	tools, tokens, warnings := dispatchHookEvent(ev, "", scrub.New(), map[string]int{}, 0)
	if len(tools) != 0 || len(tokens) != 0 {
		t.Errorf("emitted events for unknown hookName; got %d/%d", len(tools), len(tokens))
	}
	if len(warnings) != 1 {
		t.Errorf("warnings = %d; want 1", len(warnings))
	}
}

// TestDispatchHookEvent_PromptSubmitStripsWrapper confirms the
// user_input wrapper stripping applies to the hook path too (not
// just the SQLite path's messages.json).
func TestDispatchHookEvent_PromptSubmitStripsWrapper(t *testing.T) {
	t.Parallel()
	ev := &hookEvent{
		Ts:        "2026-06-05T23:21:51.520Z",
		HookName:  "prompt_submit",
		SessionID: "s",
		UserPromptSubmit: &hookUserPromptSubmit{
			Prompt: `<user_input mode="plan">Plan the refactor</user_input>`,
		},
	}
	tools, _, _ := dispatchHookEvent(ev, "", scrub.New(), map[string]int{}, 0)
	if len(tools) != 1 {
		t.Fatalf("tools = %d; want 1", len(tools))
	}
	if tools[0].Target != "Plan the refactor" {
		t.Errorf("Target = %q; want \"Plan the refactor\" (wrapper stripped)", tools[0].Target)
	}
}

// TestDispatchHookEvent_AgentMetadataPropagates confirms agent_id +
// parent_agent_id flow through to ActionMetadata.
func TestDispatchHookEvent_AgentMetadataPropagates(t *testing.T) {
	t.Parallel()
	ev := &hookEvent{
		Ts:            "2026-06-05T23:21:51.520Z",
		HookName:      "agent_start",
		SessionID:     "s",
		AgentID:       "agt_self",
		ParentAgentID: "agt_lead",
	}
	tools, _, _ := dispatchHookEvent(ev, "", scrub.New(), map[string]int{}, 0)
	if len(tools) != 1 {
		t.Fatalf("tools = %d; want 1", len(tools))
	}
	if tools[0].Metadata == nil {
		t.Fatal("Metadata = nil; want populated")
	}
	if tools[0].Metadata.AgentID != "agt_self" {
		t.Errorf("Metadata.AgentID = %q; want agt_self", tools[0].Metadata.AgentID)
	}
	if tools[0].Metadata.ParentAgentID != "agt_lead" {
		t.Errorf("Metadata.ParentAgentID = %q; want agt_lead", tools[0].Metadata.ParentAgentID)
	}
}

// TestIntToStr covers the inline int-formatter (a hot-path
// optimisation in the SourceEventID composition).
func TestIntToStr(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want any }{
		{0, "0"},
		{1, "1"},
		{42, "42"},
		{100, "100"},
		{1000, "1000"},
		{-5, "-5"},
	}
	for _, tc := range cases {
		got := intToStr(tc.in.(int))
		if got != tc.want.(string) {
			t.Errorf("intToStr(%d) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

// TestParseHooksJSONL_Adapter_E2E exercises the full
// adapter.ParseSessionFile dispatch on a hooks.jsonl path — proves
// the basename routing wired correctly.
func TestParseHooksJSONL_Adapter_E2E(t *testing.T) {
	t.Parallel()
	// Build a fake .cline data root with the fixture file.
	root := t.TempDir()
	logsDir := filepath.Join(root, ".cline", "data", "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "clinecli", "hooks-jsonl-sample.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	hookPath := filepath.Join(logsDir, "hooks.jsonl")
	if err := os.WriteFile(hookPath, body, 0o600); err != nil {
		t.Fatal(err)
	}

	a := NewWithOptions(nil, root)
	if !a.IsSessionFile(hookPath) {
		t.Fatalf("IsSessionFile(%q) = false; expected true", hookPath)
	}

	res, err := a.ParseSessionFile(context.Background(), hookPath, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.ToolEvents) == 0 {
		t.Fatal("no tool events emitted from hooks.jsonl")
	}

	// Idempotency on the hook path: re-parse from offset 0 should
	// produce the same SourceEventIDs.
	res2, _ := a.ParseSessionFile(context.Background(), hookPath, 0)
	for i := range res.ToolEvents {
		if res.ToolEvents[i].SourceEventID != res2.ToolEvents[i].SourceEventID {
			t.Errorf("hook idempotency drift: event[%d] first=%q second=%q",
				i, res.ToolEvents[i].SourceEventID, res2.ToolEvents[i].SourceEventID)
		}
	}
}

// TestHookEvent_JSONShapeCovered_AllNine confirms that every
// hookName in the synthesised sample JSONL unmarshals cleanly — a
// regression on the typed event struct would surface here.
func TestHookEvent_JSONShapeCovered_AllNine(t *testing.T) {
	t.Parallel()
	path := filepath.Join("..", "..", "..", "testdata", "clinecli", "hooks-jsonl-sample.jsonl")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"agent_start": true, "agent_resume": true, "prompt_submit": true,
		"tool_call": true, "tool_result": true, "agent_end": true,
		"agent_abort": true, "agent_error": true, "session_shutdown": true,
	}
	for i, line := range strings.Split(strings.TrimRight(string(body), "\n"), "\n") {
		var ev hookEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Errorf("line %d: unmarshal: %v", i, err)
			continue
		}
		delete(want, ev.HookName)
	}
	if len(want) > 0 {
		t.Errorf("hookNames missing from sample: %v", want)
	}
}
