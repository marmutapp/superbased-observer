package opencode

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/marmutapp/superbased-observer/internal/models"
)

func TestParseSessionFile_SQLiteCapturesRichActions(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "opencode.db")
	setupOpenCodeDB(t, path)

	a := NewWithOptions(nil, []string{root})
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if got := len(res.ToolEvents); got != 3 {
		t.Fatalf("expected 3 events, got %d", got)
	}

	if got := res.ToolEvents[0].ActionType; got != models.ActionUserPrompt {
		t.Fatalf("first event action_type = %q, want %q", got, models.ActionUserPrompt)
	}
	if got := res.ToolEvents[0].Target; got != "Build the app" {
		t.Fatalf("prompt target = %q", got)
	}

	if got := res.ToolEvents[1].ActionType; got != models.ActionRunCommand {
		t.Fatalf("second event action_type = %q, want %q", got, models.ActionRunCommand)
	}
	if res.ToolEvents[1].Success {
		t.Fatalf("expected bash event to be unsuccessful")
	}
	if got := res.ToolEvents[1].Target; got != "npm start" {
		t.Fatalf("command target = %q", got)
	}

	if got := res.ToolEvents[2].ActionType; got != models.ActionTaskComplete {
		t.Fatalf("third event action_type = %q, want %q", got, models.ActionTaskComplete)
	}
}

// TestParseSessionFile_SQLitePopulatesMessageIDAndToolOutputAndDuration
// pins the per-adapter parity with claudecode that landed in v1.4.19:
// every OpenCode event now carries a MessageID grouping (msg_xxx for
// assistant turns, "user:<id>" for prompts), tool events carry the
// scrubbed body of the tool result via ToolOutput, and DurationMs is
// derived from the part's own start/end timestamps. Pre-fix the audit
// flagged all three as silently zero — the source data was right there
// but the adapter discarded it.
func TestParseSessionFile_SQLitePopulatesMessageIDAndToolOutputAndDuration(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "opencode.db")
	setupOpenCodeDB(t, path)

	a := NewWithOptions(nil, []string{root})
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if got := res.ToolEvents[0].MessageID; got != "user:msg_user" {
		t.Errorf("user prompt MessageID = %q, want user:msg_user", got)
	}
	tool := res.ToolEvents[1]
	if tool.MessageID != "msg_tool" {
		t.Errorf("tool MessageID = %q, want msg_tool (parent message id)", tool.MessageID)
	}
	if tool.ToolOutput != "boom" {
		t.Errorf("tool ToolOutput = %q, want %q (scrubbed State.Output)", tool.ToolOutput, "boom")
	}
	if tool.DurationMs != 300 {
		t.Errorf("tool DurationMs = %d, want 300 (end-start in fixture)", tool.DurationMs)
	}
	if got := res.ToolEvents[2].MessageID; got != "msg_done" {
		t.Errorf("completion MessageID = %q, want msg_done", got)
	}
}

// TestParseSessionFile_SQLiteEmitsTokenEventsForAssistantMessages
// pins OpenCode token-extraction behaviour. Confirmed against
// OpenCode's InfoData zod schema in
// packages/opencode/src/session/message.ts:
//
//	tokens: { input, output, reasoning, cache: { read, write } }
//	cost:   number (USD)
//
// Pre-fix the adapter only extracted role/model/time from the data
// blob; the token + cost fields were silently ignored, which is why
// OpenCode rows landed on the dashboard with Source="jsonl" but no
// numbers attached. This test seeds an assistant message with the
// full token bundle and asserts each field flows through to the
// emitted TokenEvent — Reliability=approximate (not unreliable like
// Claude Code's JSONL, since OpenCode persists the upstream usage
// envelope verbatim).
func TestParseSessionFile_SQLiteEmitsTokenEventsForAssistantMessages(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "opencode.db")
	setupOpenCodeDBWithTokens(t, path)

	a := NewWithOptions(nil, []string{root})
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if got := len(res.TokenEvents); got != 1 {
		t.Fatalf("expected 1 token event, got %d", got)
	}
	te := res.TokenEvents[0]
	if te.Tool != models.ToolOpenCode {
		t.Errorf("Tool = %q, want %q", te.Tool, models.ToolOpenCode)
	}
	if te.Model != "big-pickle" {
		t.Errorf("Model = %q, want big-pickle", te.Model)
	}
	if te.InputTokens != 1234 {
		t.Errorf("InputTokens = %d, want 1234", te.InputTokens)
	}
	if te.OutputTokens != 567 {
		t.Errorf("OutputTokens = %d, want 567", te.OutputTokens)
	}
	if te.CacheReadTokens != 12345 {
		t.Errorf("CacheReadTokens = %d, want 12345", te.CacheReadTokens)
	}
	if te.CacheCreationTokens != 678 {
		t.Errorf("CacheCreationTokens = %d, want 678", te.CacheCreationTokens)
	}
	if te.ReasoningTokens != 89 {
		t.Errorf("ReasoningTokens = %d, want 89", te.ReasoningTokens)
	}
	if te.EstimatedCostUSD != 0.0532 {
		t.Errorf("EstimatedCostUSD = %v, want 0.0532", te.EstimatedCostUSD)
	}
	if te.Reliability != models.ReliabilityApproximate {
		t.Errorf("Reliability = %q, want %q", te.Reliability, models.ReliabilityApproximate)
	}
	if te.Source != models.TokenSourceJSONL {
		t.Errorf("Source = %q, want %q", te.Source, models.TokenSourceJSONL)
	}
	if te.MessageID != "msg_done" {
		t.Errorf("MessageID = %q, want msg_done (the assistant message id)", te.MessageID)
	}
}

// TestParseSessionFile_SQLiteSkipsZeroTokenAssistantRows pins the
// no-tokens guard — in-progress turns and assistant rows where the
// token bundle is empty across the board don't emit a TokenEvent
// (would otherwise pollute the cost engine with zero rows).
func TestParseSessionFile_SQLiteSkipsZeroTokenAssistantRows(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "opencode.db")
	setupOpenCodeDB(t, path) // seeds assistant rows WITHOUT tokens

	a := NewWithOptions(nil, []string{root})
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if got := len(res.TokenEvents); got != 0 {
		t.Errorf("expected 0 token events from token-less rows, got %d: %+v", got, res.TokenEvents)
	}
}

// TestParseSessionFile_SubtaskPartEmitsSpawnSubagent pins the
// subtask-part wiring added in v1.4.9. OpenCode's parent message
// emits a `subtask` part to invoke a subagent (Build/Plan/Explore/
// custom). We tag those as ActionSpawnSubagent with target=agent name
// + the subagent's model when set.
func TestParseSessionFile_SubtaskPartEmitsSpawnSubagent(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "opencode.db")
	setupOpenCodeDBWithSubtask(t, path)
	a := NewWithOptions(nil, []string{root})
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	var spawn *models.ToolEvent
	for i := range res.ToolEvents {
		if res.ToolEvents[i].ActionType == models.ActionSpawnSubagent {
			spawn = &res.ToolEvents[i]
			break
		}
	}
	if spawn == nil {
		t.Fatalf("expected a spawn_subagent event from subtask part, got %+v", res.ToolEvents)
	}
	if spawn.Target != "Explore" {
		t.Errorf("Target = %q, want Explore (subagent name)", spawn.Target)
	}
	if spawn.Model != "claude-haiku-4-5" {
		t.Errorf("Model = %q, want claude-haiku-4-5 (subagent's model)", spawn.Model)
	}
	if spawn.RawToolName != "subtask" {
		t.Errorf("RawToolName = %q, want subtask", spawn.RawToolName)
	}
	if spawn.MessageID != "msg_a" {
		t.Errorf("MessageID = %q, want msg_a (parent message id)", spawn.MessageID)
	}
}

// TestParseSessionFile_TodoTableEmitsTodoUpdate pins the todo-table
// wiring. Each row → one ActionTodoUpdate event with target=status.
func TestParseSessionFile_TodoTableEmitsTodoUpdate(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "opencode.db")
	setupOpenCodeDBWithTodos(t, path)
	a := NewWithOptions(nil, []string{root})
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	var todoCount int
	for _, e := range res.ToolEvents {
		if e.ActionType == models.ActionTodoUpdate {
			todoCount++
		}
	}
	if todoCount != 2 {
		t.Errorf("expected 2 todo events, got %d (events=%+v)", todoCount, res.ToolEvents)
	}
}

// TestParseSessionFile_NewToolNamesMappedCorrectly pins the
// mapTool() extension covering webfetch, websearch, task, todoread,
// todowrite, multiedit. Pre-fix these all fell through to mcp regex
// or stayed as ActionUnknown.
func TestParseSessionFile_NewToolNamesMappedCorrectly(t *testing.T) {
	cases := []struct {
		tool, want string
	}{
		{"webfetch", models.ActionWebFetch},
		{"websearch", models.ActionWebSearch},
		{"task", models.ActionSpawnSubagent},
		{"agent", models.ActionSpawnSubagent},
		{"todowrite", models.ActionTodoUpdate},
		{"todoread", models.ActionTodoUpdate},
		{"multiedit", models.ActionEditFile},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			part := toolPartData{Tool: tc.tool}
			at, _, _, _ := mapTool(part)
			if at != tc.want {
				t.Errorf("mapTool(%q): got %q, want %q", tc.tool, at, tc.want)
			}
		})
	}
}

func TestParseSessionFile_SQLiteWatermarkSkipsOldRows(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "opencode.db")
	setupOpenCodeDB(t, path)

	a := NewWithOptions(nil, []string{root})
	res, err := a.ParseSessionFile(context.Background(), path, 3000)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.ToolEvents) != 0 {
		t.Fatalf("expected no events, got %d", len(res.ToolEvents))
	}
	if res.NewOffset != 3000 {
		t.Fatalf("NewOffset = %d, want 3000", res.NewOffset)
	}
}

func setupOpenCodeDB(t *testing.T, path string) {
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
		`CREATE TABLE session (
			id TEXT PRIMARY KEY,
			directory TEXT NOT NULL,
			time_updated INTEGER NOT NULL
		)`,
		`CREATE TABLE message (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			time_created INTEGER NOT NULL,
			time_updated INTEGER NOT NULL,
			data TEXT NOT NULL
		)`,
		`CREATE TABLE part (
			id TEXT PRIMARY KEY,
			message_id TEXT NOT NULL,
			session_id TEXT NOT NULL,
			time_created INTEGER NOT NULL,
			time_updated INTEGER NOT NULL,
			data TEXT NOT NULL
		)`,
		`INSERT INTO session(id, directory, time_updated) VALUES
			('ses_1', 'D:\\programsx\\open-code-test', 3000)`,
		`INSERT INTO message(id, session_id, time_created, time_updated, data) VALUES
			('msg_user', 'ses_1', 1000, 1001, '{"role":"user","agent":"build","model":{"providerID":"opencode","modelID":"big-pickle"},"time":{"created":1000}}'),
			('msg_tool', 'ses_1', 2000, 2500, '{"role":"assistant","agent":"build","modelID":"big-pickle","providerID":"opencode","path":{"cwd":"D:\\programsx\\open-code-test"},"time":{"created":2000,"completed":2500},"finish":"tool-calls"}'),
			('msg_done', 'ses_1', 2900, 3000, '{"role":"assistant","agent":"build","modelID":"big-pickle","providerID":"opencode","path":{"cwd":"D:\\programsx\\open-code-test"},"time":{"created":2900,"completed":3000},"finish":"stop"}')`,
		`INSERT INTO part(id, message_id, session_id, time_created, time_updated, data) VALUES
			('prt_prompt', 'msg_user', 'ses_1', 1000, 1001, '{"type":"text","text":"Build the app"}'),
			('prt_tool', 'msg_tool', 'ses_1', 2200, 2500, '{"type":"tool","tool":"bash","callID":"call_1","state":{"status":"completed","input":{"command":"npm start","description":"Run app"},"output":"boom","metadata":{"output":"boom","exit":1,"description":"Run app","truncated":false},"title":"Run app","time":{"start":2200,"end":2500}}}')`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
}

// setupOpenCodeDBWithSubtask seeds a session with one subtask-typed
// part — the parent invoking an Explore subagent on a haiku model.
func setupOpenCodeDBWithSubtask(t *testing.T, path string) {
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
		`CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT NOT NULL, time_updated INTEGER NOT NULL)`,
		`CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL)`,
		`CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT NOT NULL, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL)`,
		`INSERT INTO session(id, directory, time_updated) VALUES ('ses_1', '/tmp/oc', 3000)`,
		`INSERT INTO message(id, session_id, time_created, time_updated, data) VALUES
			('msg_a', 'ses_1', 2900, 3000,
			 '{"role":"assistant","agent":"build","modelID":"big-pickle","providerID":"opencode","path":{"cwd":"/tmp/oc"},"time":{"created":2900,"completed":3000},"finish":"tool-calls"}')`,
		`INSERT INTO part(id, message_id, session_id, time_created, time_updated, data) VALUES
			('prt_subtask', 'msg_a', 'ses_1', 2950, 3000,
			 '{"type":"subtask","prompt":"explore the codebase","description":"map the repo","agent":"Explore","model":{"providerID":"anthropic","modelID":"claude-haiku-4-5"},"time":{"created":2950}}')`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
}

// setupOpenCodeDBWithTodos seeds the todo table with two entries —
// one pending, one completed — to exercise loadTodoEvents.
func setupOpenCodeDBWithTodos(t *testing.T, path string) {
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
		`CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT NOT NULL, time_updated INTEGER NOT NULL)`,
		`CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL)`,
		`CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT NOT NULL, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL)`,
		`CREATE TABLE todo (
			session_id TEXT NOT NULL,
			content TEXT NOT NULL,
			status TEXT NOT NULL,
			priority TEXT NOT NULL,
			position INTEGER NOT NULL,
			time_created INTEGER NOT NULL,
			time_updated INTEGER NOT NULL,
			PRIMARY KEY (session_id, position)
		)`,
		`INSERT INTO session(id, directory, time_updated) VALUES ('ses_1', '/tmp/oc', 3000)`,
		`INSERT INTO todo VALUES
			('ses_1', 'Refactor module X', 'pending',     'high', 0, 1000, 1000),
			('ses_1', 'Run go test',       'completed',   'med',  1, 1100, 1500)`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
}

// setupOpenCodeDBWithTokens seeds a minimal opencode.db with one
// assistant message carrying the full InfoData token bundle (input,
// output, reasoning, cache.read, cache.write, cost). Used by the
// token-event regression test to assert each field flows through.
func setupOpenCodeDBWithTokens(t *testing.T, path string) {
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
		`CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT NOT NULL, time_updated INTEGER NOT NULL)`,
		`CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL)`,
		`CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT NOT NULL, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL)`,
		`INSERT INTO session(id, directory, time_updated) VALUES ('ses_1', '/tmp/oc', 3000)`,
		// Numbers chosen deliberately so each field's drop-through can
		// be asserted independently:
		//   input=1234, output=567, reasoning=89,
		//   cache.read=12345, cache.write=678, cost=0.0532
		`INSERT INTO message(id, session_id, time_created, time_updated, data) VALUES
			('msg_done', 'ses_1', 2900, 3000,
			 '{"role":"assistant","agent":"build","modelID":"big-pickle","providerID":"opencode","path":{"cwd":"/tmp/oc"},"time":{"created":2900,"completed":3000},"finish":"stop","tokens":{"input":1234,"output":567,"reasoning":89,"cache":{"read":12345,"write":678}},"cost":0.0532}')`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
}
