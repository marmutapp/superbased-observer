package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// TestRunRetentionPrunesHandoffRows is the WIRING proof for the P4 handoff
// retention sweep: it exercises the runRetention orchestration path (not
// just store.PruneHandoffRows) and asserts an over-horizon handoffs row is
// pruned while a fresh one survives — guarding against the historical
// "prune func shipped but never wired" regression.
func TestRunRetentionPrunesHandoffRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "prune.db")
	database, err := db.Open(ctx, db.Options{Path: path})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer database.Close()

	s := store.New(database)

	oldID, err := s.InsertHandoff(ctx, store.HandoffRecord{
		SourceSessionID: "sess-old", SourceTool: "claude-code", TargetTool: "codex",
		CarryMode: "distilled_tail", Delivery: "file",
	})
	if err != nil {
		t.Fatalf("InsertHandoff(old): %v", err)
	}
	freshID, err := s.InsertHandoff(ctx, store.HandoffRecord{
		SourceSessionID: "sess-fresh", SourceTool: "claude-code", TargetTool: "codex",
		CarryMode: "distilled_tail", Delivery: "file",
	})
	if err != nil {
		t.Fatalf("InsertHandoff(fresh): %v", err)
	}

	// Backdate the old row past the default 180-day horizon.
	old := time.Now().UTC().Add(-200 * 24 * time.Hour).Format(time.RFC3339Nano)
	if _, err := database.ExecContext(ctx,
		`UPDATE handoffs SET created_at = ? WHERE id = ?`, old, oldID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	cfg := config.Default()
	cfg.Observer.DBPath = path
	if cfg.Handoff.RetentionDays != 180 {
		t.Fatalf("default handoff retention = %d, want 180", cfg.Handoff.RetentionDays)
	}

	res, err := runRetention(ctx, cfg, database)
	if err != nil {
		t.Fatalf("runRetention: %v", err)
	}
	if res.HandoffRowsDeleted != 1 {
		t.Errorf("HandoffRowsDeleted = %d, want 1", res.HandoffRowsDeleted)
	}

	got, err := s.ListHandoffs(ctx, 10)
	if err != nil {
		t.Fatalf("ListHandoffs: %v", err)
	}
	if len(got) != 1 || got[0].ID != freshID {
		t.Fatalf("after prune got %d rows (want 1, the fresh row id=%d)", len(got), freshID)
	}

	// Second run within the same horizon is a clean no-op (idempotent).
	res2, err := runRetention(ctx, cfg, database)
	if err != nil {
		t.Fatalf("runRetention(2): %v", err)
	}
	if res2.HandoffRowsDeleted != 0 {
		t.Errorf("second run HandoffRowsDeleted = %d, want 0", res2.HandoffRowsDeleted)
	}
}
