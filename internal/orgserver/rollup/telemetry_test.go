package rollup

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
)

// testSeatPriceUSD is the Copilot Business plan price the telemetry rollup
// multiplies seat counts by.
const testSeatPriceUSD = 19.0

// seedTelemetry populates the three native-console analytics tables with a
// small cross-vendor fixture inside the w30 window (fixedNow = 2026-05-26):
//   - Claude Code: cost, tokens, an accept/reject pair, and engagement counts.
//   - Codex: cost in BOTH units (usd openai-org + credits chatgpt-enterprise),
//     tokens, and count metrics across both surfaces.
//   - Copilot: a billing overage (usd), engagement counts, and TWO seat
//     snapshots on different days (the rollup must read only the latest).
//   - One out-of-window CC row that must be excluded.
func seedTelemetry(t *testing.T, d *sql.DB) {
	t.Helper()
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seedTelemetry exec: %v\n%s", err, q)
		}
	}

	cc := func(day, metric string, val float64) {
		exec(`INSERT INTO cc_analytics_daily (day, user_key, actor_type, metric, value, org_id, pulled_at)
		      VALUES (?, 'alice@acme.example', 'user_actor', ?, ?, 'org1', '2026-05-26T06:00:00Z')`, day, metric, val)
	}
	cc("2026-05-20", "cost_usd", 1.20)
	cc("2026-05-20", "tokens_input", 1000)
	cc("2026-05-20", "tokens_output", 400)
	cc("2026-05-20", "tokens_cache_read", 800)
	cc("2026-05-20", "tokens_cache_creation", 200)
	cc("2026-05-20", "tool_Edit_accepted", 30)
	cc("2026-05-20", "tool_Edit_rejected", 10)
	cc("2026-05-21", "tool_Write_accepted", 10)
	cc("2026-05-20", "sessions", 5)
	cc("2026-05-20", "commits", 3)
	// Out-of-window row (40 days before fixedNow) — must be excluded.
	cc("2026-04-10", "cost_usd", 99.0)

	codex := func(day, surface, unit, metric string, val float64) {
		exec(`INSERT INTO codex_analytics_daily (day, user_key, actor_type, surface, unit, metric, value, org_id, pulled_at)
		      VALUES (?, 'u1', 'user', ?, ?, ?, ?, 'org1', '2026-05-26T05:00:00Z')`, day, surface, unit, metric, val)
	}
	codex("2026-05-22", "openai_org", "usd", "cost", 0.50)
	codex("2026-05-22", "chatgpt_enterprise", "credits", "cost", 120)
	codex("2026-05-22", "openai_org", "tokens", "tokens_input", 2000)
	codex("2026-05-22", "openai_org", "tokens", "tokens_output", 600)
	codex("2026-05-22", "chatgpt_enterprise", "count", "threads", 7)
	codex("2026-05-22", "chatgpt_enterprise", "count", "turns", 25)

	copilot := func(day, surface, unit, metric, userKey string, val float64) {
		exec(`INSERT INTO copilot_analytics_daily (day, user_key, actor_type, surface, unit, metric, value, org_id, owner, pulled_at)
		      VALUES (?, ?, 'org', ?, ?, ?, ?, 'org1', 'acme', '2026-05-26T04:00:00Z')`, day, userKey, surface, unit, metric, val)
	}
	copilot("2026-05-23", "billing", "usd", "cost", "__org__", 4.00)
	copilot("2026-05-23", "engagement", "count", "code_suggestions", "__org__", 500)
	copilot("2026-05-23", "engagement", "count", "code_acceptances", "__org__", 300)
	copilot("2026-05-23", "engagement", "count", "active_users", "__org__", 9) // skipped (point-in-time)
	// Two seat snapshots — the rollup must use only the LATEST (2026-05-24).
	copilot("2026-05-19", "seats", "seats", "seats_total", "__org__", 50)
	copilot("2026-05-19", "seats", "seats", "seats_active", "__org__", 20)
	copilot("2026-05-24", "seats", "seats", "seats_total", "__org__", 50)
	copilot("2026-05-24", "seats", "seats", "seats_active", "__org__", 40)
	copilot("2026-05-24", "seats", "seats", "seats_inactive", "__org__", 10)
}

func vendorMap(in []VendorTelemetry) map[string]VendorTelemetry {
	m := map[string]VendorTelemetry{}
	for _, v := range in {
		m[v.Vendor] = v
	}
	return m
}

// TestTelemetry_EmptyIsNotConfigured pins the honest empty state: no poller has
// run, so Configured is false and Vendors is an empty (non-nil) slice.
func TestTelemetry_EmptyIsNotConfigured(t *testing.T) {
	d := newDB(t)
	got, err := Telemetry(context.Background(), d, w30, fixedNow, testSeatPriceUSD)
	if err != nil {
		t.Fatalf("Telemetry: %v", err)
	}
	if got.Configured {
		t.Errorf("Configured = true on an empty DB, want false")
	}
	if got.Vendors == nil || len(got.Vendors) != 0 {
		t.Errorf("Vendors = %v, want empty non-nil slice", got.Vendors)
	}
}

// TestTelemetry_CrossVendor pins the full cross-vendor aggregation: cost units
// kept distinct, CC acceptance rate, Codex surfaces, Copilot latest seat
// snapshot + utilization, and the out-of-window exclusion.
func TestTelemetry_CrossVendor(t *testing.T) {
	d := newDB(t)
	seedTelemetry(t, d)
	got, err := Telemetry(context.Background(), d, w30, fixedNow, testSeatPriceUSD)
	if err != nil {
		t.Fatalf("Telemetry: %v", err)
	}
	if !got.Configured || len(got.Vendors) != 3 {
		t.Fatalf("Configured=%v vendors=%d, want true/3", got.Configured, len(got.Vendors))
	}
	m := vendorMap(got.Vendors)

	cc := m["claude_code"]
	if !near(cc.CostUSD, 1.20) || cc.CostUnit != "usd" { // 99.0 out-of-window dropped
		t.Errorf("cc cost = %v unit %q, want 1.20/usd (out-of-window 99.0 must be excluded)", cc.CostUSD, cc.CostUnit)
	}
	if cc.Tokens == nil || cc.Tokens.NetInput != 1000 || cc.Tokens.Output != 400 || cc.Tokens.CacheRead != 800 || cc.Tokens.CacheWrite != 200 {
		t.Errorf("cc tokens = %+v, want 1000/400/800/200", cc.Tokens)
	}
	if cc.Acceptance == nil || cc.Acceptance.Accepted != 40 || cc.Acceptance.Rejected != 10 || !near(cc.Acceptance.AcceptRate, 0.8) {
		t.Errorf("cc acceptance = %+v, want 40 accepted / 10 rejected / 0.8", cc.Acceptance)
	}
	if cc.Days != 2 {
		t.Errorf("cc days = %d, want 2 (2026-05-20 + 21)", cc.Days)
	}

	codex := m["codex"]
	if !near(codex.CostUSD, 0.50) || !near(codex.CreditsCost, 120) || codex.CostUnit != "mixed" {
		t.Errorf("codex cost = %v usd / %v credits / unit %q, want 0.50/120/mixed", codex.CostUSD, codex.CreditsCost, codex.CostUnit)
	}
	if len(codex.Surfaces) != 2 || codex.Surfaces[0] != "chatgpt_enterprise" || codex.Surfaces[1] != "openai_org" {
		t.Errorf("codex surfaces = %v, want [chatgpt_enterprise openai_org]", codex.Surfaces)
	}
	if codex.Tokens == nil || codex.Tokens.NetInput != 2000 || codex.Tokens.Output != 600 {
		t.Errorf("codex tokens = %+v, want 2000/600", codex.Tokens)
	}

	cop := m["copilot"]
	if !near(cop.CostUSD, 4.00) || cop.CostUnit != "usd" {
		t.Errorf("copilot cost = %v unit %q, want 4.00/usd", cop.CostUSD, cop.CostUnit)
	}
	if cop.Seats == nil || cop.Seats.Total != 50 || cop.Seats.Active != 40 || cop.Seats.Inactive != 10 || !near(cop.Seats.Utilization, 0.8) {
		t.Errorf("copilot seats = %+v, want latest snapshot 50/40/10 util 0.8 (NOT the 2026-05-19 snapshot)", cop.Seats)
	}
	if cop.Tokens != nil {
		t.Errorf("copilot tokens = %+v, want nil (Copilot has no token data)", cop.Tokens)
	}
	// Engagement: code_suggestions + code_acceptances kept, active_users skipped.
	em := map[string]int64{}
	for _, e := range cop.Engagement {
		em[e.Key] = e.Count
	}
	if em["code_suggestions"] != 500 || em["code_acceptances"] != 300 {
		t.Errorf("copilot engagement = %+v, want suggestions 500 / acceptances 300", cop.Engagement)
	}
	if _, leaked := em["active_users"]; leaked {
		t.Errorf("copilot engagement leaked active_users (point-in-time, must be skipped): %+v", cop.Engagement)
	}
}

// TestTelemetry_Degradation confirms a vendor with only engagement rows (no
// seats, no cost, no tokens) degrades to honest nils rather than fake zeros.
func TestTelemetry_Degradation(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	if _, err := d.ExecContext(ctx,
		`INSERT INTO copilot_analytics_daily (day, user_key, actor_type, surface, unit, metric, value, org_id, owner, pulled_at)
		 VALUES ('2026-05-23','__org__','org','engagement','count','chats',5,'org1','acme','2026-05-26T04:00:00Z')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := Telemetry(ctx, d, w30, fixedNow, testSeatPriceUSD)
	if err != nil {
		t.Fatalf("Telemetry: %v", err)
	}
	m := vendorMap(got.Vendors)
	cop, ok := m["copilot"]
	if !ok {
		t.Fatalf("copilot vendor missing")
	}
	if cop.Seats != nil {
		t.Errorf("Seats = %+v, want nil (no seat snapshot)", cop.Seats)
	}
	if cop.Tokens != nil || cop.Acceptance != nil {
		t.Errorf("Tokens/Acceptance should be nil for engagement-only data")
	}
	if cop.CostUnit != "" {
		t.Errorf("CostUnit = %q, want empty (no cost rows)", cop.CostUnit)
	}
}

// TestTelemetry_NoSentinelColumns is the privacy guard: the marshaled result
// carries no actor identity (user_key emails/logins are never selected) and no
// raw project path.
func TestTelemetry_NoSentinelColumns(t *testing.T) {
	d := newDB(t)
	seedTelemetry(t, d)
	got, err := Telemetry(context.Background(), d, w30, fixedNow, testSeatPriceUSD)
	if err != nil {
		t.Fatalf("Telemetry: %v", err)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	if strings.Contains(s, "@") {
		t.Errorf("Telemetry JSON leaked an actor email (user_key must never be selected):\n%s", s)
	}
	if strings.Contains(s, "/repo/") || strings.Contains(s, "alice") {
		t.Errorf("Telemetry JSON leaked an identity/path:\n%s", s)
	}
}

// TestTelemetryCopilotSeatSubscriptionPricing pins the seat→USD conversion and
// the unit trap around it.
//
// Copilot has TWO cost feeds with different shapes: a per-day metered overage
// (additive USD) and a seat subscription (point-in-time, monthly). The seats
// API returns counts only, so the subscription is priced from the configured
// plan price. Before this surface existed, per_seat_price_usd was a configured
// value nothing read, and an admin saw 50 seats and $4.00 while the actual
// dominant cost — $950/mo — was nowhere on the endpoint.
func TestTelemetryCopilotSeatSubscriptionPricing(t *testing.T) {
	d := newDB(t)
	seedTelemetry(t, d)

	got, err := Telemetry(context.Background(), d, w30, fixedNow, testSeatPriceUSD)
	if err != nil {
		t.Fatalf("Telemetry: %v", err)
	}
	cop, ok := vendorMap(got.Vendors)["copilot"]
	if !ok || cop.Seats == nil {
		t.Fatalf("copilot seats missing: %+v", cop)
	}
	wantMonthly := 50 * testSeatPriceUSD // the latest snapshot's 50 seats
	if !near(cop.Seats.MonthlyUSD, wantMonthly) {
		t.Errorf("Seats.MonthlyUSD = %v, want %v (50 seats × $%v)", cop.Seats.MonthlyUSD, wantMonthly, testSeatPriceUSD)
	}
	if !near(cop.Seats.PerSeatPriceUSD, testSeatPriceUSD) {
		t.Errorf("Seats.PerSeatPriceUSD = %v, want %v", cop.Seats.PerSeatPriceUSD, testSeatPriceUSD)
	}
	// THE UNIT TRAP: the monthly subscription must never be folded into the
	// additive per-day metered overage. CostUSD stays the seeded 4.00.
	if !near(cop.CostUSD, 4.00) {
		t.Errorf("CostUSD = %v, want 4.00 — the $%v subscription must not be summed into metered overage",
			cop.CostUSD, wantMonthly)
	}

	// Unconfigured price: omit the cost rather than assert seats are free.
	unpriced, err := Telemetry(context.Background(), d, w30, fixedNow, 0)
	if err != nil {
		t.Fatalf("Telemetry(price=0): %v", err)
	}
	cop2 := vendorMap(unpriced.Vendors)["copilot"]
	if cop2.Seats == nil {
		t.Fatalf("seat counts should still be reported without a price")
	}
	if cop2.Seats.MonthlyUSD != 0 || cop2.Seats.PerSeatPriceUSD != 0 {
		t.Errorf("unpriced seats = %+v, want no priced fields", cop2.Seats)
	}
	if cop2.Seats.Total != 50 {
		t.Errorf("unpriced Seats.Total = %v, want the counts to survive", cop2.Seats.Total)
	}
	b, err := json.Marshal(cop2.Seats)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{"monthly_usd", "per_seat_price_usd"} {
		if strings.Contains(string(b), key) {
			t.Errorf("unconfigured price still emits %q: %s", key, b)
		}
	}

	// A misconfigured NEGATIVE price must not produce a negative subscription.
	// This is what makes the `> 0` guard load-bearing: at exactly 0 the guard
	// is observably equivalent to `>= 0`, because 0 × seats is 0 and omitempty
	// hides it either way. Only a negative price distinguishes them, so only
	// this case can catch the guard being dropped.
	neg, err := Telemetry(context.Background(), d, w30, fixedNow, -19)
	if err != nil {
		t.Fatalf("Telemetry(price=-19): %v", err)
	}
	cop3 := vendorMap(neg.Vendors)["copilot"]
	if cop3.Seats == nil {
		t.Fatalf("seat counts should survive a bad price")
	}
	if cop3.Seats.MonthlyUSD != 0 || cop3.Seats.PerSeatPriceUSD != 0 {
		t.Errorf("negative price produced %+v, want no priced fields (never a negative subscription)", cop3.Seats)
	}
}

// TestTelemetryCopilotOverageByDay pins the new per-day metered-overage
// series: values grouped and ordered by day, out-of-window rows excluded, and
// — the load-bearing case — the seat subscription never leaking into it (the
// unit trap: OverageByDay is additive per-day USD, MonthlyUSD is a
// point-in-time monthly figure, and they must never mix).
func TestTelemetryCopilotOverageByDay(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("exec: %v\n%s", err, q)
		}
	}
	row := `INSERT INTO copilot_analytics_daily (day, user_key, actor_type, surface, unit, metric, value, org_id, owner, pulled_at)
	        VALUES (?, '__org__', 'org', ?, ?, ?, ?, 'org1', 'acme', '2026-05-26T04:00:00Z')`
	// Multi-day metered overage, seeded out of day order to prove ORDER BY day.
	// (billing rows are always user_key='__org__' — schema UNIQUE(day, user_key,
	// surface, metric) — so one row per day is the realistic shape; SUM(value)
	// still exercises the same aggregation telemetryCC/telemetryCodex use.)
	exec(row, "2026-05-23", "billing", "usd", "cost", 4.00)
	exec(row, "2026-05-20", "billing", "usd", "cost", 1.50)
	exec(row, "2026-05-22", "billing", "usd", "cost", 2.25)
	// Out-of-window row (outside the trailing 30-day window from fixedNow).
	exec(row, "2026-03-01", "billing", "usd", "cost", 999.00)
	// A hefty seat subscription snapshot on its own day — must NOT appear in
	// OverageByDay (that would be the unit trap: summing a monthly
	// subscription into an additive per-day metered series).
	exec(row, "2026-05-24", "seats", "seats", "seats_total", 50)
	exec(row, "2026-05-24", "seats", "seats", "seats_active", 40)

	got, err := Telemetry(ctx, d, w30, fixedNow, testSeatPriceUSD)
	if err != nil {
		t.Fatalf("Telemetry: %v", err)
	}
	cop, ok := vendorMap(got.Vendors)["copilot"]
	if !ok {
		t.Fatalf("copilot vendor missing")
	}

	want := []CostPoint{
		{Date: "2026-05-20", CostUSD: 1.50},
		{Date: "2026-05-22", CostUSD: 2.25},
		{Date: "2026-05-23", CostUSD: 4.00},
	}
	if len(cop.OverageByDay) != len(want) {
		t.Fatalf("OverageByDay = %+v, want %+v", cop.OverageByDay, want)
	}
	for i, w := range want {
		g := cop.OverageByDay[i]
		if g.Date != w.Date || !near(g.CostUSD, w.CostUSD) {
			t.Errorf("OverageByDay[%d] = %+v, want %+v (order-by-day)", i, g, w)
		}
	}
	if !near(cop.CostUSD, 1.50+2.25+4.00) {
		t.Errorf("CostUSD = %v, want the same total the series sums to (7.75)", cop.CostUSD)
	}

	// THE UNIT TRAP, asserted directly on the seam this test owns: the seat
	// subscription's day (2026-05-24, 50 seats × $19 = $950/mo) must not
	// appear as an entry in OverageByDay, and the series total (8.50) must
	// stay far below the monthly subscription figure it would collide with
	// if the two feeds were ever merged.
	for _, p := range cop.OverageByDay {
		if p.Date == "2026-05-24" {
			t.Fatalf("OverageByDay leaked the seat-subscription day: %+v", cop.OverageByDay)
		}
	}
	if cop.Seats == nil || !near(cop.Seats.MonthlyUSD, 50*testSeatPriceUSD) {
		t.Fatalf("seat subscription setup broken: %+v", cop.Seats)
	}
	var sum float64
	for _, p := range cop.OverageByDay {
		sum += p.CostUSD
	}
	if sum >= cop.Seats.MonthlyUSD {
		t.Errorf("OverageByDay sum %v >= seat MonthlyUSD %v — looks like the subscription leaked into the metered series", sum, cop.Seats.MonthlyUSD)
	}
}

// TestTelemetryCopilotOverageByDay_EmptyOmitted pins the honest-empty case:
// no billing rows in the window → nil series, omitted from the JSON wire (not
// an empty array, which would render an empty chart).
func TestTelemetryCopilotOverageByDay_EmptyOmitted(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	// Engagement-only row so the copilot vendor is "configured" but has no
	// billing feed.
	if _, err := d.ExecContext(ctx,
		`INSERT INTO copilot_analytics_daily (day, user_key, actor_type, surface, unit, metric, value, org_id, owner, pulled_at)
		 VALUES ('2026-05-23','__org__','org','engagement','count','chats',5,'org1','acme','2026-05-26T04:00:00Z')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := Telemetry(ctx, d, w30, fixedNow, testSeatPriceUSD)
	if err != nil {
		t.Fatalf("Telemetry: %v", err)
	}
	cop, ok := vendorMap(got.Vendors)["copilot"]
	if !ok {
		t.Fatalf("copilot vendor missing")
	}
	if cop.OverageByDay != nil {
		t.Errorf("OverageByDay = %+v, want nil (no billing rows)", cop.OverageByDay)
	}
	b, err := json.Marshal(cop)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "overage_by_day") {
		t.Errorf("empty OverageByDay must be omitted from the JSON wire: %s", b)
	}
}
