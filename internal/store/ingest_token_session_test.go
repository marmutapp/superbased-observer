package store

import (
	"context"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// TestIngestTokenAttachesToExistingSessionWithoutProjectRoot pins the cursor
// usage-capture fix: Cursor's stop hook carries usage but no decodable project
// root, and token_usage attaches by session_id (there is no project_id column).
// A usage event with an empty ProjectRoot must still land when its session
// already exists — and must still be skipped when the session is unknown
// (nothing to attach to, and no project to bootstrap a phantom session).
func TestIngestTokenAttachesToExistingSessionWithoutProjectRoot(t *testing.T) {
	t.Parallel()
	s, d := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 5, 13, 18, 0, 0, time.UTC)

	// A prior action event creates the session with a resolved project
	// (this is what Cursor's afterAgentResponse / the watcher do before stop).
	seed := []models.ToolEvent{{
		SourceFile: "cursor:hook", SourceEventID: "seed-1",
		SessionID: "conv-existing", ProjectRoot: "/repo",
		Timestamp: now, Tool: models.ToolCursor,
		ActionType: models.ActionAssistantMessage, RawToolName: "cursor.assistant_response",
		Success: true,
	}}
	if _, err := s.Ingest(ctx, seed, nil, IngestOptions{}); err != nil {
		t.Fatalf("seed ingest: %v", err)
	}

	// The stop-hook usage event: valid session, EMPTY project root. Passed in
	// its own batch (as the hook does), so sessionsSeen is empty and only the
	// DB-existence fallback can save it.
	stopUsage := models.TokenEvent{
		SourceFile: "cursor:hook", SourceEventID: "gen-1:stop",
		SessionID: "conv-existing", ProjectRoot: "",
		Timestamp: now.Add(time.Minute), Tool: models.ToolCursor, Model: "composer-2.5",
		InputTokens: 276, OutputTokens: 3251, CacheReadTokens: 223171,
		Source: models.TokenSourceHook, Reliability: models.ReliabilityAccurate,
	}
	// A usage event whose session does NOT exist must be skipped (no phantom).
	ghostUsage := stopUsage
	ghostUsage.SessionID = "conv-ghost"
	ghostUsage.SourceEventID = "gen-9:stop"

	if _, err := s.Ingest(ctx, nil, []models.TokenEvent{stopUsage, ghostUsage}, IngestOptions{}); err != nil {
		t.Fatalf("usage ingest: %v", err)
	}

	var rows, inSum, outSum int64
	if err := d.QueryRowContext(ctx,
		`SELECT count(*), COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0)
		   FROM token_usage WHERE session_id = 'conv-existing'`).Scan(&rows, &inSum, &outSum); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || inSum != 276 || outSum != 3251 {
		t.Errorf("existing-session rows/in/out = %d/%d/%d, want 1/276/3251", rows, inSum, outSum)
	}

	var ghost int64
	if err := d.QueryRowContext(ctx,
		`SELECT count(*) FROM token_usage WHERE session_id = 'conv-ghost'`).Scan(&ghost); err != nil {
		t.Fatal(err)
	}
	if ghost != 0 {
		t.Errorf("ghost-session rows = %d, want 0 (must not bootstrap a phantom session)", ghost)
	}
}
