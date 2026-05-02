package codex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join("..", "..", "..", "testdata", "codex", name)
}

func TestParseRolloutSession(t *testing.T) {
	t.Parallel()
	a := New()
	res, err := a.ParseSessionFile(context.Background(), fixture(t, "rollout-session.jsonl"), 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.ToolEvents) != 3 {
		t.Fatalf("tool events: %d want 3", len(res.ToolEvents))
	}

	// file_read → read_file
	if res.ToolEvents[0].ActionType != models.ActionReadFile {
		t.Errorf("event 0: %s", res.ToolEvents[0].ActionType)
	}
	if res.ToolEvents[0].SessionID != "cx-001" {
		t.Errorf("event 0 session: %q", res.ToolEvents[0].SessionID)
	}
	if res.ToolEvents[0].Tool != models.ToolCodex {
		t.Errorf("event 0 tool: %q", res.ToolEvents[0].Tool)
	}
	if !res.ToolEvents[0].Success {
		t.Error("event 0 should be success")
	}
	if !strings.Contains(res.ToolEvents[0].Target, "main.go") {
		t.Errorf("event 0 target: %q", res.ToolEvents[0].Target)
	}

	// shell → run_command, failed
	e2 := res.ToolEvents[1]
	if e2.ActionType != models.ActionRunCommand {
		t.Errorf("event 1 action: %s", e2.ActionType)
	}
	if e2.Success {
		t.Error("event 1 should be failed (success=false)")
	}
	if !strings.Contains(e2.Target, "go test") {
		t.Errorf("event 1 target: %q", e2.Target)
	}
	if !strings.Contains(e2.ErrorMessage, "FAIL") {
		t.Errorf("event 1 error_message: %q", e2.ErrorMessage)
	}
	if !strings.Contains(e2.ToolOutput, "FAIL") {
		t.Errorf("event 1 tool_output: %q", e2.ToolOutput)
	}

	// web_search → web_search
	if res.ToolEvents[2].ActionType != models.ActionWebSearch {
		t.Errorf("event 2 action: %s", res.ToolEvents[2].ActionType)
	}
	if res.ToolEvents[2].Target != "go testing best practices" {
		t.Errorf("event 2 target: %q", res.ToolEvents[2].Target)
	}

	// Token events: 2 records, second should be cumulative delta.
	if len(res.TokenEvents) != 2 {
		t.Fatalf("token events: %d want 2", len(res.TokenEvents))
	}
	if res.TokenEvents[0].InputTokens != 1000 {
		t.Errorf("tk1 input: %d want 1000", res.TokenEvents[0].InputTokens)
	}
	if res.TokenEvents[1].InputTokens != 600 {
		t.Errorf("tk2 input (delta): %d want 600", res.TokenEvents[1].InputTokens)
	}
	if res.TokenEvents[0].Reliability != models.ReliabilityApproximate {
		t.Errorf("reliability: %q", res.TokenEvents[0].Reliability)
	}
	if res.TokenEvents[0].Tool != models.ToolCodex {
		t.Errorf("tk tool: %q", res.TokenEvents[0].Tool)
	}
	if res.TokenEvents[0].CacheReadTokens != 800 {
		t.Errorf("cache read: %d", res.TokenEvents[0].CacheReadTokens)
	}
}

func TestParseModernDesktopRollout(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-2026-04-23T00-29-51-thread.jsonl")
	body := strings.Join([]string{
		`{"timestamp":"2026-04-22T19:00:01.055Z","type":"session_meta","payload":{"id":"thread-1","cwd":"D:\\programsx\\partner-names","model":"gpt-5.4","git_branch":"main"}}`,
		`{"timestamp":"2026-04-22T19:00:01.068Z","type":"event_msg","payload":{"type":"user_message","message":"Please run the tests\n"}}`,
		`{"timestamp":"2026-04-22T19:00:23.361Z","type":"event_msg","payload":{"type":"exec_command_end","call_id":"call_1","turn_id":"turn-1","command":["powershell","-Command","go test ./..."],"cwd":"D:\\programsx\\partner-names","aggregated_output":"FAIL\n","exit_code":1,"duration":{"secs":1,"nanos":500000000},"status":"failed"}}`,
		`{"timestamp":"2026-04-22T19:00:30.000Z","type":"event_msg","payload":{"type":"web_search_end","call_id":"ws_1","query":"codex rollout format"}}`,
		`{"timestamp":"2026-04-22T19:00:31.000Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":10,"cached_input_tokens":4,"output_tokens":2,"reasoning_output_tokens":1,"total_tokens":12}}}}`,
		`{"timestamp":"2026-04-22T19:00:32.000Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-1","last_agent_message":"Done","completed_at":1776884432,"duration_ms":1234}}`,
		``,
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	a := NewWithOptions(nil, dir)
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.ToolEvents) != 4 {
		t.Fatalf("tool events: %d want 4", len(res.ToolEvents))
	}
	if res.ToolEvents[0].ActionType != models.ActionUserPrompt {
		t.Fatalf("event 0 action: %s", res.ToolEvents[0].ActionType)
	}
	if res.ToolEvents[0].MessageID != "user:turn-1" {
		t.Fatalf("event 0 message id: %q", res.ToolEvents[0].MessageID)
	}
	if res.ToolEvents[1].ActionType != models.ActionRunCommand || res.ToolEvents[1].Success {
		t.Fatalf("event 1: %+v", res.ToolEvents[1])
	}
	if res.ToolEvents[1].MessageID != "turn-1" {
		t.Fatalf("event 1 message id: %q", res.ToolEvents[1].MessageID)
	}
	if !strings.Contains(res.ToolEvents[1].Target, "go test ./...") {
		t.Fatalf("event 1 target: %q", res.ToolEvents[1].Target)
	}
	if res.ToolEvents[2].ActionType != models.ActionWebSearch {
		t.Fatalf("event 2 action: %s", res.ToolEvents[2].ActionType)
	}
	if res.ToolEvents[3].ActionType != models.ActionTaskComplete {
		t.Fatalf("event 3 action: %s", res.ToolEvents[3].ActionType)
	}

	if len(res.TokenEvents) != 1 {
		t.Fatalf("token events: %d want 1", len(res.TokenEvents))
	}
	if res.TokenEvents[0].InputTokens != 10 || res.TokenEvents[0].CacheReadTokens != 4 ||
		res.TokenEvents[0].ReasoningTokens != 1 {
		t.Fatalf("token event: %+v", res.TokenEvents[0])
	}
	if res.TokenEvents[0].MessageID != "turn-1" {
		t.Fatalf("token event message id: %q", res.TokenEvents[0].MessageID)
	}
	if res.TokenEvents[0].Model != "gpt-5.4" {
		t.Fatalf("token event model: %q", res.TokenEvents[0].Model)
	}
	// v1.4.28 cwd translation: a Windows-style cwd ("D:\programsx\…")
	// captured by codex on Windows must NOT round-trip through
	// filepath.Abs on a Linux host (where it'd be treated as a relative
	// path, prepended with the test process's CWD, and walked up to
	// observer's own .git). Translate to the WSL2 mount equivalent so
	// ProjectRoot reflects the real source location.
	for i, e := range res.ToolEvents {
		if e.ProjectRoot != "/mnt/d/programsx/partner-names" {
			t.Errorf("event %d ProjectRoot: %q want /mnt/d/programsx/partner-names",
				i, e.ProjectRoot)
		}
	}
}

func TestParseModernTokenCountBeforeTurnContextStillGetsTurnModel(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-2026-04-29T00-00-00-thread.jsonl")
	body := strings.Join([]string{
		`{"timestamp":"2026-04-29T00:00:01.000Z","type":"session_meta","payload":{"id":"thread-2","cwd":"D:\\programsx\\partner-names","git_branch":"main"}}`,
		`{"timestamp":"2026-04-29T00:00:01.050Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn-2"}}`,
		`{"timestamp":"2026-04-29T00:00:01.100Z","type":"event_msg","payload":{"type":"user_message","message":"Check status\n"}}`,
		`{"timestamp":"2026-04-29T00:00:01.200Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":12,"cached_input_tokens":5,"output_tokens":3,"reasoning_output_tokens":1,"total_tokens":15}}}}`,
		`{"timestamp":"2026-04-29T00:00:01.300Z","type":"turn_context","payload":{"turn_id":"turn-2","cwd":"D:\\programsx\\partner-names","model":"gpt-5.4","git_branch":"main"}}`,
		``,
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	a := NewWithOptions(nil, dir)
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.TokenEvents) != 1 {
		t.Fatalf("token events: %d want 1", len(res.TokenEvents))
	}
	if res.TokenEvents[0].MessageID != "turn-2" {
		t.Fatalf("token message id: %q", res.TokenEvents[0].MessageID)
	}
	if res.TokenEvents[0].Model != "gpt-5.4" {
		t.Fatalf("token model: %q", res.TokenEvents[0].Model)
	}
	if len(res.ToolEvents) != 1 || res.ToolEvents[0].MessageID != "user:turn-2" {
		t.Fatalf("user prompt grouping: %+v", res.ToolEvents)
	}
}

// TestParseRolloutResponseItem pins the response_item envelope dispatch
// + dedup behavior introduced in v1.4.21. The fixture exercises seven
// distinct shapes from real Codex Desktop rollouts:
//
//  1. response_item/function_call(shell_command, call_paired) followed
//     by event_msg/exec_command_end(call_paired) → merged into a single
//     ActionRunCommand row carrying the richer end-event fields
//     (success, exit_code, duration, stdout). No double-counting.
//  2. response_item/function_call(shell_command, call_orphan) with NO
//     matching exec_command_end → standalone ActionRunCommand row from
//     the call alone (the user-flagged "first call without end" case).
//  3. response_item/function_call(update_plan, call_plan) with no
//     side-channel → standalone ActionTodoUpdate row.
//  4. response_item/web_search_call (no-op for Tier 1) followed by
//     event_msg/web_search_end → single ActionWebSearch row with the
//     query resolved from the end event. The response_item line MUST
//     NOT create a row in Tier 1.
//  5. response_item/custom_tool_call(apply_patch) +
//     custom_tool_call_output + event_msg/patch_apply_end → single
//     ActionEditFile row with success=true and target from the
//     post-execution `changes` map (preferred over the in-patch path).
//  6. response_item/custom_tool_call(apply_patch) WITHOUT patch_apply_end
//     → standalone ActionEditFile row with target parsed from the patch
//     text (the "*** Update File:" header).
//  7. event_msg/patch_apply_end without a paired custom_tool_call
//     (mid-session resume) → standalone ActionEditFile row.
func TestParseRolloutResponseItem(t *testing.T) {
	t.Parallel()
	a := New()
	res, err := a.ParseSessionFile(context.Background(), fixture(t, "rollout-response-item.jsonl"), 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.ToolEvents) != 16 {
		var summary []string
		for i, evt := range res.ToolEvents {
			summary = append(summary, formatEventSummary(i, evt))
		}
		t.Fatalf("tool events: %d want 16\n%s", len(res.ToolEvents), strings.Join(summary, "\n"))
	}

	// 0: user_prompt
	if res.ToolEvents[0].ActionType != models.ActionUserPrompt {
		t.Errorf("event 0 action: %s", res.ToolEvents[0].ActionType)
	}

	// 1: paired shell_command — merged.
	row := res.ToolEvents[1]
	if row.ActionType != models.ActionRunCommand {
		t.Errorf("event 1 action: %s want run_command", row.ActionType)
	}
	if !row.Success {
		t.Errorf("event 1 should be success (exit_code=0)")
	}
	if !strings.Contains(row.Target, "go test ./...") {
		t.Errorf("event 1 target should carry merged command: %q", row.Target)
	}
	if row.DurationMs != 1250 {
		t.Errorf("event 1 duration_ms: %d want 1250", row.DurationMs)
	}
	if !strings.Contains(row.ToolOutput, "ok all tests pass") {
		t.Errorf("event 1 tool_output: %q", row.ToolOutput)
	}
	if row.RawToolName != "exec_command_end" {
		t.Errorf("event 1 raw_tool_name: %q want exec_command_end (post-merge)", row.RawToolName)
	}
	if row.SourceEventID != "call_paired" {
		t.Errorf("event 1 source_event_id: %q want call_paired", row.SourceEventID)
	}

	// 2: orphan shell_command.
	row = res.ToolEvents[2]
	if row.ActionType != models.ActionRunCommand {
		t.Errorf("event 2 action: %s want run_command", row.ActionType)
	}
	if row.RawToolName != "shell_command" {
		t.Errorf("event 2 raw_tool_name: %q want shell_command (pre-merge)", row.RawToolName)
	}
	if row.SourceEventID != "call_orphan" {
		t.Errorf("event 2 source_event_id: %q", row.SourceEventID)
	}

	// 3: update_plan → ActionTodoUpdate.
	row = res.ToolEvents[3]
	if row.ActionType != models.ActionTodoUpdate {
		t.Errorf("event 3 action: %s want todo_update", row.ActionType)
	}
	if row.RawToolName != "update_plan" {
		t.Errorf("event 3 raw_tool_name: %q", row.RawToolName)
	}

	// 4: web_search_end.
	row = res.ToolEvents[4]
	if row.ActionType != models.ActionWebSearch {
		t.Errorf("event 4 action: %s want web_search", row.ActionType)
	}
	if !strings.Contains(row.Target, "go testing patterns") {
		t.Errorf("event 4 target: %q", row.Target)
	}

	// 5: apply_patch fully paired (call + output + patch_apply_end).
	row = res.ToolEvents[5]
	if row.ActionType != models.ActionEditFile {
		t.Errorf("event 5 action: %s want edit_file", row.ActionType)
	}
	if !row.Success {
		t.Errorf("event 5 should be success")
	}
	if row.RawToolName != "patch_apply_end" {
		t.Errorf("event 5 raw_tool_name: %q want patch_apply_end (post-merge)", row.RawToolName)
	}
	if !strings.Contains(row.Target, "hello.go") {
		t.Errorf("event 5 target should reference hello.go from changes map: %q", row.Target)
	}
	if !strings.Contains(row.ToolOutput, "Success") {
		t.Errorf("event 5 tool_output: %q", row.ToolOutput)
	}
	if row.SourceEventID != "call_patch_paired" {
		t.Errorf("event 5 source_event_id: %q", row.SourceEventID)
	}

	// 6: orphan apply_patch — target parsed from patch text.
	row = res.ToolEvents[6]
	if row.ActionType != models.ActionEditFile {
		t.Errorf("event 6 action: %s want edit_file", row.ActionType)
	}
	if row.RawToolName != "apply_patch" {
		t.Errorf("event 6 raw_tool_name: %q want apply_patch (pre-merge)", row.RawToolName)
	}
	if !strings.Contains(row.Target, "lone.go") {
		t.Errorf("event 6 target should be parsed from `*** Update File:` header: %q", row.Target)
	}
	if row.SourceEventID != "call_patch_orphan" {
		t.Errorf("event 6 source_event_id: %q", row.SourceEventID)
	}

	// 7: standalone patch_apply_end (no preceding custom_tool_call).
	row = res.ToolEvents[7]
	if row.ActionType != models.ActionEditFile {
		t.Errorf("event 7 action: %s want edit_file", row.ActionType)
	}
	if row.RawToolName != "patch_apply_end" {
		t.Errorf("event 7 raw_tool_name: %q", row.RawToolName)
	}
	if !strings.Contains(row.Target, "recovered.go") {
		t.Errorf("event 7 target: %q", row.Target)
	}

	// 8: paired list_mcp_resources function_call + mcp_tool_call_end → merged.
	row = res.ToolEvents[8]
	if row.ActionType != models.ActionMCPCall {
		t.Errorf("event 8 action: %s want mcp_call", row.ActionType)
	}
	if row.Target != "codex:list_mcp_resources" {
		t.Errorf("event 8 target: %q want codex:list_mcp_resources", row.Target)
	}
	if !row.Success {
		t.Errorf("event 8 should be success (Ok branch + isError=false)")
	}
	if row.DurationMs != 300 {
		t.Errorf("event 8 duration_ms: %d want 300", row.DurationMs)
	}
	if row.RawToolName != "mcp_tool_call_end" {
		t.Errorf("event 8 raw_tool_name: %q want mcp_tool_call_end (post-merge)", row.RawToolName)
	}
	if !strings.Contains(row.ToolOutput, "resources") {
		t.Errorf("event 8 tool_output: %q", row.ToolOutput)
	}
	if row.SourceEventID != "call_mcp_paired" {
		t.Errorf("event 8 source_event_id: %q", row.SourceEventID)
	}

	// 9: standalone mcp_tool_call_end (no pending function_call) — Err branch.
	row = res.ToolEvents[9]
	if row.ActionType != models.ActionMCPCall {
		t.Errorf("event 9 action: %s want mcp_call", row.ActionType)
	}
	if row.Target != "docs:search" {
		t.Errorf("event 9 target: %q", row.Target)
	}
	if row.Success {
		t.Errorf("event 9 should be failed (Err branch)")
	}
	if !strings.Contains(row.ErrorMessage, "server unreachable") {
		t.Errorf("event 9 error_message: %q", row.ErrorMessage)
	}

	// 10: api_error — usage_limit_exceeded captured as ActionAPIError.
	row = res.ToolEvents[10]
	if row.ActionType != models.ActionAPIError {
		t.Errorf("event 10 action: %s want api_error", row.ActionType)
	}
	if row.Success {
		t.Errorf("event 10 should be failed (success=false)")
	}
	if row.Target != "usage_limit_exceeded" {
		t.Errorf("event 10 target: %q want usage_limit_exceeded", row.Target)
	}
	if !strings.Contains(row.ErrorMessage, "usage limit") {
		t.Errorf("event 10 error_message: %q", row.ErrorMessage)
	}
	if row.RawToolName != "usage_limit_exceeded" {
		t.Errorf("event 10 raw_tool_name: %q", row.RawToolName)
	}

	// 11: paired view_image function_call + view_image_tool_call → merged read_file.
	row = res.ToolEvents[11]
	if row.ActionType != models.ActionReadFile {
		t.Errorf("event 11 action: %s want read_file", row.ActionType)
	}
	if !strings.Contains(row.Target, "screen.png") {
		t.Errorf("event 11 target: %q", row.Target)
	}
	if row.RawToolName != "view_image_tool_call" {
		t.Errorf("event 11 raw_tool_name: %q want view_image_tool_call (post-merge)", row.RawToolName)
	}
	if row.SourceEventID != "call_view_paired" {
		t.Errorf("event 11 source_event_id: %q", row.SourceEventID)
	}

	// 12: standalone view_image_tool_call (no preceding function_call).
	row = res.ToolEvents[12]
	if row.ActionType != models.ActionReadFile {
		t.Errorf("event 12 action: %s want read_file", row.ActionType)
	}
	if !strings.Contains(row.Target, "orphan.png") {
		t.Errorf("event 12 target: %q", row.Target)
	}
	if row.RawToolName != "view_image_tool_call" {
		t.Errorf("event 12 raw_tool_name: %q", row.RawToolName)
	}

	// 13: dynamic_tool_call_request + response merged.
	row = res.ToolEvents[13]
	if row.RawToolName != "dynamic_tool_call_response" {
		t.Errorf("event 13 raw_tool_name: %q want dynamic_tool_call_response (post-merge)", row.RawToolName)
	}
	if !row.Success {
		t.Errorf("event 13 should be success")
	}
	if row.DurationMs != 55 {
		t.Errorf("event 13 duration_ms: %d want 55", row.DurationMs)
	}
	if !strings.Contains(row.ToolOutput, "Workspace dependencies") {
		t.Errorf("event 13 tool_output: %q", row.ToolOutput)
	}
	if row.SourceEventID != "call_dyn" {
		t.Errorf("event 13 source_event_id: %q", row.SourceEventID)
	}

	// 14: turn_aborted.
	row = res.ToolEvents[14]
	if row.ActionType != models.ActionTurnAborted {
		t.Errorf("event 14 action: %s want turn_aborted", row.ActionType)
	}
	if row.Success {
		t.Errorf("event 14 should be failed (success=false)")
	}
	if row.Target != "interrupted" {
		t.Errorf("event 14 target: %q", row.Target)
	}
	if row.DurationMs != 23898 {
		t.Errorf("event 14 duration_ms: %d want 23898", row.DurationMs)
	}

	// 15: task_complete — must remain last.
	if res.ToolEvents[15].ActionType != models.ActionTaskComplete {
		t.Errorf("event 15 action: %s want task_complete", res.ToolEvents[15].ActionType)
	}
}

func formatEventSummary(i int, evt models.ToolEvent) string {
	return fmt.Sprintf("  [%d] action=%s raw=%q target=%q src_event=%s", i, evt.ActionType, evt.RawToolName, evt.Target, evt.SourceEventID)
}

// TestParseResponseItemDurationFromTimestampGap pins v1.4.28: when
// codex's response_item function_call (or custom_tool_call) carries
// no structured duration field — typical of newer "Wall time: Xs"
// flat-text outputs and JSON-metadata variants — the adapter
// computes DurationMs from the gap between the call timestamp and
// the matching output timestamp. Previously these rows landed with
// DurationMs=0 even though wall-clock time was knowable from the
// records themselves.
func TestParseResponseItemDurationFromTimestampGap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-2026-05-03T10-00-00-thread.jsonl")
	body := strings.Join([]string{
		`{"timestamp":"2026-05-03T10:00:00.000Z","type":"session_meta","payload":{"id":"thread-d","cwd":"/tmp","model":"gpt-5","git_branch":"main"}}`,
		`{"timestamp":"2026-05-03T10:00:00.100Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn-1"}}`,
		// function_call at +0.1s, no structured duration on output.
		`{"timestamp":"2026-05-03T10:00:00.500Z","type":"response_item","payload":{"type":"function_call","name":"shell","call_id":"call_dur","arguments":"{\"command\":[\"ls\"]}"}}`,
		// function_call_output at +3.7s — adapter should compute 3200ms.
		`{"timestamp":"2026-05-03T10:00:03.700Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_dur","output":"Exit code: 0\nWall time: 3.2 seconds\nOutput:\nfoo\nbar\n"}}`,
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := New().ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	var got int64
	for _, e := range res.ToolEvents {
		if e.SourceEventID == "call_dur" {
			got = e.DurationMs
		}
	}
	if got != 3200 {
		t.Errorf("DurationMs: got %d want 3200 (call→output timestamp gap)", got)
	}
}

// TestParseRolloutSystemPrompts pins the v1.4.23 capture for codex
// system-prompt-shaped content. Three sources, all hash-deduped to
// the same row when their bodies match:
//
//  1. session_meta.base_instructions.text — emit once with role=base.
//  2. turn_context.developer_instructions — emit once per unique
//     content; identical instructions across turns dedup to the first
//     emission.
//  3. response_item.message.role=developer — same dedup behavior.
//
// The fixture has identical developer_instructions across two
// turn_contexts (must dedup to ONE row) plus a different
// developer-role response_item.message (must emit a SECOND row). The
// base_instructions text differs from both, so total = 3 system_prompt
// rows.
func TestParseRolloutSystemPrompts(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-2026-05-01T02-00-00-thread.jsonl")
	body := strings.Join([]string{
		`{"timestamp":"2026-05-01T02:00:00.000Z","type":"session_meta","payload":{"id":"thread-s","cwd":"/tmp","model":"gpt-5","base_instructions":{"text":"You are Codex, follow these rules."}}}`,
		`{"timestamp":"2026-05-01T02:00:00.500Z","type":"turn_context","payload":{"turn_id":"turn-1","model":"gpt-5","cwd":"/tmp","developer_instructions":"<permissions>workspace-write</permissions>"}}`,
		`{"timestamp":"2026-05-01T02:00:01.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn-1"}}`,
		`{"timestamp":"2026-05-01T02:00:02.000Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"<context>extra mid-turn instructions</context>"}]}}`,
		`{"timestamp":"2026-05-01T02:00:03.000Z","type":"turn_context","payload":{"turn_id":"turn-2","model":"gpt-5","cwd":"/tmp","developer_instructions":"<permissions>workspace-write</permissions>"}}`,
		``,
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	a := NewWithOptions(nil, dir)
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	var sysPrompts []models.ToolEvent
	for _, e := range res.ToolEvents {
		if e.ActionType == models.ActionSystemPrompt {
			sysPrompts = append(sysPrompts, e)
		}
	}
	if len(sysPrompts) != 3 {
		t.Fatalf("system_prompt rows: %d want 3 (base + developer + mid-turn developer; second turn_context dedup'd)", len(sysPrompts))
	}
	// 0: base_instructions.
	if !strings.Contains(sysPrompts[0].RawToolName, "base") {
		t.Errorf("event 0 raw_tool_name: %q want system_prompt.base", sysPrompts[0].RawToolName)
	}
	if !strings.Contains(sysPrompts[0].Target, "You are Codex") {
		t.Errorf("event 0 target: %q", sysPrompts[0].Target)
	}
	// 1: turn 1 developer_instructions.
	if !strings.Contains(sysPrompts[1].RawToolName, "developer") {
		t.Errorf("event 1 raw_tool_name: %q", sysPrompts[1].RawToolName)
	}
	if !strings.Contains(sysPrompts[1].Target, "permissions") {
		t.Errorf("event 1 target: %q", sysPrompts[1].Target)
	}
	// 2: response_item.message.role=developer.
	if !strings.Contains(sysPrompts[2].Target, "extra mid-turn") {
		t.Errorf("event 2 target: %q", sysPrompts[2].Target)
	}
	// MessageID dedup: identical bodies share MessageID prefix.
	if !strings.HasPrefix(sysPrompts[1].MessageID, "system:") {
		t.Errorf("event 1 message_id should be 'system:<hash>': %q", sysPrompts[1].MessageID)
	}
}

// TestParseRolloutUserEnvelopeIsCapturedAsSystemPrompt pins a v1.4.24
// follow-up: response_item.message.role=user content split into two
// classes.
//
//   - Plain text and markdown — these ARE real user prompts that
//     event_msg/user_message already captures; emitting another row
//     here would double-count.
//   - XML-envelope-shaped (`<environment_context>...`,
//     `<user_instructions>...`, etc.) — these are synthetic context
//     injections from the Codex runtime, not user input. They look
//     like user-role messages to the model but originate from the
//     runtime. Capture as ActionSystemPrompt with role=user-envelope.
//
// Detection heuristic: body trimmed of leading whitespace must start
// with `<`. The plain-text "Can you find out..." MUST stay
// uncaptured here (event_msg/user_message owns it).
func TestParseRolloutUserEnvelopeIsCapturedAsSystemPrompt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-2026-05-01T03-00-00-thread.jsonl")
	body := strings.Join([]string{
		`{"timestamp":"2026-05-01T03:00:00.000Z","type":"session_meta","payload":{"id":"thread-u","cwd":"/tmp","model":"gpt-5"}}`,
		`{"timestamp":"2026-05-01T03:00:00.500Z","type":"turn_context","payload":{"turn_id":"turn-u","model":"gpt-5","cwd":"/tmp"}}`,
		// Real user prompt via event_msg/user_message (Tier 1 path).
		`{"timestamp":"2026-05-01T03:00:01.000Z","type":"event_msg","payload":{"type":"user_message","message":"Can you find out all the active windows"}}`,
		// SAME content rebroadcast as response_item.message.role=user — should NOT emit a second row.
		`{"timestamp":"2026-05-01T03:00:01.100Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Can you find out all the active windows"}]}}`,
		// Synthetic envelope — should emit a system_prompt with role=user-envelope.
		`{"timestamp":"2026-05-01T03:00:02.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>\n  <cwd>/tmp</cwd>\n  <shell>bash</shell>\n</environment_context>"}]}}`,
		``,
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	a := NewWithOptions(nil, dir)
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	var userPrompts, systemPrompts []models.ToolEvent
	for _, ev := range res.ToolEvents {
		switch ev.ActionType {
		case models.ActionUserPrompt:
			userPrompts = append(userPrompts, ev)
		case models.ActionSystemPrompt:
			systemPrompts = append(systemPrompts, ev)
		}
	}
	if len(userPrompts) != 1 {
		t.Errorf("user_prompt rows: %d want 1 (the plain-text response_item must NOT emit a second user_prompt)", len(userPrompts))
	}
	if len(systemPrompts) != 1 {
		t.Fatalf("system_prompt rows: %d want 1 (only the <environment_context> envelope qualifies)", len(systemPrompts))
	}
	row := systemPrompts[0]
	if row.RawToolName != "system_prompt.user-envelope" {
		t.Errorf("raw_tool_name: %q want system_prompt.user-envelope", row.RawToolName)
	}
	if !strings.Contains(row.Target, "environment_context") {
		t.Errorf("target preview should reference envelope: %q", row.Target)
	}
}

// TestParseRolloutTokenCountDedupesRepeatedTotal pins the v1.4.25
// dedup behaviour: Codex's runtime sometimes re-emits identical
// event_msg/token_count records with the same last_token_usage AND
// total_token_usage. Pre-fix the adapter summed both, inflating
// session totals. The dedup uses total_token_usage as a
// fingerprint — total is monotonic, so any non-advancing total
// is a re-emission and the second event is skipped.
//
// User reported this against the
// rollout-2026-04-23T00-29-51-019db690 session: 22 token_count
// events but only 20 were real model calls; 2 were duplicates that
// inflated input by +122,680, cache_read by +88,704, etc. After
// fix, Observer's sum should equal Codex's own final
// total_token_usage figure.
func TestParseRolloutTokenCountDedupesRepeatedTotal(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-2026-05-01T05-00-00-thread.jsonl")
	body := strings.Join([]string{
		`{"timestamp":"2026-05-01T05:00:00.000Z","type":"session_meta","payload":{"id":"thread-d","cwd":"/tmp","model":"gpt-5"}}`,
		`{"timestamp":"2026-05-01T05:00:00.500Z","type":"turn_context","payload":{"turn_id":"turn-1","model":"gpt-5","cwd":"/tmp"}}`,
		// Real call 1: 100 input, 10 output, 5 cached, 2 reasoning.
		`{"timestamp":"2026-05-01T05:00:01.000Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":100,"output_tokens":10,"cached_input_tokens":5,"reasoning_output_tokens":2,"total_tokens":112},"total_token_usage":{"input_tokens":100,"output_tokens":10,"cached_input_tokens":5,"reasoning_output_tokens":2,"total_tokens":112}}}}`,
		// Real call 2: 200 cumulative (delta +100), 20 cumulative output, etc.
		`{"timestamp":"2026-05-01T05:00:02.000Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":100,"output_tokens":10,"cached_input_tokens":5,"reasoning_output_tokens":2,"total_tokens":112},"total_token_usage":{"input_tokens":200,"output_tokens":20,"cached_input_tokens":10,"reasoning_output_tokens":4,"total_tokens":224}}}}`,
		// DUPLICATE of call 2: same total, same last. Must be skipped.
		`{"timestamp":"2026-05-01T05:00:02.500Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":100,"output_tokens":10,"cached_input_tokens":5,"reasoning_output_tokens":2,"total_tokens":112},"total_token_usage":{"input_tokens":200,"output_tokens":20,"cached_input_tokens":10,"reasoning_output_tokens":4,"total_tokens":224}}}}`,
		// Real call 3: total advances.
		`{"timestamp":"2026-05-01T05:00:03.000Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":50,"output_tokens":5,"cached_input_tokens":3,"reasoning_output_tokens":1,"total_tokens":56},"total_token_usage":{"input_tokens":250,"output_tokens":25,"cached_input_tokens":13,"reasoning_output_tokens":5,"total_tokens":280}}}}`,
		``,
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	a := NewWithOptions(nil, dir)
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	// 4 token_count events in fixture; 1 is a duplicate. Want 3 emitted.
	if got := len(res.TokenEvents); got != 3 {
		t.Fatalf("token events: %d want 3 (1 duplicate must be skipped)", got)
	}
	var sumIn, sumOut, sumCacheR, sumReasoning int64
	for _, e := range res.TokenEvents {
		sumIn += e.InputTokens
		sumOut += e.OutputTokens
		sumCacheR += e.CacheReadTokens
		sumReasoning += e.ReasoningTokens
	}
	// Real per-call deltas: 100+100+50=250 input, 10+10+5=25 output,
	// 5+5+3=13 cache_read, 2+2+1=5 reasoning. Matches the final
	// total_token_usage on the last real event.
	if sumIn != 250 {
		t.Errorf("sum input: %d want 250 (matches final total_token_usage)", sumIn)
	}
	if sumOut != 25 {
		t.Errorf("sum output: %d want 25", sumOut)
	}
	if sumCacheR != 13 {
		t.Errorf("sum cache_read: %d want 13", sumCacheR)
	}
	if sumReasoning != 5 {
		t.Errorf("sum reasoning: %d want 5", sumReasoning)
	}
}

// TestParseRolloutCompacted pins the v1.4.22 capture for upstream
// codex compaction events: top-level type="compacted" carries
// `replacement_history` of summarized messages; we emit a single
// ActionContextCompacted row with msg-count + byte/token estimate
// in Target / RawToolInput. Per user direction (2026-05-01) these
// rows are NOT searchable like file edits — but they ARE captured
// (the discriminator lets dashboards filter them out cleanly while
// keeping the data for cost/compaction analytics).
func TestParseRolloutCompacted(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-2026-05-01T01-00-00-thread.jsonl")
	body := strings.Join([]string{
		`{"timestamp":"2026-05-01T01:00:00.000Z","type":"session_meta","payload":{"id":"thread-c","cwd":"/tmp","model":"gpt-5"}}`,
		`{"timestamp":"2026-05-01T01:00:00.500Z","type":"turn_context","payload":{"turn_id":"turn-c","model":"gpt-5","cwd":"/tmp"}}`,
		`{"timestamp":"2026-05-01T01:00:01.000Z","type":"compacted","payload":{"message":"summary text","replacement_history":[{"role":"user","content":[{"type":"input_text","text":"please do X"}]},{"role":"assistant","content":[{"type":"output_text","text":"working on it"}]}]}}`,
		`{"timestamp":"2026-05-01T01:00:01.000Z","type":"event_msg","payload":{"type":"context_compacted"}}`,
		``,
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	a := NewWithOptions(nil, dir)
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.ToolEvents) != 1 {
		t.Fatalf("tool events: %d want 1 (event_msg/context_compacted is no-op'd)", len(res.ToolEvents))
	}
	row := res.ToolEvents[0]
	if row.ActionType != models.ActionContextCompacted {
		t.Errorf("action: %s want context_compacted", row.ActionType)
	}
	if !strings.Contains(row.Target, "2 msgs") {
		t.Errorf("target: %q want '2 msgs, ...' format", row.Target)
	}
	if !strings.Contains(row.RawToolInput, `"messages":2`) {
		t.Errorf("raw_tool_input should carry msg count: %q", row.RawToolInput)
	}
	if !strings.Contains(row.ToolOutput, "summary text") {
		t.Errorf("tool_output: %q", row.ToolOutput)
	}
}

// TestParseRolloutResponseItemReasoning pins the v1.4.22 forward-
// compat capture for response_item.reasoning. Current Codex Desktop
// builds emit summary:[] uniformly (0% non-empty across 838 items in
// the 2026-04 corpus), but if/when summary fills in with
// {type:"summary_text", text:"..."} segments, those should thread
// into the turn's PrecedingReasoning chain — same place agent_message
// already lives. This test forces a populated summary into the
// fixture and verifies the next exec_command_end inherits it.
func TestParseRolloutResponseItemReasoning(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-2026-05-01T00-00-00-thread.jsonl")
	body := strings.Join([]string{
		`{"timestamp":"2026-05-01T00:00:00.000Z","type":"session_meta","payload":{"id":"thread-r","cwd":"/tmp","model":"gpt-5"}}`,
		`{"timestamp":"2026-05-01T00:00:00.500Z","type":"turn_context","payload":{"turn_id":"turn-r","model":"gpt-5","cwd":"/tmp"}}`,
		`{"timestamp":"2026-05-01T00:00:01.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn-r"}}`,
		`{"timestamp":"2026-05-01T00:00:02.000Z","type":"response_item","payload":{"type":"reasoning","summary":[{"type":"summary_text","text":"I should run the test suite."}],"encrypted_content":"opaque..."}}`,
		`{"timestamp":"2026-05-01T00:00:03.000Z","type":"event_msg","payload":{"type":"exec_command_end","call_id":"call_R","turn_id":"turn-r","command":["bash","-lc","go test ./..."],"cwd":"/tmp","aggregated_output":"ok","exit_code":0,"duration":{"secs":1,"nanos":0},"status":"completed"}}`,
		``,
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	a := NewWithOptions(nil, dir)
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.ToolEvents) != 1 {
		t.Fatalf("tool events: %d want 1", len(res.ToolEvents))
	}
	row := res.ToolEvents[0]
	if !strings.Contains(row.PrecedingReasoning, "run the test suite") {
		t.Errorf("preceding_reasoning should carry reasoning summary text, got %q", row.PrecedingReasoning)
	}
}

// TestParseAgentMessagePropagatesToToolPrecedingReasoning pins the
// parity fix: Codex emits assistant-text preambles via
// `event_msg`/`agent_message` per turn, and every tool_call /
// exec_command_end / web_search_end inside that turn now inherits
// it as PrecedingReasoning. Pre-fix the field was always empty for
// Codex tool events while claudecode/pi/openclaw all carried it.
func TestParseAgentMessagePropagatesToToolPrecedingReasoning(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-2026-04-30T00-00-00-thread.jsonl")
	body := strings.Join([]string{
		`{"timestamp":"2026-04-30T00:00:01.000Z","type":"session_meta","payload":{"id":"thread-3","cwd":"/x","model":"gpt-5","git_branch":"main"}}`,
		`{"timestamp":"2026-04-30T00:00:01.050Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn-3"}}`,
		`{"timestamp":"2026-04-30T00:00:01.100Z","type":"event_msg","payload":{"type":"agent_message","turn_id":"turn-3","message":"I'll inspect main.go and run the tests."}}`,
		`{"timestamp":"2026-04-30T00:00:01.200Z","type":"tool_call","payload":{"call_id":"c1","tool":"file_read","input":{"path":"main.go"}}}`,
		`{"timestamp":"2026-04-30T00:00:01.300Z","type":"event_msg","payload":{"type":"exec_command_end","call_id":"c2","turn_id":"turn-3","command":["go","test"],"aggregated_output":"PASS","exit_code":0,"duration":{"secs":0,"nanos":500000000},"status":"completed"}}`,
		`{"timestamp":"2026-04-30T00:00:01.400Z","type":"event_msg","payload":{"type":"web_search_end","call_id":"c3","turn_id":"turn-3","query":"go test best practices"}}`,
		// New turn → new agent_message → tool_call inherits the new preamble.
		`{"timestamp":"2026-04-30T00:00:02.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn-4"}}`,
		`{"timestamp":"2026-04-30T00:00:02.100Z","type":"event_msg","payload":{"type":"agent_message","turn_id":"turn-4","message":"Now I'll patch the bug."}}`,
		`{"timestamp":"2026-04-30T00:00:02.200Z","type":"tool_call","payload":{"call_id":"c4","tool":"apply_patch","input":{"path":"main.go"}}}`,
		``,
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	a := NewWithOptions(nil, dir)
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	// Expect 4 tool events: file_read (c1), exec_command_end (c2),
	// web_search_end (c3), apply_patch (c4).
	if len(res.ToolEvents) != 4 {
		t.Fatalf("tool events: %d want 4 (%+v)", len(res.ToolEvents), res.ToolEvents)
	}
	preamble1 := "I'll inspect main.go and run the tests."
	for i := 0; i < 3; i++ {
		if got := res.ToolEvents[i].PrecedingReasoning; got != preamble1 {
			t.Errorf("event[%d] (%s) PrecedingReasoning = %q, want %q",
				i, res.ToolEvents[i].RawToolName, got, preamble1)
		}
	}
	if got := res.ToolEvents[3].PrecedingReasoning; got != "Now I'll patch the bug." {
		t.Errorf("event[3] PrecedingReasoning = %q, want fresh preamble", got)
	}
}

func TestIsSessionFile(t *testing.T) {
	t.Parallel()
	a := New()
	if !a.IsSessionFile("/x/rollout-2026-04-16-abc.jsonl") {
		t.Error("rollout-*.jsonl should match")
	}
	if a.IsSessionFile("/x/events.jsonl") {
		t.Error("non-rollout .jsonl should not match")
	}
	if a.IsSessionFile("/x/rollout-x.json") {
		t.Error("non-jsonl should not match")
	}
}

func TestWatchPathsHonorsCodexHome(t *testing.T) {
	t.Setenv("CODEX_HOME", "/custom/codex")
	a := New()
	paths := a.WatchPaths()
	want := filepath.Join("/custom/codex", "sessions")
	if len(paths) != 1 || paths[0] != want {
		t.Errorf("CODEX_HOME not honored: %v", paths)
	}
}

func TestIncrementalParse(t *testing.T) {
	t.Parallel()
	// Parse first half, then resume.
	a := New()
	res1, err := a.ParseSessionFile(context.Background(), fixture(t, "rollout-session.jsonl"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if res1.NewOffset <= 0 {
		t.Fatal("offset not advanced")
	}
	// Re-parse from the end — should produce zero events.
	res2, err := a.ParseSessionFile(context.Background(), fixture(t, "rollout-session.jsonl"), res1.NewOffset)
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.ToolEvents) != 0 || len(res2.TokenEvents) != 0 {
		t.Errorf("resume from EOF produced events: tool=%d token=%d",
			len(res2.ToolEvents), len(res2.TokenEvents))
	}
}

// TestTokenCountColdResume guards audit item C1: when an incremental
// parse resumes mid-session with a fresh in-memory lastInputByID map,
// the first token_count event (whose cumulative total may be huge)
// must NOT be emitted as a delta of that full cumulative. Old
// behaviour: in = tk.InputTokens - 0 → over-count by the entire
// cumulative. Fixed: emit in=0 for the resume-baseline event, then
// correct deltas thereafter.
func TestTokenCountColdResume(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-resume.jsonl")
	// Use the legacy top-level token_count line.Type — the path the C1
	// fix targets. The modern event_msg/token_count nested shape is
	// handled by parseModernTokenCount with no delta math.
	lines := []string{
		`{"timestamp":"2026-04-22T19:00:00.000Z","type":"session_meta","payload":{"id":"sess-resume","model":"gpt-5","cwd":"/x"}}`,
		// Pretend we already parsed this once: cumulative=200.
		`{"timestamp":"2026-04-22T19:00:01.000Z","type":"token_count","payload":{"input_tokens":200,"output_tokens":10}}`,
		// Then later, cumulative=350. Real delta: 150.
		`{"timestamp":"2026-04-22T19:00:02.000Z","type":"token_count","payload":{"input_tokens":350,"output_tokens":15}}`,
		// One more: cumulative=600. Real delta: 250.
		`{"timestamp":"2026-04-22T19:00:03.000Z","type":"token_count","payload":{"input_tokens":600,"output_tokens":20}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// First parse from offset 0 — establishes the baseline path.
	a := New()
	res1, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res1.TokenEvents) != 3 {
		t.Fatalf("first parse: %d events", len(res1.TokenEvents))
	}
	wantFresh := []int64{200, 150, 250} // cumulative 0→200→350→600
	for i, ev := range res1.TokenEvents {
		if ev.InputTokens != wantFresh[i] {
			t.Errorf("fresh parse event %d input=%d want %d", i, ev.InputTokens, wantFresh[i])
		}
	}

	// Now simulate a cold restart: new adapter instance + parse from
	// offset > 0. In a fresh process, lastInputByID is empty. The first
	// event we see (cumulative=350) must NOT emit input=350 — that would
	// double-count what was already in the DB.
	//
	// Find the offset of just before line 3 (the second token_count).
	body, _ := os.ReadFile(path)
	cut := strings.Index(string(body), `"input_tokens":350`)
	if cut <= 0 {
		t.Fatal("could not find resume cut point in fixture")
	}
	// Roll back to the start of that line.
	resumeOffset := int64(strings.LastIndex(string(body[:cut]), "\n") + 1)

	a2 := New()
	res2, err := a2.ParseSessionFile(context.Background(), path, resumeOffset)
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.TokenEvents) != 2 {
		t.Fatalf("resume parse: %d events want 2", len(res2.TokenEvents))
	}
	// First post-resume event: must emit 0 (baseline), not 350.
	if res2.TokenEvents[0].InputTokens != 0 {
		t.Errorf("resume first event input=%d want 0 (baseline)",
			res2.TokenEvents[0].InputTokens)
	}
	// Second post-resume event: 600-350 = 250. Correct delta.
	if res2.TokenEvents[1].InputTokens != 250 {
		t.Errorf("resume second event input=%d want 250 (600-350)",
			res2.TokenEvents[1].InputTokens)
	}
}
