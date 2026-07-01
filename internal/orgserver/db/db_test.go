package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"sort"
	"testing"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	// A file-backed DB in a temp dir exercises WAL; :memory: would skip it.
	path := filepath.Join(t.TempDir(), "server.db")
	d, err := Open(context.Background(), Options{Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestOpenAppliesMigrations(t *testing.T) {
	d := openTestDB(t)
	v, err := Version(context.Background(), d)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v != 16 {
		t.Fatalf("version = %d, want 16", v)
	}

	want := []string{
		"actions", "api_turns", "audit_log", "budgets", "cc_analytics_daily",
		"codex_analytics_daily", "copilot_analytics_daily", "enrolment_tokens",
		"guard_events", "issued_bearers", "obs_alert_events", "obs_alert_rules",
		"obs_content", "obs_eval_summaries", "obs_span_events", "obs_spans",
		"obs_summaries", "obs_traces", "org", "org_members", "org_policy_bundles",
		"org_project_team", "org_routing_policies", "org_team_members",
		"org_teams", "otel_content", "revoked_bearers", "routing_policy_audit",
		"routing_policy_keys", "routing_summaries", "sessions", "token_usage",
	}
	rows, err := d.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name != 'schema_meta' ORDER BY name`)
	if err != nil {
		t.Fatalf("query tables: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		got = append(got, n)
	}
	sort.Strings(got)
	if len(got) != len(want) {
		t.Fatalf("tables = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tables = %v, want %v", got, want)
		}
	}
}

func TestEnsureOrgIsStableSingleton(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	o1, err := EnsureOrg(ctx, d, "https://org.acme.example")
	if err != nil {
		t.Fatalf("EnsureOrg 1: %v", err)
	}
	if o1.OrgID == "" {
		t.Fatal("org_id empty")
	}
	if o1.OrgName != "org.acme.example" {
		t.Errorf("org_name = %q, want host org.acme.example", o1.OrgName)
	}

	// Second call returns the same org_id; a changed external_url is
	// refreshed but org_id and org_name stay put.
	o2, err := EnsureOrg(ctx, d, "https://new-host.example")
	if err != nil {
		t.Fatalf("EnsureOrg 2: %v", err)
	}
	if o2.OrgID != o1.OrgID {
		t.Errorf("org_id changed: %q -> %q", o1.OrgID, o2.OrgID)
	}
	if o2.OrgName != o1.OrgName {
		t.Errorf("org_name changed: %q -> %q", o1.OrgName, o2.OrgName)
	}
	if o2.ExternalURL != "https://new-host.example" {
		t.Errorf("external_url not refreshed: %q", o2.ExternalURL)
	}

	// Singleton: at most one row.
	var n int
	if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM org`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("org row count = %d, want 1", n)
	}
}

func TestReopenIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "server.db")

	d1, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("Open 1: %v", err)
	}
	o1, err := EnsureOrg(ctx, d1, "https://org.example")
	if err != nil {
		t.Fatalf("EnsureOrg: %v", err)
	}
	_ = d1.Close()

	// Reopening must not re-run migrations destructively or change org_id.
	d2, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("Open 2: %v", err)
	}
	defer d2.Close()
	o2, err := EnsureOrg(ctx, d2, "https://org.example")
	if err != nil {
		t.Fatalf("EnsureOrg 2: %v", err)
	}
	if o2.OrgID != o1.OrgID {
		t.Errorf("org_id changed across reopen: %q -> %q", o1.OrgID, o2.OrgID)
	}
	v, err := Version(ctx, d2)
	if err != nil {
		t.Fatal(err)
	}
	if v != 16 {
		t.Errorf("version after reopen = %d, want 16", v)
	}
}
