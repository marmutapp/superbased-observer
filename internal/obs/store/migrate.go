package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

// migrationFiles embeds the obs subsystem's OWN migrations. Decision D3
// (plan §0.1): obs applies these itself, namespaced under obs_schema_meta,
// only when the subsystem is enabled — it never touches the host migrator
// (internal/db) or the host's schema_meta. This is what makes "disabled ⇒ no
// obs_* tables ever created" true, and keeps the host migrator edit-free.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrate brings the obs schema up to date on conn. It is idempotent and safe
// to call on every enabled startup: it bootstraps obs_schema_meta, reads the
// last applied version, and applies each higher-numbered migration in one
// transaction, recording progress. Filenames are NNNN_name.sql; the leading
// integer is the version. Distinct from the host migrator by design.
func migrate(ctx context.Context, conn *sql.DB) error {
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS obs_schema_meta (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("obs/store.migrate: bootstrap obs_schema_meta: %w", err)
	}

	current, err := currentVersion(ctx, conn)
	if err != nil {
		return err
	}

	pending, err := loadMigrations()
	if err != nil {
		return err
	}

	for _, m := range pending {
		if m.version <= current {
			continue
		}
		if err := applyOne(ctx, conn, m); err != nil {
			return err
		}
	}
	return nil
}

type migration struct {
	version int
	name    string
	body    string
}

// currentVersion reads obs_schema_meta's version, defaulting to 0 when unset.
func currentVersion(ctx context.Context, conn *sql.DB) (int, error) {
	var raw string
	err := conn.QueryRowContext(ctx, `SELECT value FROM obs_schema_meta WHERE key = 'version'`).Scan(&raw)
	switch {
	case err == sql.ErrNoRows:
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("obs/store.migrate: read version: %w", err)
	}
	v, convErr := strconv.Atoi(strings.TrimSpace(raw))
	if convErr != nil {
		return 0, fmt.Errorf("obs/store.migrate: parse version %q: %w", raw, convErr)
	}
	return v, nil
}

// loadMigrations reads and parses every embedded migration, sorted by version.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("obs/store.migrate: read dir: %w", err)
	}
	out := make([]migration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		ver, err := versionFromName(e.Name())
		if err != nil {
			return nil, err
		}
		body, err := fs.ReadFile(migrationFiles, "migrations/"+e.Name())
		if err != nil {
			return nil, fmt.Errorf("obs/store.migrate: read %s: %w", e.Name(), err)
		}
		out = append(out, migration{version: ver, name: e.Name(), body: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// versionFromName extracts the leading integer from an NNNN_name.sql filename.
func versionFromName(name string) (int, error) {
	idx := strings.IndexByte(name, '_')
	if idx <= 0 {
		return 0, fmt.Errorf("obs/store.migrate: bad migration name %q (want NNNN_name.sql)", name)
	}
	v, err := strconv.Atoi(name[:idx])
	if err != nil {
		return 0, fmt.Errorf("obs/store.migrate: bad version in %q: %w", name, err)
	}
	return v, nil
}

// applyOne runs a migration body and records its version in one transaction.
func applyOne(ctx context.Context, conn *sql.DB, m migration) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("obs/store.migrate: begin %s: %w", m.name, err)
	}
	if _, err := tx.ExecContext(ctx, m.body); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("obs/store.migrate: apply %s: %w", m.name, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO obs_schema_meta(key, value) VALUES ('version', ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		strconv.Itoa(m.version)); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("obs/store.migrate: record %s: %w", m.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("obs/store.migrate: commit %s: %w", m.name, err)
	}
	return nil
}
