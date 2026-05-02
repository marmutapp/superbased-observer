package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db/migrations"

	_ "modernc.org/sqlite" // sqlite driver registration.
)

// Options configures the SQLite database.
type Options struct {
	// Path is the filesystem location of the SQLite database. Use ":memory:"
	// for an in-memory instance (intended for tests).
	Path string

	// BusyTimeout is the SQLite busy_timeout pragma value. Defaults to 5s.
	BusyTimeout time.Duration
}

// Open opens (or creates) the SQLite database at opts.Path, enables WAL mode,
// applies any pending migrations, and runs a quick integrity check. The
// returned *sql.DB is safe for concurrent use.
func Open(ctx context.Context, opts Options) (*sql.DB, error) {
	if opts.Path == "" {
		return nil, errors.New("db.Open: Path is required")
	}
	busy := opts.BusyTimeout
	if busy <= 0 {
		busy = 5 * time.Second
	}

	dsn := opts.Path
	if opts.Path != ":memory:" {
		dsn = fmt.Sprintf("file:%s?_pragma=busy_timeout(%d)&_pragma=foreign_keys(1)",
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
	if err := integrityCheck(ctx, database); err != nil {
		_ = database.Close()
		return nil, err
	}
	return database, nil
}

func applyPragmas(ctx context.Context, db *sql.DB, path string) error {
	pragmas := []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA synchronous = NORMAL",
		"PRAGMA temp_store = MEMORY",
	}
	if path != ":memory:" {
		// WAL mode is incompatible with in-memory databases.
		pragmas = append([]string{"PRAGMA journal_mode = WAL"}, pragmas...)
	}
	for _, p := range pragmas {
		if _, err := db.ExecContext(ctx, p); err != nil {
			return fmt.Errorf("db.Open: %s: %w", p, err)
		}
	}
	return nil
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
	case err == sql.ErrNoRows:
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
		if _, err := conn.ExecContext(ctx,
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
