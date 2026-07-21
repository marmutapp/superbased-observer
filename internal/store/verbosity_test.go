package store

import (
	"context"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/verbosity"
)

// TestLoadSessionVerbosity proves the rollup end-to-end over seeded rows:
// assistant-text bodies segment into prose vs fenced artifacts, and
// authored-code actions attribute content_bytes to languages (writes) and
// shells (commands). Reads and searches contribute nothing.
func TestLoadSessionVerbosity(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()

	pid, _ := s.UpsertProject(ctx, "/tmp/pv", "")
	if err := s.UpsertSession(ctx, models.Session{
		ID: "sv", ProjectID: pid, Tool: models.ToolClaudeCode, StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	// Assistant visible text: narrative + a fenced go block + an untagged block.
	assistantBody := "Here is the fix.\n```go\nfunc main() {}\n```\nThat should do it.\n```\nsome quoted log\n```"
	mk := func(eid, atype, rawName, target, body string, cb int64) models.Action {
		return models.Action{
			SessionID: "sv", ProjectID: pid, Timestamp: time.Now().UTC(),
			ActionType: atype, Target: target, Success: true,
			Tool: models.ToolClaudeCode, RawToolName: rawName, RawToolOutput: body,
			ContentBytes: cb, SourceFile: "f.jsonl", SourceEventID: eid,
		}
	}
	batch := []models.Action{
		mk("e1", models.ActionTaskComplete, "claudecode.assistant_text", "preview", assistantBody, 0),
		mk("e2", models.ActionWriteFile, "Write", "internal/x/foo.go", "", 1200), // go code
		mk("e3", models.ActionEditFile, "Edit", "web/app.tsx", "", 300),          // tsx code
		mk("e4", models.ActionWriteFile, "Write", "README.md", "", 500),          // docs (not code)
		mk("e5", models.ActionRunCommand, "Bash", "go test ./...", "", 42),       // shell = code
		mk("e6", models.ActionRunCommand, "PowerShell", "Get-Content x", "", 20), // shell = code
		mk("e7", models.ActionReadFile, "Read", "internal/x/foo.go", "", 0),      // ignored
		mk("e8", models.ActionWriteFile, "Write", "data.weirdext", "", 64),       // unknown ext
	}
	if _, err := s.InsertActions(ctx, batch); err != nil {
		t.Fatal(err)
	}

	b, err := s.LoadSessionVerbosity(ctx, "sv")
	if err != nil {
		t.Fatalf("LoadSessionVerbosity: %v", err)
	}

	// Authored code by language.
	if got := b.Written["go"]; got != 1200 {
		t.Errorf("written go = %d, want 1200", got)
	}
	if got := b.Written["tsx"]; got != 300 {
		t.Errorf("written tsx = %d, want 300", got)
	}
	if got := b.Written["markdown"]; got != 500 {
		t.Errorf("written markdown = %d, want 500", got)
	}
	if got := b.Command["bash"]; got != 42 {
		t.Errorf("command bash = %d, want 42", got)
	}
	if got := b.Command["powershell"]; got != 20 {
		t.Errorf("command powershell = %d, want 20", got)
	}
	if got := b.WrittenUnknownExt[".weirdext"]; got != 64 {
		t.Errorf("unknown-ext .weirdext = %d, want 64", got)
	}

	// CodeBytes = go(1200) + tsx(300) + bash(42) + powershell(20) + fenced go block.
	// Markdown write is docs, NOT code; unknown-ext is unknown, NOT code.
	codeArtifact := b.Visible.ArtifactLang["go"]
	if codeArtifact == 0 {
		t.Fatal("expected fenced go artifact bytes")
	}
	wantCode := int64(1200+300+42+20) + codeArtifact
	if got := b.CodeBytes(); got != wantCode {
		t.Errorf("CodeBytes = %d, want %d", got, wantCode)
	}

	// ExplainBytes = narrative + untagged fenced block (prose-ish). Must be > 0
	// and must NOT include the markdown write (that's docs, a separate category).
	if b.ExplainBytes() == 0 {
		t.Error("expected non-zero ExplainBytes")
	}
	if cats := b.ByCategory(); cats[verbosity.Docs] != 500 {
		t.Errorf("category Docs = %d, want 500 (the README write)", cats[verbosity.Docs])
	}

	// Language cut surfaces go + tsx + the two shells.
	byLang := b.CodeByLang()
	for _, want := range []string{"go", "tsx", "bash", "powershell"} {
		if byLang[want] == 0 {
			t.Errorf("CodeByLang missing %q", want)
		}
	}
}

// TestLoadVerbosityAggregate proves the by-model rollup groups authored code
// across sessions by sessions.model.
func TestLoadVerbosityAggregate(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	pid, _ := s.UpsertProject(ctx, "/tmp/agg", "")

	mkSession := func(id, model string) {
		if err := s.UpsertSession(ctx, models.Session{
			ID: id, ProjectID: pid, Tool: models.ToolClaudeCode, Model: model, StartedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	mkSession("a1", "opus")
	mkSession("a2", "opus")
	mkSession("b1", "haiku")

	act := func(sid, eid, atype, target string, cb int64) models.Action {
		return models.Action{
			SessionID: sid, ProjectID: pid, Timestamp: time.Now().UTC(),
			ActionType: atype, Target: target, Success: true, Tool: models.ToolClaudeCode,
			ContentBytes: cb, SourceFile: "f.jsonl", SourceEventID: eid,
		}
	}
	if _, err := s.InsertActions(ctx, []models.Action{
		act("a1", "e1", models.ActionWriteFile, "x.go", 1000),
		act("a2", "e2", models.ActionWriteFile, "y.go", 500),
		act("b1", "e3", models.ActionWriteFile, "z.py", 300),
	}); err != nil {
		t.Fatal(err)
	}

	groups, err := s.LoadVerbosityAggregate(ctx, "model", 0)
	if err != nil {
		t.Fatalf("LoadVerbosityAggregate: %v", err)
	}
	byKey := map[string]*verbosity.Breakdown{}
	for _, g := range groups {
		byKey[g.Key] = g.Breakdown
	}
	if byKey["opus"] == nil || byKey["opus"].Written["go"] != 1500 {
		t.Errorf("opus go bytes = %v, want 1500", byKey["opus"])
	}
	if byKey["haiku"] == nil || byKey["haiku"].Written["python"] != 300 {
		t.Errorf("haiku python bytes = %v, want 300", byKey["haiku"])
	}

	if _, err := s.LoadVerbosityAggregate(ctx, "bogus", 0); err == nil {
		t.Error("expected error for unknown dimension")
	}
}

// TestSessionTokenTotals proves the est-cost substrate: model resolves from
// sessions.model (falling back to token_usage.model), and output/reasoning
// tokens sum separately across the session's token rows.
func TestSessionTokenTotals(t *testing.T) {
	t.Parallel()
	s, db := newTestStore(t)
	ctx := context.Background()
	pid, _ := s.UpsertProject(ctx, "/tmp/tok", "")
	if err := s.UpsertSession(ctx, models.Session{
		ID: "tk", ProjectID: pid, Tool: models.ToolClaudeCode, Model: "claude-opus-4-8", StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	ins := func(eid string, out, reason int64) {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO token_usage(session_id, timestamp, tool, model, input_tokens, output_tokens, reasoning_tokens, source, source_file, source_event_id)
			 VALUES('tk', ?, 'claude-code', 'claude-opus-4-8', 100, ?, ?, 'proxy', 'f.jsonl', ?)`,
			time.Now().UTC().Format(time.RFC3339), out, reason, eid); err != nil {
			t.Fatal(err)
		}
	}
	ins("t1", 400, 30)
	ins("t2", 600, 70)

	model, out, reason, err := s.SessionTokenTotals(ctx, "tk")
	if err != nil {
		t.Fatalf("SessionTokenTotals: %v", err)
	}
	if model != "claude-opus-4-8" {
		t.Errorf("model = %q, want claude-opus-4-8", model)
	}
	if out != 1000 {
		t.Errorf("output = %d, want 1000", out)
	}
	if reason != 100 {
		t.Errorf("reasoning = %d, want 100", reason)
	}
}

// TestSessionTokenTotals_ModelFallback proves the sessions.model-empty path
// falls back to the most recent non-empty token_usage.model (the Cursor/CC gap).
func TestSessionTokenTotals_ModelFallback(t *testing.T) {
	t.Parallel()
	s, db := newTestStore(t)
	ctx := context.Background()
	pid, _ := s.UpsertProject(ctx, "/tmp/tok2", "")
	if err := s.UpsertSession(ctx, models.Session{
		ID: "tk2", ProjectID: pid, Tool: models.ToolClaudeCode, StartedAt: time.Now().UTC(), // no model
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO token_usage(session_id, timestamp, tool, model, input_tokens, output_tokens, source, source_file, source_event_id)
		 VALUES('tk2', ?, 'claude-code', 'claude-sonnet-4', 10, 20, 'proxy', 'f.jsonl', 'x1')`,
		time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	model, out, _, err := s.SessionTokenTotals(ctx, "tk2")
	if err != nil {
		t.Fatalf("SessionTokenTotals: %v", err)
	}
	if model != "claude-sonnet-4" {
		t.Errorf("model = %q, want claude-sonnet-4 (token_usage fallback)", model)
	}
	if out != 20 {
		t.Errorf("output = %d, want 20", out)
	}
}

// TestLoadVerbosityAggregate_ModelFallback proves the by-model cut resolves a
// session with EMPTY sessions.model to its token_usage.model (the Cursor/CC gap
// fix), instead of bucketing it as (unknown).
func TestLoadVerbosityAggregate_ModelFallback(t *testing.T) {
	t.Parallel()
	s, db := newTestStore(t)
	ctx := context.Background()
	pid, _ := s.UpsertProject(ctx, "/tmp/mf", "")
	if err := s.UpsertSession(ctx, models.Session{
		ID: "mf", ProjectID: pid, Tool: models.ToolClaudeCode, StartedAt: time.Now().UTC(), // no model
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertActions(ctx, []models.Action{{
		SessionID: "mf", ProjectID: pid, Timestamp: time.Now().UTC(),
		ActionType: models.ActionWriteFile, Target: "x.go", Success: true, Tool: models.ToolClaudeCode,
		ContentBytes: 900, SourceFile: "f.jsonl", SourceEventID: "e1",
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO token_usage(session_id, timestamp, tool, model, input_tokens, output_tokens, reasoning_tokens, source, source_file, source_event_id)
		 VALUES('mf', ?, 'claude-code', 'claude-opus-4-8', 50, 800, 100, 'proxy', 'f.jsonl', 'tu1')`,
		time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}

	groups, err := s.LoadVerbosityAggregate(ctx, "model", 0)
	if err != nil {
		t.Fatalf("LoadVerbosityAggregate: %v", err)
	}
	byKey := map[string]*verbosity.Breakdown{}
	for _, g := range groups {
		byKey[g.Key] = g.Breakdown
	}
	if byKey["claude-opus-4-8"] == nil || byKey["claude-opus-4-8"].Written["go"] != 900 {
		t.Errorf("expected go bytes under resolved model claude-opus-4-8, got %v", byKey)
	}
	if byKey["(unknown)"] != nil {
		t.Error("session should NOT bucket as (unknown) when token_usage.model resolves")
	}

	// And the per-group token totals price under the same key.
	toks, err := s.LoadVerbosityGroupTokens(ctx, "model", 0)
	if err != nil {
		t.Fatalf("LoadVerbosityGroupTokens: %v", err)
	}
	var found bool
	for _, tk := range toks {
		if tk.Key == "claude-opus-4-8" && tk.Model == "claude-opus-4-8" {
			found = true
			if tk.Output != 800 || tk.Reasoning != 100 {
				t.Errorf("token totals = out %d / reason %d, want 800/100", tk.Output, tk.Reasoning)
			}
		}
	}
	if !found {
		t.Errorf("LoadVerbosityGroupTokens missing claude-opus-4-8 row, got %+v", toks)
	}

	if _, err := s.LoadVerbosityGroupTokens(ctx, "bogus", 0); err == nil {
		t.Error("expected error for unknown dimension")
	}
}

// TestLoadUnknownLedger proves the §4 ledger: write/edit targets that the
// FileType table can't resolve are bucketed by extension with counts, and the
// resolved/total ratio is reported. Independent of content_bytes.
func TestLoadUnknownLedger(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	pid, _ := s.UpsertProject(ctx, "/tmp/led", "")
	if err := s.UpsertSession(ctx, models.Session{
		ID: "sl", ProjectID: pid, Tool: models.ToolClaudeCode, StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	act := func(eid, atype, target string) models.Action {
		return models.Action{
			SessionID: "sl", ProjectID: pid, Timestamp: time.Now().UTC(),
			ActionType: atype, Target: target, Success: true, Tool: models.ToolClaudeCode,
			SourceFile: "f.jsonl", SourceEventID: eid,
		}
	}
	if _, err := s.InsertActions(ctx, []models.Action{
		act("e1", models.ActionWriteFile, "a.go"),         // resolved
		act("e2", models.ActionEditFile, "b.tsx"),         // resolved
		act("e3", models.ActionWriteFile, "c.zzzunk"),     // unknown ext
		act("e4", models.ActionWriteFile, "d.zzzunk"),     // unknown ext (same)
		act("e5", models.ActionReadFile, "ignored.weird"), // not write/edit → ignored
	}); err != nil {
		t.Fatal(err)
	}

	led, err := s.LoadUnknownLedger(ctx, 0)
	if err != nil {
		t.Fatalf("LoadUnknownLedger: %v", err)
	}
	if led.TotalWrites != 4 {
		t.Errorf("TotalWrites = %d, want 4 (reads excluded)", led.TotalWrites)
	}
	if led.ResolvedWrites != 2 {
		t.Errorf("ResolvedWrites = %d, want 2", led.ResolvedWrites)
	}
	if led.Extensions[".zzzunk"] != 2 {
		t.Errorf("unknown .zzzunk count = %d, want 2", led.Extensions[".zzzunk"])
	}
}
