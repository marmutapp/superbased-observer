package rollup

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
)

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
	got, err := Telemetry(context.Background(), d, w30, fixedNow)
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
	got, err := Telemetry(context.Background(), d, w30, fixedNow)
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
	got, err := Telemetry(ctx, d, w30, fixedNow)
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
	got, err := Telemetry(context.Background(), d, w30, fixedNow)
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
