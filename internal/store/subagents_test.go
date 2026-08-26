package store

import (
	"context"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
)

func ingestSubagentAction(t *testing.T, s *Store, sess, sourceEventID string, ts time.Time, ev models.ToolEvent) {
	t.Helper()
	if ev.SourceFile == "" {
		ev.SourceFile = "cc.jsonl"
	}
	if ev.ProjectRoot == "" {
		ev.ProjectRoot = "/proj"
	}
	ev.SourceEventID = sourceEventID
	ev.SessionID = sess
	ev.Timestamp = ts
	ev.Tool = models.ToolClaudeCode
	if _, err := s.Ingest(context.Background(), []models.ToolEvent{ev}, nil, IngestOptions{}); err != nil {
		t.Fatalf("Ingest %s: %v", sourceEventID, err)
	}
}

func TestSidechainActionsForSession_BracketsAndSidechainRows(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	sess, _ := mustProjectAndSession(t, s)

	base := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)

	// Non-sidechain noise must NOT appear.
	ingestSubagentAction(t, s, sess, "n1", base, models.ToolEvent{
		ActionType: models.ActionRunCommand, Target: "npm test", Success: true,
	})
	// Bracket open (hook path: structured agent id).
	ingestSubagentAction(t, s, sess, "s1", base.Add(time.Second), models.ToolEvent{
		ActionType: models.ActionSubagentStart, Target: "Explore", Success: true,
		RawToolName: "agent-1", Metadata: &models.ActionMetadata{AgentID: "agent-1"},
	})
	// Sidechain work inside the window.
	ingestSubagentAction(t, s, sess, "w1", base.Add(2*time.Second), models.ToolEvent{
		ActionType: models.ActionReadFile, Target: "/proj/a.go", Success: true, IsSidechain: true,
	})
	ingestSubagentAction(t, s, sess, "w2", base.Add(3*time.Second), models.ToolEvent{
		ActionType: models.ActionRunCommand, Target: "go test ./...", Success: false, IsSidechain: true,
	})
	// Bracket close.
	ingestSubagentAction(t, s, sess, "s2", base.Add(4*time.Second), models.ToolEvent{
		ActionType: models.ActionSubagentStop, Target: "Explore", Success: true,
		RawToolName: "agent-1", Metadata: &models.ActionMetadata{AgentID: "agent-1"},
	})

	refs, err := s.SidechainActionsForSession(ctx, sess)
	if err != nil {
		t.Fatalf("SidechainActionsForSession: %v", err)
	}
	if len(refs) != 4 {
		t.Fatalf("got %d refs (%+v), want 4 (brackets + sidechain rows, no noise)", len(refs), refs)
	}
	if refs[0].ActionType != models.ActionSubagentStart || refs[3].ActionType != models.ActionSubagentStop {
		t.Fatalf("brackets out of order: %+v", refs)
	}
	if refs[0].Metadata == nil || refs[0].Metadata.AgentID != "agent-1" {
		t.Fatalf("bracket metadata not parsed: %+v", refs[0])
	}
}

func TestSidechainActionsForSession_EmptySession(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	sess, _ := mustProjectAndSession(t, s)

	refs, err := s.SidechainActionsForSession(ctx, sess)
	if err != nil {
		t.Fatalf("SidechainActionsForSession: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("got %d refs, want 0 for a session with no sidechain activity", len(refs))
	}
}

// TestSidechainTokenUsageForSession_Roundtrip pins migration 087's write
// path end to end: TokenEvent.IsSidechain survives InsertTokenEvents into
// token_usage.is_sidechain, main-thread rows stay invisible to the seam,
// and a re-ingest of the same source_event_id with the flag now set HEALS
// the row (the scan --force backfill story — no dedicated pass exists).
func TestSidechainTokenUsageForSession_Roundtrip(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	sess, _ := mustProjectAndSession(t, s)

	base := time.Date(2026, 8, 21, 11, 0, 0, 0, time.UTC)
	mk := func(id string, ts time.Time, side bool) models.TokenEvent {
		return models.TokenEvent{
			SourceFile: "cc.jsonl", SourceEventID: id,
			SessionID: sess, ProjectRoot: "/proj",
			Timestamp: ts, Tool: models.ToolClaudeCode, Model: "claude-opus-4-8",
			InputTokens: 100, OutputTokens: 10, CacheReadTokens: 5,
			EstimatedCostUSD: 0.01,
			Source:           models.TokenSourceJSONL, Reliability: models.ReliabilityUnreliable,
			IsSidechain: side,
		}
	}
	events := []models.TokenEvent{
		mk("main-1", base, false),
		mk("side-1", base.Add(time.Second), true),
		mk("side-2", base.Add(2*time.Second), true),
	}
	if _, err := s.InsertTokenEvents(ctx, events); err != nil {
		t.Fatalf("InsertTokenEvents: %v", err)
	}

	got, err := s.SidechainTokenUsageForSession(ctx, sess)
	if err != nil {
		t.Fatalf("SidechainTokenUsageForSession: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows (%+v), want 2 sidechain rows only", len(got), got)
	}
	var inSum, outSum int64
	for _, r := range got {
		inSum += r.InputTokens
		outSum += r.OutputTokens
	}
	if inSum != 200 || outSum != 20 {
		t.Fatalf("token sums wrong: in=%d out=%d, want 200/20", inSum, outSum)
	}

	// Re-ingest the pre-flag shape of side-2 (what a pre-087 binary wrote):
	// the upsert must take excluded.is_sidechain verbatim, healing the row.
	preFlag := mk("side-2", base.Add(2*time.Second), false)
	if _, err := s.InsertTokenEvents(ctx, []models.TokenEvent{preFlag}); err != nil {
		t.Fatalf("re-ingest: %v", err)
	}
	got, err = s.SidechainTokenUsageForSession(ctx, sess)
	if err != nil {
		t.Fatalf("SidechainTokenUsageForSession(re): %v", err)
	}
	if len(got) != 1 || got[0].InputTokens != 100 {
		t.Fatalf("heal failed: %+v, want only side-1 left", got)
	}

	// And the reverse heal: re-parse with the flag restores it.
	if _, err := s.InsertTokenEvents(ctx, []models.TokenEvent{mk("side-2", base.Add(2*time.Second), true)}); err != nil {
		t.Fatalf("re-ingest 2: %v", err)
	}
	if got, err = s.SidechainTokenUsageForSession(ctx, sess); err != nil || len(got) != 2 {
		t.Fatalf("reverse heal failed: n=%d err=%v", len(got), err)
	}
}
