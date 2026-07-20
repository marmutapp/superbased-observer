package store

import (
	"context"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/obs/alert"
	"github.com/marmutapp/superbased-observer/internal/obs/span"
)

// TestAlertSummary confirms the node-side metric snapshot computes error rate,
// summed cost, and p95 latency over the window off the local obs_* tables.
func TestAlertSummary(t *testing.T) {
	ctx := context.Background()
	s := newDB(t)
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)

	// Four traces in-window: 1 error, 3 ok → error_rate = 0.25.
	traces := []struct {
		id     string
		status span.Status
		off    time.Duration // before now
	}{
		{"t1", span.StatusError, 5 * time.Minute},
		{"t2", span.StatusOK, 5 * time.Minute},
		{"t3", span.StatusOK, 5 * time.Minute},
		{"t4", span.StatusOK, 5 * time.Minute},
		{"t5", span.StatusError, 90 * time.Minute}, // OUT of a 60m window
	}
	for _, tr := range traces {
		if err := s.UpsertTrace(ctx, span.Trace{
			TraceID: tr.id, Source: "test", Status: tr.status,
			StartedAt: now.Add(-tr.off),
		}); err != nil {
			t.Fatalf("UpsertTrace: %v", err)
		}
	}

	// Spans: costs + durations. Four in-window (100/200/300/400ms → nearest-
	// rank p95 lands on 300ms), one out of window. Cost is carried on the first
	// two only; the out-of-window 99 must not count.
	dur := func(off time.Duration, ms int) (time.Time, time.Time) {
		start := now.Add(-off)
		return start, start.Add(time.Duration(ms) * time.Millisecond)
	}
	mk := func(id, trace string, off time.Duration, ms int, cost *float64) span.Span {
		st, en := dur(off, ms)
		return span.Span{
			SpanID: id, TraceID: trace, Kind: "llm", Status: span.StatusOK,
			StartedAt: st, EndedAt: en, CostUSD: cost,
		}
	}
	spans := []span.Span{
		mk("s1", "t1", 5*time.Minute, 100, f64(1.0)),
		mk("s2", "t2", 4*time.Minute, 300, f64(2.5)),
		mk("s3", "t3", 3*time.Minute, 200, nil),
		mk("s4", "t4", 2*time.Minute, 400, nil),
		mk("s5", "t5", 90*time.Minute, 999, f64(99)), // out of a 60m window
	}
	if err := s.UpsertSpansBatch(ctx, spans); err != nil {
		t.Fatalf("UpsertSpansBatch: %v", err)
	}

	got, err := s.AlertSummary(ctx, 60, now)
	if err != nil {
		t.Fatalf("AlertSummary: %v", err)
	}
	if got.ErrorRate != 0.25 {
		t.Errorf("error_rate = %v, want 0.25", got.ErrorRate)
	}
	if got.CostUSD != 3.5 {
		t.Errorf("cost_usd = %v, want 3.5 (99 out of window)", got.CostUSD)
	}
	if got.LatencyP95Ms != 300 {
		t.Errorf("latency_p95_ms = %d, want 300", got.LatencyP95Ms)
	}
}

// TestAlertEventPersistAndDedup confirms InsertAlertEvent + LastAlertFired form
// the cooldown/dedup anchor (last-fired derived from the log, one owner) and
// RecentAlertEvents reads newest-first.
func TestAlertEventPersistAndDedup(t *testing.T) {
	ctx := context.Background()
	s := newDB(t)

	// No fire yet.
	if _, ok, err := s.LastAlertFired(ctx, "err"); err != nil || ok {
		t.Fatalf("LastAlertFired empty: ok=%v err=%v", ok, err)
	}

	t1 := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(30 * time.Minute)
	if err := s.InsertAlertEvent(ctx, alert.Fired{
		RuleName: "err", Metric: alert.MetricErrorRate, Comparator: "gt",
		Threshold: 0.1, Value: 0.5, WindowMinutes: 15, FiredAt: t1,
	}, true); err != nil {
		t.Fatalf("InsertAlertEvent t1: %v", err)
	}
	if err := s.InsertAlertEvent(ctx, alert.Fired{
		RuleName: "err", Metric: alert.MetricErrorRate, Comparator: "gt",
		Threshold: 0.1, Value: 0.7, WindowMinutes: 15, FiredAt: t2,
	}, false); err != nil {
		t.Fatalf("InsertAlertEvent t2: %v", err)
	}

	// Last-fired = MAX(fired_at) = t2 (the cooldown anchor).
	lf, ok, err := s.LastAlertFired(ctx, "err")
	if err != nil || !ok {
		t.Fatalf("LastAlertFired: ok=%v err=%v", ok, err)
	}
	if !lf.Equal(t2) {
		t.Errorf("last-fired = %v, want %v", lf, t2)
	}

	// A different rule has no fire.
	if _, ok, _ := s.LastAlertFired(ctx, "cost"); ok {
		t.Error("unrelated rule should have no last-fired")
	}

	// Recent = newest first, with delivered flags preserved.
	rows, err := s.RecentAlertEvents(ctx, 10)
	if err != nil {
		t.Fatalf("RecentAlertEvents: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("recent len = %d, want 2", len(rows))
	}
	if !rows[0].FiredAt.Equal(t2) || rows[0].Delivered {
		t.Errorf("row0 = %+v, want t2 delivered=false", rows[0])
	}
	if !rows[1].FiredAt.Equal(t1) || !rows[1].Delivered {
		t.Errorf("row1 = %+v, want t1 delivered=true", rows[1])
	}
}
