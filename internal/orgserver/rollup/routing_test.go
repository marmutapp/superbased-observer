package rollup

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
)

// seedRouting populates routing_summaries with a small fixture inside the w30
// window (fixedNow = 2026-05-26 → sinceDay 2026-04-26):
//   - two rows on 2026-05-20 (premium/trivial_prompt) split by mode (advise +
//     enforce) — the UNIQUE natural key differs only by mode;
//   - one enforce row on 2026-05-21 (flagship/effort_downshift);
//   - one out-of-window row (2026-04-10) that must be excluded.
func seedRouting(t *testing.T, d *sql.DB) {
	t.Helper()
	ctx := context.Background()
	ins := func(email, day, tier, reason, mode string, dec, app int64, est, forfeit float64) {
		t.Helper()
		if _, err := d.ExecContext(ctx,
			`INSERT INTO routing_summaries
			   (org_id, user_email, day, tier, reason, mode,
			    decisions, applied, est_savings_usd, cache_forfeit_usd,
			    pushed_at, pushed_by_user_id)
			 VALUES ('org1',?,?,?,?,?,?,?,?,?,'2026-05-26T06:00:00Z','u-alice')`,
			email, day, tier, reason, mode, dec, app, est, forfeit); err != nil {
			t.Fatalf("seedRouting insert: %v", err)
		}
	}
	ins("alice@acme.example", "2026-05-20", "premium", "trivial_prompt", "advise", 10, 0, 2.0, 0.0)
	ins("alice@acme.example", "2026-05-20", "premium", "trivial_prompt", "enforce", 5, 5, 1.5, 0.3)
	ins("bob@acme.example", "2026-05-21", "flagship", "effort_downshift", "enforce", 4, 4, 1.0, 0.2)
	// Out-of-window (40 days before fixedNow) — must be excluded.
	ins("alice@acme.example", "2026-04-10", "premium", "trivial_prompt", "enforce", 99, 99, 99.0, 9.0)
}

// TestRouting_EmptyIsNotConfigured pins the honest empty state.
func TestRouting_EmptyIsNotConfigured(t *testing.T) {
	d := newDB(t)
	got, err := Routing(context.Background(), d, w30, fixedNow)
	if err != nil {
		t.Fatalf("Routing: %v", err)
	}
	if got.Configured {
		t.Errorf("Configured = true on empty DB, want false")
	}
	if got.ByDay == nil || got.ByTier == nil || got.ByReason == nil {
		t.Errorf("slices must be non-nil empty, got %+v", got)
	}
	if got.TotalDecisions != 0 || got.NetSavingsUSD != 0 {
		t.Errorf("non-zero totals on empty DB: %+v", got)
	}
}

// TestRouting_Aggregates pins the full aggregation: totals, mode split, net
// savings, the by-day trend, the tier/reason leaderboards, and out-of-window
// exclusion.
func TestRouting_Aggregates(t *testing.T) {
	d := newDB(t)
	seedRouting(t, d)
	got, err := Routing(context.Background(), d, w30, fixedNow)
	if err != nil {
		t.Fatalf("Routing: %v", err)
	}
	if !got.Configured {
		t.Fatalf("Configured = false, want true")
	}
	if got.TotalDecisions != 19 || got.TotalApplied != 9 {
		t.Errorf("decisions/applied = %d/%d, want 19/9 (out-of-window 99 excluded)", got.TotalDecisions, got.TotalApplied)
	}
	if !near(got.EstSavingsUSD, 4.5) || !near(got.CacheForfeitUSD, 0.5) || !near(got.NetSavingsUSD, 4.0) {
		t.Errorf("savings = est %v / forfeit %v / net %v, want 4.5/0.5/4.0", got.EstSavingsUSD, got.CacheForfeitUSD, got.NetSavingsUSD)
	}
	if got.AdviseDecisions != 10 || got.EnforceDecisions != 9 {
		t.Errorf("mode split = advise %d / enforce %d, want 10/9", got.AdviseDecisions, got.EnforceDecisions)
	}

	// By-day trend, ascending.
	if len(got.ByDay) != 2 {
		t.Fatalf("ByDay len = %d, want 2", len(got.ByDay))
	}
	if got.ByDay[0].Date != "2026-05-20" || got.ByDay[0].Decisions != 15 || got.ByDay[0].Applied != 5 || !near(got.ByDay[0].EstSavingsUSD, 3.5) {
		t.Errorf("ByDay[0] = %+v, want 2026-05-20 / 15 / 5 / 3.5", got.ByDay[0])
	}
	if got.ByDay[1].Date != "2026-05-21" || got.ByDay[1].Decisions != 4 {
		t.Errorf("ByDay[1] = %+v, want 2026-05-21 / 4", got.ByDay[1])
	}

	// By-tier, descending by decisions.
	if len(got.ByTier) != 2 || got.ByTier[0].Key != "premium" || got.ByTier[0].Decisions != 15 || got.ByTier[1].Key != "flagship" {
		t.Errorf("ByTier = %+v, want premium(15) then flagship(4)", got.ByTier)
	}
	// By-reason, descending by decisions.
	if len(got.ByReason) != 2 || got.ByReason[0].Key != "trivial_prompt" || got.ByReason[0].Decisions != 15 || got.ByReason[1].Key != "effort_downshift" {
		t.Errorf("ByReason = %+v, want trivial_prompt(15) then effort_downshift(4)", got.ByReason)
	}
}

// TestRouting_NoSentinelColumns is the privacy guard: the marshaled result
// carries no actor identity (user_email / pushed_by_user_id are never selected)
// and the dimensions are closed enums, not model ids.
func TestRouting_NoSentinelColumns(t *testing.T) {
	d := newDB(t)
	seedRouting(t, d)
	got, err := Routing(context.Background(), d, w30, fixedNow)
	if err != nil {
		t.Fatalf("Routing: %v", err)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	if strings.Contains(s, "@") || strings.Contains(s, "alice") || strings.Contains(s, "bob") || strings.Contains(s, "u-alice") {
		t.Errorf("Routing JSON leaked an actor identity (user_email/pushed_by_user_id must never be selected):\n%s", s)
	}
}
