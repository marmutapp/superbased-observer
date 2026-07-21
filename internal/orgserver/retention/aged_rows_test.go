package retention

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// exec runs a statement, failing the test on error.
func exec(t *testing.T, d *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := d.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

// rowCount returns the number of rows in a table.
func rowCount(t *testing.T, d *sql.DB, table string) int {
	t.Helper()
	var n int
	// table is a test-literal.
	if err := d.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil { //nolint:gosec // test literal
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

const (
	oldTS    = "2026-05-14T12:00:00Z" // ~40d before fixedNow (2026-06-23)
	recentTS = "2026-06-22T12:00:00Z" // ~1d before fixedNow
	oldDay   = "2026-05-14"
	recDay   = "2026-06-22"
)

// TestPruneAgedRows_PrunesEventTables checks the four core agent-pushed event
// tables: an old row (past the 30d horizon) is deleted, a recent one kept.
func TestPruneAgedRows_PrunesEventTables(t *testing.T) {
	d := openDB(t)
	// sessions
	exec(t, d, `INSERT INTO sessions (id, user_id, started_at, pushed_at, pushed_by_user_id) VALUES ('s-old','u1',?, ?, 'u1')`, oldTS, oldTS)
	exec(t, d, `INSERT INTO sessions (id, user_id, started_at, pushed_at, pushed_by_user_id) VALUES ('s-new','u1',?, ?, 'u1')`, recentTS, recentTS)
	// actions
	exec(t, d, `INSERT INTO actions (source_file, source_event_id, user_id, timestamp, pushed_at, pushed_by_user_id) VALUES ('f','e-old','u1',?, ?, 'u1')`, oldTS, oldTS)
	exec(t, d, `INSERT INTO actions (source_file, source_event_id, user_id, timestamp, pushed_at, pushed_by_user_id) VALUES ('f','e-new','u1',?, ?, 'u1')`, recentTS, recentTS)
	// api_turns
	exec(t, d, `INSERT INTO api_turns (user_id, timestamp, request_id, pushed_at, pushed_by_user_id) VALUES ('u1',?, 'r-old', ?, 'u1')`, oldTS, oldTS)
	exec(t, d, `INSERT INTO api_turns (user_id, timestamp, request_id, pushed_at, pushed_by_user_id) VALUES ('u1',?, 'r-new', ?, 'u1')`, recentTS, recentTS)
	// token_usage
	exec(t, d, `INSERT INTO token_usage (source_file, source_event_id, user_id, timestamp, pushed_at, pushed_by_user_id) VALUES ('f','t-old','u1',?, ?, 'u1')`, oldTS, oldTS)
	exec(t, d, `INSERT INTO token_usage (source_file, source_event_id, user_id, timestamp, pushed_at, pushed_by_user_id) VALUES ('f','t-new','u1',?, ?, 'u1')`, recentTS, recentTS)

	results, err := PruneAgedRows(context.Background(), d, 30, fixedNow)
	if err != nil {
		t.Fatalf("PruneAgedRows: %v", err)
	}
	got := map[string]int64{}
	for _, r := range results {
		got[r.Table] = r.Deleted
	}
	for _, tbl := range []string{"sessions", "actions", "api_turns", "token_usage"} {
		if got[tbl] != 1 {
			t.Errorf("%s deleted = %d, want 1", tbl, got[tbl])
		}
		if n := rowCount(t, d, tbl); n != 1 {
			t.Errorf("%s remaining = %d, want 1 (the recent row)", tbl, n)
		}
	}
}

// TestPruneAgedRows_PrunesDayAggregates checks day-keyed aggregate rollups
// (agent-pushed obs_summaries + server-polled cc_analytics_daily).
func TestPruneAgedRows_PrunesDayAggregates(t *testing.T) {
	d := openDB(t)
	exec(t, d, `INSERT INTO obs_summaries (org_id, day, pushed_at, pushed_by_user_id) VALUES ('org-1',?, 'p','u1')`, oldDay)
	exec(t, d, `INSERT INTO obs_summaries (org_id, day, pushed_at, pushed_by_user_id) VALUES ('org-1',?, 'p','u1')`, recDay)
	exec(t, d, `INSERT INTO cc_analytics_daily (day, user_key, metric, value, pulled_at) VALUES (?, 'k','sessions',1,'p')`, oldDay)
	exec(t, d, `INSERT INTO cc_analytics_daily (day, user_key, metric, value, pulled_at) VALUES (?, 'k','sessions',1,'p')`, recDay)

	if _, err := PruneAgedRows(context.Background(), d, 30, fixedNow); err != nil {
		t.Fatalf("PruneAgedRows: %v", err)
	}
	for _, tbl := range []string{"obs_summaries", "cc_analytics_daily"} {
		if n := rowCount(t, d, tbl); n != 1 {
			t.Errorf("%s remaining = %d, want 1 (recent day kept)", tbl, n)
		}
	}
}

// TestPruneAgedRows_DeletesContentRows confirms the content stores lose the
// WHOLE aged row (not merely a NULLed body — that is the separate sweep).
func TestPruneAgedRows_DeletesContentRows(t *testing.T) {
	d := openDB(t)
	body := "a body"
	seedOTel(t, d, "c-old", oldTS, &body)
	seedOTel(t, d, "c-new", recentTS, &body)
	seedObs(t, d, "o-old", oldTS, &body)
	seedObs(t, d, "o-new", recentTS, &body)

	if _, err := PruneAgedRows(context.Background(), d, 30, fixedNow); err != nil {
		t.Fatalf("PruneAgedRows: %v", err)
	}
	if n := rowCount(t, d, "otel_content"); n != 1 {
		t.Errorf("otel_content rows = %d, want 1 (old row deleted, not just NULLed)", n)
	}
	if n := rowCount(t, d, "obs_content"); n != 1 {
		t.Errorf("obs_content rows = %d, want 1", n)
	}
}

// TestPruneAgedRows_FallbackToPushedAt: a row whose event time is blank still
// ages out on its pushed_at fallback, and a blank-event recent-pushed row stays.
func TestPruneAgedRows_FallbackToPushedAt(t *testing.T) {
	d := openDB(t)
	// started_at empty; pushed_at old → pruned.
	exec(t, d, `INSERT INTO sessions (id, user_id, started_at, pushed_at, pushed_by_user_id) VALUES ('s-blankold','u1','', ?, 'u1')`, oldTS)
	// started_at empty; pushed_at recent → kept.
	exec(t, d, `INSERT INTO sessions (id, user_id, started_at, pushed_at, pushed_by_user_id) VALUES ('s-blanknew','u1','', ?, 'u1')`, recentTS)

	if _, err := PruneAgedRows(context.Background(), d, 30, fixedNow); err != nil {
		t.Fatalf("PruneAgedRows: %v", err)
	}
	var kept string
	if err := d.QueryRow(`SELECT id FROM sessions`).Scan(&kept); err != nil {
		t.Fatalf("read surviving session: %v", err)
	}
	if kept != "s-blanknew" {
		t.Errorf("surviving session = %q, want s-blanknew (fallback pruned the blank-old one)", kept)
	}
}

// TestPruneAgedRows_BoundaryDayKept: a row whose day equals the horizon boundary
// is KEPT (exactly N days old); the day before is pruned.
func TestPruneAgedRows_BoundaryDayKept(t *testing.T) {
	d := openDB(t)
	// fixedNow 2026-06-23, horizon 30 → cutoff date 2026-05-24.
	exec(t, d, `INSERT INTO obs_summaries (org_id, day, pushed_at, pushed_by_user_id) VALUES ('org-1','2026-05-24','p','u1')`) // boundary — keep
	exec(t, d, `INSERT INTO obs_summaries (org_id, day, pushed_at, pushed_by_user_id) VALUES ('org-1','2026-05-23','p','u1')`) // older — prune

	if _, err := PruneAgedRows(context.Background(), d, 30, fixedNow); err != nil {
		t.Fatalf("PruneAgedRows: %v", err)
	}
	var day string
	if err := d.QueryRow(`SELECT day FROM obs_summaries`).Scan(&day); err != nil {
		t.Fatalf("read surviving day: %v", err)
	}
	if day != "2026-05-24" {
		t.Errorf("surviving day = %q, want 2026-05-24 (boundary kept)", day)
	}
}

// TestPruneAgedRows_ExcludedTablesUntouched seeds ancient rows in every EXCLUDED
// table and asserts a 1-day prune leaves them all intact.
func TestPruneAgedRows_ExcludedTablesUntouched(t *testing.T) {
	d := openDB(t)
	ancient := "2020-01-01T00:00:00Z"
	// tamper-evident audit chain
	exec(t, d, `INSERT INTO guard_events (chain_hash, user_id, timestamp, pushed_at, pushed_by_user_id) VALUES ('h1','u1',?, ?, 'u1')`, ancient, ancient)
	// append-only audit log
	exec(t, d, `INSERT INTO audit_log (actor_user_id, action, timestamp) VALUES ('u1','x',?)`, ancient)
	// identity
	exec(t, d, `INSERT INTO org_members (user_id, user_name, email, created_at, updated_at) VALUES ('u1','n','e',?, ?)`, ancient, ancient)
	// admin-authored alert RULE (events prune, rules don't)
	exec(t, d, `INSERT INTO obs_alert_rules (id, org_id, metric, threshold, created_at) VALUES ('r1','org-1','cost_usd',1,?)`, ancient)
	// admin-authored budget
	exec(t, d, `INSERT INTO budgets (id, scope, scope_id, monthly_usd_cap, created_at, updated_at) VALUES ('b1','team','t1',10,?, ?)`, ancient, ancient)
	// signed policy config
	exec(t, d, `INSERT INTO org_policy_bundles (bundle_toml, signature, public_key, signed_at) VALUES ('x','s','k',?)`, ancient)

	if _, err := PruneAgedRows(context.Background(), d, 1, fixedNow); err != nil {
		t.Fatalf("PruneAgedRows: %v", err)
	}
	for _, tbl := range []string{"guard_events", "audit_log", "org_members", "obs_alert_rules", "budgets", "org_policy_bundles"} {
		if n := rowCount(t, d, tbl); n != 1 {
			t.Errorf("EXCLUDED table %s rows = %d, want 1 (must never auto-prune)", tbl, n)
		}
	}
}

// TestPruneAgedRows_DisabledIsNoop: horizon ≤ 0 prunes nothing.
func TestPruneAgedRows_DisabledIsNoop(t *testing.T) {
	d := openDB(t)
	exec(t, d, `INSERT INTO sessions (id, user_id, started_at, pushed_at, pushed_by_user_id) VALUES ('s','u1',?, ?, 'u1')`, "2020-01-01T00:00:00Z", "2020-01-01T00:00:00Z")
	for _, h := range []int{0, -1} {
		res, err := PruneAgedRows(context.Background(), d, h, fixedNow)
		if err != nil {
			t.Fatalf("PruneAgedRows(%d): %v", h, err)
		}
		if res != nil {
			t.Errorf("horizon %d returned %v, want nil (disabled)", h, res)
		}
	}
	if n := rowCount(t, d, "sessions"); n != 1 {
		t.Errorf("disabled prune removed a row: sessions = %d", n)
	}
}

// TestPruneTable_Batching drains a table larger than the batch size in multiple
// passes, deleting only the aged rows and leaving recent ones.
func TestPruneTable_Batching(t *testing.T) {
	d := openDB(t)
	const oldN, recentN = 12, 3
	for i := 0; i < oldN; i++ {
		exec(t, d, `INSERT INTO obs_alert_events (rule_id, org_id, metric, threshold, value, fired_at) VALUES ('r','org-1','cost_usd',1,1,?)`, fmt.Sprintf("2026-05-%02dT00:00:00Z", i+1))
	}
	for i := 0; i < recentN; i++ {
		exec(t, d, `INSERT INTO obs_alert_events (rule_id, org_id, metric, threshold, value, fired_at) VALUES ('r','org-1','cost_usd',1,1,?)`, recentTS)
	}
	tbl := retentionTable{name: "obs_alert_events", tsColumn: "fired_at", kind: kindDatetime}
	// batch of 5 forces 3 passes (5+5+2) over the 12 aged rows.
	n, err := pruneTable(context.Background(), d, tbl, "2026-05-24T12:00:00Z", 5)
	if err != nil {
		t.Fatalf("pruneTable: %v", err)
	}
	if n != oldN {
		t.Errorf("deleted = %d, want %d", n, oldN)
	}
	if got := rowCount(t, d, "obs_alert_events"); got != recentN {
		t.Errorf("remaining = %d, want %d (recent kept)", got, recentN)
	}
}
