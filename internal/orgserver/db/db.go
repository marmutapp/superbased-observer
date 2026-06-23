package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/orgserver/db/migrations"

	_ "modernc.org/sqlite" // pure-Go sqlite driver registration (no CGO).
)

// Options configures the server SQLite database.
type Options struct {
	// Path is the filesystem location of the server DB. Use ":memory:" for
	// an in-memory instance (tests only).
	Path string

	// BusyTimeout is the SQLite busy_timeout pragma. Defaults to 30s, matching
	// the agent DB — generous headroom for the migration lock under
	// concurrent opens.
	BusyTimeout time.Duration
}

// Open opens (or creates) the server database, enables WAL mode, applies
// pending migrations, and runs a quick integrity check. The returned *sql.DB
// is safe for concurrent use.
//
// As in the agent DB, every transaction takes the write lock upfront via
// _txlock=immediate so busy_timeout's backoff (not an upgrade-deadlock)
// handles writer contention.
func Open(ctx context.Context, opts Options) (*sql.DB, error) {
	if opts.Path == "" {
		return nil, errors.New("orgserver/db.Open: Path is required")
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
		return nil, fmt.Errorf("orgserver/db.Open: sql.Open: %w", err)
	}
	if err := database.PingContext(ctx); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("orgserver/db.Open: ping: %w", err)
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
		pragmas = append([]string{"PRAGMA journal_mode = WAL"}, pragmas...)
	}
	for _, p := range pragmas {
		if _, err := db.ExecContext(ctx, p); err != nil {
			return fmt.Errorf("orgserver/db.Open: %s: %w", p, err)
		}
	}
	return nil
}

func integrityCheck(ctx context.Context, db *sql.DB) error {
	var result string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result); err != nil {
		return fmt.Errorf("orgserver/db.Open: quick_check: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("orgserver/db.Open: integrity check failed: %s", result)
	}
	return nil
}

// runMigrations applies pending migrations inside a single BEGIN IMMEDIATE
// transaction on a pinned connection — identical concurrency contract to the
// agent's internal/db runner (see its comment for the full rationale).
func runMigrations(ctx context.Context, db *sql.DB) error {
	entries, err := readMigrationEntries()
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_meta (
		key TEXT PRIMARY KEY, value TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("orgserver/db.runMigrations: bootstrap schema_meta: %w", err)
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("orgserver/db.runMigrations: acquire connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "PRAGMA busy_timeout = 30000"); err != nil {
		return fmt.Errorf("orgserver/db.runMigrations: set busy_timeout: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("orgserver/db.runMigrations: acquire migration lock: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()

	var applied int
	var s sql.NullString
	row := conn.QueryRowContext(ctx, `SELECT value FROM schema_meta WHERE key = 'version'`)
	switch err := row.Scan(&s); {
	case errors.Is(err, sql.ErrNoRows):
		applied = 0
	case err != nil:
		return fmt.Errorf("orgserver/db.runMigrations: read applied version: %w", err)
	default:
		if s.Valid {
			applied, err = strconv.Atoi(s.String)
			if err != nil {
				return fmt.Errorf("orgserver/db.runMigrations: parse applied version %q: %w", s.String, err)
			}
		}
	}

	for _, e := range entries {
		if e.version <= applied {
			continue
		}
		body, readErr := fs.ReadFile(migrations.Files, e.filename)
		if readErr != nil {
			return fmt.Errorf("orgserver/db.runMigrations: read %s: %w", e.filename, readErr)
		}
		if _, err := conn.ExecContext(ctx, string(body)); err != nil {
			return fmt.Errorf("orgserver/db.runMigrations: exec %s: %w", e.filename, err)
		}
		if _, err := conn.ExecContext(
			ctx,
			`INSERT INTO schema_meta(key, value) VALUES ('version', ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
			strconv.Itoa(e.version),
		); err != nil {
			return fmt.Errorf("orgserver/db.runMigrations: record version %d: %w", e.version, err)
		}
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("orgserver/db.runMigrations: commit: %w", err)
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
		return nil, fmt.Errorf("orgserver/db.readMigrationEntries: %w", err)
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
			return nil, fmt.Errorf("orgserver/db.readMigrationEntries: unparseable migration %q: %w", name, err)
		}
		entries = append(entries, migrationEntry{version: v, filename: name})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].version < entries[j].version })
	return entries, nil
}

// Version reports the highest applied migration version (0 on a fresh DB).
func Version(ctx context.Context, db *sql.DB) (int, error) {
	var raw string
	err := db.QueryRowContext(ctx, "SELECT value FROM schema_meta WHERE key = 'version'").Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("orgserver/db.Version: %w", err)
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("orgserver/db.Version: parse %q: %w", raw, err)
	}
	return v, nil
}

// Org identifies the single organisation a server deployment represents.
type Org struct {
	OrgID       string
	OrgName     string
	ExternalURL string
}

// EnsureOrg returns the org singleton, creating it on first call. The org_id
// is generated once (16 random bytes, hex) and persisted, so it is stable
// across restarts and independent of externalURL. org_name defaults to the
// external URL's host. externalURL is refreshed on every call so a
// reconfigured URL is reflected without disturbing org_id.
func EnsureOrg(ctx context.Context, db *sql.DB, externalURL string) (Org, error) {
	var o Org
	err := db.QueryRowContext(ctx,
		`SELECT org_id, org_name, external_url FROM org WHERE id = 1`).
		Scan(&o.OrgID, &o.OrgName, &o.ExternalURL)
	switch {
	case err == nil:
		if externalURL != "" && externalURL != o.ExternalURL {
			if _, uerr := db.ExecContext(ctx,
				`UPDATE org SET external_url = ? WHERE id = 1`, externalURL); uerr != nil {
				return Org{}, fmt.Errorf("orgserver/db.EnsureOrg: refresh external_url: %w", uerr)
			}
			o.ExternalURL = externalURL
		}
		return o, nil
	case errors.Is(err, sql.ErrNoRows):
		// fall through to create.
	default:
		return Org{}, fmt.Errorf("orgserver/db.EnsureOrg: read org: %w", err)
	}

	orgID, err := randomID()
	if err != nil {
		return Org{}, fmt.Errorf("orgserver/db.EnsureOrg: generate org_id: %w", err)
	}
	o = Org{OrgID: orgID, OrgName: hostOf(externalURL), ExternalURL: externalURL}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO org (id, org_id, org_name, external_url, created_at)
		 VALUES (1, ?, ?, ?, ?)
		 ON CONFLICT(id) DO NOTHING`,
		o.OrgID, o.OrgName, o.ExternalURL, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return Org{}, fmt.Errorf("orgserver/db.EnsureOrg: insert org: %w", err)
	}
	// Re-read in case a concurrent boot won the INSERT race.
	if err := db.QueryRowContext(ctx,
		`SELECT org_id, org_name, external_url FROM org WHERE id = 1`).
		Scan(&o.OrgID, &o.OrgName, &o.ExternalURL); err != nil {
		return Org{}, fmt.Errorf("orgserver/db.EnsureOrg: reread org: %w", err)
	}
	return o, nil
}

func randomID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func hostOf(rawURL string) string {
	if rawURL == "" {
		return "organisation"
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return rawURL
	}
	return u.Host
}
