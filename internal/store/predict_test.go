package store

import (
	"context"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/models"
)

func newPredictTestStore(t *testing.T) *Store {
	t.Helper()
	database, err := db.Open(context.Background(), db.Options{Path: t.TempDir() + "/p.db"})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return New(database)
}

func f64(v float64) *float64 { return &v }
func i64(v int64) *int64     { return &v }

func TestLimitSnapshot_RoundTrip(t *testing.T) {
	st := newPredictTestStore(t)
	ctx := context.Background()

	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	in := models.LimitSnapshot{
		ScopeHash:     "default",
		Provider:      "anthropic",
		SessionID:     "sX",
		ObservedAt:    now,
		Window5hUtil:  f64(0.42),
		Window5hReset: i64(now.Add(time.Hour).Unix()),
		Window7dUtil:  f64(0.13),
		ReqRemaining:  i64(987),
		Status:        "allowed",
		Raw:           "anthropic-ratelimit-unified-5h-utilization=42",
	}
	if err := st.InsertLimitSnapshot(ctx, in); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// A newer snapshot should win the "latest" read.
	in2 := in
	in2.ObservedAt = now.Add(time.Minute)
	in2.Window5hUtil = f64(0.55)
	if err := st.InsertLimitSnapshot(ctx, in2); err != nil {
		t.Fatalf("insert 2: %v", err)
	}

	got, ok, err := st.LatestLimitSnapshot(ctx, "default", "anthropic")
	if err != nil || !ok {
		t.Fatalf("latest: ok=%v err=%v", ok, err)
	}
	if got.Window5hUtil == nil || *got.Window5hUtil != 0.55 {
		t.Errorf("latest 5h util = %v, want 0.55 (newest)", got.Window5hUtil)
	}
	if got.Window7dUtil == nil || *got.Window7dUtil != 0.13 {
		t.Errorf("7d util = %v", got.Window7dUtil)
	}
	if got.ReqRemaining == nil || *got.ReqRemaining != 987 {
		t.Errorf("req remaining = %v", got.ReqRemaining)
	}
	if got.Status != "allowed" {
		t.Errorf("status = %q", got.Status)
	}
}

func TestLimitSnapshot_NoneOK(t *testing.T) {
	st := newPredictTestStore(t)
	_, ok, err := st.LatestLimitSnapshot(context.Background(), "default", "anthropic")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ok {
		t.Error("expected ok=false when no snapshot exists")
	}
}

func TestLimitSnapshot_NullablesPreserved(t *testing.T) {
	st := newPredictTestStore(t)
	ctx := context.Background()
	// classic-only snapshot: no window utilization at all.
	in := models.LimitSnapshot{
		ScopeHash:    "default",
		Provider:     "openai",
		ObservedAt:   time.Now().UTC(),
		TokRemaining: i64(120000),
	}
	if err := st.InsertLimitSnapshot(ctx, in); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, ok, err := st.LatestLimitSnapshot(ctx, "default", "openai")
	if err != nil || !ok {
		t.Fatalf("latest: ok=%v err=%v", ok, err)
	}
	if got.Window5hUtil != nil || got.Window7dUtil != nil {
		t.Error("absent window headers should round-trip as nil, not 0")
	}
	if got.TokRemaining == nil || *got.TokRemaining != 120000 {
		t.Errorf("tok remaining = %v, want 120000", got.TokRemaining)
	}
}
