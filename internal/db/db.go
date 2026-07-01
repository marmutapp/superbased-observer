package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db/migrations"

	sqlite "modernc.org/sqlite" // sqlite driver registration + typed errors.
)

// Options configures the SQLite database.
type Options struct {
	// Path is the filesystem location of the SQLite database. Use ":memory:"
	// for an in-memory instance (intended for tests).
	Path string

	// BusyTimeout is the SQLite busy_timeout pragma value. Defaults to 30s.
	//
	// 30s headroom matches the migration-runner's own busy_timeout (set
	// on its pinned connection at runMigrations:148) and absorbs WAL
	// write contention when a second writer process — typically
	// `observer backfill --all` while `observer start` is also running
	// — competes for the write lock. The previous 5s default produced
	// SQLITE_BUSY on multi-thousand-row backfill batches against a busy
	// watcher.
	BusyTimeout time.Duration

	// SkipIntegrityCheck disables the `PRAGMA quick_check` probe that
	// normally runs at the end of Open. Set this from short-lived hook
	// subprocesses: the daemon already runs the probe at startup, so
	// re-running it on every hook invocation just contends with the
	// daemon's WAL holder for no diagnostic benefit. Memory note
	// `feedback_hook_db_open_no_timeout` captured the contention; this
	// option is the structural fix. Leave false for daemons, tests, and
	// `observer backfill` / `observer doctor` style commands.
	SkipIntegrityCheck bool
}

// Open opens (or creates) the SQLite database at opts.Path, enables WAL mode,
// applies any pending migrations, and runs a quick integrity check. The
// returned *sql.DB is safe for concurrent use.
//
// Concurrency note: every transaction acquires the SQLite write lock
// upfront via _txlock=immediate. The default BEGIN DEFERRED behavior
// would take a read lock at BeginTx and try to upgrade to a write lock
// at the first write — when two writers race that upgrade, one gets
// SQLITE_BUSY immediately (busy_timeout doesn't kick in on
// upgrade-deadlocks). BEGIN IMMEDIATE serializes writers through the
// file lock so busy_timeout's exponential backoff handles contention
// properly. All four BeginTx callers in this codebase
// (store.InsertActions, store.InsertTokenEvents, retention.deleteActionsOlder,
// indexing.EmbedBatch) are write-only, so the IMMEDIATE upgrade is
// always correct — no read-only tx is being unnecessarily serialized.
func Open(ctx context.Context, opts Options) (*sql.DB, error) {
	if opts.Path == "" {
		return nil, errors.New("db.Open: Path is required")
	}
	busy := opts.BusyTimeout
	if busy <= 0 {
		busy = 30 * time.Second
	}

	dsn := opts.Path
	if opts.Path != ":memory:" {
		dsn = fmt.Sprintf("file:%s?_pragma=busy_timeout(%d)&_pragma=foreign_keys(1)&_txlock=immediate",
			opts.Path, busy.Milliseconds())
	}

	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("db.Open: sql.Open: %w", err)
	}
	if err := database.PingContext(ctx); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("db.Open: ping: %w", err)
	}

	if err := applyPragmas(ctx, database, opts.Path); err != nil {
		_ = database.Close()
		return nil, err
	}
	if err := runMigrations(ctx, database); err != nil {
		_ = database.Close()
		return nil, err
	}
	// Migration 034 (v1.8.0) added denormalized SHA256 hash columns for
	// org-push privacy. The schema change is a NOT NULL DEFAULT '' column
	// add so existing rows pass the constraint immediately, but the org
	// push seam reads the hash columns — leaving them empty would ship
	// degenerate empty hashes. Backfill them here in Go (modernc/sqlite
	// has no built-in sha256 SQL function). Idempotent: only updates rows
	// where the hash column is empty AND the source value is non-empty.
	if err := backfillPathHashes(ctx, database); err != nil {
		_ = database.Close()
		return nil, err
	}
	if !opts.SkipIntegrityCheck {
		if err := integrityCheck(ctx, database); err != nil {
			_ = database.Close()
			return nil, err
		}
	}
	return database, nil
}

func applyPragmas(ctx context.Context, db *sql.DB, path string) error {
	if path != ":memory:" {
		// WAL mode is incompatible with in-memory databases.
		//
		// The conversion out of rollback-journal mode (first-ever open of
		// a fresh DB) needs an EXCLUSIVE lock. Among N simultaneous first
		// openers — `observer start` bringing up proxy + watcher +
		// dashboard against a new DB, or a bridged hook racing the
		// daemon's first start — two connections can each hold a SHARED
		// lock while both requesting the upgrade, and SQLite reports
		// SQLITE_BUSY immediately WITHOUT invoking the busy handler (the
		// same upgrade-deadlock class BEGIN IMMEDIATE solves for
		// transactions; see Open's doc comment). Retry with backoff: the
		// winner converts in milliseconds, and every later attempt sees
		// journal_mode already WAL, which no longer needs the lock.
		if err := execRetryBusy(ctx, db, "PRAGMA journal_mode = WAL", 5*time.Second); err != nil {
			return err
		}
	}
	pragmas := []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA synchronous = NORMAL",
		"PRAGMA temp_store = MEMORY",
	}
	for _, p := range pragmas {
		if _, err := db.ExecContext(ctx, p); err != nil {
			return fmt.Errorf("db.Open: %s: %w", p, err)
		}
	}
	return nil
}

// execRetryBusy executes stmt, retrying on SQLITE_BUSY until window
// elapses. Only lock contention is retried — every other error (and a
// still-busy statement past the deadline) is returned wrapped.
func execRetryBusy(ctx context.Context, db *sql.DB, stmt string, window time.Duration) error {
	deadline := time.Now().Add(window)
	for {
		_, err := db.ExecContext(ctx, stmt)
		if err == nil {
			return nil
		}
		if !isBusy(err) || ctx.Err() != nil || time.Now().After(deadline) {
			return fmt.Errorf("db.Open: %s: %w", stmt, err)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// isBusy reports whether err is SQLITE_BUSY (primary code 5, including
// extended busy codes such as SQLITE_BUSY_RECOVERY, whose low byte is 5).
func isBusy(err error) bool {
	var se *sqlite.Error
	if errors.As(err, &se) {
		return se.Code()&0xff == 5
	}
	return false
}

func integrityCheck(ctx context.Context, db *sql.DB) error {
	var result string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result); err != nil {
		return fmt.Errorf("db.Open: quick_check: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("db.Open: integrity check failed: %s", result)
	}
	return nil
}

// runMigrations applies any .sql files under the embedded migrations FS
// that have not already been applied, recording progress in schema_meta.
//
// Concurrency contract: the entire pending-migration batch runs inside
// a single BEGIN IMMEDIATE transaction on a dedicated connection. SQLite
// serializes BEGIN IMMEDIATE across processes via file locks, and PRAGMA
// busy_timeout makes a contending caller wait rather than fail
// immediately. This makes parallel daemon startups (watch + dashboard +
// proxy as three separate processes opening the same DB file) race-free:
// whoever wins the lock first applies pending migrations and commits;
// later contenders re-read schema_meta INSIDE their own lock, see the
// updated applied version, and skip everything. Pre-fix, racing daemons
// would each read applied=N, each try to apply migration N+1, and
// non-idempotent statements (ALTER TABLE ADD COLUMN) would error with
// "duplicate column name" on whichever lost the race.
//
// Tradeoff vs the previous per-migration tx approach: if migration K
// succeeds but K+1 fails in the same batch, K is rolled back too. That's
// preferable to partial application — the next run re-attempts both,
// not just K+1, so a botched migration script can't leave the schema
// in a half-state.
func runMigrations(ctx context.Context, db *sql.DB) error {
	entries, err := readMigrationEntries()
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	maxVersion := entries[len(entries)-1].version

	// Fast path: a lock-free read of the applied version (a plain SELECT;
	// in WAL mode it never blocks on a writer). When the schema is already
	// current there is provably nothing to apply, so we skip the BEGIN
	// IMMEDIATE write lock below entirely. This is the overwhelmingly
	// common case for short-lived CLI/hook db.Open calls (observer index,
	// the hook subprocesses, doctor, guard, …): without it, every such
	// open contends for the write lock the running daemon holds and burns
	// up to busy_timeout (30s) on SQLITE_BUSY backoff just to confirm
	// there is no migration to run. A missing schema_meta (fresh DB) or
	// any read error makes currentVersion return a non-nil error, and a
	// stale lower read returns applied < maxVersion — both fall through to
	// the authoritative locked path, so the concurrent-first-migration
	// race the lock guards against is unaffected. We only ever skip the
	// lock when there is nothing to do.
	if applied, err := currentVersion(ctx, db); err == nil && applied >= maxVersion {
		return nil
	}

	// schema_meta must exist before we read it. CREATE TABLE IF NOT
	// EXISTS is idempotent so this is safe to run from multiple
	// processes concurrently and doesn't need the migration lock.
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_meta (
		key TEXT PRIMARY KEY, value TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("db.runMigrations: bootstrap schema_meta: %w", err)
	}

	// Pin a single connection so BEGIN IMMEDIATE / SQL / COMMIT all run
	// against the same SQLite session — database/sql.DB.BeginTx may
	// return a tx on any connection in the pool by default.
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("db.runMigrations: acquire connection: %w", err)
	}
	defer conn.Close()

	// 30s is generous; well-formed migrations apply in milliseconds
	// even on multi-GB DBs. The wait is for OTHER daemons holding the
	// lock during their own migration pass.
	if _, err := conn.ExecContext(ctx, "PRAGMA busy_timeout = 30000"); err != nil {
		return fmt.Errorf("db.runMigrations: set busy_timeout: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("db.runMigrations: acquire migration lock: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			// Best-effort rollback if the function returns an error
			// before COMMIT. Errors here are intentionally swallowed —
			// we're already on the error path; surfacing a rollback
			// failure would mask the real cause.
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()

	// Re-read applied INSIDE the lock so we observe any commits that
	// landed while we were waiting on BEGIN IMMEDIATE.
	var applied int
	var s sql.NullString
	row := conn.QueryRowContext(ctx, `SELECT value FROM schema_meta WHERE key = 'version'`)
	switch err := row.Scan(&s); {
	case errors.Is(err, sql.ErrNoRows):
		applied = 0
	case err != nil:
		return fmt.Errorf("db.runMigrations: read applied version: %w", err)
	default:
		if s.Valid {
			applied, err = strconv.Atoi(s.String)
			if err != nil {
				return fmt.Errorf("db.runMigrations: parse applied version %q: %w", s.String, err)
			}
		}
	}

	for _, e := range entries {
		if e.version <= applied {
			continue
		}
		body, readErr := fs.ReadFile(migrations.Files, e.filename)
		if readErr != nil {
			return fmt.Errorf("db.runMigrations: read %s: %w", e.filename, readErr)
		}
		if _, err := conn.ExecContext(ctx, string(body)); err != nil {
			return fmt.Errorf("db.runMigrations: exec %s: %w", e.filename, err)
		}
		if _, err := conn.ExecContext(
			ctx,
			`INSERT INTO schema_meta(key, value) VALUES ('version', ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
			strconv.Itoa(e.version),
		); err != nil {
			return fmt.Errorf("db.runMigrations: record version %d: %w", e.version, err)
		}
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("db.runMigrations: commit: %w", err)
	}
	committed = true
	return nil
}

type migrationEntry struct {
	version  int
	filename string
}

func readMigrationEntries() ([]migrationEntry, error) {
	dirEntries, err := fs.ReadDir(migrations.Files, ".")
	if err != nil {
		return nil, fmt.Errorf("db.readMigrationEntries: %w", err)
	}
	var entries []migrationEntry
	for _, de := range dirEntries {
		if de.IsDir() {
			continue
		}
		name := de.Name()
		if filepath.Ext(name) != ".sql" {
			continue
		}
		prefix := strings.SplitN(name, "_", 2)[0]
		v, err := strconv.Atoi(prefix)
		if err != nil {
			return nil, fmt.Errorf("db.readMigrationEntries: unparseable migration %q: %w", name, err)
		}
		entries = append(entries, migrationEntry{version: v, filename: name})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].version < entries[j].version })
	return entries, nil
}

func currentVersion(ctx context.Context, db *sql.DB) (int, error) {
	var raw string
	err := db.QueryRowContext(ctx, "SELECT value FROM schema_meta WHERE key = 'version'").Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("db.currentVersion: %w", err)
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("db.currentVersion: parse %q: %w", raw, err)
	}
	return v, nil
}

// Version reports the highest applied migration version. Returns 0 on a
// fresh database.
func Version(ctx context.Context, db *sql.DB) (int, error) {
	return currentVersion(ctx, db)
}

// backfillPathHashes populates the sha256-hex hash columns introduced by
// migration 034 (v1.8.0 org-push privacy fix). The migration adds the
// columns with NOT NULL DEFAULT ” so existing rows pass the constraint,
// but the org-push seam at internal/store/orgpush.go reads the hash
// columns and would otherwise ship empty strings for legacy rows.
//
// Idempotent: the WHERE clause skips already-populated rows, so this is
// cheap on every-startup re-run. Batched at 5000 rows per UPDATE to keep
// the in-Go SHA loop bounded and avoid pathological memory spikes on
// hosts with months of corpus.
//
// Scoping rationale: each table has a unique (table, source-column,
// hash-column) tuple; we walk all four. The SHA256 input is the raw
// source string (UTF-8 bytes); empty source → empty hash (left as the
// DEFAULT).
func backfillPathHashes(ctx context.Context, db *sql.DB) error {
	jobs := []struct {
		name        string
		idCol       string
		sourceCol   string
		hashCol     string
		table       string
		extraFilter string // optional WHERE addition (e.g. NULL handling)
	}{
		{"projects.root_path_hash", "id", "root_path", "root_path_hash", "projects", ""},
		{"projects.git_remote_hash", "id", "git_remote", "git_remote_hash", "projects", "AND git_remote IS NOT NULL AND git_remote != ''"},
		{"actions.source_file_hash", "id", "source_file", "source_file_hash", "actions", "AND source_file IS NOT NULL AND source_file != ''"},
		{"token_usage.source_file_hash", "rowid", "source_file", "source_file_hash", "token_usage", "AND source_file IS NOT NULL AND source_file != ''"},
	}
	const batchSize = 5000
	for _, j := range jobs {
		for {
			// All format args are in-package literals from the `jobs`
			// allowlist above, not user input — gosec G201 is a false
			// positive for table/column substitution.
			query := fmt.Sprintf( //nolint:gosec // G201: table/column names from in-package allowlist
				`SELECT %s, %s FROM %s WHERE %s = '' %s LIMIT %d`,
				j.idCol, j.sourceCol, j.table, j.hashCol, j.extraFilter, batchSize,
			)
			rows, err := db.QueryContext(ctx, query)
			if err != nil {
				return fmt.Errorf("db.backfillPathHashes: %s scan: %w", j.name, err)
			}
			type pair struct {
				id   int64
				hash string
			}
			var batch []pair
			for rows.Next() {
				var id int64
				var src string
				if err := rows.Scan(&id, &src); err != nil {
					_ = rows.Close()
					return fmt.Errorf("db.backfillPathHashes: %s row scan: %w", j.name, err)
				}
				if src == "" {
					continue
				}
				sum := sha256.Sum256([]byte(src))
				batch = append(batch, pair{id: id, hash: hex.EncodeToString(sum[:])})
			}
			if err := rows.Close(); err != nil {
				return fmt.Errorf("db.backfillPathHashes: %s rows.Close: %w", j.name, err)
			}
			if len(batch) == 0 {
				break
			}
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				return fmt.Errorf("db.backfillPathHashes: %s begin: %w", j.name, err)
			}
			updateSQL := fmt.Sprintf( //nolint:gosec // G201: table/column names from in-package allowlist
				`UPDATE %s SET %s = ? WHERE %s = ?`, j.table, j.hashCol, j.idCol,
			)
			stmt, err := tx.PrepareContext(ctx, updateSQL)
			if err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("db.backfillPathHashes: %s prepare: %w", j.name, err)
			}
			for _, p := range batch {
				if _, err := stmt.ExecContext(ctx, p.hash, p.id); err != nil {
					_ = stmt.Close()
					_ = tx.Rollback()
					return fmt.Errorf("db.backfillPathHashes: %s update: %w", j.name, err)
				}
			}
			_ = stmt.Close()
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("db.backfillPathHashes: %s commit: %w", j.name, err)
			}
			if len(batch) < batchSize {
				break
			}
		}
	}
	return nil
}
