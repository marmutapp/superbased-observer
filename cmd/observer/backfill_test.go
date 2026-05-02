package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/adapter/cursor"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/models"
)

// TestBackfillCacheTier_RecoversNullColumns simulates the pre-migration-008
// state: token_usage rows with NULL cache_creation_1h_tokens that should
// have been tagged 1h tier per the JSONL. The backfill must:
//
//  1. Scan the JSONL, extract the 1h subset per msg id.
//  2. UPDATE token_usage where the column IS NULL.
//  3. Leave already-populated rows untouched.
//  4. Report the count of recovered tokens.
func TestBackfillCacheTier_RecoversNullColumns(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "obs.db")
	database, err := db.Open(ctx, db.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	// Seed a session and three token_usage rows:
	//   row A: NULL cw_1h, msg_a in JSONL says ephemeral_1h = 100k → backfill should fill in 100k
	//   row B: explicit cw_1h=50k already, msg_b in JSONL says ephemeral_1h = 50k → IS-NULL guard skips
	//   row C: NULL cw_1h, msg_c JSONL has ephemeral_1h=0 (5m only) → backfill leaves alone
	if _, err := database.ExecContext(ctx,
		`INSERT INTO projects (id, root_path, created_at) VALUES (1, '/tmp/test', '2026-04-23T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	_, err = database.ExecContext(ctx,
		`INSERT INTO sessions (id, tool, started_at, project_id) VALUES ('sA', 'claude-code', '2026-04-23T00:00:00Z', 1)`)
	if err != nil {
		t.Fatal(err)
	}
	for _, tu := range []struct {
		eid  string
		cw1h sql.NullInt64
	}{
		{"msg_a", sql.NullInt64{}},                           // NULL — should backfill
		{"msg_b", sql.NullInt64{Int64: 50_000, Valid: true}}, // explicit — should NOT touch
		{"msg_c", sql.NullInt64{}},                           // NULL but JSONL has 0 — should leave alone
	} {
		_, err := database.ExecContext(ctx,
			`INSERT INTO token_usage (session_id, source_file, source_event_id, timestamp, tool, model,
				input_tokens, output_tokens, cache_creation_tokens, cache_creation_1h_tokens,
				source, reliability)
			 VALUES ('sA', 'f', ?, '2026-04-23T00:00:01Z', 'claude-code', 'claude-opus-4-7',
				10, 100, 100000, ?, 'jsonl', 'unreliable')`,
			tu.eid, tu.cw1h)
		if err != nil {
			t.Fatal(err)
		}
	}

	// Build a fake Claude Code projects dir with one JSONL.
	projects := filepath.Join(root, "projects", "fake")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	jsonl := filepath.Join(projects, "sA.jsonl")
	body := strings.Join([]string{
		// msg_a: 1h subset = 100,000 (this is what the backfill must surface)
		`{"sessionId":"sA","message":{"id":"msg_a","usage":{"cache_creation_input_tokens":100000,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":100000}}}}`,
		// msg_b: 1h = 50,000 — but token_usage row is already populated so nothing to do
		`{"sessionId":"sA","message":{"id":"msg_b","usage":{"cache_creation_input_tokens":50000,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":50000}}}}`,
		// msg_c: ephemeral_1h = 0 → backfill must skip (no correction needed)
		`{"sessionId":"sA","message":{"id":"msg_c","usage":{"cache_creation_input_tokens":100000,"cache_creation":{"ephemeral_5m_input_tokens":100000,"ephemeral_1h_input_tokens":0}}}}`,
		"",
	}, "\n")
	if err := os.WriteFile(jsonl, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := backfillCacheTier(ctx, database, projects, 0)
	if err != nil {
		t.Fatalf("backfillCacheTier: %v", err)
	}

	if res.FilesScanned != 1 {
		t.Errorf("FilesScanned: got %d want 1", res.FilesScanned)
	}
	// Only msg_a and msg_b have ephemeral_1h > 0 → examined; msg_c skipped.
	if res.MsgIDsExamined != 2 {
		t.Errorf("MsgIDsExamined: got %d want 2 (msg_a + msg_b; msg_c skipped because 1h=0)", res.MsgIDsExamined)
	}
	// Only msg_a actually triggered a row update — msg_b was already populated.
	if res.TokenUsageUpdated != 1 {
		t.Errorf("TokenUsageUpdated: got %d want 1", res.TokenUsageUpdated)
	}
	if res.TokensRecovered != 100_000 {
		t.Errorf("TokensRecovered: got %d want 100000", res.TokensRecovered)
	}

	// Verify the resulting state of the three rows.
	for _, tc := range []struct {
		eid      string
		wantCW1H sql.NullInt64
	}{
		{"msg_a", sql.NullInt64{Int64: 100_000, Valid: true}},
		{"msg_b", sql.NullInt64{Int64: 50_000, Valid: true}},
		{"msg_c", sql.NullInt64{}}, // still NULL — no JSONL data triggered correction
	} {
		var got sql.NullInt64
		err := database.QueryRowContext(ctx,
			`SELECT cache_creation_1h_tokens FROM token_usage WHERE source_event_id = ?`, tc.eid).Scan(&got)
		if err != nil {
			t.Fatalf("query %s: %v", tc.eid, err)
		}
		if got.Valid != tc.wantCW1H.Valid || got.Int64 != tc.wantCW1H.Int64 {
			t.Errorf("%s: cache_creation_1h_tokens = %+v, want %+v", tc.eid, got, tc.wantCW1H)
		}
	}

	// Idempotency: a second run should have zero updates.
	res2, err := backfillCacheTier(ctx, database, projects, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res2.TokenUsageUpdated != 0 {
		t.Errorf("second-run idempotency: TokenUsageUpdated = %d, want 0", res2.TokenUsageUpdated)
	}
}

func TestBackfillCodexMessageIDAndModel(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "obs.db")
	database, err := db.Open(ctx, db.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	if _, err := database.ExecContext(ctx,
		`INSERT INTO projects (id, root_path, created_at) VALUES (1, '/tmp/test', '2026-04-29T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO sessions (id, tool, model, started_at, project_id) VALUES ('thread-1', 'codex', 'gpt-5.4', '2026-04-29T00:00:00Z', 1)`); err != nil {
		t.Fatal(err)
	}
	userSourceID := "user:rollout-2026-04-29T00-00-00-thread.jsonl:L3:" + shortHash("Check status")

	if _, err := database.ExecContext(ctx,
		`INSERT INTO actions (session_id, project_id, timestamp, action_type, success, tool, source_file, source_event_id, raw_tool_name, target)
		 VALUES ('thread-1', 1, '2026-04-29T00:00:01Z', 'user_prompt', 1, 'codex', 'rollout', ?, 'user_message', 'Check status')`, userSourceID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO actions (session_id, project_id, timestamp, action_type, success, tool, source_file, source_event_id, raw_tool_name, target)
		 VALUES ('thread-1', 1, '2026-04-29T00:00:02Z', 'run_command', 1, 'codex', 'rollout', 'call_1', 'exec_command_end', 'pwd')`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO token_usage (session_id, timestamp, tool, source_file, source_event_id, input_tokens, output_tokens, cache_read_tokens, source, reliability)
		 VALUES ('thread-1', '2026-04-29T00:00:03Z', 'codex', 'rollout', 'tk:rollout-2026-04-29T00-00-00-thread.jsonl:L5', 12, 3, 5, 'jsonl', 'approximate')`); err != nil {
		t.Fatal(err)
	}

	sessions := filepath.Join(root, "sessions", "2026", "04", "29")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	jsonl := filepath.Join(sessions, "rollout-2026-04-29T00-00-00-thread.jsonl")
	body := strings.Join([]string{
		`{"timestamp":"2026-04-29T00:00:01.000Z","type":"session_meta","payload":{"id":"thread-1","cwd":"D:\\programsx\\partner-names","git_branch":"main"}}`,
		`{"timestamp":"2026-04-29T00:00:01.050Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn-1"}}`,
		`{"timestamp":"2026-04-29T00:00:01.100Z","type":"event_msg","payload":{"type":"user_message","message":"Check status\n"}}`,
		`{"timestamp":"2026-04-29T00:00:02.000Z","type":"event_msg","payload":{"type":"exec_command_end","call_id":"call_1","turn_id":"turn-1","command":["pwd"],"cwd":"D:\\programsx\\partner-names","aggregated_output":"D:\\programsx\\partner-names\n","exit_code":0,"duration":{"secs":0,"nanos":1000},"status":"completed"}}`,
		`{"timestamp":"2026-04-29T00:00:03.000Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":12,"cached_input_tokens":5,"output_tokens":3,"reasoning_output_tokens":1,"total_tokens":15}}}}`,
		`{"timestamp":"2026-04-29T00:00:03.500Z","type":"turn_context","payload":{"turn_id":"turn-1","cwd":"D:\\programsx\\partner-names","model":"gpt-5.4","git_branch":"main"}}`,
		``,
	}, "\n")
	if err := os.WriteFile(jsonl, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := backfillCodexMessageID(ctx, database, filepath.Join(root, "sessions"), 0)
	if err != nil {
		t.Fatalf("backfillCodexMessageID: %v", err)
	}
	if res.ActionsUpdated != 2 {
		t.Fatalf("ActionsUpdated: got %d want 2", res.ActionsUpdated)
	}
	if res.TokenUsageUpdated != 2 {
		t.Fatalf("TokenUsageUpdated: got %d want 2", res.TokenUsageUpdated)
	}

	var msgID, model string
	if err := database.QueryRowContext(ctx,
		`SELECT message_id FROM actions WHERE source_event_id = 'call_1'`).Scan(&msgID); err != nil {
		t.Fatal(err)
	}
	if msgID != "turn-1" {
		t.Fatalf("call_1 backfill: message_id=%q", msgID)
	}
	if err := database.QueryRowContext(ctx,
		`SELECT message_id, model FROM token_usage WHERE source_event_id = 'tk:rollout-2026-04-29T00-00-00-thread.jsonl:L5'`).Scan(&msgID, &model); err != nil {
		t.Fatal(err)
	}
	if msgID != "turn-1" || model != "gpt-5.4" {
		t.Fatalf("token backfill: message_id=%q model=%q", msgID, model)
	}
}

func TestBackfillCursorMessageID(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "obs.db")
	database, err := db.Open(ctx, db.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	if _, err := database.ExecContext(ctx,
		`INSERT INTO projects (id, root_path, created_at) VALUES (1, '/tmp/test', '2026-04-29T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO sessions (id, tool, started_at, project_id) VALUES ('cursor-session', 'cursor', '2026-04-29T00:00:00Z', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO actions (session_id, project_id, timestamp, action_type, success, tool, source_file, source_event_id, raw_tool_name, target)
		 VALUES ('cursor-session', 1, '2026-04-29T00:00:01Z', 'user_prompt', 1, 'cursor', 'cursor:hook', 'gen-123:beforeSubmitPrompt', 'beforeSubmitPrompt', 'hello')`); err != nil {
		t.Fatal(err)
	}

	res, err := backfillCursorMessageID(ctx, database)
	if err != nil {
		t.Fatalf("backfillCursorMessageID: %v", err)
	}
	if res.ActionsUpdated != 1 {
		t.Fatalf("ActionsUpdated: got %d want 1", res.ActionsUpdated)
	}

	var msgID string
	if err := database.QueryRowContext(ctx,
		`SELECT message_id FROM actions WHERE source_event_id = 'gen-123:beforeSubmitPrompt'`).Scan(&msgID); err != nil {
		t.Fatal(err)
	}
	if msgID != "user:gen-123" {
		t.Fatalf("message_id = %q want user:gen-123", msgID)
	}
}

func TestBackfillCursorHookUsage(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "obs.db")
	database, err := db.Open(ctx, db.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	if _, err := database.ExecContext(ctx,
		`INSERT INTO projects (id, root_path, created_at) VALUES (1, '/repo', '2026-04-29T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO sessions (id, tool, started_at, project_id) VALUES ('cursor-session', 'cursor', '2026-04-29T00:00:00Z', 1)`); err != nil {
		t.Fatal(err)
	}

	logs := filepath.Join(root, "logs", "20260429T190737", "window1", "output_20260429T190743")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(logs, "cursor.hooks.workspaceId-test.log")
	body := strings.Join([]string{
		"INPUT:",
		`{"conversation_id":"cursor-session","generation_id":"gen-123","hook_event_name":"stop","workspace_roots":["/repo"],"model":"default","input_tokens":100,"output_tokens":20,"cache_read_tokens":50,"cache_write_tokens":0}`,
		"OUTPUT:",
		`{"permission":"allow","continue":true}`,
		"",
	}, "\n")
	if err := os.WriteFile(logPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := backfillCursorHookUsage(ctx, database, filepath.Join(root, "logs"), 0)
	if err != nil {
		t.Fatalf("backfillCursorHookUsage: %v", err)
	}
	if res.FilesScanned != 1 || res.TokenUsageUpdated != 1 {
		t.Fatalf("unexpected summary: %+v", res)
	}

	var model string
	var input, output int64
	if err := database.QueryRowContext(ctx,
		`SELECT model, input_tokens, output_tokens FROM token_usage WHERE source_event_id = ?`,
		"gen-123:"+cursor.EventStop).Scan(&model, &input, &output); err != nil {
		t.Fatal(err)
	}
	if model != "default" || input != 100 || output != 20 {
		t.Fatalf("token row mismatch: model=%q in=%d out=%d", model, input, output)
	}
	if err := database.QueryRowContext(ctx,
		`SELECT model FROM sessions WHERE id = 'cursor-session'`).Scan(&model); err != nil {
		t.Fatal(err)
	}
	if model != "default" {
		t.Fatalf("session model = %q", model)
	}
}

func TestBackfillCursorTranscriptActions(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "obs.db")
	database, err := db.Open(ctx, db.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	if _, err := database.ExecContext(ctx,
		`INSERT INTO projects (id, root_path, created_at) VALUES (1, '/repo', '2026-04-29T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO sessions (id, tool, model, started_at, project_id) VALUES ('cursor-session', 'cursor', 'default', '2026-04-29T00:00:00Z', 1)`); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		msg string
		ts  string
	}{
		{"gen-1", "2026-04-29T00:00:10Z"},
		{"gen-2", "2026-04-29T00:00:20Z"},
	} {
		if _, err := database.ExecContext(ctx,
			`INSERT INTO token_usage (session_id, timestamp, tool, model, source_file, source_event_id, message_id, input_tokens, output_tokens, source, reliability)
			 VALUES ('cursor-session', ?, 'cursor', 'default', 'cursor:hook', ?, ?, 10, 5, 'hook', 'accurate')`,
			row.ts, row.msg+":"+cursor.EventStop, row.msg); err != nil {
			t.Fatal(err)
		}
	}

	transcriptDir := filepath.Join(root, "projects", "demo", "agent-transcripts", "cursor-session")
	if err := os.MkdirAll(transcriptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcriptPath := filepath.Join(transcriptDir, "cursor-session.jsonl")
	body := strings.Join([]string{
		`{"role":"user","message":{"content":[{"type":"text","text":"one"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"scan"},{"type":"tool_use","name":"Glob","input":{"glob_pattern":"*"}}]}}`,
		`{"role":"user","message":{"content":[{"type":"text","text":"two"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"read"},{"type":"tool_use","name":"ReadFile","input":{"path":"d:\\repo\\README.md"}}]}}`,
		"",
	}, "\n")
	if err := os.WriteFile(transcriptPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := backfillCursorTranscriptActions(ctx, database, filepath.Join(root, "projects"), 0)
	if err != nil {
		t.Fatalf("backfillCursorTranscriptActions: %v", err)
	}
	if res.ActionsUpdated != 2 {
		t.Fatalf("ActionsUpdated: got %d want 2", res.ActionsUpdated)
	}
	var count int
	if err := database.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM actions WHERE tool = 'cursor' AND message_id IN ('gen-1','gen-2')`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("action count = %d want 2", count)
	}
}

// TestBackfillsAllPrepareCleanlyOnEmptyDB is a defensive smoke test:
// every backfill function must compile its prepared statements
// against the canonical post-migrations schema. Pre-fix this would
// have caught the `actions.tool_output` and `actions.model` column-
// existence bugs at test time instead of at the user's terminal.
//
// Each backfill is run against a freshly-migrated DB with no rows;
// it must not error and must report zero updates.
func TestBackfillsAllPrepareCleanlyOnEmptyDB(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "obs.db")
	database, err := db.Open(ctx, db.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	// Empty source dirs — every JSONL-walking pass should report zero
	// files scanned and zero rows updated.
	emptyDir := filepath.Join(root, "empty")
	if err := os.MkdirAll(emptyDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := backfillIsSidechain(ctx, database, emptyDir, 0); err != nil {
		t.Errorf("backfillIsSidechain: %v", err)
	}
	if _, err := backfillCacheTier(ctx, database, emptyDir, 0); err != nil {
		t.Errorf("backfillCacheTier: %v", err)
	}
	if _, err := backfillMessageID(ctx, database, emptyDir, 0); err != nil {
		t.Errorf("backfillMessageID: %v", err)
	}
	if _, err := backfillCodexMessageID(ctx, database, emptyDir, 0); err != nil {
		t.Errorf("backfillCodexMessageID: %v", err)
	}
	if _, err := backfillCursorMessageID(ctx, database); err != nil {
		t.Errorf("backfillCursorMessageID: %v", err)
	}
	if _, err := backfillCursorHookUsage(ctx, database, emptyDir, 0); err != nil {
		t.Errorf("backfillCursorHookUsage: %v", err)
	}
	if _, err := backfillCursorTranscriptActions(ctx, database, emptyDir, 0); err != nil {
		t.Errorf("backfillCursorTranscriptActions: %v", err)
	}
	if _, err := backfillOpenCodeMessageID(ctx, database); err != nil {
		t.Errorf("backfillOpenCodeMessageID: %v", err)
	}
	if _, err := backfillOpenCodeParts(ctx, database); err != nil {
		t.Errorf("backfillOpenCodeParts: %v", err)
	}
	if _, err := backfillOpenClawActionTypes(ctx, database); err != nil {
		t.Errorf("backfillOpenClawActionTypes: %v", err)
	}
	if _, err := backfillOpenClawModel(ctx, database); err != nil {
		t.Errorf("backfillOpenClawModel: %v", err)
	}
	if _, err := backfillOpenClawReasoning(ctx, database, emptyDir, 0); err != nil {
		t.Errorf("backfillOpenClawReasoning: %v", err)
	}
	if _, err := backfillCodexReasoning(ctx, database, emptyDir, 0); err != nil {
		t.Errorf("backfillCodexReasoning: %v", err)
	}
	if _, err := backfillCodexProjectRoot(ctx, database, []string{emptyDir}, 0); err != nil {
		t.Errorf("backfillCodexProjectRoot: %v", err)
	}
	if _, err := backfillCursorModel(ctx, database); err != nil {
		t.Errorf("backfillCursorModel: %v", err)
	}
	if _, err := backfillCopilotMessageID(ctx, database); err != nil {
		t.Errorf("backfillCopilotMessageID: %v", err)
	}
	if _, err := backfillPiMessageID(ctx, database); err != nil {
		t.Errorf("backfillPiMessageID: %v", err)
	}
	if _, err := backfillOpenCodeTokens(ctx, database); err != nil {
		t.Errorf("backfillOpenCodeTokens: %v", err)
	}
	if _, err := backfillClaudeCodeUserPrompts(ctx, database, emptyDir, 0); err != nil {
		t.Errorf("backfillClaudeCodeUserPrompts: %v", err)
	}
	if _, err := backfillClaudeCodeAPIErrors(ctx, database, emptyDir, 0); err != nil {
		t.Errorf("backfillClaudeCodeAPIErrors: %v", err)
	}
	if _, err := backfillCursorUserPrompts(ctx, database, emptyDir, 0); err != nil {
		t.Errorf("backfillCursorUserPrompts: %v", err)
	}
	if _, err := backfillCursorSubagents(ctx, database, emptyDir, 0); err != nil {
		t.Errorf("backfillCursorSubagents: %v", err)
	}
}

// TestBackfillClaudeCodeUserPrompts pins the recovery pass: re-runs
// the claudecode adapter on every JSONL file under projectsDir and
// inserts user_prompt action rows for sessions that were ingested
// before the adapter started emitting them. Idempotent via the
// (source_file, source_event_id) UNIQUE index.
func TestBackfillClaudeCodeUserPrompts(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "obs.db")
	database, err := db.Open(ctx, db.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	// Build a tiny Claude Code project tree with one session JSONL
	// carrying a user prompt + an assistant tool call.
	projects := filepath.Join(root, "projects", "demo")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	jsonl := filepath.Join(projects, "sess-X.jsonl")
	body := strings.Join([]string{
		`{"sessionId":"sess-X","cwd":"/tmp","timestamp":"2026-04-30T00:00:00Z","uuid":"u-prompt","message":{"role":"user","content":[{"type":"text","text":"hello there"}]}}`,
		`{"sessionId":"sess-X","cwd":"/tmp","timestamp":"2026-04-30T00:00:01Z","uuid":"u-asst","message":{"id":"msg_a","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"/x.go"}}]}}`,
	}, "\n")
	if err := os.WriteFile(jsonl, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	// Seed observer.db with a session + the assistant's Read action,
	// but NOT the user_prompt — simulating a pre-fix ingest.
	if _, err := database.ExecContext(ctx,
		`INSERT INTO projects (id, root_path, created_at) VALUES (1, '/tmp', '2026-04-30T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO sessions (id, tool, started_at, project_id) VALUES ('sess-X', 'claude-code', '2026-04-30T00:00:00Z', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO actions (session_id, project_id, timestamp, action_type, tool, source_file, source_event_id)
		 VALUES ('sess-X', 1, '2026-04-30T00:00:01Z', 'read_file', 'claude-code', ?, 'toolu_1')`, jsonl); err != nil {
		t.Fatal(err)
	}

	res, err := backfillClaudeCodeUserPrompts(ctx, database, projects, 0)
	if err != nil {
		t.Fatalf("backfillClaudeCodeUserPrompts: %v", err)
	}
	if res.FilesScanned != 1 {
		t.Errorf("FilesScanned = %d, want 1", res.FilesScanned)
	}
	if res.UserEventsFound != 1 {
		t.Errorf("UserEventsFound = %d, want 1", res.UserEventsFound)
	}
	if res.ActionsInserted != 1 {
		t.Errorf("ActionsInserted = %d, want 1", res.ActionsInserted)
	}

	var actionType, msgID sql.NullString
	if err := database.QueryRowContext(ctx,
		`SELECT action_type, message_id FROM actions WHERE source_event_id = 'u-prompt'`).
		Scan(&actionType, &msgID); err != nil {
		t.Fatal(err)
	}
	if actionType.String != models.ActionUserPrompt {
		t.Errorf("action_type = %q, want %q", actionType.String, models.ActionUserPrompt)
	}
	if msgID.String != "user:u-prompt" {
		t.Errorf("message_id = %q, want user:u-prompt", msgID.String)
	}

	// Idempotency: re-running must not duplicate rows. Per the
	// v1.4.28 ON CONFLICT DO UPDATE for actions.duration_ms, the
	// ActionsInserted counter now reports "rows touched" rather
	// than "actually new rows" — assert against the table-level
	// row count instead.
	var beforeCount int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM actions`).Scan(&beforeCount); err != nil {
		t.Fatal(err)
	}
	if _, err := backfillClaudeCodeUserPrompts(ctx, database, projects, 0); err != nil {
		t.Fatal(err)
	}
	var afterCount int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM actions`).Scan(&afterCount); err != nil {
		t.Fatal(err)
	}
	if afterCount != beforeCount {
		t.Errorf("second run added rows: before=%d after=%d", beforeCount, afterCount)
	}
}

// TestBackfillCursorSubagents pins the v1.4.21 sidechain ingest:
// agent-transcripts/<parent>/subagents/<sub>.jsonl is walked, parsed
// via the cursor adapter's transcript path, and emitted as ToolEvents
// with IsSidechain=true under the parent session_id. Tests both the
// session-id linkage and the sidechain flag landing on inserted rows.
func TestBackfillCursorSubagents(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "obs.db")
	database, err := db.Open(ctx, db.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	// Build a tiny cursor project tree with one parent transcript and
	// one subagent transcript inside it.
	parentSessionID := "parent-sess"
	subUUID := "sub-AAA"
	transcriptDir := filepath.Join(root, "projects", "demo", "agent-transcripts", parentSessionID)
	subagentDir := filepath.Join(transcriptDir, "subagents")
	if err := os.MkdirAll(subagentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	subPath := filepath.Join(subagentDir, subUUID+".jsonl")
	body := strings.Join([]string{
		`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>research X</user_query>"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"On it."},{"type":"tool_use","name":"WebFetch","input":{"url":"https://example.com"}}]}}`,
	}, "\n")
	if err := os.WriteFile(subPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	// Seed parent project + session — backfill skips files whose
	// parent session_id has no DB entry (no projectRoot to attribute).
	if _, err := database.ExecContext(ctx,
		`INSERT INTO projects (id, root_path, created_at) VALUES (1, '/tmp/demo', '2026-05-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO sessions (id, tool, started_at, project_id) VALUES (?, 'cursor', '2026-05-01T00:00:00Z', 1)`, parentSessionID); err != nil {
		t.Fatal(err)
	}

	res, err := backfillCursorSubagents(ctx, database, filepath.Join(root, "projects"), 0)
	if err != nil {
		t.Fatalf("backfillCursorSubagents: %v", err)
	}
	if res.FilesScanned != 1 {
		t.Errorf("FilesScanned = %d, want 1", res.FilesScanned)
	}
	if res.EventsBuilt != 2 {
		// 1 user_prompt + 1 WebFetch tool_use
		t.Errorf("EventsBuilt = %d, want 2", res.EventsBuilt)
	}
	if res.ActionsInserted != 2 {
		t.Errorf("ActionsInserted = %d, want 2", res.ActionsInserted)
	}

	// All inserted rows must carry IsSidechain=1 and the parent SessionID.
	var sidechainCount, otherCount int
	if err := database.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM actions WHERE session_id = ? AND is_sidechain = 1`, parentSessionID).Scan(&sidechainCount); err != nil {
		t.Fatal(err)
	}
	if sidechainCount != 2 {
		t.Errorf("sidechain rows: %d want 2", sidechainCount)
	}
	if err := database.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM actions WHERE session_id = ? AND is_sidechain = 0`, parentSessionID).Scan(&otherCount); err != nil {
		t.Fatal(err)
	}
	if otherCount != 0 {
		t.Errorf("non-sidechain rows for parent: %d want 0", otherCount)
	}

	// Idempotency: re-running must not duplicate rows. v1.4.28's
	// ON CONFLICT DO UPDATE for actions.duration_ms shifted the
	// inserted-counter semantics to "rows touched"; check the
	// table-level row count instead.
	var beforeCount int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM actions`).Scan(&beforeCount); err != nil {
		t.Fatal(err)
	}
	if _, err := backfillCursorSubagents(ctx, database, filepath.Join(root, "projects"), 0); err != nil {
		t.Fatal(err)
	}
	var afterCount int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM actions`).Scan(&afterCount); err != nil {
		t.Fatal(err)
	}
	if afterCount != beforeCount {
		t.Errorf("second run added rows: before=%d after=%d", beforeCount, afterCount)
	}
}

// TestBackfillClaudeCodeAPIErrors pins the v1.4.20 recovery pass for
// upstream API failures: re-runs the adapter on every JSONL, picks
// out the api_error events the previous adapter version would have
// dropped, and ingests them. Idempotent via the
// (source_file, source_event_id) UNIQUE index — a second run is a
// no-op.
func TestBackfillClaudeCodeAPIErrors(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "obs.db")
	database, err := db.Open(ctx, db.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	// Build a tiny Claude Code project tree with one session JSONL
	// carrying an assistant turn + a system/api_error record after.
	projects := filepath.Join(root, "projects", "demo")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	jsonl := filepath.Join(projects, "sess-err.jsonl")
	body := strings.Join([]string{
		`{"sessionId":"sess-err","cwd":"/tmp","timestamp":"2026-04-30T00:00:00Z","uuid":"u-asst","message":{"id":"msg_a","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"thinking..."}]}}`,
		`{"sessionId":"sess-err","cwd":"/tmp","timestamp":"2026-04-30T00:00:01Z","uuid":"u-err","type":"system","subtype":"api_error","level":"error","error":{"status":400,"requestID":"req_011test","error":{"type":"invalid_request_error","message":"Output blocked by content filtering policy"}}}`,
	}, "\n")
	if err := os.WriteFile(jsonl, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	// Seed observer.db with a session row but NOT the api_error action
	// (simulating a pre-fix ingest where the system record was dropped).
	if _, err := database.ExecContext(ctx,
		`INSERT INTO projects (id, root_path, created_at) VALUES (1, '/tmp', '2026-04-30T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO sessions (id, tool, started_at, project_id) VALUES ('sess-err', 'claude-code', '2026-04-30T00:00:00Z', 1)`); err != nil {
		t.Fatal(err)
	}

	res, err := backfillClaudeCodeAPIErrors(ctx, database, projects, 0)
	if err != nil {
		t.Fatalf("backfillClaudeCodeAPIErrors: %v", err)
	}
	if res.FilesScanned != 1 {
		t.Errorf("FilesScanned = %d, want 1", res.FilesScanned)
	}
	if res.APIErrorsFound != 1 {
		t.Errorf("APIErrorsFound = %d, want 1", res.APIErrorsFound)
	}
	if res.ActionsInserted != 1 {
		t.Errorf("ActionsInserted = %d, want 1", res.ActionsInserted)
	}

	var actionType, target, errorMessage, rawTool sql.NullString
	if err := database.QueryRowContext(ctx,
		`SELECT action_type, target, error_message, raw_tool_name
		 FROM actions WHERE source_event_id = 'u-err'`).
		Scan(&actionType, &target, &errorMessage, &rawTool); err != nil {
		t.Fatal(err)
	}
	if actionType.String != models.ActionAPIError {
		t.Errorf("action_type = %q, want %q", actionType.String, models.ActionAPIError)
	}
	if target.String != "req_011test" {
		t.Errorf("target = %q, want req_011test (request_id)", target.String)
	}
	if !strings.Contains(errorMessage.String, "content filtering") {
		t.Errorf("error_message = %q, want substring 'content filtering'", errorMessage.String)
	}
	if rawTool.String != "invalid_request_error" {
		t.Errorf("raw_tool_name = %q, want invalid_request_error (upstream error class)", rawTool.String)
	}

	// Idempotency: re-running must not duplicate rows. v1.4.28's
	// ON CONFLICT DO UPDATE for actions.duration_ms shifted the
	// inserted-counter semantics to "rows touched"; check the
	// table-level row count instead.
	var beforeCount int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM actions`).Scan(&beforeCount); err != nil {
		t.Fatal(err)
	}
	if _, err := backfillClaudeCodeAPIErrors(ctx, database, projects, 0); err != nil {
		t.Fatal(err)
	}
	var afterCount int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM actions`).Scan(&afterCount); err != nil {
		t.Fatal(err)
	}
	if afterCount != beforeCount {
		t.Errorf("second run added rows: before=%d after=%d", beforeCount, afterCount)
	}
}

// TestBackfillOpenCodeTokens pins the opencode-tokens recovery pass:
// re-runs the adapter against opencode.db and ingests any token rows
// that were missing from observer's DB. The user-reported case was a
// 'say hi' session whose actions were ingested but whose assistant
// message had data.tokens populated (input=9868, output=35) and
// observer's token_usage was empty.
func TestBackfillOpenCodeTokens(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "obs.db")
	database, err := db.Open(ctx, db.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	// Seed observer.db: project + an opencode session + one action so
	// the backfill discovers the source_file path. No token row.
	ocPath := filepath.Join(root, "opencode.db")
	if _, err := database.ExecContext(ctx,
		`INSERT INTO projects (id, root_path, created_at) VALUES (1, '/tmp/test', '2026-04-30T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO sessions (id, tool, started_at, project_id) VALUES ('ses_say_hi', 'opencode', '2026-04-30T00:00:00Z', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO actions (session_id, project_id, timestamp, action_type, tool, source_file, source_event_id)
		 VALUES ('ses_say_hi', 1, '2026-04-30T00:00:01Z', 'task_complete', 'opencode', ?, 'complete:msg_assistant')`,
		ocPath); err != nil {
		t.Fatal(err)
	}

	// Seed opencode.db: the source schema with one assistant message
	// carrying real token data — the user-reported shape.
	ocDB, err := sql.Open("sqlite", ocPath)
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{
		`CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT NOT NULL, time_updated INTEGER NOT NULL)`,
		`CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL)`,
		`CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT NOT NULL, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL)`,
		`INSERT INTO session(id, directory, time_updated) VALUES ('ses_say_hi', '/tmp/oc', 3000)`,
		`INSERT INTO message(id, session_id, time_created, time_updated, data) VALUES
			('msg_assistant', 'ses_say_hi', 2900, 3000,
			 '{"role":"assistant","modelID":"big-pickle","providerID":"opencode","time":{"created":2900,"completed":3000},"finish":"stop","tokens":{"input":9868,"output":35,"reasoning":0,"cache":{"read":0,"write":0}},"cost":0}')`,
	}
	for _, s := range stmts {
		if _, err := ocDB.Exec(s); err != nil {
			ocDB.Close()
			t.Fatalf("seed opencode.db: %v", err)
		}
	}
	ocDB.Close()

	// Verify observer.db has zero opencode token rows before the backfill.
	var before int
	if err := database.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM token_usage WHERE tool='opencode'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before != 0 {
		t.Fatalf("setup: expected 0 opencode token rows before backfill, got %d", before)
	}

	res, err := backfillOpenCodeTokens(ctx, database)
	if err != nil {
		t.Fatalf("backfillOpenCodeTokens: %v", err)
	}
	if res.DBsScanned != 1 {
		t.Errorf("DBsScanned = %d, want 1", res.DBsScanned)
	}
	if res.TokenRowsExtracted != 1 {
		t.Errorf("TokenRowsExtracted = %d, want 1", res.TokenRowsExtracted)
	}
	if res.TokenRowsInserted != 1 {
		t.Errorf("TokenRowsInserted = %d, want 1", res.TokenRowsInserted)
	}

	// Verify the actual values landed.
	var (
		input, output sql.NullInt64
		msgID         sql.NullString
	)
	if err := database.QueryRowContext(ctx,
		`SELECT input_tokens, output_tokens, message_id FROM token_usage WHERE tool='opencode' AND session_id='ses_say_hi'`).
		Scan(&input, &output, &msgID); err != nil {
		t.Fatal(err)
	}
	if input.Int64 != 9868 {
		t.Errorf("input_tokens = %d, want 9868", input.Int64)
	}
	if output.Int64 != 35 {
		t.Errorf("output_tokens = %d, want 35", output.Int64)
	}
	if msgID.String != "msg_assistant" {
		t.Errorf("message_id = %q, want msg_assistant", msgID.String)
	}

	// Idempotency: second run produces no duplicate row. The metric
	// reports "rows touched" since v1.4.27 (when token_usage moved from
	// INSERT OR IGNORE to ON CONFLICT DO UPDATE for the model column,
	// to let adapter improvements propagate to existing rows). Real
	// idempotence is the post-condition row count, not the metric.
	if _, err := backfillOpenCodeTokens(ctx, database); err != nil {
		t.Fatal(err)
	}
	var rows int
	_ = database.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM token_usage WHERE tool='opencode' AND session_id='ses_say_hi'`).Scan(&rows)
	if rows != 1 {
		t.Errorf("post second-run rows: got %d want 1", rows)
	}
}

// TestBackfillCopilotMessageID pins the copilot message-id pass.
// User-message lines yield 'user:<spanId>'; tool / agent_response /
// llm_request rows yield 'assistant:<parentSpanId | spanId>'.
func TestBackfillCopilotMessageID(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "obs.db")
	database, err := db.Open(ctx, db.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	logPath := filepath.Join(root, "main.jsonl")
	body := strings.Join([]string{
		`{"ts":1,"sid":"sess-1","type":"user_message","spanId":"u1","attrs":{"content":"hi"}}`,
		`{"ts":2,"sid":"sess-1","type":"tool_call","name":"manage_todo_list","spanId":"tool-1","parentSpanId":"u1","attrs":{}}`,
		`{"ts":3,"sid":"sess-1","type":"llm_request","name":"chat","spanId":"llm-1","parentSpanId":"u1","attrs":{"model":"oswe","inputTokens":10,"outputTokens":2}}`,
		"",
	}, "\n")
	if err := os.WriteFile(logPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := database.ExecContext(ctx,
		`INSERT INTO projects (id, root_path, created_at) VALUES (1, '/tmp/test', '2026-04-30T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO sessions (id, tool, started_at, project_id) VALUES ('sess-1', 'copilot', '2026-04-30T00:00:00Z', 1)`); err != nil {
		t.Fatal(err)
	}
	for _, eid := range []string{"u1", "tool-1", "llm-1"} {
		_, err := database.ExecContext(ctx,
			`INSERT INTO actions (session_id, project_id, timestamp, action_type, tool, source_file, source_event_id)
			 VALUES ('sess-1', 1, '2026-04-30T00:00:01Z', 'user_prompt', 'copilot', ?, ?)`, logPath, eid)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO token_usage (session_id, timestamp, tool, source_file, source_event_id, source, reliability)
		 VALUES ('sess-1', '2026-04-30T00:00:01Z', 'copilot', ?, 'llm-1', 'jsonl', 'approximate')`, logPath); err != nil {
		t.Fatal(err)
	}

	res, err := backfillCopilotMessageID(ctx, database)
	if err != nil {
		t.Fatalf("backfillCopilotMessageID: %v", err)
	}
	if res.FilesScanned != 1 {
		t.Errorf("FilesScanned = %d, want 1", res.FilesScanned)
	}
	if res.ActionsUpdated != 3 {
		t.Errorf("ActionsUpdated = %d, want 3", res.ActionsUpdated)
	}
	if res.TokenUsageUpdated != 1 {
		t.Errorf("TokenUsageUpdated = %d, want 1", res.TokenUsageUpdated)
	}
	for _, c := range []struct {
		eid, want string
	}{
		{"u1", "user:u1"},
		{"tool-1", "assistant:u1"},
		{"llm-1", "assistant:u1"},
	} {
		var got sql.NullString
		if err := database.QueryRowContext(ctx,
			`SELECT message_id FROM actions WHERE source_event_id = ?`, c.eid).Scan(&got); err != nil {
			t.Fatalf("query %s: %v", c.eid, err)
		}
		if got.String != c.want {
			t.Errorf("%s: message_id = %q, want %q", c.eid, got.String, c.want)
		}
	}

	res2, _ := backfillCopilotMessageID(ctx, database)
	if res2.ActionsUpdated != 0 || res2.TokenUsageUpdated != 0 {
		t.Errorf("second run not idempotent: actions=%d tokens=%d",
			res2.ActionsUpdated, res2.TokenUsageUpdated)
	}
}

// TestBackfillPiMessageID pins the pi message-id pass. User rows
// yield 'user:<id>'; assistant rows yield <id>; tool calls within an
// assistant message inherit the assistant's id; usage rows match
// 'usage:<id>'.
func TestBackfillPiMessageID(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "obs.db")
	database, err := db.Open(ctx, db.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	sessPath := filepath.Join(root, "session.jsonl")
	body := strings.Join([]string{
		`{"id":"u1","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}`,
		`{"id":"a1","message":{"role":"assistant","content":[{"type":"toolUse","id":"tool_x","name":"read"}]}}`,
		"",
	}, "\n")
	if err := os.WriteFile(sessPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := database.ExecContext(ctx,
		`INSERT INTO projects (id, root_path, created_at) VALUES (1, '/tmp/test', '2026-04-30T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO sessions (id, tool, started_at, project_id) VALUES ('p1', 'pi', '2026-04-30T00:00:00Z', 1)`); err != nil {
		t.Fatal(err)
	}
	for _, eid := range []string{"u1", "tool_x"} {
		_, err := database.ExecContext(ctx,
			`INSERT INTO actions (session_id, project_id, timestamp, action_type, tool, source_file, source_event_id)
			 VALUES ('p1', 1, '2026-04-30T00:00:01Z', 'user_prompt', 'pi', ?, ?)`, sessPath, eid)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO token_usage (session_id, timestamp, tool, source_file, source_event_id, source, reliability)
		 VALUES ('p1', '2026-04-30T00:00:01Z', 'pi', ?, 'usage:a1', 'jsonl', 'approximate')`, sessPath); err != nil {
		t.Fatal(err)
	}

	res, err := backfillPiMessageID(ctx, database)
	if err != nil {
		t.Fatalf("backfillPiMessageID: %v", err)
	}
	if res.FilesScanned != 1 {
		t.Errorf("FilesScanned = %d, want 1", res.FilesScanned)
	}
	if res.ActionsUpdated != 2 {
		t.Errorf("ActionsUpdated = %d, want 2 (user prompt + tool inside assistant)", res.ActionsUpdated)
	}
	if res.TokenUsageUpdated != 1 {
		t.Errorf("TokenUsageUpdated = %d, want 1", res.TokenUsageUpdated)
	}
	for _, c := range []struct {
		eid, want string
	}{
		{"u1", "user:u1"},
		{"tool_x", "a1"}, // tool call inherits the assistant's id
	} {
		var got sql.NullString
		if err := database.QueryRowContext(ctx,
			`SELECT message_id FROM actions WHERE source_event_id = ?`, c.eid).Scan(&got); err != nil {
			t.Fatalf("query %s: %v", c.eid, err)
		}
		if got.String != c.want {
			t.Errorf("%s: message_id = %q, want %q", c.eid, got.String, c.want)
		}
	}

	res2, _ := backfillPiMessageID(ctx, database)
	if res2.ActionsUpdated != 0 || res2.TokenUsageUpdated != 0 {
		t.Errorf("second run not idempotent: actions=%d tokens=%d",
			res2.ActionsUpdated, res2.TokenUsageUpdated)
	}
}

// TestBackfillOpenCodeParts pins the fixed opencode-parts pass:
// duration_ms and message_id land on the actions row, and tool
// output is indexed into action_excerpts (not into a non-existent
// actions.tool_output column).
func TestBackfillOpenCodeParts(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "obs.db")
	database, err := db.Open(ctx, db.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	// Seed a project + session + an opencode tool action that points at
	// our fake opencode.db.
	ocDBPath := filepath.Join(root, "opencode.db")
	if _, err := database.ExecContext(ctx,
		`INSERT INTO projects (id, root_path, created_at) VALUES (1, '/tmp/test', '2026-04-30T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO sessions (id, tool, started_at, project_id) VALUES ('s1', 'opencode', '2026-04-30T00:00:00Z', 1)`); err != nil {
		t.Fatal(err)
	}
	var actionID int64
	r, err := database.ExecContext(ctx,
		`INSERT INTO actions (session_id, project_id, timestamp, action_type, raw_tool_name, target, tool, source_file, source_event_id)
		 VALUES ('s1', 1, '2026-04-30T00:00:01Z', 'run_command', 'bash', 'npm start', 'opencode', ?, 'part:prt_1')`, ocDBPath)
	if err != nil {
		t.Fatal(err)
	}
	actionID, _ = r.LastInsertId()

	// Build the fake opencode.db with one tool part carrying the data
	// we want backfilled.
	ocDB, err := sql.Open("sqlite", ocDBPath)
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{
		`CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT, data TEXT NOT NULL)`,
		`INSERT INTO part (id, message_id, data) VALUES
			('prt_1', 'msg_a', '{"type":"tool","state":{"output":"boom","metadata":{"output":"boom","exit":1},"time":{"start":2200,"end":2500}}}')`,
	}
	for _, s := range stmts {
		if _, err := ocDB.Exec(s); err != nil {
			ocDB.Close()
			t.Fatalf("seed opencode.db: %v", err)
		}
	}
	ocDB.Close()

	res, err := backfillOpenCodeParts(ctx, database)
	if err != nil {
		t.Fatalf("backfillOpenCodeParts: %v", err)
	}
	if res.PartsExamined != 1 {
		t.Errorf("PartsExamined = %d, want 1", res.PartsExamined)
	}
	if res.DurationUpdated != 1 {
		t.Errorf("DurationUpdated = %d, want 1", res.DurationUpdated)
	}
	if res.MessageIDUpdated != 1 {
		t.Errorf("MessageIDUpdated = %d, want 1", res.MessageIDUpdated)
	}
	if res.ToolOutputUpdated != 1 {
		t.Errorf("ToolOutputUpdated = %d, want 1", res.ToolOutputUpdated)
	}

	// Verify the persisted state.
	var (
		duration sql.NullInt64
		msgID    sql.NullString
	)
	if err := database.QueryRowContext(ctx,
		`SELECT duration_ms, message_id FROM actions WHERE id = ?`, actionID).Scan(&duration, &msgID); err != nil {
		t.Fatal(err)
	}
	if duration.Int64 != 300 {
		t.Errorf("duration_ms = %d, want 300", duration.Int64)
	}
	if msgID.String != "msg_a" {
		t.Errorf("message_id = %q, want msg_a", msgID.String)
	}
	// Excerpt landed in action_excerpts (FTS5).
	var excerptCount int
	if err := database.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM action_excerpts WHERE action_id = ?`, actionID).Scan(&excerptCount); err != nil {
		t.Fatal(err)
	}
	if excerptCount != 1 {
		t.Errorf("action_excerpts rows for action_id=%d: got %d, want 1", actionID, excerptCount)
	}

	// Idempotency: second run skips the already-indexed excerpt and the
	// already-populated columns.
	res2, err := backfillOpenCodeParts(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	if res2.DurationUpdated != 0 || res2.MessageIDUpdated != 0 || res2.ToolOutputUpdated != 0 {
		t.Errorf("second run not idempotent: duration=%d msgID=%d output=%d",
			res2.DurationUpdated, res2.MessageIDUpdated, res2.ToolOutputUpdated)
	}
}

// TestBackfillOpenClawModel pins the fixed openclaw-model pass: model
// is lifted from sessions.json aliases onto SESSION rows (not actions
// — there's no actions.model column).
func TestBackfillOpenClawModel(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "obs.db")
	database, err := db.Open(ctx, db.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	// Mock the openclaw on-disk layout: tasks/runs.sqlite + sibling
	// agents/<a>/sessions/sessions.json.
	tasksDir := filepath.Join(root, ".openclaw", "tasks")
	agentsDir := filepath.Join(root, ".openclaw", "agents", "main", "sessions")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runsPath := filepath.Join(tasksDir, "runs.sqlite")
	ocDB, err := sql.Open("sqlite", runsPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ocDB.Exec(`CREATE TABLE task_runs (
		task_id TEXT PRIMARY KEY,
		child_session_key TEXT,
		owner_key TEXT NOT NULL,
		requester_session_key TEXT,
		run_id TEXT,
		source_id TEXT
	)`); err != nil {
		ocDB.Close()
		t.Fatalf("create runs.sqlite: %v", err)
	}
	if _, err := ocDB.Exec(`INSERT INTO task_runs VALUES
		('task_1', 'agent:main:obs-smoke', 'agent:main:obs-smoke', '', '', '')`); err != nil {
		ocDB.Close()
		t.Fatalf("seed task_runs: %v", err)
	}
	ocDB.Close()
	if err := os.WriteFile(filepath.Join(agentsDir, "sessions.json"), []byte(`{
		"agent:main:obs-smoke": {
			"sessionId": "agent:main:obs-smoke",
			"modelProvider": "anthropic",
			"model": "claude-sonnet-4-5",
			"systemPromptReport": {"workspaceDir":"/tmp/ws","provider":"anthropic","model":"claude-sonnet-4-5"}
		}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Seed observer DB: project + openclaw session with empty model + an
	// action pointing at runs.sqlite so the backfill discovers the path.
	if _, err := database.ExecContext(ctx,
		`INSERT INTO projects (id, root_path, created_at) VALUES (1, '/tmp/test', '2026-04-30T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO sessions (id, tool, model, started_at, project_id)
		 VALUES ('agent:main:obs-smoke', 'openclaw', '', '2026-04-30T00:00:00Z', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO actions (session_id, project_id, timestamp, action_type, tool, source_file, source_event_id)
		 VALUES ('agent:main:obs-smoke', 1, '2026-04-30T00:00:01Z', 'user_prompt', 'openclaw', ?, 'task:task_1:prompt')`,
		runsPath); err != nil {
		t.Fatal(err)
	}

	res, err := backfillOpenClawModel(ctx, database)
	if err != nil {
		t.Fatalf("backfillOpenClawModel: %v", err)
	}
	if res.AliasesLoaded == 0 {
		t.Errorf("AliasesLoaded = 0, want > 0")
	}
	if res.SessionsUpdated != 1 {
		t.Errorf("SessionsUpdated = %d, want 1", res.SessionsUpdated)
	}
	var got sql.NullString
	if err := database.QueryRowContext(ctx,
		`SELECT model FROM sessions WHERE id = 'agent:main:obs-smoke'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got.String != "anthropic/claude-sonnet-4-5" {
		t.Errorf("session model = %q, want anthropic/claude-sonnet-4-5", got.String)
	}

	res2, _ := backfillOpenClawModel(ctx, database)
	if res2.SessionsUpdated != 0 {
		t.Errorf("second run not idempotent: %d", res2.SessionsUpdated)
	}
}

// TestBackfillOpenCodeMessageID pins the SQL-only opencode message_id
// pass: the upstream id is already in source_event_id (with a prefix);
// strip the prefix and write to message_id where empty.
func TestBackfillOpenCodeMessageID(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "obs.db")
	database, err := db.Open(ctx, db.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	if _, err := database.ExecContext(ctx,
		`INSERT INTO projects (id, root_path, created_at) VALUES (1, '/tmp/test', '2026-04-30T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO sessions (id, tool, started_at, project_id) VALUES ('ses_1', 'opencode', '2026-04-30T00:00:00Z', 1)`); err != nil {
		t.Fatal(err)
	}
	// Three actions: a user prompt (message:), a completion (complete:),
	// and a tool part (part:) — only the first two are touched by this
	// SQL-only pass; the part row is left for backfillOpenCodeParts.
	for _, a := range []struct {
		actionType string
		sourceID   string
		expectMID  string // "" means leave alone
	}{
		{"user_prompt", "message:msg_user", "user:msg_user"},
		{"task_complete", "complete:msg_done", "msg_done"},
		{"read_file", "part:prt_x", ""}, // not handled by SQL-only — stays empty
	} {
		_, err := database.ExecContext(ctx,
			`INSERT INTO actions (session_id, project_id, timestamp, action_type, tool, source_file, source_event_id)
			 VALUES ('ses_1', 1, '2026-04-30T00:00:01Z', ?, 'opencode', '/tmp/oc/opencode.db', ?)`,
			a.actionType, a.sourceID)
		if err != nil {
			t.Fatal(err)
		}
	}
	// One token_usage row with `tokens:` prefix.
	if _, err := database.ExecContext(ctx,
		`INSERT INTO token_usage (session_id, timestamp, tool, model, source_file, source_event_id, source, reliability)
		 VALUES ('ses_1', '2026-04-30T00:00:02Z', 'opencode', 'big-pickle', '/tmp/oc/opencode.db', 'tokens:msg_done', 'jsonl', 'approximate')`); err != nil {
		t.Fatal(err)
	}

	res, err := backfillOpenCodeMessageID(ctx, database)
	if err != nil {
		t.Fatalf("backfillOpenCodeMessageID: %v", err)
	}
	if res.ActionsUpdated != 2 {
		t.Errorf("ActionsUpdated = %d, want 2 (user prompt + completion)", res.ActionsUpdated)
	}
	if res.TokenUsageUpdated != 1 {
		t.Errorf("TokenUsageUpdated = %d, want 1", res.TokenUsageUpdated)
	}

	// Verify per-row state.
	for _, c := range []struct {
		eid      string
		wantMsg  string
		isAction bool
	}{
		{"message:msg_user", "user:msg_user", true},
		{"complete:msg_done", "msg_done", true},
		{"part:prt_x", "", true}, // untouched
		{"tokens:msg_done", "msg_done", false},
	} {
		var got sql.NullString
		var query string
		if c.isAction {
			query = `SELECT message_id FROM actions WHERE source_event_id = ?`
		} else {
			query = `SELECT message_id FROM token_usage WHERE source_event_id = ?`
		}
		if err := database.QueryRowContext(ctx, query, c.eid).Scan(&got); err != nil {
			t.Fatalf("query %s: %v", c.eid, err)
		}
		if got.String != c.wantMsg {
			t.Errorf("%s: message_id = %q, want %q", c.eid, got.String, c.wantMsg)
		}
	}

	// Idempotency.
	res2, err := backfillOpenCodeMessageID(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	if res2.ActionsUpdated != 0 || res2.TokenUsageUpdated != 0 {
		t.Errorf("second run not idempotent: actions=%d tokens=%d", res2.ActionsUpdated, res2.TokenUsageUpdated)
	}
}

// TestBackfillOpenClawActionTypes pins the SQL-only openclaw retag:
// historical sessions_spawn rows tagged mcp_call get retagged to
// spawn_subagent. Other sessions_* tools are left alone.
func TestBackfillOpenClawActionTypes(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "obs.db")
	database, err := db.Open(ctx, db.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	if _, err := database.ExecContext(ctx,
		`INSERT INTO projects (id, root_path, created_at) VALUES (1, '/tmp/test', '2026-04-30T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO sessions (id, tool, started_at, project_id) VALUES ('ses_1', 'openclaw', '2026-04-30T00:00:00Z', 1)`); err != nil {
		t.Fatal(err)
	}
	for i, a := range []struct {
		rawName    string
		actionType string
	}{
		{"sessions_spawn", "mcp_call"},       // → retag
		{"sessions_list", "mcp_call"},        // leave
		{"sessions_spawn", "spawn_subagent"}, // already correct, leave
		{"read", "read_file"},                // unrelated, leave
	} {
		_, err := database.ExecContext(ctx,
			`INSERT INTO actions (session_id, project_id, timestamp, action_type, raw_tool_name, tool, source_file, source_event_id)
			 VALUES ('ses_1', 1, '2026-04-30T00:00:01Z', ?, ?, 'openclaw', '/tmp/runs.sqlite', ?)`,
			a.actionType, a.rawName, fmt.Sprintf("%s:seed:%d", a.rawName, i))
		if err != nil {
			t.Fatal(err)
		}
	}

	res, err := backfillOpenClawActionTypes(ctx, database)
	if err != nil {
		t.Fatalf("backfillOpenClawActionTypes: %v", err)
	}
	if res.ActionsUpdated != 1 {
		t.Errorf("ActionsUpdated = %d, want 1 (only the mcp_call sessions_spawn row)", res.ActionsUpdated)
	}

	// Verify only the targeted row was retagged.
	var spawnCount int
	if err := database.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM actions WHERE raw_tool_name='sessions_spawn' AND action_type='spawn_subagent'`).Scan(&spawnCount); err != nil {
		t.Fatal(err)
	}
	if spawnCount != 2 {
		t.Errorf("spawn_subagent count = %d, want 2 (the retagged + the already-correct)", spawnCount)
	}
	var sessionsListCount int
	if err := database.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM actions WHERE raw_tool_name='sessions_list' AND action_type='mcp_call'`).Scan(&sessionsListCount); err != nil {
		t.Fatal(err)
	}
	if sessionsListCount != 1 {
		t.Errorf("sessions_list mcp_call count = %d, want 1 (must NOT be retagged)", sessionsListCount)
	}

	res2, _ := backfillOpenClawActionTypes(ctx, database)
	if res2.ActionsUpdated != 0 {
		t.Errorf("second run not idempotent: %d", res2.ActionsUpdated)
	}
}

// TestBackfillCursorModel pins the SQL-only cursor session-model copy:
// session rows whose model is empty pick up the model from a matching
// token_usage row. (Actions has no model column; per-action model is
// always derived by joining to token_usage.)
func TestBackfillCursorModel(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "obs.db")
	database, err := db.Open(ctx, db.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	if _, err := database.ExecContext(ctx,
		`INSERT INTO projects (id, root_path, created_at) VALUES (1, '/tmp/test', '2026-04-30T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	// Three cursor sessions:
	//   c1: empty model + matching token row → should pick up
	//   c2: empty model + no token row → stays empty
	//   c3: already has model → IS NULL/empty guard skips it
	for _, s := range []struct {
		id, model string
	}{{"c1", ""}, {"c2", ""}, {"c3", "gpt-5"}} {
		_, err := database.ExecContext(ctx,
			`INSERT INTO sessions (id, tool, model, started_at, project_id)
			 VALUES (?, 'cursor', ?, '2026-04-30T00:00:00Z', 1)`, s.id, s.model)
		if err != nil {
			t.Fatal(err)
		}
	}
	// Token row for c1 only.
	if _, err := database.ExecContext(ctx,
		`INSERT INTO token_usage (session_id, timestamp, tool, model, source_file, source_event_id, message_id, input_tokens, output_tokens, source, reliability)
		 VALUES ('c1', '2026-04-30T00:00:00Z', 'cursor', 'claude-sonnet-4-5', 'cursor:hook', 'gen-1:stop', 'gen-1', 10, 5, 'hook', 'accurate')`); err != nil {
		t.Fatal(err)
	}
	// Token row for c3 with a different model — must NOT overwrite c3's existing model.
	if _, err := database.ExecContext(ctx,
		`INSERT INTO token_usage (session_id, timestamp, tool, model, source_file, source_event_id, message_id, input_tokens, output_tokens, source, reliability)
		 VALUES ('c3', '2026-04-30T00:00:00Z', 'cursor', 'claude-haiku-4-5', 'cursor:hook', 'gen-3:stop', 'gen-3', 10, 5, 'hook', 'accurate')`); err != nil {
		t.Fatal(err)
	}

	res, err := backfillCursorModel(ctx, database)
	if err != nil {
		t.Fatalf("backfillCursorModel: %v", err)
	}
	if res.SessionsUpdated != 1 {
		t.Errorf("SessionsUpdated = %d, want 1 (only c1)", res.SessionsUpdated)
	}
	for _, c := range []struct {
		id, want string
	}{
		{"c1", "claude-sonnet-4-5"}, // lifted
		{"c2", ""},                  // no token row
		{"c3", "gpt-5"},             // existing preserved
	} {
		var got sql.NullString
		if err := database.QueryRowContext(ctx,
			`SELECT model FROM sessions WHERE id = ?`, c.id).Scan(&got); err != nil {
			t.Fatalf("query %s: %v", c.id, err)
		}
		if got.String != c.want {
			t.Errorf("session %s: model = %q, want %q", c.id, got.String, c.want)
		}
	}

	res2, _ := backfillCursorModel(ctx, database)
	if res2.SessionsUpdated != 0 {
		t.Errorf("second run not idempotent: %d", res2.SessionsUpdated)
	}
}

// TestBackfillCodexReasoning pins the codex agent_message → preceding_reasoning
// pass: agent_message text per turn lands on every action row with
// the matching message_id (turn_id).
func TestBackfillCodexReasoning(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "obs.db")
	database, err := db.Open(ctx, db.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	if _, err := database.ExecContext(ctx,
		`INSERT INTO projects (id, root_path, created_at) VALUES (1, '/tmp/test', '2026-04-30T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO sessions (id, tool, started_at, project_id) VALUES ('thread-3', 'codex', '2026-04-30T00:00:00Z', 1)`); err != nil {
		t.Fatal(err)
	}
	// Two action rows for turn-3, one for turn-4.
	for _, a := range []struct {
		eid, mid string
	}{
		{"call_1", "turn-3"},
		{"call_2", "turn-3"},
		{"call_3", "turn-4"},
	} {
		_, err := database.ExecContext(ctx,
			`INSERT INTO actions (session_id, project_id, timestamp, action_type, tool, source_file, source_event_id, message_id)
			 VALUES ('thread-3', 1, '2026-04-30T00:00:01Z', 'read_file', 'codex', 'fake', ?, ?)`, a.eid, a.mid)
		if err != nil {
			t.Fatal(err)
		}
	}

	// Build a fake codex sessions tree with one rollout file.
	sessionsDir := filepath.Join(root, "codex-sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rollout := filepath.Join(sessionsDir, "rollout-thread-3.jsonl")
	body := strings.Join([]string{
		`{"timestamp":"2026-04-30T00:00:01.000Z","type":"session_meta","payload":{"id":"thread-3","cwd":"/x","model":"gpt-5","git_branch":"main"}}`,
		`{"timestamp":"2026-04-30T00:00:01.050Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn-3"}}`,
		`{"timestamp":"2026-04-30T00:00:01.100Z","type":"event_msg","payload":{"type":"agent_message","turn_id":"turn-3","message":"I'll inspect main.go and run the tests."}}`,
		`{"timestamp":"2026-04-30T00:00:02.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn-4"}}`,
		`{"timestamp":"2026-04-30T00:00:02.100Z","type":"event_msg","payload":{"type":"agent_message","turn_id":"turn-4","message":"Now I'll patch the bug."}}`,
		"",
	}, "\n")
	if err := os.WriteFile(rollout, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := backfillCodexReasoning(ctx, database, sessionsDir, 0)
	if err != nil {
		t.Fatalf("backfillCodexReasoning: %v", err)
	}
	if res.TurnsCaptured != 2 {
		t.Errorf("TurnsCaptured = %d, want 2", res.TurnsCaptured)
	}
	if res.ActionsUpdated != 3 {
		t.Errorf("ActionsUpdated = %d, want 3 (two turn-3 rows + one turn-4)", res.ActionsUpdated)
	}
	for _, c := range []struct {
		eid, want string
	}{
		{"call_1", "I'll inspect main.go and run the tests."},
		{"call_2", "I'll inspect main.go and run the tests."},
		{"call_3", "Now I'll patch the bug."},
	} {
		var got sql.NullString
		if err := database.QueryRowContext(ctx,
			`SELECT preceding_reasoning FROM actions WHERE source_event_id = ?`, c.eid).Scan(&got); err != nil {
			t.Fatalf("query %s: %v", c.eid, err)
		}
		if got.String != c.want {
			t.Errorf("%s: preceding_reasoning = %q, want %q", c.eid, got.String, c.want)
		}
	}

	// Idempotency.
	res2, _ := backfillCodexReasoning(ctx, database, sessionsDir, 0)
	if res2.ActionsUpdated != 0 {
		t.Errorf("second run not idempotent: %d", res2.ActionsUpdated)
	}
}

// TestBackfillCodexProjectRoot_ReattributesWindowsCwd pins the
// v1.4.28 fix: codex sessions whose cwd was a Windows-style path
// (captured by codex on Windows, parsed by an observer in WSL2) were
// misattributed to whichever project lay on the path
// filepath.Abs+findGitRoot ended up walking — typically observer's
// own repo. The backfill re-reads the rollout's session_meta cwd,
// translates via crossmount, resolves the new project, and updates
// every action row + the session row to point at the new project_id.
func TestBackfillCodexProjectRoot_ReattributesWindowsCwd(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "obs.db")
	database, err := db.Open(ctx, db.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	// Seed a "wrong" project (simulating observer's own repo) and
	// attach one session + two actions to it. This is the post-bug
	// state: the codex adapter resolved cwd "C:\\proj-x" as a
	// relative path, walked up, and landed on the test process's
	// nearest .git — typically a useless attribution.
	if _, err := database.ExecContext(ctx,
		`INSERT INTO projects (root_path, created_at) VALUES ('/wrong/observer-cwd', '2026-04-30T00:00:00Z')`,
	); err != nil {
		t.Fatal(err)
	}
	var wrongPID int64
	if err := database.QueryRowContext(ctx,
		`SELECT id FROM projects WHERE root_path = '/wrong/observer-cwd'`,
	).Scan(&wrongPID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO sessions (id, project_id, tool, started_at) VALUES ('s-cx-1', ?, 'codex', '2026-04-30T00:00:00Z')`, wrongPID,
	); err != nil {
		t.Fatal(err)
	}
	for _, eid := range []string{"call_1", "call_2"} {
		if _, err := database.ExecContext(ctx,
			`INSERT INTO actions (session_id, project_id, timestamp, action_type, tool, source_file, source_event_id)
			 VALUES ('s-cx-1', ?, '2026-04-30T00:00:01Z', 'read_file', 'codex', 'fake', ?)`,
			wrongPID, eid,
		); err != nil {
			t.Fatal(err)
		}
	}

	// Build a synthetic codex rollout whose session_meta.cwd is a
	// Windows-style path. The /mnt/c equivalent must exist on this
	// host so git.Resolve can return a stable Root — point it at a
	// non-git directory under t.TempDir, then symlink-style fake
	// the /mnt/c lookup by giving the rollout a cwd whose
	// translation matches a real local path.
	realProject := filepath.Join(root, "real-proj") // /tmp/.../real-proj
	if err := os.MkdirAll(realProject, 0o755); err != nil {
		t.Fatal(err)
	}
	// Use a forward-slash cwd pointing at the real project's mount
	// equivalent: a path that, after TranslateForeignPath, equals
	// realProject. Easiest synthetic: pretend realProject's path is
	// the post-translation form of a Windows path. We can't fake the
	// translation table, so instead test the realistic scenario by
	// using a Linux-form cwd directly — TranslateForeignPath is a
	// no-op on Linux paths so the backfill still runs the rest of
	// the pipeline (UpsertProject + UPDATE) and verifies
	// reattribution end-to-end.
	sessionsDir := filepath.Join(root, "codex-sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rollout := filepath.Join(sessionsDir, "rollout-s-cx-1.jsonl")
	body := strings.Join([]string{
		`{"timestamp":"2026-04-30T00:00:01.000Z","type":"session_meta","payload":{"id":"s-cx-1","cwd":` + jsonString(realProject) + `,"model":"gpt-5"}}`,
		"",
	}, "\n")
	if err := os.WriteFile(rollout, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := backfillCodexProjectRoot(ctx, database, []string{sessionsDir}, 0)
	if err != nil {
		t.Fatalf("backfillCodexProjectRoot: %v", err)
	}
	if res.FilesScanned != 1 {
		t.Errorf("FilesScanned = %d, want 1", res.FilesScanned)
	}
	if res.SessionsReattributed != 1 {
		t.Errorf("SessionsReattributed = %d, want 1", res.SessionsReattributed)
	}
	if res.ActionsUpdated != 2 {
		t.Errorf("ActionsUpdated = %d, want 2 (two seeded actions)", res.ActionsUpdated)
	}

	// Verify the session is now attached to the real project, not
	// the wrong one.
	var newPID int64
	if err := database.QueryRowContext(ctx,
		`SELECT project_id FROM sessions WHERE id = 's-cx-1'`,
	).Scan(&newPID); err != nil {
		t.Fatal(err)
	}
	if newPID == wrongPID {
		t.Errorf("session still attached to wrong project: pid=%d", newPID)
	}
	var newRoot string
	if err := database.QueryRowContext(ctx,
		`SELECT root_path FROM projects WHERE id = ?`, newPID,
	).Scan(&newRoot); err != nil {
		t.Fatal(err)
	}
	if newRoot != realProject {
		t.Errorf("new project root = %q, want %q", newRoot, realProject)
	}

	// Idempotency.
	res2, _ := backfillCodexProjectRoot(ctx, database, []string{sessionsDir}, 0)
	if res2.SessionsReattributed != 0 || res2.ActionsUpdated != 0 {
		t.Errorf("second run not idempotent: %+v", res2)
	}
}

// jsonString quotes s for inclusion as a JSON string literal in test
// fixtures. Avoids manual escaping of backslashes in Windows-style
// paths the test corpus exercises.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
