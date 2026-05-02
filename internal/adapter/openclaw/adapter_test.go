package openclaw

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/marmutapp/superbased-observer/internal/models"
)

func TestParseSessionFile_TaskRunsCapturesPromptAndCompletion(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "runs.sqlite")
	setupTaskRunsDB(t, dbPath)

	a := NewWithOptions(nil, []string{root})
	res, err := a.ParseSessionFile(context.Background(), dbPath, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if got := len(res.ToolEvents); got != 2 {
		t.Fatalf("expected 2 events, got %d", got)
	}
	if got := res.ToolEvents[0].ActionType; got != models.ActionUserPrompt {
		t.Fatalf("first event action_type = %q, want %q", got, models.ActionUserPrompt)
	}
	if got := res.ToolEvents[0].Target; got != "Say hello from OpenClaw setup smoke test." {
		t.Fatalf("prompt target = %q", got)
	}
	if got := res.ToolEvents[1].ActionType; got != models.ActionTaskComplete {
		t.Fatalf("second event action_type = %q, want %q", got, models.ActionTaskComplete)
	}
	if res.ToolEvents[1].Success {
		t.Fatalf("failed task should not be successful")
	}
	if got := res.ToolEvents[1].DurationMs; got != 7258 {
		t.Fatalf("duration = %d, want 7258", got)
	}
	if res.NewOffset != 1776892389769 {
		t.Fatalf("NewOffset = %d", res.NewOffset)
	}
}

func TestParseSessionFile_TaskRunsWatermarkSkipsOldRows(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "runs.sqlite")
	setupTaskRunsDB(t, dbPath)

	a := NewWithOptions(nil, []string{root})
	res, err := a.ParseSessionFile(context.Background(), dbPath, 1776892389769)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.ToolEvents) != 0 {
		t.Fatalf("expected no events, got %d", len(res.ToolEvents))
	}
	if res.NewOffset != 1776892389769 {
		t.Fatalf("NewOffset = %d", res.NewOffset)
	}
}

func TestParseSessionFile_TaskRunsSuppressesRowsWhenSessionTraceExists(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "tasks", "runs.sqlite")
	setupTaskRunsDB(t, dbPath)
	indexPath := filepath.Join(root, "agents", "main", "sessions", "sessions.json")
	if err := os.MkdirAll(filepath.Dir(indexPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(indexPath, []byte(`{
		"agent:main:explicit:observer-smoke": {
			"sessionId": "observer-smoke",
			"sessionFile": "C:\\Users\\marmu\\.openclaw\\agents\\main\\sessions\\ced44276-571e-4bc8-8777-e6653fc1634d.jsonl"
		}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	a := NewWithOptions(nil, []string{root})
	res, err := a.ParseSessionFile(context.Background(), dbPath, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if got := len(res.ToolEvents); got != 0 {
		t.Fatalf("expected task-run rows to be suppressed, got %d", got)
	}
}

func TestParseSessionFile_SessionsIndexSkipsEntriesWithSessionFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sessions.json")
	body := `{
		"agent:main:explicit:observer-ollama-gemma4-smoke": {
			"sessionId": "9ca34b34-65cb-4389-9f52-522f3f962144",
			"updatedAt": 1776893738354,
			"status": "succeeded",
			"endedAt": 1776893738354,
			"runtimeMs": 44486,
			"sessionFile": "C:\\Users\\marmu\\.openclaw\\agents\\main\\sessions\\9ca34b34-65cb-4389-9f52-522f3f962144.jsonl"
		}
	}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	a := NewWithOptions(nil, []string{root})
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if got := len(res.ToolEvents); got != 0 {
		t.Fatalf("expected session-index completion to be suppressed, got %d", got)
	}
}

func TestParseSessionFile_JSONLCapturesMessagesToolsAndUsage(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "session.jsonl")
	body := strings.Join([]string{
		`{"type":"session","version":3,"id":"ses_1","timestamp":"2026-04-22T21:34:53.850Z","cwd":"D:\\work\\openclaw"}`,
		`{"type":"model_change","id":"m1","timestamp":"2026-04-22T21:34:53.855Z","provider":"ollama","modelId":"gemma4:e4b"}`,
		`{"type":"message","id":"u1","timestamp":"2026-04-22T21:34:53.870Z","message":{"role":"user","content":[{"type":"text","text":"Read the file"}],"timestamp":1776893693868}}`,
		`{"type":"message","id":"a1","timestamp":"2026-04-22T21:35:24.489Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"call_1","name":"read","arguments":{"path":"BOOTSTRAP.md"}}],"stopReason":"toolUse","provider":"ollama","model":"gemma4:e4b","usage":{"input":10,"output":2,"cacheRead":1,"cacheWrite":0,"totalTokens":12},"timestamp":1776893724488}}`,
		`{"type":"message","id":"r1","timestamp":"2026-04-22T21:35:24.536Z","message":{"role":"toolResult","toolCallId":"call_1","toolName":"read","content":[{"type":"text","text":"hello"}],"isError":false,"timestamp":1776893724520}}`,
		`{"type":"message","id":"a2","timestamp":"2026-04-22T21:35:38.353Z","message":{"role":"assistant","content":[{"type":"text","text":"Done"}],"stopReason":"stop","provider":"ollama","model":"gemma4:e4b","usage":{"input":20,"output":3,"cacheRead":0,"cacheWrite":0,"totalTokens":23},"timestamp":1776893738351}}`,
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	a := NewWithOptions(nil, []string{root})
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if got := len(res.ToolEvents); got != 3 {
		t.Fatalf("expected 3 tool events, got %d", got)
	}
	if got := res.ToolEvents[0].ActionType; got != models.ActionUserPrompt {
		t.Fatalf("first action_type = %q", got)
	}
	if got := res.ToolEvents[0].MessageID; got != "user:u1" {
		t.Fatalf("user message_id = %q", got)
	}
	if got := res.ToolEvents[1].ActionType; got != models.ActionReadFile {
		t.Fatalf("second action_type = %q", got)
	}
	if got := res.ToolEvents[1].MessageID; got != "a1" {
		t.Fatalf("tool message_id = %q", got)
	}
	if got := res.ToolEvents[1].Target; got != "BOOTSTRAP.md" {
		t.Fatalf("tool target = %q", got)
	}
	if got := res.ToolEvents[1].ToolOutput; got != "hello" {
		t.Fatalf("tool output = %q", got)
	}
	if got := res.ToolEvents[2].ActionType; got != models.ActionTaskComplete {
		t.Fatalf("third action_type = %q", got)
	}
	if got := res.ToolEvents[2].MessageID; got != "a2" {
		t.Fatalf("task_complete message_id = %q", got)
	}
	if got := len(res.TokenEvents); got != 2 {
		t.Fatalf("expected 2 token events, got %d", got)
	}
	if got := res.TokenEvents[0].Model; got != "ollama/gemma4:e4b" {
		t.Fatalf("token model = %q", got)
	}
	if got := res.TokenEvents[0].MessageID; got != "a1" {
		t.Fatalf("token[0] message_id = %q", got)
	}
	if got := res.TokenEvents[1].MessageID; got != "a2" {
		t.Fatalf("token[1] message_id = %q", got)
	}
}

// TestParseSessionFile_JSONLEmitsAPIErrorOnStopReasonError pins the
// v1.4.22 stopReason="error" → ActionAPIError parity. Real openclaw
// JSONL emits an empty-content assistant message with stopReason="error"
// and an `errorMessage` field carrying the upstream provider's verbatim
// response (e.g. `400 {"error":"...does not support tools"}`). Pre-fix
// these were silently dropped because the existing stop-reason gate
// only fired on "stop".
func TestParseSessionFile_JSONLEmitsAPIErrorOnStopReasonError(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "session.jsonl")
	body := strings.Join([]string{
		`{"type":"session","version":3,"id":"ses_e","timestamp":"2026-04-22T21:33:00.000Z","cwd":"D:\\work\\openclaw"}`,
		`{"type":"model_change","id":"m1","timestamp":"2026-04-22T21:33:00.500Z","provider":"ollama","modelId":"gemma3:1b"}`,
		`{"type":"message","id":"u1","timestamp":"2026-04-22T21:33:01.000Z","message":{"role":"user","content":[{"type":"text","text":"do thing"}],"timestamp":1776893581000}}`,
		`{"type":"message","id":"a_err","timestamp":"2026-04-22T21:33:10.063Z","message":{"role":"assistant","content":[],"stopReason":"error","provider":"ollama","model":"gemma3:1b","errorMessage":"400 {\"error\":\"registry.ollama.ai/library/gemma3:1b does not support tools\"}","usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"totalTokens":0}}}`,
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	a := NewWithOptions(nil, []string{root})
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	// 1 user_prompt + 1 api_error
	if got := len(res.ToolEvents); got != 2 {
		t.Fatalf("expected 2 tool events, got %d", got)
	}
	row := res.ToolEvents[1]
	if row.ActionType != models.ActionAPIError {
		t.Errorf("action: %s want api_error", row.ActionType)
	}
	if row.Success {
		t.Errorf("api_error row should be Success=false")
	}
	if row.Target != "http_400" {
		t.Errorf("target: %q want http_400 (status-prefix discriminator)", row.Target)
	}
	if !strings.Contains(row.ErrorMessage, "does not support tools") {
		t.Errorf("error_message: %q", row.ErrorMessage)
	}
	if row.RawToolName != "http_400" {
		t.Errorf("raw_tool_name: %q want http_400", row.RawToolName)
	}
}

// TestParseSessionFile_JSONLEmitsSystemPromptOnBootstrapContext pins
// the v1.4.23 capture for openclaw custom/openclaw:bootstrap-context:full
// events. The data payload in observed corpora is just a marker
// (timestamp + runId + sessionId, no embedded prompt body) — pre-fix
// these were silently dropped because the adapter had no `case
// "custom":`. Per user direction (2026-05-01) we capture the marker
// anyway as ActionSystemPrompt with the data JSON in RawToolInput,
// hash-deduped so duplicate emissions on resume only land once.
// model-snapshot custom events stay no-op'd (redundant with
// model_change which already lifts provider+model).
func TestParseSessionFile_JSONLEmitsSystemPromptOnBootstrapContext(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "session.jsonl")
	body := strings.Join([]string{
		`{"type":"session","version":3,"id":"ses_b","timestamp":"2026-04-22T21:35:00.000Z","cwd":"D:\\work\\openclaw"}`,
		`{"type":"custom","customType":"model-snapshot","data":{"timestamp":1776893693864,"provider":"ollama","modelApi":"ollama","modelId":"gemma4:e4b"}}`,
		`{"type":"custom","customType":"openclaw:bootstrap-context:full","data":{"timestamp":1776893738357,"runId":"r1","sessionId":"ses_b"}}`,
		`{"type":"custom","customType":"openclaw:bootstrap-context:full","data":{"timestamp":1776893738357,"runId":"r1","sessionId":"ses_b"}}`,
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	a := NewWithOptions(nil, []string{root})
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	var sysPrompts []models.ToolEvent
	for _, ev := range res.ToolEvents {
		if ev.ActionType == models.ActionSystemPrompt {
			sysPrompts = append(sysPrompts, ev)
		}
	}
	// 2 identical bootstrap-context events → 1 row (dedup'd).
	// 1 model-snapshot → 0 rows (no-op'd).
	if len(sysPrompts) != 1 {
		t.Fatalf("system_prompt rows: %d want 1 (duplicate bootstrap dedup'd, model-snapshot ignored)", len(sysPrompts))
	}
	row := sysPrompts[0]
	if row.RawToolName != "system_prompt.bootstrap" {
		t.Errorf("raw_tool_name: %q want system_prompt.bootstrap", row.RawToolName)
	}
	if !strings.Contains(row.Target, "bootstrap-context") {
		t.Errorf("target: %q", row.Target)
	}
	if !strings.Contains(row.RawToolInput, "runId") {
		t.Errorf("raw_tool_input should include runId field: %q", row.RawToolInput)
	}
	if !strings.HasPrefix(row.MessageID, "system:") {
		t.Errorf("message_id: %q want 'system:<hash>'", row.MessageID)
	}
}

func TestParseSessionFile_JSONLUsesSessionsIndexAlias(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "sessions.json"), []byte(`{
		"agent:main:explicit:observer-ollama-gemma4-smoke": {
			"sessionId": "9ca34b34-65cb-4389-9f52-522f3f962144",
			"modelProvider": "ollama",
			"model": "gemma4:e4b",
			"sessionFile": "C:\\Users\\marmu\\.openclaw\\agents\\main\\sessions\\9ca34b34-65cb-4389-9f52-522f3f962144.jsonl",
			"systemPromptReport": {
				"workspaceDir": "C:\\Users\\marmu\\.openclaw\\workspace",
				"sessionKey": "agent:main:explicit:observer-ollama-gemma4-smoke",
				"provider": "ollama",
				"model": "gemma4:e4b"
			}
		}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "9ca34b34-65cb-4389-9f52-522f3f962144.jsonl")
	body := strings.Join([]string{
		`{"type":"session","version":3,"id":"9ca34b34-65cb-4389-9f52-522f3f962144","timestamp":"2026-04-22T21:34:53.850Z","cwd":"C:\\Users\\marmu\\.openclaw\\workspace"}`,
		`{"type":"model_change","id":"m1","timestamp":"2026-04-22T21:34:53.855Z","provider":"ollama","modelId":"gemma4:e4b"}`,
		`{"type":"message","id":"a1","timestamp":"2026-04-22T21:35:24.489Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"call_1","name":"read","arguments":{"path":"BOOTSTRAP.md"}}],"stopReason":"toolUse","provider":"ollama","model":"gemma4:e4b","usage":{"input":10,"output":2,"cacheRead":1,"cacheWrite":0,"totalTokens":12},"timestamp":1776893724488}}`,
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	a := NewWithOptions(nil, []string{root})
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.ToolEvents) != 1 || len(res.TokenEvents) != 1 {
		t.Fatalf("unexpected counts: tools=%d tokens=%d", len(res.ToolEvents), len(res.TokenEvents))
	}
	if got := res.ToolEvents[0].SessionID; got != "agent:main:explicit:observer-ollama-gemma4-smoke" {
		t.Fatalf("tool session_id = %q", got)
	}
	if got := res.TokenEvents[0].SessionID; got != "agent:main:explicit:observer-ollama-gemma4-smoke" {
		t.Fatalf("token session_id = %q", got)
	}
	if got := res.TokenEvents[0].Model; got != "ollama/gemma4:e4b" {
		t.Fatalf("token model = %q", got)
	}
}

// TestMapToolName_SessionsSpawnIsSubagent pins the parity fix: sessions_spawn
// is OpenClaw's sub-agent invocation. It used to be bucketed with the rest of
// sessions_* / agents_* / gateway calls under ActionMCPCall, which hid agent
// fan-out from dashboard counts that key off ActionSpawnSubagent.
func TestMapToolName_SessionsSpawnIsSubagent(t *testing.T) {
	if got := mapToolName("sessions_spawn"); got != models.ActionSpawnSubagent {
		t.Errorf("mapToolName(sessions_spawn) = %q, want %q", got, models.ActionSpawnSubagent)
	}
	// The other sessions_* tools stay as MCP calls — they're not spawns.
	for _, n := range []string{"sessions_list", "sessions_history", "sessions_send", "agents_list", "gateway"} {
		if got := mapToolName(n); got != models.ActionMCPCall {
			t.Errorf("mapToolName(%q) = %q, want %q (still mcp_call)", n, got, models.ActionMCPCall)
		}
	}
}

// TestParseSessionFile_JSONLPropagatesPrecedingTextToToolCalls pins
// the parity fix: the assistant's text/thinking content that introduces
// a tool call now flows through to the tool event's PrecedingReasoning,
// the same way claudecode and pi do it. Pre-fix the field was always
// empty for OpenClaw jsonl tool calls.
func TestParseSessionFile_JSONLPropagatesPrecedingTextToToolCalls(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "session.jsonl")
	body := strings.Join([]string{
		`{"type":"session","version":3,"id":"ses_1","timestamp":"2026-04-22T21:34:53.850Z","cwd":"/tmp/oc"}`,
		`{"type":"message","id":"a1","timestamp":"2026-04-22T21:35:24.489Z","message":{"role":"assistant","content":[{"type":"text","text":"I'll inspect BOOTSTRAP.md to understand the layout."},{"type":"toolCall","id":"call_1","name":"read","arguments":{"path":"BOOTSTRAP.md"}},{"type":"toolCall","id":"call_2","name":"read","arguments":{"path":"README.md"}},{"type":"text","text":"Now I'll search."},{"type":"toolCall","id":"call_3","name":"memory_search","arguments":{"query":"hello"}}],"stopReason":"toolUse","provider":"ollama","model":"gemma4:e4b","timestamp":1776893724488}}`,
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	a := NewWithOptions(nil, []string{root})
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.ToolEvents) != 3 {
		t.Fatalf("expected 3 tool events, got %d", len(res.ToolEvents))
	}
	// Both tool calls that follow the same text inherit the same preamble.
	preamble := "I'll inspect BOOTSTRAP.md to understand the layout."
	if got := res.ToolEvents[0].PrecedingReasoning; got != preamble {
		t.Errorf("tool[0] PrecedingReasoning = %q, want %q", got, preamble)
	}
	if got := res.ToolEvents[1].PrecedingReasoning; got != preamble {
		t.Errorf("tool[1] PrecedingReasoning = %q, want %q", got, preamble)
	}
	// The third tool call follows a fresh text part — it should pick up the new one.
	if got := res.ToolEvents[2].PrecedingReasoning; got != "Now I'll search." {
		t.Errorf("tool[2] PrecedingReasoning = %q, want %q", got, "Now I'll search.")
	}
}

// TestParseSessionFile_TaskRunsLiftsModelFromSessionsAlias pins the
// parity fix: the sqlite path now backfills Model + ProjectRoot from
// the matching sessions.json alias instead of emitting empty model
// strings + the literal "[openclaw]" placeholder. The alias's
// systemPromptReport carries provider/model + workspaceDir.
func TestParseSessionFile_TaskRunsLiftsModelFromSessionsAlias(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "tasks", "runs.sqlite")
	setupTaskRunsDB(t, dbPath)
	indexPath := filepath.Join(root, "agents", "main", "sessions", "sessions.json")
	if err := os.MkdirAll(filepath.Dir(indexPath), 0o755); err != nil {
		t.Fatal(err)
	}
	// Note the absence of `sessionFile` — without it suppressTaskRun keeps
	// the row, so it reaches taskPromptEvent / taskCompleteEvent and the
	// alias's model + workspaceDir flow through.
	if err := os.WriteFile(indexPath, []byte(`{
		"agent:main:explicit:observer-smoke": {
			"sessionId": "observer-smoke",
			"modelProvider": "anthropic",
			"model": "claude-sonnet-4-5",
			"systemPromptReport": {
				"workspaceDir": "/tmp/openclaw-ws",
				"provider": "anthropic",
				"model": "claude-sonnet-4-5"
			}
		}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	a := NewWithOptions(nil, []string{root})
	res, err := a.ParseSessionFile(context.Background(), dbPath, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.ToolEvents) != 2 {
		t.Fatalf("expected 2 events, got %d", len(res.ToolEvents))
	}
	wantModel := "anthropic/claude-sonnet-4-5"
	if res.ToolEvents[0].Model != wantModel {
		t.Errorf("prompt model = %q, want %q", res.ToolEvents[0].Model, wantModel)
	}
	if res.ToolEvents[1].Model != wantModel {
		t.Errorf("complete model = %q, want %q", res.ToolEvents[1].Model, wantModel)
	}
	// ProjectRoot lifted from systemPromptReport.workspaceDir; resolveProjectRoot
	// returns it unchanged because /tmp/openclaw-ws isn't a git repo.
	if res.ToolEvents[0].ProjectRoot != "/tmp/openclaw-ws" {
		t.Errorf("prompt project_root = %q, want /tmp/openclaw-ws", res.ToolEvents[0].ProjectRoot)
	}
}

func setupTaskRunsDB(t *testing.T, path string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	stmts := []string{
		`CREATE TABLE task_runs (
			task_id TEXT PRIMARY KEY,
			runtime TEXT NOT NULL,
			task_kind TEXT,
			source_id TEXT,
			requester_session_key TEXT,
			owner_key TEXT NOT NULL,
			scope_kind TEXT NOT NULL,
			child_session_key TEXT,
			parent_flow_id TEXT,
			parent_task_id TEXT,
			agent_id TEXT,
			run_id TEXT,
			label TEXT,
			task TEXT NOT NULL,
			status TEXT NOT NULL,
			delivery_status TEXT NOT NULL,
			notify_policy TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			started_at INTEGER,
			ended_at INTEGER,
			last_event_at INTEGER,
			cleanup_after INTEGER,
			error TEXT,
			progress_summary TEXT,
			terminal_summary TEXT,
			terminal_outcome TEXT
		)`,
		`INSERT INTO task_runs (
			task_id, runtime, source_id, requester_session_key, owner_key,
			scope_kind, child_session_key, agent_id, run_id, label, task, status,
			delivery_status, notify_policy, created_at, started_at, ended_at,
			last_event_at, cleanup_after, error, progress_summary,
			terminal_summary, terminal_outcome
		) VALUES (
			'task_1', 'cli', 'run_1', 'agent:main:explicit:observer-smoke',
			'agent:main:explicit:observer-smoke', 'session',
			'agent:main:explicit:observer-smoke', 'main', 'run_1', '',
			'[Thu 2026-04-23 02:42 GMT+5:30] Say hello from OpenClaw setup smoke test.',
			'failed', 'not_applicable', 'silent', 1776892338035, 1776892382511,
			1776892389769, 1776892389769, 1777497189769,
			'No API key found for provider "openai".', '', '', ''
		)`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
}
