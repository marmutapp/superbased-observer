package db

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db/migrations"
)

// orgTables are the six tables migration 028 introduces.
var orgTables = []string{
	"org_enrolment", "org_members", "org_teams",
	"org_team_members", "org_project_team", "org_push_log",
}

// orgAttributionTables maps each table migration 029 touches to the two
// attribution columns it adds.
var orgAttributionTables = []string{"actions", "sessions", "api_turns", "token_usage"}

// orgPartialIndexes are the four partial indexes migration 029 creates.
var orgPartialIndexes = []string{
	"idx_actions_org_user", "idx_sessions_org_user",
	"idx_api_turns_org_user", "idx_token_usage_org_user",
}

// TestMigrationsFresh_AllApplied proves a fresh database migrates to the
// highest embedded version and exposes the M0 org schema (028 tables +
// 029 attribution columns + partial indexes).
func TestMigrationsFresh_AllApplied(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	entries, err := readMigrationEntries()
	if err != nil {
		t.Fatalf("readMigrationEntries: %v", err)
	}
	wantVersion := entries[len(entries)-1].version

	database, err := Open(ctx, Options{Path: ":memory:"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()

	got, err := Version(ctx, database)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if got != wantVersion {
		t.Fatalf("version = %d, want highest embedded migration %d", got, wantVersion)
	}

	for _, table := range orgTables {
		if !tableExists(t, database, table) {
			t.Errorf("migration 028: table %q missing on fresh DB", table)
		}
	}
	for _, table := range orgAttributionTables {
		for _, col := range []string{"org_id", "user_email"} {
			if !columnExists(t, database, table, col) {
				t.Errorf("migration 029: %s.%s column missing on fresh DB", table, col)
			}
		}
	}
	for _, idx := range orgPartialIndexes {
		if !indexExists(t, database, idx) {
			t.Errorf("migration 029: partial index %q missing on fresh DB", idx)
		}
	}

	// org_enrolment is a singleton (CHECK id = 1): a second row must fail.
	if _, err := database.ExecContext(ctx,
		`INSERT INTO org_enrolment (id, org_id, org_name, org_server_url, user_id, user_email, enrolled_at, bearer_key_id)
		 VALUES (2, 'o', 'n', 'u', 'uid', 'e', 't', 'k')`); err == nil {
		t.Error("org_enrolment accepted id=2; CHECK (id = 1) not enforced")
	}
}

// TestMigrationsUpgrade_27_then_28_29 proves the upgrade path: a database
// already at version 27 with pre-existing rows upgrades cleanly to the
// latest version, the new attribution columns land NULL on those existing
// rows, and the new tables/indexes appear. This exercises the real
// runMigrations runner (not a fresh-apply), matching what an existing
// install hits on first launch of the M0 binary.
func TestMigrationsUpgrade_27_then_28_29(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "upgrade.db")

	// Raw open (no migrations) so we can stop at version 27 deliberately.
	dsn := "file:" + path + "?_pragma=busy_timeout(30000)&_pragma=foreign_keys(1)&_txlock=immediate"
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer database.Close()
	if err := database.PingContext(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	entries, err := readMigrationEntries()
	if err != nil {
		t.Fatalf("readMigrationEntries: %v", err)
	}

	// Bootstrap schema_meta and replay migration bodies 001..027 in order.
	if _, err := database.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatalf("bootstrap schema_meta: %v", err)
	}
	for _, e := range entries {
		if e.version > 27 {
			continue
		}
		body, readErr := fs.ReadFile(migrations.Files, e.filename)
		if readErr != nil {
			t.Fatalf("read %s: %v", e.filename, readErr)
		}
		if _, err := database.ExecContext(ctx, string(body)); err != nil {
			t.Fatalf("apply %s: %v", e.filename, err)
		}
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO schema_meta(key, value) VALUES ('version', '27')
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`); err != nil {
		t.Fatalf("pin version 27: %v", err)
	}

	// Sanity: at version 27, the M0 columns/tables do NOT yet exist.
	if v, err := Version(ctx, database); err != nil || v != 27 {
		t.Fatalf("pre-upgrade version = %d (err=%v), want 27", v, err)
	}
	if columnExists(t, database, "actions", "org_id") {
		t.Fatal("actions.org_id present before migration 029")
	}
	if tableExists(t, database, "org_enrolment") {
		t.Fatal("org_enrolment present before migration 028")
	}

	// Seed pre-existing rows so we can assert the new columns land NULL.
	if _, err := database.ExecContext(ctx,
		`INSERT INTO projects (id, root_path, created_at) VALUES (1, '/p', '2026-05-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO sessions (id, project_id, tool, started_at, total_actions)
		 VALUES ('sA', 1, 'claude-code', '2026-05-01T00:00:00Z', 0)`); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO token_usage(session_id, timestamp, tool, model,
		   input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
		   source, reliability, source_file, source_event_id)
		 VALUES ('sA', '2026-05-01T01:00:00Z', 'claude-code', 'claude-opus-4-7',
		   10, 20, 0, 0, 'jsonl', 'unreliable', '/f.jsonl', 'evt-1')`); err != nil {
		t.Fatalf("seed token_usage: %v", err)
	}

	// Run the REAL runner — it sees applied=27 and applies 028 + 029.
	if err := runMigrations(ctx, database); err != nil {
		t.Fatalf("upgrade runMigrations: %v", err)
	}

	wantVersion := entries[len(entries)-1].version
	if v, err := Version(ctx, database); err != nil || v != wantVersion {
		t.Fatalf("post-upgrade version = %d (err=%v), want %d", v, err, wantVersion)
	}

	// New schema present after upgrade.
	for _, table := range orgTables {
		if !tableExists(t, database, table) {
			t.Errorf("post-upgrade: table %q missing", table)
		}
	}
	for _, table := range orgAttributionTables {
		for _, col := range []string{"org_id", "user_email"} {
			if !columnExists(t, database, table, col) {
				t.Errorf("post-upgrade: %s.%s column missing", table, col)
			}
		}
	}
	for _, idx := range orgPartialIndexes {
		if !indexExists(t, database, idx) {
			t.Errorf("post-upgrade: partial index %q missing", idx)
		}
	}

	// Pre-existing rows must have NULL attribution (additive, no backfill).
	for _, q := range []string{
		`SELECT COUNT(*) FROM sessions WHERE id = 'sA' AND org_id IS NULL AND user_email IS NULL`,
		`SELECT COUNT(*) FROM token_usage WHERE source_event_id = 'evt-1' AND org_id IS NULL AND user_email IS NULL`,
	} {
		var n int
		if err := database.QueryRowContext(ctx, q).Scan(&n); err != nil {
			t.Fatalf("null-attribution check: %v\nquery: %s", err, q)
		}
		if n != 1 {
			t.Errorf("expected pre-existing row with NULL attribution, got %d for: %s", n, q)
		}
	}
}

// cacheTrackTables are the three tables migration 036 introduces.
var cacheTrackTables = []string{"cache_segments", "cache_entries", "cache_events"}

// cacheTrackIndexes are the six indexes migration 036 creates.
var cacheTrackIndexes = []string{
	"idx_cache_segments_session", "idx_cache_segments_turn", "idx_cache_segments_hash",
	"idx_cache_entries_state",
	"idx_cache_events_session", "idx_cache_events_kind",
}

// TestMigration036Fresh_CacheTrackingApplied proves a fresh database
// has the three cache-tracking tables + six indexes that migration 036
// introduces. Composes with the existing fresh-apply test above; this
// one is targeted so a regression on the cachetrack migration is named
// in the test failure.
func TestMigration036Fresh_CacheTrackingApplied(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	database, err := Open(ctx, Options{Path: ":memory:"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()

	for _, table := range cacheTrackTables {
		if !tableExists(t, database, table) {
			t.Errorf("migration 036: table %q missing on fresh DB", table)
		}
	}
	for _, idx := range cacheTrackIndexes {
		if !indexExists(t, database, idx) {
			t.Errorf("migration 036: index %q missing on fresh DB", idx)
		}
	}

	// cache_entries UNIQUE(model, cache_scope, prefix_hash) — duplicate insert fails.
	if _, err := database.ExecContext(ctx,
		`INSERT INTO cache_entries (model, cache_scope, prefix_hash, token_count, tier, created_at, last_refresh_at, expires_at)
		 VALUES ('m', 's', 'h', 1, 'proxy', 't', 't', 't')`); err != nil {
		t.Fatalf("cache_entries first insert: %v", err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO cache_entries (model, cache_scope, prefix_hash, token_count, tier, created_at, last_refresh_at, expires_at)
		 VALUES ('m', 's', 'h', 9, 'proxy', 't', 't', 't')`); err == nil {
		t.Error("cache_entries accepted duplicate (model, cache_scope, prefix_hash); UNIQUE not enforced")
	}

	// Defaults: cache_scope='default', ttl_tier='5m', state='live'.
	if _, err := database.ExecContext(ctx,
		`INSERT INTO cache_entries (model, prefix_hash, token_count, tier, created_at, last_refresh_at, expires_at)
		 VALUES ('m2', 'h2', 1, 'proxy', 't', 't', 't')`); err != nil {
		t.Fatalf("cache_entries default insert: %v", err)
	}
	var scope, ttl, state string
	if err := database.QueryRowContext(ctx,
		`SELECT cache_scope, ttl_tier, state FROM cache_entries WHERE model = 'm2'`).Scan(&scope, &ttl, &state); err != nil {
		t.Fatalf("read defaults: %v", err)
	}
	if scope != "default" || ttl != "5m" || state != "live" {
		t.Errorf("defaults wrong: cache_scope=%q ttl_tier=%q state=%q (want default/5m/live)", scope, ttl, state)
	}

	// cache_events tokens_* columns default to 0 (NOT NULL DEFAULT 0).
	if _, err := database.ExecContext(ctx,
		`INSERT INTO cache_events (session_id, tier, timestamp, model, kind)
		 VALUES ('s', 'proxy', 't', 'm', 'hit')`); err != nil {
		t.Fatalf("cache_events minimal insert: %v", err)
	}
	var read, written, written1h int64
	if err := database.QueryRowContext(ctx,
		`SELECT tokens_read, tokens_written, tokens_written_1h FROM cache_events WHERE session_id = 's'`).Scan(&read, &written, &written1h); err != nil {
		t.Fatalf("read defaults: %v", err)
	}
	if read != 0 || written != 0 || written1h != 0 {
		t.Errorf("token defaults wrong: read=%d written=%d written1h=%d (want 0/0/0)", read, written, written1h)
	}
}

// TestMigration036Upgrade_35_then_36 proves the upgrade path: a
// database already at version 35 with pre-existing rows (in tables
// the migration does NOT touch) upgrades cleanly to the latest
// version, the three cache-tracking tables + indexes appear, and the
// pre-existing rows survive untouched. Mirrors the 27-then-28/29 test
// above. Migration 036 is purely additive (CREATE TABLE / CREATE
// INDEX, no ALTER), so the pre-existing-rows check is just a survival
// assertion — there are no new columns on existing tables.
func TestMigration036Upgrade_35_then_36(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "upgrade-036.db")

	// Raw open (no migrations) so we can stop at version 35 deliberately.
	dsn := "file:" + path + "?_pragma=busy_timeout(30000)&_pragma=foreign_keys(1)&_txlock=immediate"
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer database.Close()
	if err := database.PingContext(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	entries, err := readMigrationEntries()
	if err != nil {
		t.Fatalf("readMigrationEntries: %v", err)
	}

	// Bootstrap schema_meta and replay migration bodies 001..035 in order.
	if _, err := database.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatalf("bootstrap schema_meta: %v", err)
	}
	for _, e := range entries {
		if e.version > 35 {
			continue
		}
		body, readErr := fs.ReadFile(migrations.Files, e.filename)
		if readErr != nil {
			t.Fatalf("read %s: %v", e.filename, readErr)
		}
		if _, err := database.ExecContext(ctx, string(body)); err != nil {
			t.Fatalf("apply %s: %v", e.filename, err)
		}
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO schema_meta(key, value) VALUES ('version', '35')
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`); err != nil {
		t.Fatalf("pin version 35: %v", err)
	}

	// Sanity: at version 35, the cachetrack tables do NOT yet exist.
	if v, err := Version(ctx, database); err != nil || v != 35 {
		t.Fatalf("pre-upgrade version = %d (err=%v), want 35", v, err)
	}
	for _, table := range cacheTrackTables {
		if tableExists(t, database, table) {
			t.Fatalf("%s present before migration 036", table)
		}
	}

	// Seed a pre-existing api_turns row so we can assert survival.
	if _, err := database.ExecContext(ctx,
		`INSERT INTO projects (id, root_path, created_at) VALUES (1, '/p', '2026-05-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO sessions (id, project_id, tool, started_at, total_actions)
		 VALUES ('sA', 1, 'claude-code', '2026-06-01T00:00:00Z', 0)`); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO api_turns (session_id, timestamp, provider, model, input_tokens, output_tokens)
		 VALUES ('sA', '2026-06-01T01:00:00Z', 'anthropic', 'claude-sonnet-4-6', 100, 200)`); err != nil {
		t.Fatalf("seed api_turns: %v", err)
	}

	// Run the REAL runner — it sees applied=35 and applies 036.
	if err := runMigrations(ctx, database); err != nil {
		t.Fatalf("upgrade runMigrations: %v", err)
	}

	wantVersion := entries[len(entries)-1].version
	if v, err := Version(ctx, database); err != nil || v != wantVersion {
		t.Fatalf("post-upgrade version = %d (err=%v), want %d", v, err, wantVersion)
	}

	// New schema present after upgrade.
	for _, table := range cacheTrackTables {
		if !tableExists(t, database, table) {
			t.Errorf("post-upgrade: table %q missing", table)
		}
	}
	for _, idx := range cacheTrackIndexes {
		if !indexExists(t, database, idx) {
			t.Errorf("post-upgrade: index %q missing", idx)
		}
	}

	// Pre-existing api_turns row must survive untouched.
	var input, output int64
	if err := database.QueryRowContext(ctx,
		`SELECT input_tokens, output_tokens FROM api_turns WHERE session_id = 'sA'`).Scan(&input, &output); err != nil {
		t.Fatalf("pre-existing row survival check: %v", err)
	}
	if input != 100 || output != 200 {
		t.Errorf("api_turns row mutated by upgrade: input=%d output=%d (want 100/200)", input, output)
	}

	// Idempotency: re-running migrations from latest version must be a no-op.
	if err := runMigrations(ctx, database); err != nil {
		t.Fatalf("idempotent re-run: %v", err)
	}
	if v, err := Version(ctx, database); err != nil || v != wantVersion {
		t.Fatalf("post-rerun version = %d (err=%v), want %d", v, err, wantVersion)
	}
}

// guardTables are the four tables migration 040 introduces.
var guardTables = []string{
	"guard_events", "guard_pins", "guard_policy_state", "guard_approvals",
}

// guardIndexes are the five indexes migration 040 creates.
var guardIndexes = []string{
	"idx_guard_events_session", "idx_guard_events_rule", "idx_guard_events_ts",
	"idx_guard_policy_state_layer", "idx_guard_approvals_rule",
}

// TestMigration040Fresh_GuardLayerApplied proves a fresh database has
// the four guard tables + five indexes that migration 040 introduces,
// plus the load-bearing constraints (guard_pins natural-identity
// UNIQUE; guard_events NOT NULL chain columns).
func TestMigration040Fresh_GuardLayerApplied(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	database, err := Open(ctx, Options{Path: ":memory:"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()

	for _, table := range guardTables {
		if !tableExists(t, database, table) {
			t.Errorf("migration 040: table %q missing on fresh DB", table)
		}
	}
	for _, idx := range guardIndexes {
		if !indexExists(t, database, idx) {
			t.Errorf("migration 040: index %q missing on fresh DB", idx)
		}
	}

	// guard_pins UNIQUE(kind, name, client) — duplicate insert fails.
	if _, err := database.ExecContext(ctx,
		`INSERT INTO guard_pins (kind, name, client, pin_hash, first_seen, last_verified)
		 VALUES ('mcp_server', 'srv', 'claude-code', 'h', 't', 't')`); err != nil {
		t.Fatalf("guard_pins first insert: %v", err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO guard_pins (kind, name, client, pin_hash, first_seen, last_verified)
		 VALUES ('mcp_server', 'srv', 'claude-code', 'h2', 't', 't')`); err == nil {
		t.Error("guard_pins accepted duplicate (kind, name, client); UNIQUE not enforced")
	}
	// Same (kind, name) under a different client is a distinct pin.
	if _, err := database.ExecContext(ctx,
		`INSERT INTO guard_pins (kind, name, client, pin_hash, first_seen, last_verified)
		 VALUES ('mcp_server', 'srv', 'cursor', 'h', 't', 't')`); err != nil {
		t.Errorf("guard_pins per-client identity insert failed: %v", err)
	}

	// guard_events chain columns are NOT NULL (a chain row without its
	// link material is structurally invalid).
	if _, err := database.ExecContext(ctx,
		`INSERT INTO guard_events (ts, rule_id, chain_prev) VALUES ('t', 'R-101', '')`); err == nil {
		t.Error("guard_events accepted NULL chain_hash; NOT NULL not enforced")
	}

	// Defaults: guard_pins.status='pinned', guard_events.enforced=0.
	var status string
	if err := database.QueryRowContext(ctx,
		`SELECT status FROM guard_pins WHERE client = 'claude-code'`).Scan(&status); err != nil {
		t.Fatalf("read pin status default: %v", err)
	}
	if status != "pinned" {
		t.Errorf("guard_pins.status default = %q, want 'pinned'", status)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO guard_events (ts, rule_id, chain_prev, chain_hash) VALUES ('t', 'R-101', '', 'h')`); err != nil {
		t.Fatalf("guard_events minimal insert: %v", err)
	}
	var enforced int64
	if err := database.QueryRowContext(ctx,
		`SELECT enforced FROM guard_events WHERE rule_id = 'R-101'`).Scan(&enforced); err != nil {
		t.Fatalf("read enforced default: %v", err)
	}
	if enforced != 0 {
		t.Errorf("guard_events.enforced default = %d, want 0", enforced)
	}
}

// TestMigration040Upgrade_39_then_40 proves the upgrade path: a
// database already at version 39 with pre-existing rows upgrades
// cleanly to the latest version, the four guard tables + indexes
// appear, and the pre-existing rows survive untouched. Migration 040
// is purely additive (CREATE TABLE / CREATE INDEX, no ALTER), so the
// pre-existing-rows check is a survival assertion. Mirrors the
// 35-then-36 cachetrack test above.
func TestMigration040Upgrade_39_then_40(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "upgrade-040.db")

	// Raw open (no migrations) so we can stop at version 39 deliberately.
	dsn := "file:" + path + "?_pragma=busy_timeout(30000)&_pragma=foreign_keys(1)&_txlock=immediate"
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer database.Close()
	if err := database.PingContext(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	entries, err := readMigrationEntries()
	if err != nil {
		t.Fatalf("readMigrationEntries: %v", err)
	}

	// Bootstrap schema_meta and replay migration bodies 001..039 in order.
	if _, err := database.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatalf("bootstrap schema_meta: %v", err)
	}
	for _, e := range entries {
		if e.version > 39 {
			continue
		}
		body, readErr := fs.ReadFile(migrations.Files, e.filename)
		if readErr != nil {
			t.Fatalf("read %s: %v", e.filename, readErr)
		}
		if _, err := database.ExecContext(ctx, string(body)); err != nil {
			t.Fatalf("apply %s: %v", e.filename, err)
		}
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO schema_meta(key, value) VALUES ('version', '39')
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`); err != nil {
		t.Fatalf("pin version 39: %v", err)
	}

	// Sanity: at version 39, the guard tables do NOT yet exist.
	if v, err := Version(ctx, database); err != nil || v != 39 {
		t.Fatalf("pre-upgrade version = %d (err=%v), want 39", v, err)
	}
	for _, table := range guardTables {
		if tableExists(t, database, table) {
			t.Fatalf("%s present before migration 040", table)
		}
	}

	// Seed a pre-existing actions row so we can assert survival.
	if _, err := database.ExecContext(ctx,
		`INSERT INTO projects (id, root_path, created_at) VALUES (1, '/p', '2026-06-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO sessions (id, project_id, tool, started_at, total_actions)
		 VALUES ('sA', 1, 'claude-code', '2026-06-01T00:00:00Z', 0)`); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO actions (session_id, project_id, source_file, source_event_id, timestamp, tool, action_type, target)
		 VALUES ('sA', 1, 'f.jsonl', 'evt-1', '2026-06-01T01:00:00Z', 'claude-code', 'run_command', 'go test')`); err != nil {
		t.Fatalf("seed action: %v", err)
	}

	// Run the REAL runner — it sees applied=39 and applies 040.
	if err := runMigrations(ctx, database); err != nil {
		t.Fatalf("upgrade runMigrations: %v", err)
	}

	wantVersion := entries[len(entries)-1].version
	if v, err := Version(ctx, database); err != nil || v != wantVersion {
		t.Fatalf("post-upgrade version = %d (err=%v), want %d", v, err, wantVersion)
	}

	// New schema present after upgrade.
	for _, table := range guardTables {
		if !tableExists(t, database, table) {
			t.Errorf("post-upgrade: table %q missing", table)
		}
	}
	for _, idx := range guardIndexes {
		if !indexExists(t, database, idx) {
			t.Errorf("post-upgrade: index %q missing", idx)
		}
	}

	// Pre-existing actions row must survive untouched.
	var target string
	if err := database.QueryRowContext(ctx,
		`SELECT target FROM actions WHERE source_event_id = 'evt-1'`).Scan(&target); err != nil {
		t.Fatalf("pre-existing row survival check: %v", err)
	}
	if target != "go test" {
		t.Errorf("actions row mutated by upgrade: target=%q (want 'go test')", target)
	}

	// Idempotency: re-running migrations from latest version must be a no-op.
	if err := runMigrations(ctx, database); err != nil {
		t.Fatalf("idempotent re-run: %v", err)
	}
	if v, err := Version(ctx, database); err != nil || v != wantVersion {
		t.Fatalf("post-rerun version = %d (err=%v), want %d", v, err, wantVersion)
	}
}

// TestRunMigrationsFastPathSkipsWriteLock proves the lock-free fast path:
// when the schema is already current, runMigrations returns WITHOUT
// contending for the write lock. This is the fix for short-lived CLI/hook
// db.Open calls timing out on SQLITE_BUSY while the running daemon holds a
// write transaction. We hold an IMMEDIATE (write-lock) transaction open on
// one connection — mimicking the daemon mid-write — and assert a concurrent
// runMigrations on a fully-migrated DB completes well under the 30s
// busy_timeout it would otherwise have to wait out.
func TestRunMigrationsFastPathSkipsWriteLock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fastpath.db")

	// Open migrates the DB to the latest version (WAL mode, real runner).
	database, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()

	// Hold the write lock on a pinned connection, as the daemon would
	// mid-write. In WAL mode this blocks other writers but not readers.
	conn, err := database.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("BEGIN IMMEDIATE: %v", err)
	}
	defer func() { _, _ = conn.ExecContext(ctx, "ROLLBACK") }()

	// On a fully-migrated DB the fast path is a pure SELECT, so it must
	// return immediately rather than waiting ~30s for the held write lock.
	done := make(chan error, 1)
	go func() { done <- runMigrations(ctx, database) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runMigrations (fast path): %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runMigrations blocked on the write lock; fast path not taken")
	}
}

func tableExists(t *testing.T, database *sql.DB, name string) bool {
	t.Helper()
	var got string
	err := database.QueryRowContext(context.Background(),
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&got)
	switch {
	case err == sql.ErrNoRows:
		return false
	case err != nil:
		t.Fatalf("tableExists(%s): %v", name, err)
	}
	return true
}

func indexExists(t *testing.T, database *sql.DB, name string) bool {
	t.Helper()
	var got string
	err := database.QueryRowContext(context.Background(),
		`SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`, name).Scan(&got)
	switch {
	case err == sql.ErrNoRows:
		return false
	case err != nil:
		t.Fatalf("indexExists(%s): %v", name, err)
	}
	return true
}

func columnExists(t *testing.T, database *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := database.QueryContext(context.Background(),
		`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatalf("columnExists(%s.%s): %v", table, column, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("columnExists scan: %v", err)
		}
		if name == column {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("columnExists rows: %v", err)
	}
	return false
}
