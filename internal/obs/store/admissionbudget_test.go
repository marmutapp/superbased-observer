package store

import (
	"context"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/obs/span"
)

// TestUserSpend confirms the per-end-user spend seam sums obs_spans.cost_usd
// through obs_traces.user, filters by the window, isolates users, and excludes
// unattributed spend.
func TestUserSpend(t *testing.T) {
	ctx := context.Background()
	s := newDB(t)
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-1 * time.Hour)
	old := now.Add(-10 * 24 * time.Hour) // 10 days ago (outside a 7-day window)

	// alice: one recent span ($0.30) + one old span ($0.50).
	mustTrace(t, s, "ta1", "alice", recent)
	mustTrace(t, s, "ta2", "alice", old)
	// bob: one recent span ($0.20).
	mustTrace(t, s, "tb1", "bob", recent)
	// anonymous (no user): one recent span ($9.99) — must never count.
	mustTrace(t, s, "tx1", "", recent)

	mustSpan(t, s, "sa1", "ta1", 0.30, recent)
	mustSpan(t, s, "sa2", "ta2", 0.50, old)
	mustSpan(t, s, "sb1", "tb1", 0.20, recent)
	mustSpan(t, s, "sx1", "tx1", 9.99, recent)

	weekAgo := now.AddDate(0, 0, -7)

	// alice weekly window: only the recent $0.30 (old $0.50 excluded).
	if got, err := s.UserSpend(ctx, "alice", weekAgo); err != nil {
		t.Fatalf("UserSpend alice weekly: %v", err)
	} else if !approxEq(got, 0.30) {
		t.Errorf("alice weekly spend = %v, want 0.30", got)
	}

	// alice all-time (since epoch): $0.30 + $0.50 = $0.80.
	if got, err := s.UserSpend(ctx, "alice", time.Unix(0, 0)); err != nil {
		t.Fatalf("UserSpend alice all: %v", err)
	} else if !approxEq(got, 0.80) {
		t.Errorf("alice all-time spend = %v, want 0.80", got)
	}

	// bob is isolated from alice.
	if got, err := s.UserSpend(ctx, "bob", weekAgo); err != nil {
		t.Fatalf("UserSpend bob: %v", err)
	} else if !approxEq(got, 0.20) {
		t.Errorf("bob weekly spend = %v, want 0.20", got)
	}

	// empty user → 0 (anonymous spend is never a per-user total).
	if got, err := s.UserSpend(ctx, "", weekAgo); err != nil {
		t.Fatalf("UserSpend empty: %v", err)
	} else if got != 0 {
		t.Errorf("empty-user spend = %v, want 0", got)
	}
}

func mustTrace(t *testing.T, s *Store, id, user string, start time.Time) {
	t.Helper()
	if err := s.UpsertTrace(context.Background(), span.Trace{
		TraceID: id, User: user, Source: span.SourceOTLPTrace, StartedAt: start,
	}); err != nil {
		t.Fatalf("UpsertTrace %s: %v", id, err)
	}
}

func mustSpan(t *testing.T, s *Store, id, trace string, cost float64, start time.Time) {
	t.Helper()
	c := cost
	if err := s.UpsertSpansBatch(context.Background(), []span.Span{
		{SpanID: id, TraceID: trace, Kind: span.KindLLM, Name: "chat", CostUSD: &c, StartedAt: start},
	}); err != nil {
		t.Fatalf("UpsertSpansBatch %s: %v", id, err)
	}
}

func approxEq(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }

// TestTopUserSpend confirms the status seam ranks end-users by month-to-date
// spend with per-window totals, and excludes unattributed spend.
func TestTopUserSpend(t *testing.T) {
	ctx := context.Background()
	s := newDB(t)
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-1 * time.Hour)     // within 5h + week + month
	older := now.Add(-3 * 24 * time.Hour) // within week + month, outside 5h

	mustTrace(t, s, "tp-a", "alice", recent)
	mustTrace(t, s, "tp-a2", "alice", older)
	mustTrace(t, s, "tp-b", "bob", recent)
	mustTrace(t, s, "tp-x", "", recent) // anonymous — excluded
	mustSpan(t, s, "sp-a", "tp-a", 2.00, recent)
	mustSpan(t, s, "sp-a2", "tp-a2", 3.00, older)
	mustSpan(t, s, "sp-b", "tp-b", 1.00, recent)
	mustSpan(t, s, "sp-x", "tp-x", 99.0, recent)

	rows, err := s.TopUserSpend(ctx, now, 10)
	if err != nil {
		t.Fatalf("TopUserSpend: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (anonymous excluded): %+v", len(rows), rows)
	}
	if rows[0].User != "alice" {
		t.Errorf("top spender = %q, want alice", rows[0].User)
	}
	if !approxEq(rows[0].Monthly, 5.00) || !approxEq(rows[0].Weekly, 5.00) || !approxEq(rows[0].FiveHour, 2.00) {
		t.Errorf("alice windows = 5h %.2f / wk %.2f / mo %.2f, want 2.00 / 5.00 / 5.00",
			rows[0].FiveHour, rows[0].Weekly, rows[0].Monthly)
	}
}

// TestCountBudgetBreaches confirms only budget.user_* verdicts in-window count.
func TestCountBudgetBreaches(t *testing.T) {
	ctx := context.Background()
	s := newDB(t)
	now := time.Now().UTC()
	ins := func(crit string, when time.Time) {
		if _, err := s.InsertAdmissionEvent(ctx, AdmissionEventRow{
			TS: when, Mode: "observe", Decision: "deny", CriterionID: crit, MessageHash: "h",
		}); err != nil {
			t.Fatalf("InsertAdmissionEvent: %v", err)
		}
	}
	ins("budget.user_monthly", now.Add(-1*time.Hour)) // counts
	ins("budget.user_5h", now.Add(-2*time.Hour))      // counts
	ins("jailbreak", now.Add(-1*time.Hour))           // wrong criterion
	ins("budget.user_weekly", now.Add(-48*time.Hour)) // out of 24h window

	n, err := s.CountBudgetBreaches(ctx, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("CountBudgetBreaches: %v", err)
	}
	if n != 2 {
		t.Errorf("count = %d, want 2 (budget.user_* within 24h only)", n)
	}
}
