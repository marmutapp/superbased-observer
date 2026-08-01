package db

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"strconv"
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

// TestMigration071_AntigravityCLIRetag proves migration 071 retags the
// historical agy-CLI rows (mislabeled tool='antigravity' pre-commit
// 71bb58c2) across token_usage + actions + sessions using the LAYOUT-PATH
// discriminator (mirrors adapter.go::classifyLayout: source_file under
// .gemini/antigravity-cli/conversations/, backslash-normalized). It covers
// CLI .db + CLI .pb (ambiguous struct namespace) + Windows-backslash token
// rows, CLI transcript actions, and sessions retagged by child evidence —
// while every desktop-path row (incl. a desktop transcript action whose
// event id carries the CLI-looking prefix) stays tool='antigravity'.
func TestMigration071_AntigravityCLIRetag(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "retag.db")

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

	// Replay every migration body BELOW 071, then pin the version so the
	// real runner applies only 071 against our seeded rows.
	if _, err := database.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatalf("bootstrap schema_meta: %v", err)
	}
	for _, e := range entries {
		if e.version >= 71 {
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
		`INSERT INTO schema_meta(key, value) VALUES ('version', '70')
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`); err != nil {
		t.Fatalf("pin version 70: %v", err)
	}

	// Seed a project + two sessions: one CLI (mislabeled) and one true
	// desktop, both currently tool='antigravity'.
	if _, err := database.ExecContext(ctx,
		`INSERT INTO projects (id, root_path, created_at) VALUES (1, '/p', '2026-05-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	// Discriminator is the LAYOUT PATH (mirrors adapter.go::classifyLayout),
	// so seeds use REALISTIC source_file paths:
	//   sess-cli     — CLI .db + CLI .pb (struct-namespace) token rows.
	//   sess-win     — Windows-backslash CLI .pb token row (norm proof).
	//   sess-cli-act — ONLY a CLI-path transcript action (child-evidence
	//                  via the actions table, no token row).
	//   sess-desk    — desktop .pb token + a desktop transcript action whose
	//                  event id carries the CLI-LOOKING antigravity-cli-
	//                  transcript: prefix but a DESKTOP path → must NOT retag.
	if _, err := database.ExecContext(ctx,
		`INSERT INTO sessions (id, project_id, tool, started_at, total_actions) VALUES
		   ('sess-cli', 1, 'antigravity', '2026-05-01T00:00:00Z', 0),
		   ('sess-win', 1, 'antigravity', '2026-05-01T00:00:00Z', 0),
		   ('sess-cli-act', 1, 'antigravity', '2026-05-01T00:00:00Z', 0),
		   ('sess-desk', 1, 'antigravity', '2026-05-01T00:00:00Z', 0)`); err != nil {
		t.Fatalf("seed sessions: %v", err)
	}

	// token_usage: three CLI-PATH rows (must retag) — incl. a .pb row under
	// the AMBIGUOUS antigravity-struct-* namespace (Finding-1 coverage) and
	// a Windows-backslash path — plus one desktop-path row (must NOT retag).
	if _, err := database.ExecContext(ctx,
		`INSERT INTO token_usage(session_id, timestamp, tool, model,
		   input_tokens, output_tokens, source, reliability, source_file, source_event_id)
		 VALUES
		   ('sess-cli', '2026-05-01T01:00:00Z', 'antigravity', 'gemini', 10, 20, 'estimated', 'unreliable', '/home/u/.gemini/antigravity-cli/conversations/conv1.db', 'antigravity-cli-db:conv1:gen:0'),
		   ('sess-cli', '2026-05-01T01:00:00Z', 'antigravity', 'gemini', 11, 21, 'estimated', 'unreliable', '/home/u/.gemini/antigravity-cli/conversations/conv4.pb', 'antigravity-struct-final:conv4:2'),
		   ('sess-win', '2026-05-01T01:00:00Z', 'antigravity', 'gemini', 12, 22, 'estimated', 'unreliable', 'C:\Users\u\.gemini\antigravity-cli\conversations\conv5.pb', 'antigravity-struct-final:conv5:1'),
		   ('sess-desk', '2026-05-01T01:00:00Z', 'antigravity', 'gemini', 30, 40, 'estimated', 'unreliable', '/home/u/.gemini/antigravity/conversations/conv2.pb', 'antigravity-struct-final:conv2:5')`); err != nil {
		t.Fatalf("seed token_usage: %v", err)
	}

	// actions: one CLI-PATH transcript row (must retag) + one DESKTOP-PATH
	// transcript row whose event id carries the shared antigravity-cli-
	// transcript: prefix (must NOT retag — the path, not the prefix, decides).
	if _, err := database.ExecContext(ctx,
		`INSERT INTO actions(session_id, project_id, timestamp, action_type, tool, source_file, source_event_id)
		 VALUES
		   ('sess-cli-act', 1, '2026-05-01T01:00:00Z', 'message', 'antigravity', '/home/u/.gemini/antigravity-cli/conversations/conv1.pb', 'antigravity-cli-transcript:conv1:step:0:user'),
		   ('sess-desk',    1, '2026-05-01T01:00:00Z', 'message', 'antigravity', '/home/u/.gemini/antigravity/conversations/conv3.pb',     'antigravity-cli-transcript:conv3:step:0:user')`); err != nil {
		t.Fatalf("seed actions: %v", err)
	}

	// Run the REAL runner — sees applied=70, applies 071.
	if err := runMigrations(ctx, database); err != nil {
		t.Fatalf("upgrade runMigrations: %v", err)
	}
	wantVersion := entries[len(entries)-1].version
	if v, err := Version(ctx, database); err != nil || v != wantVersion {
		t.Fatalf("post-upgrade version = %d (err=%v), want %d", v, err, wantVersion)
	}

	assertTool := func(query, want string) {
		t.Helper()
		var got string
		if err := database.QueryRowContext(ctx, query).Scan(&got); err != nil {
			t.Fatalf("scan %q: %v", query, err)
		}
		if got != want {
			t.Errorf("query %q: tool = %q, want %q", query, got, want)
		}
	}

	// All CLI-PATH token rows retag — including the .pb row under the
	// ambiguous antigravity-struct-* namespace (Finding-1) and the
	// Windows-backslash path (normalization).
	assertTool(`SELECT tool FROM token_usage WHERE source_event_id = 'antigravity-cli-db:conv1:gen:0'`, "antigravity-cli")
	assertTool(`SELECT tool FROM token_usage WHERE source_event_id = 'antigravity-struct-final:conv4:2'`, "antigravity-cli")
	assertTool(`SELECT tool FROM token_usage WHERE source_event_id = 'antigravity-struct-final:conv5:1'`, "antigravity-cli")

	// Desktop-path token row untouched.
	assertTool(`SELECT tool FROM token_usage WHERE source_event_id = 'antigravity-struct-final:conv2:5'`, "antigravity")

	// CLI-path transcript action retags; the desktop-path transcript action
	// (same CLI-looking event-id prefix, but a desktop path) does NOT.
	assertTool(`SELECT tool FROM actions WHERE source_event_id = 'antigravity-cli-transcript:conv1:step:0:user'`, "antigravity-cli")
	assertTool(`SELECT tool FROM actions WHERE source_event_id = 'antigravity-cli-transcript:conv3:step:0:user'`, "antigravity")

	// Sessions retag when they OWN a CLI-path child in EITHER table:
	// sess-cli (token), sess-win (Windows-path token), sess-cli-act
	// (actions-only evidence). The desktop-only session stays.
	assertTool(`SELECT tool FROM sessions WHERE id = 'sess-cli'`, "antigravity-cli")
	assertTool(`SELECT tool FROM sessions WHERE id = 'sess-win'`, "antigravity-cli")
	assertTool(`SELECT tool FROM sessions WHERE id = 'sess-cli-act'`, "antigravity-cli")
	assertTool(`SELECT tool FROM sessions WHERE id = 'sess-desk'`, "antigravity")
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

// TestMigration058Upgrade_57_then_58 proves the codex reasoning-output
// net data fix: a database pinned at version 57 with pre-existing token
// rows upgrades to the latest version, and only gross codex rows have
// their reasoning netted out of output_tokens.
//
//   - codex, output 228 / reasoning 16   -> output 212 (228-16), reasoning kept
//   - gemini, output 15 / reasoning 521  -> UNTOUCHED (disjoint wire, wrong tool)
//   - codex, output 100 / reasoning 0    -> UNTOUCHED (nothing double-billed)
//   - codex, output 5 / reasoning 40     -> output 0 (MAX clamp, not negative)
func TestMigration058Upgrade_57_then_58(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "upgrade-058.db")

	// Raw open (no migrations) so we can stop at version 57 deliberately.
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

	// Bootstrap schema_meta and replay migration bodies 001..057 in order.
	if _, err := database.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatalf("bootstrap schema_meta: %v", err)
	}
	for _, e := range entries {
		if e.version > 57 {
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
		`INSERT INTO schema_meta(key, value) VALUES ('version', '57')
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`); err != nil {
		t.Fatalf("pin version 57: %v", err)
	}
	if v, err := Version(ctx, database); err != nil || v != 57 {
		t.Fatalf("pre-upgrade version = %d (err=%v), want 57", v, err)
	}

	// Seed a project + session, then the four pre-migration token rows.
	if _, err := database.ExecContext(ctx,
		`INSERT INTO projects (id, root_path, created_at) VALUES (1, '/p', '2026-05-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO sessions (id, project_id, tool, started_at, total_actions)
		 VALUES ('sA', 1, 'codex', '2026-05-01T00:00:00Z', 0)`); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	seed := func(evtID, tool string, output, reasoning int) {
		if _, err := database.ExecContext(ctx,
			`INSERT INTO token_usage(session_id, timestamp, tool, model,
			   input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
			   reasoning_tokens, source, reliability, source_file, source_event_id)
			 VALUES ('sA', '2026-05-01T01:00:00Z', ?, 'm',
			   10, ?, 0, 0, ?, 'jsonl', 'unreliable', '/f.jsonl', ?)`,
			tool, output, reasoning, evtID); err != nil {
			t.Fatalf("seed token_usage %s: %v", evtID, err)
		}
	}
	seed("codex-gross", "codex", 228, 16)      // -> output 212
	seed("gemini-disjoint", "gemini", 15, 521) // -> untouched (wrong tool)
	seed("codex-zero", "codex", 100, 0)        // -> untouched (reasoning 0)
	seed("codex-clamp", "codex", 5, 40)        // -> output 0 (MAX clamp)

	// Run the REAL runner — it sees applied=57 and applies 058.
	if err := runMigrations(ctx, database); err != nil {
		t.Fatalf("upgrade runMigrations: %v", err)
	}
	wantVersion := entries[len(entries)-1].version
	if v, err := Version(ctx, database); err != nil || v != wantVersion {
		t.Fatalf("post-upgrade version = %d (err=%v), want %d", v, err, wantVersion)
	}

	outputOf := func(evtID string) int {
		var out int
		if err := database.QueryRowContext(ctx,
			`SELECT output_tokens FROM token_usage WHERE source_event_id = ?`, evtID).Scan(&out); err != nil {
			t.Fatalf("read output_tokens %s: %v", evtID, err)
		}
		return out
	}

	if got := outputOf("codex-gross"); got != 212 {
		t.Errorf("codex gross row: output_tokens = %d, want 212 (228-16)", got)
	}
	if got := outputOf("gemini-disjoint"); got != 15 {
		t.Errorf("gemini row mutated: output_tokens = %d, want 15 (untouched)", got)
	}
	if got := outputOf("codex-zero"); got != 100 {
		t.Errorf("codex reasoning=0 row mutated: output_tokens = %d, want 100 (untouched)", got)
	}
	if got := outputOf("codex-clamp"); got != 0 {
		t.Errorf("codex clamp row: output_tokens = %d, want 0 (MAX(5-40,0), not negative)", got)
	}

	// reasoning_tokens is never touched by the fix — the double count lived
	// in output_tokens, not here.
	var reasoning int
	if err := database.QueryRowContext(ctx,
		`SELECT reasoning_tokens FROM token_usage WHERE source_event_id = 'codex-gross'`).Scan(&reasoning); err != nil {
		t.Fatalf("read reasoning_tokens: %v", err)
	}
	if reasoning != 16 {
		t.Errorf("codex reasoning_tokens = %d, want 16 (unchanged)", reasoning)
	}

	// Idempotency: re-running migrations from latest version is a no-op and
	// does NOT subtract reasoning a second time.
	if err := runMigrations(ctx, database); err != nil {
		t.Fatalf("idempotent re-run: %v", err)
	}
	if got := outputOf("codex-gross"); got != 212 {
		t.Errorf("codex gross row after re-run: output_tokens = %d, want 212 (stable)", got)
	}
}

// TestMigration059Upgrade_58_then_59 proves the Copilot CLI reasoning-output
// history fix: a node pinned at version 58 with pre-existing token rows
// upgrades, and ONLY gross Tier-1 copilot-cli rows have their reasoning
// netted out of output_tokens.
//
//   - copilot-cli, output 565 / reasoning 448 -> output 117 (565-448)
//   - copilot-cli, output 0   / reasoning 448 -> UNTOUCHED (Tier-0 shutdown
//     row: output already 0, guard output>=reasoning excludes it)
//   - copilot     , output 264 / reasoning 192 -> UNTOUCHED (VS Code wire,
//     wrong tool — unverifiable, left alone)
//   - copilot-cli, output 15  / reasoning 27  -> UNTOUCHED (output<reasoning,
//     guard excludes — not a gross-subset row)
//   - copilot-cli, output 100 / reasoning 0   -> UNTOUCHED (nothing double-billed)
func TestMigration059Upgrade_58_then_59(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "upgrade-059.db")

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

	// Bootstrap schema_meta and replay migration bodies 001..058 in order.
	if _, err := database.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatalf("bootstrap schema_meta: %v", err)
	}
	for _, e := range entries {
		if e.version > 58 {
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
		`INSERT INTO schema_meta(key, value) VALUES ('version', '58')
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`); err != nil {
		t.Fatalf("pin version 58: %v", err)
	}
	if v, err := Version(ctx, database); err != nil || v != 58 {
		t.Fatalf("pre-upgrade version = %d (err=%v), want 58", v, err)
	}

	if _, err := database.ExecContext(ctx,
		`INSERT INTO projects (id, root_path, created_at) VALUES (1, '/p', '2026-05-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO sessions (id, project_id, tool, started_at, total_actions)
		 VALUES ('sA', 1, 'copilot-cli', '2026-05-01T00:00:00Z', 0)`); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	seed := func(evtID, tool string, output, reasoning int) {
		if _, err := database.ExecContext(ctx,
			`INSERT INTO token_usage(session_id, timestamp, tool, model,
			   input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
			   reasoning_tokens, source, reliability, source_file, source_event_id)
			 VALUES ('sA', '2026-05-01T01:00:00Z', ?, 'm',
			   10, ?, 0, 0, ?, 'jsonl', 'unreliable', '/f.jsonl', ?)`,
			tool, output, reasoning, evtID); err != nil {
			t.Fatalf("seed token_usage %s: %v", evtID, err)
		}
	}
	seed("cli-gross", "copilot-cli", 565, 448) // -> output 117
	seed("cli-tier0", "copilot-cli", 0, 448)   // -> untouched (output already 0)
	seed("vscode-disjoint", "copilot", 264, 192)
	seed("cli-out-lt-reason", "copilot-cli", 15, 27) // -> untouched (guard excludes)
	seed("cli-zero", "copilot-cli", 100, 0)          // -> untouched (reasoning 0)

	// Run the REAL runner — it sees applied=58 and applies 059.
	if err := runMigrations(ctx, database); err != nil {
		t.Fatalf("upgrade runMigrations: %v", err)
	}
	wantVersion := entries[len(entries)-1].version
	if v, err := Version(ctx, database); err != nil || v != wantVersion {
		t.Fatalf("post-upgrade version = %d (err=%v), want %d", v, err, wantVersion)
	}

	outputOf := func(evtID string) int {
		var out int
		if err := database.QueryRowContext(ctx,
			`SELECT output_tokens FROM token_usage WHERE source_event_id = ?`, evtID).Scan(&out); err != nil {
			t.Fatalf("read output_tokens %s: %v", evtID, err)
		}
		return out
	}

	if got := outputOf("cli-gross"); got != 117 {
		t.Errorf("copilot-cli gross row: output_tokens = %d, want 117 (565-448)", got)
	}
	if got := outputOf("cli-tier0"); got != 0 {
		t.Errorf("copilot-cli Tier-0 row mutated: output_tokens = %d, want 0 (untouched)", got)
	}
	if got := outputOf("vscode-disjoint"); got != 264 {
		t.Errorf("copilot (VS Code) row mutated: output_tokens = %d, want 264 (untouched, wrong tool)", got)
	}
	if got := outputOf("cli-out-lt-reason"); got != 15 {
		t.Errorf("copilot-cli output<reasoning row mutated: output_tokens = %d, want 15 (guard excludes)", got)
	}
	if got := outputOf("cli-zero"); got != 100 {
		t.Errorf("copilot-cli reasoning=0 row mutated: output_tokens = %d, want 100 (untouched)", got)
	}

	// reasoning_tokens is never touched — the double count lived in output.
	var reasoning int
	if err := database.QueryRowContext(ctx,
		`SELECT reasoning_tokens FROM token_usage WHERE source_event_id = 'cli-gross'`).Scan(&reasoning); err != nil {
		t.Fatalf("read reasoning_tokens: %v", err)
	}
	if reasoning != 448 {
		t.Errorf("copilot-cli reasoning_tokens = %d, want 448 (unchanged)", reasoning)
	}

	// Idempotency: re-running is a no-op, not a second subtraction.
	if err := runMigrations(ctx, database); err != nil {
		t.Fatalf("idempotent re-run: %v", err)
	}
	if got := outputOf("cli-gross"); got != 117 {
		t.Errorf("copilot-cli gross row after re-run: output_tokens = %d, want 117 (stable)", got)
	}
}

// TestMigration068Upgrade_67_then_68 proves the benchmark attempt_no rebuild:
// a node pinned at version 67 with a benchmark run (attempt + session member +
// score children) upgrades cleanly, the attempt_no column + widened UNIQUE key
// land, all child rows survive with their ids/FKs intact, and a retry of the
// same logical cell (attempt_no=1) now inserts without a UNIQUE collision.
func TestMigration068Upgrade_67_then_68(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "upgrade-068.db")

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

	// Bootstrap schema_meta and replay migration bodies 001..067 in order.
	if _, err := database.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatalf("bootstrap schema_meta: %v", err)
	}
	for _, e := range entries {
		if e.version > 67 {
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
		`INSERT INTO schema_meta(key, value) VALUES ('version', '67')
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`); err != nil {
		t.Fatalf("pin version 67: %v", err)
	}
	if v, err := Version(ctx, database); err != nil || v != 67 {
		t.Fatalf("pre-upgrade version = %d (err=%v), want 67", v, err)
	}
	if columnExists(t, database, "benchmark_attempts", "attempt_no") {
		t.Fatal("attempt_no present before migration 068")
	}

	// Seed a run + attempt + a session member + a score (children with FKs).
	if _, err := database.ExecContext(ctx,
		`INSERT INTO benchmark_runs (run_id, spec_name, spec_hash, spec_json, started_at, status)
		 VALUES ('run-1', 'corpus', 'h', '{}', '2026-07-16T00:00:00Z', 'running')`); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO benchmark_attempts (id, run_id, task_id, config_id, harness, model_requested, repeat_idx, status, started_at)
		 VALUES (5, 'run-1', 't1', 'c1', 'codex', 'm', 0, 'ok', '2026-07-16T00:00:01Z')`); err != nil {
		t.Fatalf("seed attempt: %v", err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO benchmark_session_members (attempt_id, run_id, session_id, role)
		 VALUES (5, 'run-1', 'sess-a', 'primary')`); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO benchmark_scores (attempt_id, run_id, scorer, score, passed)
		 VALUES (5, 'run-1', 'tests_pass', 1, 1)`); err != nil {
		t.Fatalf("seed score: %v", err)
	}

	// Run the REAL runner — it sees applied=67 and applies 068 (+ any later).
	if err := runMigrations(ctx, database); err != nil {
		t.Fatalf("upgrade runMigrations: %v", err)
	}
	wantVersion := entries[len(entries)-1].version
	if v, err := Version(ctx, database); err != nil || v != wantVersion {
		t.Fatalf("post-upgrade version = %d (err=%v), want %d", v, err, wantVersion)
	}

	if !columnExists(t, database, "benchmark_attempts", "attempt_no") {
		t.Fatal("attempt_no missing after migration 068")
	}
	// The seeded attempt survives with attempt_no defaulted to 0.
	var attNo, repeat int
	if err := database.QueryRowContext(ctx,
		`SELECT attempt_no, repeat_idx FROM benchmark_attempts WHERE id = 5`).Scan(&attNo, &repeat); err != nil {
		t.Fatalf("read migrated attempt: %v", err)
	}
	if attNo != 0 {
		t.Errorf("migrated attempt_no = %d, want 0", attNo)
	}
	// Children survived with their FKs still resolving to attempt id 5.
	var memberN, scoreN int
	if err := database.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM benchmark_session_members WHERE attempt_id = 5`).Scan(&memberN); err != nil {
		t.Fatalf("count members: %v", err)
	}
	if err := database.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM benchmark_scores WHERE attempt_id = 5`).Scan(&scoreN); err != nil {
		t.Fatalf("count scores: %v", err)
	}
	if memberN != 1 || scoreN != 1 {
		t.Errorf("children lost in rebuild: members=%d scores=%d, want 1/1", memberN, scoreN)
	}

	// The whole point: a retry of the SAME logical cell (attempt_no=1) inserts
	// without a UNIQUE collision — the pre-068 bug.
	if _, err := database.ExecContext(ctx,
		`INSERT INTO benchmark_attempts (run_id, task_id, config_id, harness, model_requested, repeat_idx, attempt_no, status, started_at)
		 VALUES ('run-1', 't1', 'c1', 'codex', 'm', 0, 1, 'ok', '2026-07-16T00:00:02Z')`); err != nil {
		t.Fatalf("retry insert (attempt_no=1) should not collide: %v", err)
	}
	// But a duplicate attempt_no on the same cell is still rejected.
	if _, err := database.ExecContext(ctx,
		`INSERT INTO benchmark_attempts (run_id, task_id, config_id, harness, model_requested, repeat_idx, attempt_no, status, started_at)
		 VALUES ('run-1', 't1', 'c1', 'codex', 'm', 0, 1, 'ok', '2026-07-16T00:00:03Z')`); err == nil {
		t.Error("duplicate (cell, attempt_no) accepted; widened UNIQUE not enforced")
	}

	// A child FK to a non-existent attempt is still rejected (FK intact).
	if _, err := database.ExecContext(ctx,
		`INSERT INTO benchmark_scores (attempt_id, run_id, scorer) VALUES (9999, 'run-1', 'x')`); err == nil {
		t.Error("benchmark_scores accepted a dangling attempt_id; FK not enforced post-rebuild")
	}

	// Idempotency: re-running is a no-op.
	if err := runMigrations(ctx, database); err != nil {
		t.Fatalf("idempotent re-run: %v", err)
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

// taxBackfillCase is one seeded pre-fix action row for migration 077.
// id doubles as the row's source_event_id and as the assertion key.
type taxBackfillCase struct {
	id     string
	tool   string
	raw    string
	before string
	after  string
	why    string
}

// taxBackfillCases is the table migration 077 is graded against: the
// three families the plan names, plus the controls that prove the
// migration is SCOPED rather than a blanket retag.
//
// Expected values come from internal/tooltax (the migration's own
// source), verified against tooltax.Resolve during implementation.
var taxBackfillCases = []taxBackfillCase{
	// --- codex agent-orchestration family (plan §0: ~1,900 rows) ---
	{"t77-1", "codex", "wait", "unknown", "subagent_wait", "codex blocks on a spawned agent"},
	{"t77-2", "codex", "wait_agent", "unknown", "subagent_wait", "the named variant of the same verb"},
	{"t77-3", "codex", "spawn_agent", "unknown", "spawn_subagent", "the launch, not the wait"},
	{"t77-4", "codex", "send_message", "unknown", "agent_message", "work handed to an existing thread"},
	{"t77-5", "codex", "write_stdin", "unknown", "stdin_write", "channel into a running thread, not a new shell"},
	{"t77-6", "codex", "list_agents", "unknown", "agent_control", "inspecting the pool"},

	// --- claude-code harness family (plan §0: ~1,100 rows) ---
	{"t77-7", "claude-code", "Monitor", "unknown", "agent_control", "024 deferred this one by name"},
	{"t77-8", "claude-code", "SendMessage", "unknown", "agent_message", "inter-agent message"},
	{"t77-9", "claude-code", "Skill", "unknown", "skill_invoke", "the new skill category"},
	{"t77-10", "claude-code", "ToolSearch", "unknown", "tool_search", "search over the HARNESS, not the project"},
	{"t77-11", "claude-code", "ScheduleWakeup", "unknown", "schedule", "024 deferred this one by name"},
	{"t77-12", "claude-code", "StructuredOutput", "unknown", "harness_call", "host builtin, no other honest bucket"},

	// --- the mcp__ namespace sweep (plan §0: 264 unresolved rows) ---
	{"t77-13", "claude-code", "mcp__observer__get_file", "unknown", "mcp_call", "an identity, not a heuristic"},
	{"t77-14", "codex", "mcp__ide__executeCode", "unknown", "mcp_call", "codex has no mcp__ branch at all"},
	{"t77-15", "qoder", "mcp__srv__tool", "unknown", "mcp_call", "the sweep is tool-less by design"},

	// --- tool-specific prefix rules ---
	{"t77-16", "hermes", "browser_click", "unknown", "browser_action", "hermes browser_* prefix rule"},
	{"t77-17", "opencode", "todo.in_progress", "unknown", "todo_update", "opencode todo.* prefix rule"},

	// --- the tool-less fallback applies to a tool with NO rows of its own ---
	{"t77-18", "chatgpt-web", "bash", "unknown", "run_command", "fallback rows apply to any tool, as tooltax.Resolve does"},

	// --- CONTROL: the same native name, a different tool, a different
	// mapping. cowork deliberately routes Skill through mcp_call; the
	// claude-code row above says skill_invoke. Tool scoping is the only
	// thing keeping these apart, so this is THE control for it.
	{"t77-19", "cowork", "Skill", "unknown", "mcp_call", "CONTROL: cowork's semantic remap survives"},

	// --- CONTROL: rows that are NOT unknown are never re-classified.
	// This is the post_tool_batch shadow class the plan leaves alone.
	{"t77-20", "claude-code", "mcp__observer__get_file", "post_tool_batch", "post_tool_batch", "CONTROL: shadow row untouched"},
	{"t77-21", "codex", "wait", "run_command", "run_command", "CONTROL: an already-classified row is never re-derived"},

	// --- CONTROL: an unknown row whose name is not in the table stays
	// unknown. The migration only ever moves rows tooltax can name.
	{"t77-22", "claude-code", "TotallyUnknownTool", "unknown", "unknown", "CONTROL: unmapped name keeps its bucket"},

	// --- CONTROL: matching is EXACT and case-sensitive. tooltax.Resolve
	// has a second, normalized pass (lower-cased, punctuation stripped);
	// the migration deliberately does not replay it, because a fuzzy
	// match is not a basis for rewriting history.
	{"t77-23", "claude-code", "monitor", "unknown", "unknown", "CONTROL: normalized pass is not replayed"},
	{"t77-24", "hermes", "BROWSER_click", "unknown", "unknown", "CONTROL: prefix rules are case-sensitive (substr, not LIKE)"},
	{"t77-25", "claude-code", "MCP__srv__tool", "unknown", "unknown", "CONTROL: the mcp__ sweep is case-sensitive too"},

	// --- CONTROL: a tool-specific name is not leaked to other tools.
	// `attempt_completion` is a cline/roo-code name; codex never emits it
	// and must not be retagged by cline's row.
	{"t77-26", "codex", "attempt_completion", "unknown", "unknown", "CONTROL: another tool's name does not cross over"},
}

// TestMigration077_TooltaxActionTypeBackfill proves the generated
// taxonomy backfill repairs the historical `unknown` rows the plan
// names — the codex agent-orchestration family, the claude-code harness
// family and the unresolved mcp__ namespace — while leaving every
// control row exactly where it was: another tool's mapping of the same
// name, an already-classified row, an unmapped name, and the
// case-variant names the exact-match predicates must not reach.
//
// It also asserts idempotency by executing the migration body a second
// time and diffing the full post-state.
func TestMigration077_TooltaxActionTypeBackfill(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "taxbackfill.db")

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

	// Replay every migration body BELOW 077, then pin the version so the
	// real runner applies only 077 against our seeded rows.
	if _, err := database.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatalf("bootstrap schema_meta: %v", err)
	}
	var backfillBody string
	for _, e := range entries {
		body, readErr := fs.ReadFile(migrations.Files, e.filename)
		if readErr != nil {
			t.Fatalf("read %s: %v", e.filename, readErr)
		}
		if e.version == 77 {
			backfillBody = string(body)
			continue
		}
		if e.version > 77 {
			continue
		}
		if _, err := database.ExecContext(ctx, string(body)); err != nil {
			t.Fatalf("apply %s: %v", e.filename, err)
		}
	}
	if backfillBody == "" {
		t.Fatal("migration 077 is not embedded")
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO schema_meta(key, value) VALUES ('version', '76')
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`); err != nil {
		t.Fatalf("pin version 76: %v", err)
	}

	if _, err := database.ExecContext(ctx,
		`INSERT INTO projects (id, root_path, created_at) VALUES (1, '/p', '2026-07-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	// One session per tool the table exercises.
	seenTool := map[string]bool{}
	for _, c := range taxBackfillCases {
		if seenTool[c.tool] {
			continue
		}
		seenTool[c.tool] = true
		if _, err := database.ExecContext(ctx,
			`INSERT INTO sessions (id, project_id, tool, started_at, total_actions)
			 VALUES (?, 1, ?, '2026-07-01T00:00:00Z', 0)`, "sess-"+c.tool, c.tool); err != nil {
			t.Fatalf("seed session %s: %v", c.tool, err)
		}
	}
	for _, c := range taxBackfillCases {
		if _, err := database.ExecContext(ctx,
			`INSERT INTO actions (session_id, project_id, timestamp, action_type, tool,
			   raw_tool_name, source_file, source_event_id)
			 VALUES (?, 1, '2026-07-01T01:00:00Z', ?, ?, ?, 'seed.jsonl', ?)`,
			"sess-"+c.tool, c.before, c.tool, c.raw, c.id); err != nil {
			t.Fatalf("seed action %s: %v", c.id, err)
		}
	}

	// Run the REAL runner — sees applied=76, applies 077.
	if err := runMigrations(ctx, database); err != nil {
		t.Fatalf("upgrade runMigrations: %v", err)
	}
	wantVersion := entries[len(entries)-1].version
	if v, err := Version(ctx, database); err != nil || v != wantVersion {
		t.Fatalf("post-upgrade version = %d (err=%v), want %d", v, err, wantVersion)
	}

	post := taxBackfillSnapshot(ctx, t, database)
	if len(post) != len(taxBackfillCases) {
		t.Fatalf("snapshot has %d rows, seeded %d", len(post), len(taxBackfillCases))
	}
	for _, c := range taxBackfillCases {
		if got := post[c.id]; got != c.after {
			t.Errorf("%s (tool=%s raw_tool_name=%s, was %s): action_type = %q, want %q — %s",
				c.id, c.tool, c.raw, c.before, got, c.after, c.why)
		}
	}

	// Idempotency: applying the same body again must change nothing. The
	// runner itself would refuse (version 77 is recorded), so the body is
	// executed directly — this grades the SQL, not the bookkeeping.
	if _, err := database.ExecContext(ctx, backfillBody); err != nil {
		t.Fatalf("second apply of 077: %v", err)
	}
	second := taxBackfillSnapshot(ctx, t, database)
	for id, want := range post {
		if got := second[id]; got != want {
			t.Errorf("%s: not idempotent — action_type %q after one run, %q after two", id, want, got)
		}
	}
}

// taxBackfillSnapshot reads every seeded row's action_type, keyed by
// source_event_id.
func taxBackfillSnapshot(ctx context.Context, t *testing.T, database *sql.DB) map[string]string {
	t.Helper()
	rows, err := database.QueryContext(ctx,
		`SELECT source_event_id, action_type FROM actions WHERE source_file = 'seed.jsonl'`)
	if err != nil {
		t.Fatalf("snapshot query: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, at string
		if err := rows.Scan(&id, &at); err != nil {
			t.Fatalf("snapshot scan: %v", err)
		}
		out[id] = at
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("snapshot rows: %v", err)
	}
	return out
}

// asstRelabelCase is one seeded action row for migration 078. sourceFile
// is a pointer so a case can seed a NULL source_file — the shape the
// migration's COALESCE guard exists for.
type asstRelabelCase struct {
	id         string
	tool       string
	raw        string
	sourceFile *string
	before     string
	after      string
	why        string
}

func sf(s string) *string { return &s }

// asstRelabelCases is the table migration 078 is graded against: the
// `<tool>.assistant_text` rows it must relabel task_complete ->
// assistant_message, plus the CONTROLS that must survive untouched.
//
// The controls are the point. 078 rewrites rows an adapter already
// classified, so unlike 077 it has no monotone "can only leave the
// unknown bucket" safety property — its safety is entirely in the four
// ANDed predicates, and each control below removes one way the rewrite
// could over-reach.
var asstRelabelCases = []asstRelabelCase{
	// ---- must change: the seven emit sites WP-T6/B2a re-typed ----
	{"t78-1", "claude-code", "claudecode.assistant_text", sf("/h/.claude/projects/p/s.jsonl"), "task_complete", "assistant_message", "the JSONL walker's per-text-block row"},
	{"t78-2", "codex", "codex.assistant_text", sf("/h/.codex/sessions/r.jsonl"), "task_complete", "assistant_message", "one row per agent_message, several per turn"},
	{"t78-3", "kilo-code-cli", "kilo-code-cli.assistant_text", sf("/h/.local/share/kilo/kilo.db"), "task_complete", "assistant_message", "per text part, same shape as opencode"},
	{"t78-4", "aider", "aider.assistant_text", sf("/p/.aider.chat.history.md"), "task_complete", "assistant_message", "flushed at seven prose boundaries, not only at end"},
	{"t78-5", "crush", "crush.assistant_text", sf("/p/.crush/crush.db"), "task_complete", "assistant_message", "per text part of a message"},
	{"t78-6", "antigravity", "structured.assistant_text", sf("/h/.gemini/antigravity/conversations/c.pb"), "task_complete", "assistant_message", "per synthesized step, many steps per turn"},
	{"t78-7", "antigravity", "transcript.assistant_text", sf("/h/.gemini/antigravity/conversations/c.pb"), "task_complete", "assistant_message", "per MODEL/PLANNER_RESPONSE step"},

	// ---- must change: the retag-seam tools carrying the same raw name ----
	{"t78-8", "antigravity-cli", "transcript.assistant_text", sf("/h/.gemini/antigravity-cli/conversations/c.db"), "task_complete", "assistant_message", "migration 071 retagged CLI rows by path; the raw name is unchanged"},
	{"t78-9", "open-interpreter", "codex.assistant_text", sf("/h/.openinterpreter/sessions/r.jsonl"), "task_complete", "assistant_message", "codex parser retagged by NewOpenInterpreter"},
	{"t78-10", "kilo-code", "cline.assistant_text", sf("/h/.vscode/kilo/task.json"), "task_complete", "assistant_message", "cline parser retagged by kilocode/legacy.go"},

	// ---- must change: the five tools swept in an earlier cycle ----
	{"t78-11", "cowork", "cowork.assistant_text", sf("/h/.cowork/s.jsonl"), "task_complete", "assistant_message", "stale history; code already emits assistant_message"},
	{"t78-12", "cursor", "cursor.assistant_text", sf("/h/.cursor/t.jsonl"), "task_complete", "assistant_message", "stale history"},
	{"t78-13", "cline", "cline.assistant_text", sf("/h/.vscode/cline/task.json"), "task_complete", "assistant_message", "stale history"},
	{"t78-14", "opencode", "opencode.assistant_text", sf("/h/.local/share/opencode/db"), "task_complete", "assistant_message", "stale history"},
	{"t78-15", "openclaw", "openclaw.assistant_text", sf("/h/.openclaw/s.jsonl"), "task_complete", "assistant_message", "stale history; openclaw is the exemplar"},

	// ---- must change: NULL source_file ----
	// actions.source_file is NULLable, so a bare `source_file <> '...'`
	// carve-out evaluates to NULL (hence FALSE) here and would silently
	// SKIP this row. COALESCE(source_file, '') is what makes it move.
	{"t78-16", "claude-code", "claudecode.assistant_text", nil, "task_complete", "assistant_message", "walker row with no source_file must still be relabeled (COALESCE guard)"},

	// ---- CONTROL 1: the claude-code Stop-hook rows (3,141 in the live
	// corpus) are genuinely turn-terminal and keep task_complete ----
	{"t78-c1", "claude-code", "claudecode.assistant_text", sf("claude-code:hook"), "task_complete", "task_complete", "CONTROL: the Stop hook fires once on turn end — the one carve-out"},

	// ---- CONTROL 2: unknown rows stay unknown (078 is a pair rewrite of
	// task_complete only; moving rows out of unknown is 077's job) ----
	{"t78-c2", "claude-code", "claudecode.assistant_text", sf("/h/.claude/projects/p/u.jsonl"), "unknown", "unknown", "CONTROL: not task_complete, so the pair predicate cannot match it"},

	// ---- CONTROL 3: another action type on a matching tool ----
	{"t78-c3", "claude-code", "Read", sf("/h/.claude/projects/p/s.jsonl"), "read_file", "read_file", "CONTROL: a different action type on an in-scope tool"},

	// ---- CONTROL 4: another tool carrying an identical-looking name ----
	{"t78-c4", "gemini-cli", "claudecode.assistant_text", sf("/h/.gemini/s.json"), "task_complete", "task_complete", "CONTROL: tool scoping — gemini-cli is not in the table"},

	// ---- CONTROL 5: rows already at the new type ----
	{"t78-c5", "codex", "codex.assistant_text", sf("/h/.codex/sessions/new.jsonl"), "assistant_message", "assistant_message", "CONTROL: post-sweep row; the rewrite is a no-op on it"},

	// ---- CONTROL 6: future / lookalike names the exact-literal and
	// case-sensitive predicates must not reach ----
	{"t78-c6a", "claude-code", "CLAUDECODE.ASSISTANT_TEXT", sf("/h/.claude/projects/p/case.jsonl"), "task_complete", "task_complete", "CONTROL: SQLite LIKE would match this case variant; exact literals do not"},
	{"t78-c6b", "claude-code", "claudecode.assistant_texts", sf("/h/.claude/projects/p/plural.jsonl"), "task_complete", "task_complete", "CONTROL: a longer lookalike name"},
	{"t78-c6c", "claude-code", "claudecode_assistant_text", sf("/h/.claude/projects/p/us.jsonl"), "task_complete", "task_complete", "CONTROL: `_` is a LIKE wildcard; exact literals are immune"},
	{"t78-c6d", "codex", "futuretool.assistant_text", sf("/h/.codex/sessions/f.jsonl"), "task_complete", "task_complete", "CONTROL: a FUTURE .assistant_text name a LIKE '%.assistant_text' would have captured"},

	// ---- CONTROL 7: genuine, evidence-grounded turn termini keep
	// task_complete — they are the reason the type still exists ----
	{"t78-c7a", "openclaw", "message.assistant.stop", sf("/h/.openclaw/s.jsonl"), "task_complete", "task_complete", "CONTROL: openclaw's stop-reason-gated terminal row (the exemplar)"},
	{"t78-c7b", "kilo-code-cli", "assistant.stop", sf("/h/.local/share/kilo/kilo.db"), "task_complete", "task_complete", "CONTROL: kilo-code-cli's terminus"},
	{"t78-c7c", "cline", "attempt_completion", sf("/h/.vscode/cline/task.json"), "task_complete", "task_complete", "CONTROL: a native completion tool the model called on purpose"},
	{"t78-c7d", "codex", "task_complete", sf("/h/.codex/sessions/r.jsonl"), "task_complete", "task_complete", "CONTROL: codex's own task_complete event_msg row"},
	{"t78-c7e", "antigravity", "structured.final_summary", sf("/h/.gemini/antigravity/conversations/c.pb"), "task_complete", "task_complete", "CONTROL: antigravity's turn-terminal summary row"},
}

// TestMigration078_AssistantTextActionTypeRelabel grades the generated
// relabel migration: it seeds the pre-fix rows at pinned version 77,
// runs the REAL migration runner, asserts the exact post-state for every
// row (the ones that must move AND the controls that must not), and then
// executes the body a second time and diffs the full snapshot to prove
// idempotency.
func TestMigration078_AssistantTextActionTypeRelabel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "asstrelabel.db")

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

	// Replay every migration body BELOW 078, then pin the version so the
	// real runner applies only 078 against our seeded rows.
	if _, err := database.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatalf("bootstrap schema_meta: %v", err)
	}
	var relabelBody string
	for _, e := range entries {
		body, readErr := fs.ReadFile(migrations.Files, e.filename)
		if readErr != nil {
			t.Fatalf("read %s: %v", e.filename, readErr)
		}
		if e.version == 78 {
			relabelBody = string(body)
			continue
		}
		if e.version > 78 {
			continue
		}
		if _, err := database.ExecContext(ctx, string(body)); err != nil {
			t.Fatalf("apply %s: %v", e.filename, err)
		}
	}
	if relabelBody == "" {
		t.Fatal("migration 078 is not embedded")
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO schema_meta(key, value) VALUES ('version', '77')
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`); err != nil {
		t.Fatalf("pin version 77: %v", err)
	}

	if _, err := database.ExecContext(ctx,
		`INSERT INTO projects (id, root_path, created_at) VALUES (1, '/p', '2026-07-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	seenTool := map[string]bool{}
	for _, c := range asstRelabelCases {
		if seenTool[c.tool] {
			continue
		}
		seenTool[c.tool] = true
		if _, err := database.ExecContext(ctx,
			`INSERT INTO sessions (id, project_id, tool, started_at, total_actions)
			 VALUES (?, 1, ?, '2026-07-01T00:00:00Z', 0)`, "sess-"+c.tool, c.tool); err != nil {
			t.Fatalf("seed session %s: %v", c.tool, err)
		}
	}
	for _, c := range asstRelabelCases {
		var src any
		if c.sourceFile != nil {
			src = *c.sourceFile
		}
		if _, err := database.ExecContext(ctx,
			`INSERT INTO actions (session_id, project_id, timestamp, action_type, tool,
			   raw_tool_name, source_file, source_event_id)
			 VALUES (?, 1, '2026-07-01T01:00:00Z', ?, ?, ?, ?, ?)`,
			"sess-"+c.tool, c.before, c.tool, c.raw, src, c.id); err != nil {
			t.Fatalf("seed action %s: %v", c.id, err)
		}
	}

	// Run the REAL runner — sees applied=77, applies 078.
	if err := runMigrations(ctx, database); err != nil {
		t.Fatalf("upgrade runMigrations: %v", err)
	}
	wantVersion := entries[len(entries)-1].version
	if v, err := Version(ctx, database); err != nil || v != wantVersion {
		t.Fatalf("post-upgrade version = %d (err=%v), want %d", v, err, wantVersion)
	}

	post := asstRelabelSnapshot(ctx, t, database)
	if len(post) != len(asstRelabelCases) {
		t.Fatalf("snapshot has %d rows, seeded %d", len(post), len(asstRelabelCases))
	}
	for _, c := range asstRelabelCases {
		if got := post[c.id]; got != c.after {
			t.Errorf("%s (tool=%s raw_tool_name=%s, was %s): action_type = %q, want %q — %s",
				c.id, c.tool, c.raw, c.before, got, c.after, c.why)
		}
	}

	// Idempotency: applying the same body again must change nothing. The
	// runner itself would refuse (version 78 is recorded), so the body is
	// executed directly — this grades the SQL, not the bookkeeping.
	if _, err := database.ExecContext(ctx, relabelBody); err != nil {
		t.Fatalf("second apply of 078: %v", err)
	}
	second := asstRelabelSnapshot(ctx, t, database)
	for id, want := range post {
		if got := second[id]; got != want {
			t.Errorf("%s: not idempotent — action_type %q after one run, %q after two", id, want, got)
		}
	}
}

// asstRelabelSnapshot reads every seeded row's action_type, keyed by
// source_event_id. It selects on the seed id prefix rather than
// source_file because the 078 table deliberately varies source_file
// (including a NULL) to exercise the carve-out.
func asstRelabelSnapshot(ctx context.Context, t *testing.T, database *sql.DB) map[string]string {
	t.Helper()
	rows, err := database.QueryContext(ctx,
		`SELECT source_event_id, action_type FROM actions WHERE source_event_id LIKE 't78-%'`)
	if err != nil {
		t.Fatalf("snapshot query: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, at string
		if err := rows.Scan(&id, &at); err != nil {
			t.Fatalf("snapshot scan: %v", err)
		}
		out[id] = at
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("snapshot rows: %v", err)
	}
	return out
}

// ---------------------------------------------------------------------
// Migration 079 — reasoning-row convergence (B3).
//
// 079 is the first migration in this family that DELETES rows, so it is
// graded harder than 078: every candidate shape AND every near-miss
// control, the full dependent-table protocol (a seeded row in each of
// the seven tables that reference actions(id)), the FTS-ghost gate, the
// cursor fold-in with devin's deliberate branch as its control, and the
// ID high-water marker.
// ---------------------------------------------------------------------

// reasonCase is one seeded action row for migration 079. target and
// output are pointers so a case can seed NULLs — the shape the
// producer-invariant predicate deliberately under-reaches on.
type reasonCase struct {
	id       string
	tool     string
	raw      string
	before   string
	target   *string
	output   *string
	survives bool
	// after is the expected action_type for a surviving row.
	after string
	why   string
}

// reasonCases is the table migration 079 is graded against.
//
// The controls are the point. A DELETE has no monotone safety property
// at all: every row it removes is gone. Each control below removes one
// way the predicate could over-reach, and each is a shape the retired
// producer could NOT have written.
var reasonCases = []reasonCase{
	// ---- must be DELETED: the content-free codex placeholders ----
	{
		"t79-del-enc", "codex", "codex.reasoning", "task_complete",
		sf("(encrypted reasoning, 972 bytes)"), sf("(encrypted reasoning, 972 bytes)"), false, "",
		"the dominant placeholder shape, witnessed verbatim in the probe corpus",
	},
	{
		"t79-del-plain", "codex", "codex.reasoning", "task_complete",
		sf("(reasoning)"), sf("(reasoning)"), false, "",
		"the no-summary/no-encrypted-body placeholder",
	},
	{
		"t79-del-big", "codex", "codex.reasoning", "task_complete",
		sf("(encrypted reasoning, 1048576 bytes)"), sf("(encrypted reasoning, 1048576 bytes)"), false, "",
		"a multi-digit byte count — the digit test must not be length-bound",
	},
	{
		"t79-del-oi", "open-interpreter", "codex.reasoning", "task_complete",
		sf("(encrypted reasoning, 4 bytes)"), sf("(encrypted reasoning, 4 bytes)"), false, "",
		"the NewOpenInterpreter retag of the same parser — the silent island a codex-only delete would leave",
	},
	{
		"t79-del-max19", "codex", "codex.reasoning", "task_complete",
		sf("(encrypted reasoning, 9223372036854775807 bytes)"),
		sf("(encrypted reasoning, 9223372036854775807 bytes)"), false, "",
		"exactly 19 digits (the int64 ceiling) — the digit-count cap must be INCLUSIVE at its boundary",
	},

	// ---- CONTROL 1: the codex REAL-TEXT survivor. The whole point of
	// the target-shape conditions: a reasoning row that actually carries
	// a summary is content and must not be deleted ----
	{
		"t79-keep-real", "codex", "codex.reasoning", "task_complete",
		sf("**Considering the request**\n\nThe user wants the migration to keep real summaries."),
		sf("**Considering the request**\n\nThe user wants the migration to keep real summaries."),
		true, "task_complete",
		"CONTROL: real summary text — 329 such rows exist and all of them stay",
	},

	// ---- CONTROL 2: gemini rows are LOSSY to delete and out of scope ----
	{
		"t79-keep-gemini", "gemini-cli", "gemini.reasoning", "task_complete",
		sf("Thinking about the user's request in some detail…"),
		sf("Thinking about the user's request in some detail…"), true, "task_complete",
		"CONTROL: gemini's raw_tool_output holds 211-2,926 bytes that exist nowhere else",
	},

	// ---- CONTROL 3: tool scoping — another tool's reasoning row, even
	// in the identical placeholder shape ----
	{
		"t79-keep-crush", "crush", "crush.reasoning", "task_complete",
		sf("(reasoning)"), sf("(reasoning)"), true, "task_complete",
		"CONTROL: tool scoping — crush is not in the delete table",
	},

	// ---- CONTROL 4: the producer invariant raw_tool_output = target ----
	{
		"t79-keep-mismatch", "codex", "codex.reasoning", "task_complete",
		sf("(reasoning)"), sf("something the placeholder producer never wrote"), true, "task_complete",
		"CONTROL: output != target, so the producer invariant does not hold",
	},
	{
		"t79-keep-nulloutput", "codex", "codex.reasoning", "task_complete",
		sf("(reasoning)"), nil, true, "task_complete",
		"CONTROL: NULL raw_tool_output (pre-migration-027 history) — the documented, deliberate under-reach",
	},

	// ---- CONTROL 5: the action_type guard ----
	{
		"t79-keep-retyped", "codex", "codex.reasoning", "assistant_message",
		sf("(reasoning)"), sf("(reasoning)"), true, "assistant_message",
		"CONTROL: a row somebody re-typed on purpose is out of reach",
	},

	// ---- CONTROL 6: the printf tail matched structurally, not loosely ----
	{
		"t79-keep-nondigit", "codex", "codex.reasoning", "task_complete",
		sf("(encrypted reasoning, many bytes)"), sf("(encrypted reasoning, many bytes)"), true, "task_complete",
		"CONTROL: the middle segment is not digits, so it is not a %d rendering",
	},
	{
		"t79-keep-nodigits", "codex", "codex.reasoning", "task_complete",
		sf("(encrypted reasoning,  bytes)"), sf("(encrypted reasoning,  bytes)"), true, "task_complete",
		"CONTROL: an EMPTY numeric segment — the BETWEEN 1 AND 19 lower bound rejects it",
	},

	// ---- CONTROL 6b: shapes that ARE all digits but are not a canonical
	// positive %d, so the retired producer could not have written them.
	// `%d` of len(non-empty body) has a first digit in 1..9 and at most
	// 19 of them; anything else is somebody else's string ----
	{
		"t79-keep-zero", "codex", "codex.reasoning", "task_complete",
		sf("(encrypted reasoning, 0 bytes)"), sf("(encrypted reasoning, 0 bytes)"), true, "task_complete",
		"CONTROL: a ZERO byte count — the producer only rendered this branch with a body to measure",
	},
	{
		"t79-keep-leadingzero", "codex", "codex.reasoning", "task_complete",
		sf("(encrypted reasoning, 0972 bytes)"), sf("(encrypted reasoning, 0972 bytes)"), true, "task_complete",
		"CONTROL: a LEADING ZERO — `%d` never pads",
	},
	{
		"t79-keep-doublezero", "codex", "codex.reasoning", "task_complete",
		sf("(encrypted reasoning, 00 bytes)"), sf("(encrypted reasoning, 00 bytes)"), true, "task_complete",
		"CONTROL: `00` is all digits and non-empty, and still not a `%d` rendering",
	},
	{
		"t79-keep-20digits", "codex", "codex.reasoning", "task_complete",
		sf("(encrypted reasoning, 12345678901234567890 bytes)"),
		sf("(encrypted reasoning, 12345678901234567890 bytes)"), true, "task_complete",
		"CONTROL: 20 digits — one past the int64 decimal ceiling, so no Go int can render it",
	},
	{
		"t79-keep-suffix", "codex", "codex.reasoning", "task_complete",
		sf("(encrypted reasoning, 972 bytes) and then some"), sf("(encrypted reasoning, 972 bytes) and then some"),
		true, "task_complete",
		"CONTROL: prefix matches but the string continues past the suffix",
	},

	// ---- CONTROL 7: exact raw-name literal, case-sensitively ----
	{
		"t79-keep-case", "codex", "CODEX.REASONING", "task_complete",
		sf("(reasoning)"), sf("(reasoning)"), true, "task_complete",
		"CONTROL: SQLite LIKE would match this case variant; an exact literal does not",
	},
	{
		"t79-keep-lookalike", "codex", "codex_reasoning", "task_complete",
		sf("(reasoning)"), sf("(reasoning)"), true, "task_complete",
		"CONTROL: `_` is a LIKE wildcard; exact literals are immune",
	},

	// ---- the cursor fold-in, its already-correct control, and devin ----
	{
		"t79-cursor-stale", "cursor", "cursor.assistant_response", "task_complete",
		sf("Here is the summary you asked for."), sf("Here is the summary you asked for."), true, "assistant_message",
		"pre-sweep history; adapter.go:433 types this assistant_message unconditionally",
	},
	{
		"t79-cursor-current", "cursor", "cursor.assistant_response", "assistant_message",
		sf("A post-sweep row."), sf("A post-sweep row."), true, "assistant_message",
		"CONTROL: already correct — the pair rewrite is a no-op on it",
	},
	{
		"t79-devin", "devin", "devin.assistant_message", "task_complete",
		sf("Done — the change is committed."), sf("Done — the change is committed."), true, "task_complete",
		"CONTROL: devin's LIVE branch (adapter.go:265-267) keyed on the provider's finish_reason — UNTOUCHED",
	},
	{
		"t79-cursor-text", "cursor", "cursor.assistant_text", "task_complete",
		sf("A row 078 handles, not 079."), sf("A row 078 handles, not 079."), true, "task_complete",
		"CONTROL: a different raw name on an in-scope tool — 079's rewrite is name-exact",
	},
}

// TestMigration079_ReasoningRowConvergence grades the generated
// convergence migration: it seeds every candidate and control shape plus
// a dependent row in each of the seven tables that reference actions(id)
// at pinned version 78, runs the REAL migration runner with foreign keys
// ON, and asserts the exact post-state — which rows are gone, which
// survived, which dependent rows were cleared, which were merely
// unlinked, and what the high-water marker recorded. Then it executes
// the body a second time and proves nothing moves.
func TestMigration079_ReasoningRowConvergence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reasonconv.db")

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

	// Replay every migration body BELOW 079, then pin the version so the
	// real runner applies only 079 against our seeded rows.
	if _, err := database.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatalf("bootstrap schema_meta: %v", err)
	}
	var convergenceBody string
	for _, e := range entries {
		body, readErr := fs.ReadFile(migrations.Files, e.filename)
		if readErr != nil {
			t.Fatalf("read %s: %v", e.filename, readErr)
		}
		if e.version == 79 {
			convergenceBody = string(body)
			continue
		}
		if e.version > 79 {
			continue
		}
		if _, err := database.ExecContext(ctx, string(body)); err != nil {
			t.Fatalf("apply %s: %v", e.filename, err)
		}
	}
	if convergenceBody == "" {
		t.Fatal("migration 079 is not embedded")
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO schema_meta(key, value) VALUES ('version', '78')
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`); err != nil {
		t.Fatalf("pin version 78: %v", err)
	}

	if _, err := database.ExecContext(ctx,
		`INSERT INTO projects (id, root_path, created_at) VALUES (1, '/p', '2026-07-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	seenTool := map[string]bool{}
	for _, c := range reasonCases {
		if seenTool[c.tool] {
			continue
		}
		seenTool[c.tool] = true
		if _, err := database.ExecContext(ctx,
			`INSERT INTO sessions (id, project_id, tool, started_at, total_actions)
			 VALUES (?, 1, ?, '2026-07-01T00:00:00Z', 0)`, "sess-"+c.tool, c.tool); err != nil {
			t.Fatalf("seed session %s: %v", c.tool, err)
		}
	}
	for _, c := range reasonCases {
		var target, output any
		if c.target != nil {
			target = *c.target
		}
		if c.output != nil {
			output = *c.output
		}
		if _, err := database.ExecContext(ctx,
			`INSERT INTO actions (session_id, project_id, timestamp, action_type, tool,
			   raw_tool_name, target, raw_tool_output, source_event_id)
			 VALUES (?, 1, '2026-07-01T01:00:00Z', ?, ?, ?, ?, ?, ?)`,
			"sess-"+c.tool, c.before, c.tool, c.raw, target, output, c.id); err != nil {
			t.Fatalf("seed action %s: %v", c.id, err)
		}
	}

	ids := reasonActionIDs(ctx, t, database)

	// The dependency protocol is graded on TWO anchors: a row that is
	// about to be deleted (its dependents must be cleared / unlinked)
	// and a row that survives (its dependents must be untouched). With
	// foreign_keys ON, getting the order wrong fails the migration
	// outright with "FOREIGN KEY constraint failed (787)" — the live
	// 2026-06-18 regression this protocol exists to avoid.
	doomed, ok := ids["t79-del-enc"]
	if !ok {
		t.Fatal("seeded doomed row not found")
	}
	survivor, ok := ids["t79-keep-real"]
	if !ok {
		t.Fatal("seeded survivor row not found")
	}
	seedDependents(ctx, t, database, "doomed", doomed)
	seedDependents(ctx, t, database, "survivor", survivor)

	var maxBefore int64
	if err := database.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM actions`).Scan(&maxBefore); err != nil {
		t.Fatalf("read MAX(actions.id): %v", err)
	}

	// Run the REAL runner — sees applied=78, applies 079.
	if err := runMigrations(ctx, database); err != nil {
		t.Fatalf("upgrade runMigrations: %v", err)
	}
	wantVersion := entries[len(entries)-1].version
	if v, err := Version(ctx, database); err != nil || v != wantVersion {
		t.Fatalf("post-upgrade version = %d (err=%v), want %d", v, err, wantVersion)
	}

	// ---- (a) rows: deleted vs survived, with the exact surviving type.
	post := reasonSnapshot(ctx, t, database)
	for _, c := range reasonCases {
		got, present := post[c.id]
		switch {
		case c.survives && !present:
			t.Errorf("%s (tool=%s raw=%s target=%v) was DELETED and must not have been — %s",
				c.id, c.tool, c.raw, deref(c.target), c.why)
		case !c.survives && present:
			t.Errorf("%s (tool=%s raw=%s target=%v) SURVIVED as %q and must not have — %s",
				c.id, c.tool, c.raw, deref(c.target), got, c.why)
		case c.survives && got != c.after:
			t.Errorf("%s: action_type = %q, want %q — %s", c.id, got, c.after, c.why)
		}
	}

	// ---- (b) the dependency protocol, per table.
	//
	// action_excerpts is the FTS gate: its search path never joins
	// actions, so a surviving excerpt for a deleted action is a
	// SEARCHABLE GHOST, not a harmless orphan.
	if n := countInt(ctx, t, database,
		`SELECT COUNT(*) FROM action_excerpts WHERE action_id = ?`, doomed); n != 0 {
		t.Errorf("FTS ghost: %d action_excerpts row(s) survive for deleted action %d", n, doomed)
	}
	if n := countInt(ctx, t, database,
		`SELECT COUNT(*) FROM action_excerpts WHERE action_id = ?`, survivor); n != 1 {
		t.Errorf("the survivor's excerpt was collateral damage: %d rows, want 1", n)
	}
	if n := countInt(ctx, t, database,
		`SELECT COUNT(*) FROM failure_context WHERE action_id = ?`, doomed); n != 0 {
		t.Errorf("%d failure_context row(s) survive for deleted action %d", n, doomed)
	}
	if n := countInt(ctx, t, database,
		`SELECT COUNT(*) FROM failure_context WHERE action_id = ?`, survivor); n != 1 {
		t.Errorf("the survivor's failure_context row was deleted: %d rows, want 1", n)
	}
	// The five NULLed references: the row must SURVIVE with a NULL link,
	// never be deleted (retention.go's rationale, transcribed).
	for _, c := range []struct{ table, column, key string }{
		{"file_state", "last_action_id", "file_path"},
		{"retrieval_signals", "action_id", "signal_type"},
		{"guard_events", "action_id", "rule_id"},
		{"process_runs", "action_id", "process_key"},
		{"process_events", "action_id", "process_key"},
	} {
		if n := countInt(ctx, t, database,
			`SELECT COUNT(*) FROM `+c.table+` WHERE `+c.key+` = ?`, "doomed"); n != 1 {
			t.Errorf("%s: the doomed row's dependent was DELETED (%d rows, want 1) — the protocol NULLs it", c.table, n)
		}
		if n := countInt(ctx, t, database,
			`SELECT COUNT(*) FROM `+c.table+` WHERE `+c.key+` = ? AND `+c.column+` IS NULL`, "doomed"); n != 1 {
			t.Errorf("%s.%s was not NULLed for the deleted action", c.table, c.column)
		}
		if n := countInt(ctx, t, database,
			`SELECT COUNT(*) FROM `+c.table+` WHERE `+c.key+` = ? AND `+c.column+` = ?`, "survivor", survivor); n != 1 {
			t.Errorf("%s.%s was NULLed for a SURVIVING action — the cleanup is not scoped by the candidate predicate",
				c.table, c.column)
		}
	}

	// ---- (e) the ID high-water marker.
	var marker string
	if err := database.QueryRowContext(ctx,
		`SELECT value FROM schema_meta WHERE key = 'migration_079_max_action_id'`).Scan(&marker); err != nil {
		t.Fatalf("read the high-water marker: %v", err)
	}
	if marker != strconv.FormatInt(maxBefore, 10) {
		t.Errorf("marker = %q, want %q (MAX(actions.id) BEFORE the delete — recording it after would leave "+
			"surviving pre-079 rows above the mark)", marker, strconv.FormatInt(maxBefore, 10))
	}

	// ---- idempotency: applying the same body again must change
	// nothing. The runner itself would refuse (version 79 is recorded),
	// so the body is executed directly — this grades the SQL, not the
	// bookkeeping.
	if _, err := database.ExecContext(ctx, convergenceBody); err != nil {
		t.Fatalf("second apply of 079: %v", err)
	}
	second := reasonSnapshot(ctx, t, database)
	if len(second) != len(post) {
		t.Errorf("not idempotent — %d rows after one run, %d after two", len(post), len(second))
	}
	for id, want := range post {
		if got, ok := second[id]; !ok || got != want {
			t.Errorf("%s: not idempotent — %q after one run, %q (present=%v) after two", id, want, got, ok)
		}
	}
	var marker2 string
	if err := database.QueryRowContext(ctx,
		`SELECT value FROM schema_meta WHERE key = 'migration_079_max_action_id'`).Scan(&marker2); err != nil {
		t.Fatalf("re-read the high-water marker: %v", err)
	}
	if marker2 != marker {
		t.Errorf("the marker was overwritten by a second apply: %q -> %q (ON CONFLICT DO NOTHING keeps the "+
			"FIRST apply authoritative)", marker, marker2)
	}

	// ---- the CORPUS RE-VERIFICATION backstop.
	//
	// The AST guard in internal/db/reasoninggen raises the bar against
	// accidentally re-adding an emit site, but it cannot see a raw name
	// assembled indirectly — its own doc says so. The ground truth is
	// this query, which asks the DATABASE instead of the source and is
	// therefore blind to how the row was constructed. It is the reason
	// the high-water marker exists, so it is exercised here rather than
	// left as prose. Kept in sync with
	// reasoninggen/main_test.go::corpusReVerificationSQL (the substr
	// offsets are the suffix lengths: ".reasoning" = 10, ".thinking" = 9).
	const corpusReVerification = `SELECT COUNT(*) FROM actions
	 WHERE id > (SELECT CAST(value AS INTEGER) FROM schema_meta WHERE key = 'migration_079_max_action_id')
	   AND (substr(raw_tool_name, -10) = '.reasoning' OR substr(raw_tool_name, -9) = '.thinking')`

	if n := countInt(ctx, t, database, corpusReVerification); n != 0 {
		t.Errorf("corpus re-verification = %d, want 0: every seeded reasoning row predates the marker, so "+
			"none of them may count as post-079 decay", n)
	}

	// And the negative half — a check that cannot fail is not a check.
	// A reasoning row minted AFTER 079 (an old daemon still running, or
	// an emit site the AST guard could not see) must make it non-zero.
	if _, err := database.ExecContext(ctx,
		`INSERT INTO actions (session_id, project_id, timestamp, action_type, tool,
		   raw_tool_name, target, raw_tool_output, source_event_id)
		 VALUES ('sess-codex', 1, '2026-08-01T00:00:00Z', 'task_complete', 'codex',
		   'codex.reasoning', '(encrypted reasoning, 512 bytes)', '(encrypted reasoning, 512 bytes)',
		   'post79-decay')`); err != nil {
		t.Fatalf("seed a post-079 decay row: %v", err)
	}
	if n := countInt(ctx, t, database, corpusReVerification); n != 1 {
		t.Errorf("corpus re-verification = %d after seeding one post-079 reasoning row, want 1 — the "+
			"ground-truth backstop the AST guard defers to does not actually detect decay", n)
	}
}

// deref renders a nullable seed value for an error message.
func deref(s *string) string {
	if s == nil {
		return "NULL"
	}
	return *s
}

// reasonActionIDs maps each seeded source_event_id to its rowid.
func reasonActionIDs(ctx context.Context, t *testing.T, database *sql.DB) map[string]int64 {
	t.Helper()
	rows, err := database.QueryContext(ctx,
		`SELECT source_event_id, id FROM actions WHERE source_event_id LIKE 't79-%'`)
	if err != nil {
		t.Fatalf("id query: %v", err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var key string
		var id int64
		if err := rows.Scan(&key, &id); err != nil {
			t.Fatalf("id scan: %v", err)
		}
		out[key] = id
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("id rows: %v", err)
	}
	if len(out) != len(reasonCases) {
		t.Fatalf("seeded %d rows, found %d", len(reasonCases), len(out))
	}
	return out
}

// reasonSnapshot reads every surviving seeded row's action_type, keyed
// by source_event_id. A missing key means the row was deleted.
func reasonSnapshot(ctx context.Context, t *testing.T, database *sql.DB) map[string]string {
	t.Helper()
	rows, err := database.QueryContext(ctx,
		`SELECT source_event_id, action_type FROM actions WHERE source_event_id LIKE 't79-%'`)
	if err != nil {
		t.Fatalf("snapshot query: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, at string
		if err := rows.Scan(&id, &at); err != nil {
			t.Fatalf("snapshot scan: %v", err)
		}
		out[id] = at
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("snapshot rows: %v", err)
	}
	return out
}

// seedDependents puts one row in EVERY table that references actions(id)
// without ON DELETE CASCADE, anchored on actionID and tagged with label
// so the assertions can find it. The tag doubles as each table's natural
// text key (file_path, signal_type, rule_id, process_key).
func seedDependents(ctx context.Context, t *testing.T, database *sql.DB, label string, actionID int64) {
	t.Helper()
	stmts := []struct {
		what string
		sql  string
		args []any
	}{
		{"action_excerpts", `INSERT INTO action_excerpts (action_id, tool_name, target, excerpt, error_message)
		   VALUES (?, 'codex.reasoning', 'placeholder', 'searchable body', '')`, []any{actionID}},
		{"failure_context", `INSERT INTO failure_context (action_id, session_id, project_id, timestamp,
		     command_hash, command_summary)
		   VALUES (?, 'sess-codex', 1, '2026-07-01T01:00:00Z', 'h', ?)`, []any{actionID, label}},
		{
			"file_state", `INSERT INTO file_state (project_id, file_path, content_hash, file_mtime,
		     file_size_bytes, last_action_id, last_action_type, last_seen_at)
		   VALUES (1, ?, 'h', '2026-07-01T01:00:00Z', 1, ?, 'read_file', '2026-07-01T01:00:00Z')`,
			[]any{label, actionID},
		},
		{"retrieval_signals", `INSERT INTO retrieval_signals (action_id, signal_type, signal_at)
		   VALUES (?, ?, '2026-07-01T01:00:00Z')`, []any{actionID, label}},
		{"guard_events", `INSERT INTO guard_events (ts, action_id, rule_id, chain_prev, chain_hash)
		   VALUES ('2026-07-01T01:00:00Z', ?, ?, '', 'c')`, []any{actionID, label}},
		{"process_runs", `INSERT INTO process_runs (process_key, pid, action_id, attribution_source,
		     attribution_confidence, started_at, last_seen_at)
		   VALUES (?, 1, ?, 's', 'c', '2026-07-01T01:00:00Z', '2026-07-01T01:00:00Z')`, []any{label, actionID}},
		{"process_events", `INSERT INTO process_events (process_key, timestamp, event_type, action_id)
		   VALUES (?, '2026-07-01T01:00:00Z', 'exec', ?)`, []any{label, actionID}},
	}
	for _, s := range stmts {
		if _, err := database.ExecContext(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed %s (%s): %v", s.what, label, err)
		}
	}
}

// countInt runs a scalar COUNT query.
func countInt(ctx context.Context, t *testing.T, database *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := database.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}
