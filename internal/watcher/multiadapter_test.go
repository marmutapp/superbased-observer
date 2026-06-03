package watcher

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/adapter"
	"github.com/marmutapp/superbased-observer/internal/adapter/claudecode"
	"github.com/marmutapp/superbased-observer/internal/adapter/codex"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// TestPollerDispatchesByRoot_NotByShape pins the v1.4.51 dispatch
// fix: when both claude-code and codex adapters are registered, a
// Codex rollout-*.jsonl file under the Codex watch root must be
// re-parsed by Codex via the poll fallback, not by claude-code's
// broad .jsonl predicate winning by alphabetical sort.
//
// Pre-fix:  pollCursors → adapterFor(path) iterated registry.Detected
//
//	(name-sorted), returned first IsSessionFile match. Claude
//	Code's IsSessionFile was `.jsonl` extension only, so any
//	Codex rollout-*.jsonl matched and "claude-code" sorted
//	before "codex" → claude-code parsed the file, silently
//	advanced the cursor to EOF (Codex JSONL unmarshals as
//	valid JSON), emitted zero events because none of the
//	Codex-schema fields matched claude-code's record types →
//	token_usage / api_turns rows stranded.
//
// Post-fix: pollCursors → adapterFor uses longest-watched-root
//
//	prefix → codex picks up its own files.
//
// Defense-in-depth: after the fix, every adapter's IsSessionFile also
// AND's its shape filter with UnderAnyWatchRoot(a.WatchPaths()), so
// claude-code's predicate would self-limit to its own root regardless
// of what dispatch does. Either layer alone fixes the bug; both
// together make the bug-class regression-proof.
func TestPollerDispatchesByRoot_NotByShape(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	dbPath := filepath.Join(t.TempDir(), "w.db")
	database, err := db.Open(ctx, db.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	s := store.New(database)

	// Two separate roots, both under disjoint tmpdirs — mirrors the
	// production layout where ~/.claude/projects and ~/.codex/sessions
	// are non-overlapping siblings.
	claudeRoot := t.TempDir()
	codexHome := t.TempDir()
	codexSessions := filepath.Join(codexHome, "sessions")
	if err := os.MkdirAll(codexSessions, 0o700); err != nil {
		t.Fatal(err)
	}

	// A complete Codex rollout — leading meta + a shell_command turn
	// with token_count emissions. Reused from
	// TestCodexShortSessionMultiPassIngest fixture data; carrying the
	// session UUID 019dfbc2-… so the cost-engine session attribution
	// is deterministic.
	const sessionUUID = "019dfbc2-344c-7a41-9766-9e6d97c28825"
	rolloutPath := filepath.Join(codexSessions, "rollout-2026-05-06T10-58-21-"+sessionUUID+".jsonl")
	lines := []string{
		`{"timestamp":"2026-05-06T05:28:28.414Z","type":"session_meta","payload":{"id":"` + sessionUUID + `","cwd":"/tmp/test","model":"gpt-5.5"}}`,
		`{"timestamp":"2026-05-06T05:28:28.416Z","type":"event_msg","payload":{"type":"task_started","turn_id":"019dfbc2-3c66-72b1-9bcf-97654ee68beb"}}`,
		`{"timestamp":"2026-05-06T05:28:28.417Z","type":"turn_context","payload":{"turn_id":"019dfbc2-3c66-72b1-9bcf-97654ee68beb","cwd":"/tmp/test","model":"gpt-5.5"}}`,
		`{"timestamp":"2026-05-06T05:28:28.418Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"run a command"}]}}`,
		`{"timestamp":"2026-05-06T05:28:31.185Z","type":"response_item","payload":{"type":"function_call","name":"shell_command","arguments":"{\"command\":\"echo dispatch-test\"}","call_id":"call_x"}}`,
		`{"timestamp":"2026-05-06T05:28:31.186Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":13445,"cached_input_tokens":7552,"output_tokens":45,"reasoning_output_tokens":0,"total_tokens":13490},"total_token_usage":{"input_tokens":13445,"cached_input_tokens":7552,"output_tokens":45,"reasoning_output_tokens":0,"total_tokens":13490}}}}`,
		`{"timestamp":"2026-05-06T05:28:31.828Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_x","output":"dispatch-test\n"}}`,
		`{"timestamp":"2026-05-06T05:28:33.651Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"019dfbc2-3c66-72b1-9bcf-97654ee68beb","last_agent_message":"done","completed_at":1778045313,"duration_ms":3000}}`,
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(rolloutPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	// Both adapters registered. claude-code's Name() sorts before
	// codex's — the misrouting trigger pre-v1.4.51.
	reg := adapter.NewRegistry()
	reg.Register(claudecode.NewWithOptions(nil, claudeRoot))
	reg.Register(codex.NewWithOptions(nil, codexSessions))

	w := New(s, reg, Options{
		Logger:   slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		Debounce: 50 * time.Millisecond,
	})

	// Seed parse_cursors at offset 0 so pollCursors sees the file as
	// behind and re-processes it through the buggy/fixed adapterFor
	// path. (Skipping Scan first because Scan walks per-adapter and
	// would route correctly even pre-fix — the bug only manifests in
	// the poller's cursor pass.)
	if err := s.SetCursor(ctx, rolloutPath, 0); err != nil {
		t.Fatal(err)
	}

	if err := w.pollCursors(ctx); err != nil {
		t.Fatalf("pollCursors: %v", err)
	}

	// Codex emits a run_command action for shell_command turns. If
	// claude-code's adapter parsed the file (pre-fix misroute) the
	// row would be missing entirely, since none of the Codex JSONL
	// shapes match claude-code's record types.
	var runCmdCount int
	if err := database.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM actions WHERE session_id = ? AND action_type = ?`,
		sessionUUID, "run_command").Scan(&runCmdCount); err != nil {
		t.Fatal(err)
	}
	if runCmdCount == 0 {
		t.Errorf("expected codex-shaped run_command action for session %s after pollCursors; got 0 (poll dispatched to wrong adapter)", sessionUUID)
	}

	// Same check on the tool attribution column — any action for this
	// source_file MUST be tool=codex. A tool=claude-code row here
	// would prove the misroute.
	rows, err := database.QueryContext(ctx,
		`SELECT DISTINCT tool FROM actions WHERE source_file = ?`,
		rolloutPath)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	tools := map[string]struct{}{}
	for rows.Next() {
		var tool string
		if err := rows.Scan(&tool); err != nil {
			t.Fatal(err)
		}
		tools[tool] = struct{}{}
	}
	if _, ok := tools["claude-code"]; ok {
		t.Errorf("misroute: actions for codex rollout file recorded under tool=claude-code; tools seen=%v", tools)
	}
	if _, ok := tools["codex"]; !ok {
		t.Errorf("codex did not emit any actions for its own rollout file; tools seen=%v", tools)
	}

	// Cursor should have advanced past offset 0 — proof that *some*
	// adapter parsed past the seed. Pre-fix this also moves (claude
	// code walks every line as valid JSON), so cursor-advance alone
	// isn't a bug signal; combined with the action-count checks above
	// it pins the right semantics.
	off, err := s.GetCursor(ctx, rolloutPath)
	if err != nil {
		t.Fatal(err)
	}
	if off == 0 {
		t.Errorf("parse cursor still at 0 after pollCursors")
	}
}
