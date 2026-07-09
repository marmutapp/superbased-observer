package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/intelligence/modelvalue"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/routing"
)

// seedModelValueCorpus stands up a project + session with one proxy
// turn, three token_usage rows (one duplicate by event id, one duplicate
// by shape, one genuinely new), and four actions covering each boundary
// resolution.
func seedModelValueCorpus(t *testing.T) (*Store, context.Context, time.Time) {
	t.Helper()
	ctx := context.Background()
	database, err := db.Open(ctx, db.Options{Path: filepath.Join(t.TempDir(), "mv_test.db")})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	st := New(database)

	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	projectID, err := st.UpsertProject(ctx, "/repo/acme", "")
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	if err := st.UpsertSession(ctx, models.Session{
		ID: "sess-mv", ProjectID: projectID, Tool: "claude-code",
		StartedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}

	// Proxy turn: full fidelity (latency + status + request id).
	if _, err := st.InsertAPITurn(ctx, models.APITurn{
		SessionID: "sess-mv", ProjectID: projectID, Provider: "anthropic",
		Model: "claude-opus-4-8", RequestID: "req_dup",
		Timestamp:   now.Add(-30 * time.Minute),
		InputTokens: 1000, OutputTokens: 500, CacheReadTokens: 200,
		MessageCount: 12, ToolUseCount: 6,
		TimeToFirstTokenMS: 400, TotalResponseMS: 2200,
		HTTPStatus: 200, CostUSD: 0.05,
	}); err != nil {
		t.Fatalf("InsertAPITurn: %v", err)
	}

	// Three JSONL rows: dup-by-id, dup-by-shape, genuinely new.
	if _, err := st.InsertTokenEvents(ctx, []models.TokenEvent{
		{
			SourceFile: "f.jsonl", SourceEventID: "req_dup", SessionID: "sess-mv",
			ProjectRoot: "/repo/acme", Tool: "claude-code", Model: "claude-opus-4-8",
			Timestamp:   now.Add(-30 * time.Minute).Add(10 * time.Second),
			InputTokens: 999, OutputTokens: 499, // different shape; id wins dedup
		},
		{
			SourceFile: "f.jsonl", SourceEventID: "ev_shape_twin", SessionID: "sess-mv",
			ProjectRoot: "/repo/acme", Tool: "claude-code", Model: "claude-opus-4-8",
			Timestamp:   now.Add(-30 * time.Minute).Add(12 * time.Second),
			InputTokens: 1000, OutputTokens: 500, CacheReadTokens: 200, // shape twin of the proxy turn
		},
		{
			SourceFile: "f.jsonl", SourceEventID: "ev_new", SessionID: "sess-mv",
			ProjectRoot: "/repo/acme", Tool: "claude-code", Model: "claude-haiku-4-5",
			Timestamp:   now.Add(-20 * time.Minute),
			InputTokens: 300, OutputTokens: 100, ReasoningTokens: 50, Fast: true,
		},
	}); err != nil {
		t.Fatalf("InsertTokenEvents: %v", err)
	}

	// Actions: one of each boundary-resolved type + a content-bearing
	// read whose target must NOT leak.
	if _, err := st.InsertActions(ctx, []models.Action{
		{
			SessionID: "sess-mv", ProjectID: projectID, Tool: "claude-code",
			Timestamp: now.Add(-29 * time.Minute), ActionType: models.ActionRunCommand,
			Target: "go test ./...", Success: true,
			SourceFile: "f.jsonl", SourceEventID: "a1",
		},
		{
			SessionID: "sess-mv", ProjectID: projectID, Tool: "claude-code",
			Timestamp: now.Add(-28 * time.Minute), ActionType: models.ActionPermissionMode,
			Target: "plan", Success: true,
			SourceFile: "f.jsonl", SourceEventID: "a2",
		},
		{
			SessionID: "sess-mv", ProjectID: projectID, Tool: "claude-code",
			Timestamp: now.Add(-27 * time.Minute), ActionType: models.ActionSubagentStart,
			Target: "code-reviewer", Success: true, IsSidechain: true,
			SourceFile: "f.jsonl", SourceEventID: "a3",
		},
		{
			SessionID: "sess-mv", ProjectID: projectID, Tool: "claude-code",
			Timestamp: now.Add(-26 * time.Minute), ActionType: models.ActionReadFile,
			Target: "/secret/path/creds.env", Success: true,
			SourceFile: "f.jsonl", SourceEventID: "a4",
		},
	}); err != nil {
		t.Fatalf("InsertActions: %v", err)
	}
	return st, ctx, now
}

// TestLoadModelValueFacts_DedupAndCapabilities pins the loader contract:
// the proxy row wins both dedup levels, capability flags reflect what
// each substrate actually observed, and the window scopes rows.
func TestLoadModelValueFacts_DedupAndCapabilities(t *testing.T) {
	t.Parallel()
	st, ctx, now := seedModelValueCorpus(t)

	f, err := st.LoadModelValueFacts(ctx, modelvalue.LoadOptions{WindowDays: 1, Now: now})
	if err != nil {
		t.Fatalf("LoadModelValueFacts: %v", err)
	}
	if f.WindowDays != 1 || !f.GeneratedAt.Equal(now) {
		t.Errorf("window meta = (%d, %v)", f.WindowDays, f.GeneratedAt)
	}
	if len(f.Turns) != 2 {
		t.Fatalf("turns = %d, want 2 (proxy + genuinely-new jsonl); got %+v", len(f.Turns), f.Turns)
	}
	var proxy, jsonl *modelvalue.TurnRow
	for i := range f.Turns {
		if f.Turns[i].Model == "claude-opus-4-8" {
			proxy = &f.Turns[i]
		} else {
			jsonl = &f.Turns[i]
		}
	}
	if proxy == nil || jsonl == nil {
		t.Fatalf("missing substrate arm: %+v", f.Turns)
	}
	if !proxy.HasLatency || !proxy.HasStatus || proxy.TotalMs != 2200 || proxy.HTTPStatus != 200 {
		t.Errorf("proxy capabilities = %+v", proxy)
	}
	if proxy.StoredCostUSD != 0.05 || proxy.MessageCount != 12 || proxy.ToolUseCount != 6 {
		t.Errorf("proxy fidelity fields = %+v", proxy)
	}
	if jsonl.HasLatency || jsonl.HasStatus {
		t.Errorf("jsonl row claims unobserved capabilities: %+v", jsonl)
	}
	if jsonl.Model != "claude-haiku-4-5" || jsonl.Reasoning != 50 || !jsonl.Fast {
		t.Errorf("jsonl fidelity fields = %+v", jsonl)
	}
	if proxy.ProjectRoot != "/repo/acme" || jsonl.ProjectRoot != "/repo/acme" {
		t.Errorf("project roots = %q / %q", proxy.ProjectRoot, jsonl.ProjectRoot)
	}
}

// TestLoadModelValueFacts_ActionBoundaryResolution pins §24.3 at the
// seam: command text becomes a class, permission-mode a phase hint,
// subagent_start a persona name — and no other action's target survives
// into the bundle.
func TestLoadModelValueFacts_ActionBoundaryResolution(t *testing.T) {
	t.Parallel()
	st, ctx, now := seedModelValueCorpus(t)

	f, err := st.LoadModelValueFacts(ctx, modelvalue.LoadOptions{WindowDays: 1, Now: now})
	if err != nil {
		t.Fatalf("LoadModelValueFacts: %v", err)
	}
	if len(f.Actions) != 4 {
		t.Fatalf("actions = %d, want 4", len(f.Actions))
	}
	byType := map[string]modelvalue.ActionRow{}
	for _, a := range f.Actions {
		byType[a.Type] = a
	}
	if got := byType[models.ActionRunCommand]; got.CommandClass != routing.CommandTest {
		t.Errorf("run_command class = %q, want test", got.CommandClass)
	}
	if got := byType[models.ActionPermissionMode]; got.PhaseHint != "plan" {
		t.Errorf("permission_mode phase = %q, want plan", got.PhaseHint)
	}
	if got := byType[models.ActionSubagentStart]; got.SubagentName != "code-reviewer" || !got.IsSidechain {
		t.Errorf("subagent_start = %+v", got)
	}
	// The read_file row's target must not leak through any field.
	read := byType[models.ActionReadFile]
	if read.PhaseHint != "" || read.SubagentName != "" || read.CommandClass != routing.CommandNone {
		t.Errorf("read_file leaked content-derived fields: %+v", read)
	}
}

// TestLoadModelValueFacts_ProjectFilter pins the ProjectRoot scope.
func TestLoadModelValueFacts_ProjectFilter(t *testing.T) {
	t.Parallel()
	st, ctx, now := seedModelValueCorpus(t)

	f, err := st.LoadModelValueFacts(ctx, modelvalue.LoadOptions{
		WindowDays: 1, Now: now, ProjectRoot: "/repo/other",
	})
	if err != nil {
		t.Fatalf("LoadModelValueFacts: %v", err)
	}
	if len(f.Turns) != 0 || len(f.Actions) != 0 {
		t.Errorf("foreign project filter leaked rows: %d turns, %d actions", len(f.Turns), len(f.Actions))
	}
	f, err = st.LoadModelValueFacts(ctx, modelvalue.LoadOptions{
		WindowDays: 1, Now: now, ProjectRoot: "/repo/acme",
	})
	if err != nil {
		t.Fatalf("LoadModelValueFacts (scoped): %v", err)
	}
	if len(f.Turns) != 2 || len(f.Actions) != 4 {
		t.Errorf("scoped load = %d turns, %d actions; want 2/4", len(f.Turns), len(f.Actions))
	}
}

// TestLoadModelValueFacts_WindowExcludesOldRows pins the since cutoff.
func TestLoadModelValueFacts_WindowExcludesOldRows(t *testing.T) {
	t.Parallel()
	st, ctx, now := seedModelValueCorpus(t)

	// Anchor "now" three days later with a 1-day window: everything
	// seeded (≤ 1h old relative to the original anchor) falls out.
	f, err := st.LoadModelValueFacts(ctx, modelvalue.LoadOptions{
		WindowDays: 1, Now: now.AddDate(0, 0, 3),
	})
	if err != nil {
		t.Fatalf("LoadModelValueFacts: %v", err)
	}
	if len(f.Turns) != 0 || len(f.Actions) != 0 {
		t.Errorf("window leaked old rows: %d turns, %d actions", len(f.Turns), len(f.Actions))
	}
}
