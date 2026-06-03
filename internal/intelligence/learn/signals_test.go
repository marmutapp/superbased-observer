package learn

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// seedAction inserts a single read_file action and returns its id, so
// search_hit signals (whose action_id is FK-checked) have something
// real to point at. Keep deps light — `learn` already imports models
// and store via the rest of the package.
func seedAction(t *testing.T, database *sql.DB, idx int) int64 {
	t.Helper()
	st := store.New(database)
	ctx := context.Background()
	root := t.TempDir()
	pid, err := st.UpsertProject(ctx, root, "")
	if err != nil {
		t.Fatal(err)
	}
	sessionID := "sess-K43-" + string(rune('a'+idx))
	if err := st.UpsertSession(ctx, models.Session{
		ID: sessionID, ProjectID: pid, Tool: models.ToolClaudeCode,
		StartedAt: time.Date(2026, 5, 7, 12, idx, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Ingest(ctx, []models.ToolEvent{{
		SourceFile: "k.jsonl", SourceEventID: sessionID,
		SessionID: sessionID, ProjectRoot: root,
		Timestamp:  time.Date(2026, 5, 7, 12, idx, 1, 0, time.UTC),
		Tool:       models.ToolClaudeCode,
		ActionType: models.ActionReadFile, Target: "x.go",
		Success: true, RawToolName: "Read",
	}}, nil, store.IngestOptions{}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	var id int64
	if err := database.QueryRowContext(ctx,
		`SELECT id FROM actions WHERE session_id = ? LIMIT 1`, sessionID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestSignalRecorder_RecordRetrieveStashed pins one row landing per
// successful Record call.
func TestSignalRecorder_RecordRetrieveStashed(t *testing.T) {
	database, err := db.Open(context.Background(), db.Options{Path: filepath.Join(t.TempDir(), "k.db")})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	r := NewSignalRecorder(database)
	for i := 0; i < 3; i++ {
		if err := r.RecordRetrieveStashed(context.Background(),
			"deadbeef00112233445566778899aabbccddeeff00112233445566778899aabb",
			"sess-K43"); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM retrieval_signals WHERE signal_type = 'retrieve_stashed'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("count: %d, want 3", n)
	}
}

// TestSignalRecorder_RecordSearchHit pins action_id is preserved on
// "search_hit" signals.
func TestSignalRecorder_RecordSearchHit(t *testing.T) {
	database, err := db.Open(context.Background(), db.Options{Path: filepath.Join(t.TempDir(), "k.db")})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	aid := seedAction(t, database, 0)
	r := NewSignalRecorder(database)
	if err := r.RecordSearchHit(context.Background(), aid, "FAIL", ""); err != nil {
		t.Fatalf("Record: %v", err)
	}

	var actionID int64
	var sigType, payload string
	if err := database.QueryRow(
		`SELECT action_id, signal_type, payload FROM retrieval_signals LIMIT 1`,
	).Scan(&actionID, &sigType, &payload); err != nil {
		t.Fatal(err)
	}
	if actionID != aid {
		t.Errorf("action_id: %d, want %d", actionID, aid)
	}
	if sigType != "search_hit" {
		t.Errorf("signal_type: %q", sigType)
	}
	if payload != "FAIL" {
		t.Errorf("payload: %q", payload)
	}
}

// TestSignalRecorder_NilSafe pins that calling Record on a nil
// recorder is a no-op (the interface lets callers omit it).
func TestSignalRecorder_NilSafe(t *testing.T) {
	var r *SignalRecorder
	if err := r.RecordRetrieveStashed(context.Background(), "x", ""); err != nil {
		t.Errorf("nil-recv RecordRetrieveStashed: %v", err)
	}
	if err := r.RecordSearchHit(context.Background(), 1, "q", ""); err != nil {
		t.Errorf("nil-recv RecordSearchHit: %v", err)
	}
}

// TestPatternMiner_ReportAggregates pins the K43 aggregate shape: per-
// type counts + top-shas + top-actions, scoped to the lookback window.
func TestPatternMiner_ReportAggregates(t *testing.T) {
	database, err := db.Open(context.Background(), db.Options{Path: filepath.Join(t.TempDir(), "k.db")})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	r := NewSignalRecorder(database)
	ctx := context.Background()
	// Sha A retrieved 5 times, sha B 2 times.
	for i := 0; i < 5; i++ {
		_ = r.RecordRetrieveStashed(ctx, "sha-A", "")
	}
	for i := 0; i < 2; i++ {
		_ = r.RecordRetrieveStashed(ctx, "sha-B", "")
	}
	a1 := seedAction(t, database, 0)
	a2 := seedAction(t, database, 1)
	for i := 0; i < 3; i++ {
		_ = r.RecordSearchHit(ctx, a1, "FAIL", "")
	}
	_ = r.RecordSearchHit(ctx, a2, "panic", "")

	rep, err := NewPatternMiner(database).Report(ctx, ReportOptions{Days: 7})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if rep.StashRetrievals != 7 {
		t.Errorf("StashRetrievals: %d, want 7", rep.StashRetrievals)
	}
	if rep.SearchHits != 4 {
		t.Errorf("SearchHits: %d, want 4", rep.SearchHits)
	}
	if len(rep.TopRetrievedShas) == 0 || rep.TopRetrievedShas[0].Sha != "sha-A" || rep.TopRetrievedShas[0].Count != 5 {
		t.Errorf("top sha mismatch: %v", rep.TopRetrievedShas)
	}
	if len(rep.TopSearchedActions) == 0 || rep.TopSearchedActions[0].ActionID != a1 || rep.TopSearchedActions[0].Count != 3 {
		t.Errorf("top action mismatch (want id=%d count=3): %v", a1, rep.TopSearchedActions)
	}
}

// seedStashEvent inserts a compression_events row with mechanism =
// 'stash' so PatternMiner.Report has a denominator for retrieve_rate.
// We need an api_turn_id to satisfy the FK; the helper creates a
// minimal session + api_turn parent chain.
func seedStashEvent(t *testing.T, database *sql.DB, count int) {
	t.Helper()
	st := store.New(database)
	ctx := context.Background()
	root := t.TempDir()
	pid, err := st.UpsertProject(ctx, root, "")
	if err != nil {
		t.Fatal(err)
	}
	sessionID := "sess-stash"
	if err := st.UpsertSession(ctx, models.Session{
		ID: sessionID, ProjectID: pid, Tool: models.ToolClaudeCode,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := database.ExecContext(ctx,
		`INSERT INTO api_turns (session_id, timestamp, provider, model, input_tokens, output_tokens)
		 VALUES (?, ?, 'anthropic', 'claude-sonnet', 10, 10)`,
		sessionID, now)
	if err != nil {
		t.Fatal(err)
	}
	turnID, _ := res.LastInsertId()
	for i := 0; i < count; i++ {
		if _, err := database.ExecContext(ctx,
			`INSERT INTO compression_events (api_turn_id, timestamp, mechanism, original_bytes, compressed_bytes)
			 VALUES (?, ?, 'stash', 9000, 80)`,
			turnID, now); err != nil {
			t.Fatalf("seed stash event: %v", err)
		}
	}
}

// TestPatternMiner_RetrieveRateAndHints pins the v1.4.43+ K43 hint
// surface: TotalStashes is read from compression_events; RetrieveRate
// is the ratio; Hints fires the "consider lowering threshold" band
// when retrieve_rate >= 5%, and the "info: zero retrieves" band
// when no retrieves but ≥10 stashes.
func TestPatternMiner_RetrieveRateAndHints(t *testing.T) {
	t.Run("zero retrieves on many stashes → info hint", func(t *testing.T) {
		database, err := db.Open(context.Background(), db.Options{Path: filepath.Join(t.TempDir(), "k.db")})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { database.Close() })
		seedStashEvent(t, database, 12) // ≥10 trigger
		rep, err := NewPatternMiner(database).Report(context.Background(), ReportOptions{Days: 7})
		if err != nil {
			t.Fatal(err)
		}
		if rep.TotalStashes != 12 {
			t.Errorf("TotalStashes: %d want 12", rep.TotalStashes)
		}
		if rep.RetrieveRate != 0 {
			t.Errorf("RetrieveRate: %v want 0", rep.RetrieveRate)
		}
		if len(rep.Hints) != 1 || rep.Hints[0].Severity != "info" {
			t.Fatalf("expected one info hint, got %+v", rep.Hints)
		}
		if rep.Hints[0].Kind != "stash-retrieve-rate" {
			t.Errorf("Hint Kind: %q", rep.Hints[0].Kind)
		}
	})
	t.Run("retrieve_rate >= 5% → consider hint", func(t *testing.T) {
		database, err := db.Open(context.Background(), db.Options{Path: filepath.Join(t.TempDir(), "k.db")})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { database.Close() })
		seedStashEvent(t, database, 10)
		r := NewSignalRecorder(database)
		ctx := context.Background()
		// 1 retrieve / 10 stashes = 10% rate (≥5% trigger).
		_ = r.RecordRetrieveStashed(ctx, "sha-X", "")
		rep, err := NewPatternMiner(database).Report(ctx, ReportOptions{Days: 7})
		if err != nil {
			t.Fatal(err)
		}
		if rep.RetrieveRate < 0.099 || rep.RetrieveRate > 0.101 {
			t.Errorf("RetrieveRate: %v want ~0.10", rep.RetrieveRate)
		}
		if len(rep.Hints) != 1 || rep.Hints[0].Severity != "consider" {
			t.Fatalf("expected one 'consider' hint, got %+v", rep.Hints)
		}
	})
	t.Run("zero stashes → no hints", func(t *testing.T) {
		database, err := db.Open(context.Background(), db.Options{Path: filepath.Join(t.TempDir(), "k.db")})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { database.Close() })
		rep, err := NewPatternMiner(database).Report(context.Background(), ReportOptions{Days: 7})
		if err != nil {
			t.Fatal(err)
		}
		if len(rep.Hints) != 0 {
			t.Errorf("fresh install should produce no hints, got %v", rep.Hints)
		}
	})
}

// TestPatternMiner_Report_OutsideWindowExcluded pins that signals
// older than the lookback window aren't counted.
func TestPatternMiner_Report_OutsideWindowExcluded(t *testing.T) {
	database, err := db.Open(context.Background(), db.Options{Path: filepath.Join(t.TempDir(), "k.db")})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	// Backdate one row to 30 days ago.
	old := time.Now().UTC().AddDate(0, 0, -30).Format(time.RFC3339Nano)
	if _, err := database.Exec(
		`INSERT INTO retrieval_signals (action_id, signal_type, signal_at, session_id, payload)
		 VALUES (NULL, 'retrieve_stashed', ?, NULL, 'sha-old')`,
		old,
	); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	// Recent row.
	_ = NewSignalRecorder(database).RecordRetrieveStashed(context.Background(), "sha-fresh", "")

	rep, err := NewPatternMiner(database).Report(context.Background(), ReportOptions{Days: 7}) // 7-day window
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if rep.StashRetrievals != 1 {
		t.Errorf("StashRetrievals: %d, want 1 (older row excluded)", rep.StashRetrievals)
	}
}
