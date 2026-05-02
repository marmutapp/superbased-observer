package claudecode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
)

func fixturePath(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join("..", "..", "..", "testdata", "claudecode", name)
}

// TestParseAPIError pins the v1.4.20 fix: Claude Code writes upstream
// API failures (content-policy blocks, rate limits, invalid-request
// errors) as JSONL records with type="system" + subtype="api_error" +
// no `message` field. Pre-fix the adapter dropped these because the
// `len(line.Message) == 0` short-circuit fired first. Now they emit
// ActionAPIError rows with the upstream request_id + error class +
// human message preserved.
func TestParseAPIError(t *testing.T) {
	t.Parallel()
	a := New()
	res, err := a.ParseSessionFile(context.Background(), fixturePath(t, "api-error.jsonl"), 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}

	var errors []models.ToolEvent
	for _, ev := range res.ToolEvents {
		if ev.ActionType == models.ActionAPIError {
			errors = append(errors, ev)
		}
	}
	if len(errors) != 3 {
		t.Fatalf("expected 3 api_error events, got %d (total events: %d)", len(errors), len(res.ToolEvents))
	}

	// First error: invalid_request_error / content-policy block.
	e0 := errors[0]
	if e0.RawToolName != "invalid_request_error" {
		t.Errorf("first error class: got %q want invalid_request_error", e0.RawToolName)
	}
	if e0.Target != "req_011CaZnwqf6Cw5zQpQq7VUAp" {
		t.Errorf("first error target (request_id): got %q", e0.Target)
	}
	if !strings.Contains(e0.ErrorMessage, "content filtering") {
		t.Errorf("first error message: got %q", e0.ErrorMessage)
	}
	if e0.Success {
		t.Error("api_error event must have Success=false")
	}
	if e0.SessionID != "sess-err" {
		t.Errorf("session id: got %q want sess-err", e0.SessionID)
	}
	if e0.MessageID != "req_011CaZnwqf6Cw5zQpQq7VUAp" {
		t.Errorf("message_id should mirror request_id for join compatibility with api_turns: got %q", e0.MessageID)
	}

	// Second error: rate_limit_error.
	e1 := errors[1]
	if e1.RawToolName != "rate_limit_error" {
		t.Errorf("second error class: got %q want rate_limit_error", e1.RawToolName)
	}
	if !strings.Contains(e1.ErrorMessage, "rate limit") {
		t.Errorf("second error message: got %q", e1.ErrorMessage)
	}

	// Third error: overloaded_error in a doubly-nested envelope —
	// matches the live Claude Code shape where error.error.error.{type,
	// message} is the leaf. findInnermostAPIError walks until message
	// is non-empty, so the leaf wins over the generic "error" middle.
	e2 := errors[2]
	if e2.RawToolName != "overloaded_error" {
		t.Errorf("doubly-nested error class: got %q want overloaded_error", e2.RawToolName)
	}
	if e2.ErrorMessage != "Overloaded" {
		t.Errorf("doubly-nested error message: got %q want Overloaded", e2.ErrorMessage)
	}
	if e2.Target != "req_011deeplynest" {
		t.Errorf("doubly-nested target: got %q want req_011deeplynest", e2.Target)
	}
}

func TestParseSimpleSession(t *testing.T) {
	t.Parallel()
	a := New()
	res, err := a.ParseSessionFile(context.Background(), fixturePath(t, "simple-session.jsonl"), 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	// 3 tool events: user_prompt (from msg-001 user text) + Read + Bash.
	// The two follow-up user lines (msg-003, msg-005) carry only
	// tool_result blocks and don't emit user_prompts.
	if len(res.ToolEvents) != 3 {
		t.Fatalf("expected 3 tool events, got %d", len(res.ToolEvents))
	}
	// Event 0: user_prompt — mirrors what every other adapter produces.
	up := res.ToolEvents[0]
	if up.ActionType != models.ActionUserPrompt {
		t.Errorf("event 0 action_type: %q want %q", up.ActionType, models.ActionUserPrompt)
	}
	if up.MessageID != "user:msg-001" {
		t.Errorf("event 0 message_id: %q want user:msg-001", up.MessageID)
	}
	if up.SourceEventID != "msg-001" {
		t.Errorf("event 0 source_event_id: %q want msg-001 (line.UUID)", up.SourceEventID)
	}
	if up.RawToolName != "user_message" {
		t.Errorf("event 0 raw_tool_name: %q want user_message", up.RawToolName)
	}
	if !strings.Contains(up.Target, "main.go") {
		t.Errorf("event 0 target should echo prompt text: %q", up.Target)
	}

	// Event 1: Read
	e1 := res.ToolEvents[1]
	if e1.ActionType != models.ActionReadFile {
		t.Errorf("event 1: action_type %q", e1.ActionType)
	}
	if e1.RawToolName != "Read" {
		t.Errorf("event 1: raw name %q", e1.RawToolName)
	}
	if e1.SessionID != "sess-001" {
		t.Errorf("event 1: session_id %q", e1.SessionID)
	}
	if e1.SourceEventID != "toolu_01" {
		t.Errorf("event 1: source_event_id %q", e1.SourceEventID)
	}
	if e1.Tool != models.ToolClaudeCode {
		t.Errorf("event 1: tool %q", e1.Tool)
	}
	if !e1.Success {
		t.Error("event 1 should be success")
	}
	if e1.PrecedingReasoning == "" {
		t.Error("event 1 should have preceding reasoning")
	}
	if !strings.Contains(e1.Target, "main.go") {
		t.Errorf("event 1 target: %q", e1.Target)
	}

	// Event 2: Bash, failed
	e2 := res.ToolEvents[2]
	if e2.ActionType != models.ActionRunCommand {
		t.Errorf("event 2: action_type %q", e2.ActionType)
	}
	if e2.Success {
		t.Error("event 2 should be failure (is_error=true)")
	}
	if !strings.Contains(e2.ErrorMessage, "FAIL") {
		t.Errorf("event 2 error_message: %q", e2.ErrorMessage)
	}
	if !strings.Contains(e2.Target, "go test") {
		t.Errorf("event 2 target: %q", e2.Target)
	}

	// Token events: one per assistant message with usage.
	if len(res.TokenEvents) < 1 {
		t.Fatalf("expected at least 1 token event, got %d", len(res.TokenEvents))
	}
	tk := res.TokenEvents[0]
	if tk.Source != models.TokenSourceJSONL || tk.Reliability != models.ReliabilityUnreliable {
		t.Errorf("token reliability: source=%s reliability=%s", tk.Source, tk.Reliability)
	}
	if tk.CacheReadTokens != 200 {
		t.Errorf("cache read tokens: %d", tk.CacheReadTokens)
	}

	if res.NewOffset <= 0 {
		t.Errorf("offset not advanced: %d", res.NewOffset)
	}
}

func TestParseMultiToolTurn(t *testing.T) {
	t.Parallel()
	a := New()
	res, err := a.ParseSessionFile(context.Background(), fixturePath(t, "multi-tool-turn.jsonl"), 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.ToolEvents) != 3 {
		t.Fatalf("expected 3 tool events, got %d", len(res.ToolEvents))
	}
	want := []string{models.ActionSearchText, models.ActionSearchFiles, models.ActionWebSearch}
	for i, w := range want {
		if res.ToolEvents[i].ActionType != w {
			t.Errorf("event %d: %s, want %s", i, res.ToolEvents[i].ActionType, w)
		}
	}
	// Third (WebSearch) should be marked failed from the tool_result.
	if res.ToolEvents[2].Success {
		t.Error("WebSearch should be failure")
	}
	// First two should be success.
	if !res.ToolEvents[0].Success || !res.ToolEvents[1].Success {
		t.Error("Grep/Glob should be success")
	}
}

func TestMalformedLineSkipped(t *testing.T) {
	t.Parallel()
	a := New()
	res, err := a.ParseSessionFile(context.Background(), fixturePath(t, "malformed-line.jsonl"), 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.ToolEvents) != 2 {
		t.Fatalf("expected 2 tool events around the malformed line, got %d", len(res.ToolEvents))
	}
	if len(res.Warnings) == 0 {
		t.Error("expected at least one warning for the malformed line")
	}
}

func TestIncrementalParse(t *testing.T) {
	t.Parallel()
	// Copy fixture so we can truncate and grow it.
	src := fixturePath(t, "simple-session.jsonl")
	body, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "incr.jsonl")
	// Write only the first 2 lines initially.
	lines := strings.Split(string(body), "\n")
	if err := os.WriteFile(dst, []byte(strings.Join(lines[:2], "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := New()
	res1, err := a.ParseSessionFile(context.Background(), dst, 0)
	if err != nil {
		t.Fatalf("first parse: %v", err)
	}
	// 2 events from the first 2 lines: user_prompt (from line 1's user text)
	// + Read (from line 2's tool_use).
	if len(res1.ToolEvents) != 2 {
		t.Fatalf("first parse expected 2 events, got %d", len(res1.ToolEvents))
	}

	// Append the rest and resume.
	if err := os.WriteFile(dst, body, 0o600); err != nil {
		t.Fatal(err)
	}
	res2, err := a.ParseSessionFile(context.Background(), dst, res1.NewOffset)
	if err != nil {
		t.Fatalf("second parse: %v", err)
	}
	// Lines 3-5 add: tool_result (no event), Bash tool_use, tool_result is_error
	// (updates Bash). So 1 new tool event: the Bash.
	if len(res2.ToolEvents) != 1 {
		t.Fatalf("second parse expected 1 new event, got %d", len(res2.ToolEvents))
	}
	if res2.ToolEvents[0].ActionType != models.ActionRunCommand {
		t.Errorf("second event: %s", res2.ToolEvents[0].ActionType)
	}
	if res2.NewOffset <= res1.NewOffset {
		t.Errorf("offset did not advance: %d -> %d", res1.NewOffset, res2.NewOffset)
	}
}

// TestParseDedupsByMessageID guards against the A1 finding: Claude Code
// writes one JSONL line per content block of an assistant message, all
// sharing the same Anthropic message.id and echoing the same accumulating
// usage envelope. The adapter must collapse same-msg.id events into one
// TokenEvent (with the final cumulative output_tokens) so the cost engine
// doesn't sum them as N independent API calls.
func TestParseDedupsByMessageID(t *testing.T) {
	t.Parallel()
	a := New()
	res, err := a.ParseSessionFile(context.Background(), fixturePath(t, "multi-block-dedup.jsonl"), 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}

	// Fixture: 4 blocks share msg_01ABCDEDUP (collapse → 1) + 1 synthetic
	// no-msg.id row (kept as-is, falls back to per-record UUID).
	if got, want := len(res.TokenEvents), 2; got != want {
		t.Fatalf("TokenEvents: got %d want %d (4-block dedup + 1 no-id)", got, want)
	}

	dd := res.TokenEvents[0]
	if dd.SourceEventID != "msg_01ABCDEDUP" {
		t.Errorf("SourceEventID: got %q want msg_01ABCDEDUP", dd.SourceEventID)
	}
	if dd.OutputTokens != 197 {
		t.Errorf("OutputTokens: got %d want 197 (final cumulative, not 8+8+8+197)", dd.OutputTokens)
	}
	if dd.InputTokens != 3 || dd.CacheCreationTokens != 13316 {
		t.Errorf("Input/CacheCreation should match the echoed envelope: in=%d cc=%d",
			dd.InputTokens, dd.CacheCreationTokens)
	}

	noID := res.TokenEvents[1]
	if noID.SourceEventID != "u-no-msg-id" {
		t.Errorf("no-msg.id row should fall back to line.UUID, got %q", noID.SourceEventID)
	}

	// C4: a JSONL line with model="<synthetic>" must never produce a
	// TokenEvent. Fixture has one; if the filter regresses we'll see 3
	// events instead of 2.
	for _, ev := range res.TokenEvents {
		if ev.Model == "<synthetic>" {
			t.Errorf("synthetic-model rows must be dropped: %+v", ev)
		}
	}

	// All three tool_use blocks across the four msg-id-shared lines
	// remain distinct ToolEvents (Grep, Grep, WebSearch) — dedup affects
	// token counting only, not tool-call capture. Plus the leading
	// user_prompt from u-prompt's text content = 4 tool events.
	if got, want := len(res.ToolEvents), 4; got != want {
		t.Fatalf("ToolEvents: got %d want %d (user_prompt + Grep+Grep+WebSearch)", got, want)
	}
	if res.ToolEvents[0].ActionType != models.ActionUserPrompt {
		t.Errorf("first event should be user_prompt, got %s", res.ToolEvents[0].ActionType)
	}
}

// TestParseUserPromptEmission pins the parity fix: claudecode now
// emits a user_prompt action for user-role lines that carry text,
// matching what every other adapter produces. Tool-result-only user
// messages stay as-is — their tool_result blocks update the matching
// tool event but no user_prompt is emitted.
func TestParseUserPromptEmission(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	body := strings.Join([]string{
		// User text → user_prompt expected.
		`{"sessionId":"s","cwd":"/tmp","timestamp":"2026-04-30T00:00:00Z","uuid":"u-1","message":{"role":"user","content":[{"type":"text","text":"hello world"}]}}`,
		// Assistant tool_use → Read tool event.
		`{"sessionId":"s","cwd":"/tmp","timestamp":"2026-04-30T00:00:01Z","uuid":"u-2","message":{"id":"msg_a","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"/x.go"}}]}}`,
		// User with only tool_result → NO user_prompt.
		`{"sessionId":"s","cwd":"/tmp","timestamp":"2026-04-30T00:00:02Z","uuid":"u-3","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok","is_error":false}]}}`,
		// User with text AND tool_result → user_prompt fires; tool_result
		// updates pending tool (none here, so it's a no-op).
		`{"sessionId":"s","cwd":"/tmp","timestamp":"2026-04-30T00:00:03Z","uuid":"u-4","message":{"role":"user","content":[{"type":"text","text":"thanks, now do this"},{"type":"tool_result","tool_use_id":"toolu_orphan","content":"x","is_error":false}]}}`,
	}, "\n")
	p := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := New().ParseSessionFile(context.Background(), p, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	// Expect: user_prompt (u-1) + Read (u-2) + user_prompt (u-4 — text portion).
	// u-3 emits nothing (tool_result-only).
	if len(res.ToolEvents) != 3 {
		t.Fatalf("ToolEvents: got %d want 3 (%+v)", len(res.ToolEvents), res.ToolEvents)
	}
	for i, want := range []struct{ action, sourceID, msgID string }{
		{models.ActionUserPrompt, "u-1", "user:u-1"},
		{models.ActionReadFile, "toolu_1", "msg_a"},
		{models.ActionUserPrompt, "u-4", "user:u-4"},
	} {
		got := res.ToolEvents[i]
		if got.ActionType != want.action {
			t.Errorf("event %d action_type: got %q want %q", i, got.ActionType, want.action)
		}
		if got.SourceEventID != want.sourceID {
			t.Errorf("event %d source_event_id: got %q want %q", i, got.SourceEventID, want.sourceID)
		}
		if got.MessageID != want.msgID {
			t.Errorf("event %d message_id: got %q want %q", i, got.MessageID, want.msgID)
		}
	}
}

// TestParseToolUseDurationMs pins the v1.4.28 wall-clock duration
// capture: claude-code's JSONL doesn't emit a structured per-tool
// elapsed field, so the adapter computes DurationMs as the gap from
// the assistant's tool_use timestamp to the matching user
// tool_result timestamp.
func TestParseToolUseDurationMs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	body := strings.Join([]string{
		// Assistant tool_use at t0.
		`{"sessionId":"s","cwd":"/tmp","timestamp":"2026-05-03T10:00:00Z","uuid":"u-1","message":{"id":"msg_a","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"tool_use","id":"toolu_dur","name":"Read","input":{"file_path":"/x.go"}}]}}`,
		// User tool_result at t0+2.5s — adapter should record 2500ms.
		`{"sessionId":"s","cwd":"/tmp","timestamp":"2026-05-03T10:00:02.500Z","uuid":"u-2","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_dur","content":"ok","is_error":false}]}}`,
	}, "\n")
	p := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := New().ParseSessionFile(context.Background(), p, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.ToolEvents) != 1 {
		t.Fatalf("ToolEvents: got %d want 1", len(res.ToolEvents))
	}
	got := res.ToolEvents[0].DurationMs
	if got != 2500 {
		t.Errorf("DurationMs: got %d want 2500 (t0+2500ms tool_result)", got)
	}
}

// TestParseCacheCreationTierBreakdown verifies the adapter captures
// usage.cache_creation.{ephemeral_5m_input_tokens, ephemeral_1h_input_tokens}
// when present and falls back to cache_creation_input_tokens otherwise.
// Audit item C5.
func TestParseCacheCreationTierBreakdown(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	body := strings.Join([]string{
		// Tier-aware line: both legacy total and breakdown.
		`{"sessionId":"s","cwd":"/tmp","timestamp":"2026-04-25T10:00:00Z","uuid":"u-tier","message":{"id":"msg_tier","model":"claude-sonnet-4-6","content":[{"type":"text","text":"x"}],"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":100,"cache_creation_input_tokens":600,"cache_creation":{"ephemeral_5m_input_tokens":400,"ephemeral_1h_input_tokens":200}}}}`,
		// Tier-only line: breakdown without legacy total.
		`{"sessionId":"s","cwd":"/tmp","timestamp":"2026-04-25T10:00:01Z","uuid":"u-only","message":{"id":"msg_only","model":"claude-sonnet-4-6","content":[{"type":"text","text":"x"}],"usage":{"input_tokens":10,"output_tokens":5,"cache_creation":{"ephemeral_5m_input_tokens":300,"ephemeral_1h_input_tokens":150}}}}`,
		// Legacy-only line: total set, no breakdown.
		`{"sessionId":"s","cwd":"/tmp","timestamp":"2026-04-25T10:00:02Z","uuid":"u-legacy","message":{"id":"msg_legacy","model":"claude-sonnet-4-6","content":[{"type":"text","text":"x"}],"usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":800}}}`,
	}, "\n")
	p := filepath.Join(dir, "tier.jsonl")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	a := New()
	res, err := a.ParseSessionFile(context.Background(), p, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.TokenEvents) != 3 {
		t.Fatalf("TokenEvents: got %d want 3", len(res.TokenEvents))
	}

	// Index by message.id since map ordering isn't guaranteed across
	// Go versions but the slice preserves insertion order in practice.
	byID := map[string]models.TokenEvent{}
	for _, ev := range res.TokenEvents {
		byID[ev.SourceEventID] = ev
	}

	tier := byID["msg_tier"]
	if tier.CacheCreationTokens != 600 {
		t.Errorf("tier total: got %d want 600 (legacy field wins when both present)", tier.CacheCreationTokens)
	}
	if tier.CacheCreation1hTokens != 200 {
		t.Errorf("tier 1h: got %d want 200", tier.CacheCreation1hTokens)
	}

	only := byID["msg_only"]
	if only.CacheCreationTokens != 450 {
		t.Errorf("breakdown-only total: got %d want 450 (sum of 5m+1h)", only.CacheCreationTokens)
	}
	if only.CacheCreation1hTokens != 150 {
		t.Errorf("breakdown-only 1h: got %d want 150", only.CacheCreation1hTokens)
	}

	legacy := byID["msg_legacy"]
	if legacy.CacheCreationTokens != 800 {
		t.Errorf("legacy total: got %d want 800", legacy.CacheCreationTokens)
	}
	if legacy.CacheCreation1hTokens != 0 {
		t.Errorf("legacy 1h: got %d want 0 (no breakdown present)", legacy.CacheCreation1hTokens)
	}
}

func TestIsSessionFile(t *testing.T) {
	t.Parallel()
	a := New()
	if !a.IsSessionFile("x.jsonl") {
		t.Error(".jsonl should be recognized")
	}
	if a.IsSessionFile("x.json") {
		t.Error(".json should NOT be a session file")
	}
}

func TestWatchPathsUsesHome(t *testing.T) {
	t.Parallel()
	a := New()
	paths := a.WatchPaths()
	if len(paths) == 0 {
		t.Fatal("expected at least the native-home watch path")
	}
	// Invariant: every emitted path ends with ".claude/projects".
	// Count alone isn't asserted because crossmount expansion adds one
	// path per /mnt/c/Users/<u> on WSL2 hosts (and per
	// \\wsl.localhost\<distro>\home\<user> on Windows hosts), and the
	// CI machine's user list is host-dependent.
	for _, p := range paths {
		if !strings.HasSuffix(p, filepath.Join(".claude", "projects")) {
			t.Errorf("watch path doesn't end with .claude/projects: %q", p)
		}
	}
	// Native home is always present and comes first.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("UserHomeDir unavailable")
	}
	want := filepath.Join(home, ".claude", "projects")
	if paths[0] != want {
		t.Errorf("native-home path: got %q want %q (must come first)", paths[0], want)
	}
}

func TestScrubbingAppliedToBashCommand(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "secret.jsonl")
	body := `{"type":"assistant","sessionId":"s","cwd":"/tmp","uuid":"u","timestamp":"2026-04-16T00:00:00Z","message":{"role":"assistant","model":"m","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"curl -H 'Authorization: Bearer sk-secret-abc123XYZ99999' https://api"}}]}}
`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	a := New()
	res, err := a.ParseSessionFile(context.Background(), p, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(res.ToolEvents) != 1 {
		t.Fatalf("expected 1 event, got %d", len(res.ToolEvents))
	}
	evt := res.ToolEvents[0]
	if strings.Contains(evt.Target, "sk-secret-abc123XYZ99999") {
		t.Errorf("secret leaked into target: %q", evt.Target)
	}
	if strings.Contains(evt.RawToolInput, "sk-secret-abc123XYZ99999") {
		t.Errorf("secret leaked into raw_tool_input: %q", evt.RawToolInput)
	}
}
