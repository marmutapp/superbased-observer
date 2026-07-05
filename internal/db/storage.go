package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Storage manager primitives (usability arc P6.8). This file is the
// one owner of SQLite maintenance operations — the `observer db` CLI
// and the dashboard's Storage section both consume these instead of
// re-writing the SQL.

// StorageTable is one table's on-disk footprint. Bytes aggregates the
// table's own b-tree plus its indexes and (for FTS5 virtual tables)
// shadow tables, so the list reads like the schema the operator
// knows, not SQLite internals. Rows is -1 when counting failed.
type StorageTable struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
	Rows  int64  `json:"rows"`
}

// StorageReport is the per-table breakdown plus the whole-file
// numbers the vacuum decision needs.
type StorageReport struct {
	PageSize      int64 `json:"page_size"`
	PageCount     int64 `json:"page_count"`
	FreelistPages int64 `json:"freelist_pages"`
	TotalBytes    int64 `json:"total_bytes"`
	// ReclaimableBytes estimates what VACUUM would free right now
	// (freelist pages × page size). Fragmentation inside live pages
	// isn't counted, so a vacuum can reclaim somewhat more.
	ReclaimableBytes int64          `json:"reclaimable_bytes"`
	Tables           []StorageTable `json:"tables"`
}

// StorageStats walks dbstat for per-b-tree sizes and aggregates them
// per user-visible table (indexes and FTS5 shadow tables fold into
// their owner). NOTE: dbstat reads every page of every b-tree — on a
// multi-hundred-MB database this is a full file scan. Call on demand,
// never on a poll loop.
func StorageStats(ctx context.Context, database *sql.DB) (StorageReport, error) {
	var rep StorageReport
	for pragma, dst := range map[string]*int64{
		"page_size":      &rep.PageSize,
		"page_count":     &rep.PageCount,
		"freelist_count": &rep.FreelistPages,
	} {
		if err := database.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(dst); err != nil {
			return rep, fmt.Errorf("db.StorageStats: pragma %s: %w", pragma, err)
		}
	}
	rep.TotalBytes = rep.PageSize * rep.PageCount
	rep.ReclaimableBytes = rep.PageSize * rep.FreelistPages

	owners, ftsTables, err := schemaOwners(ctx, database)
	if err != nil {
		return rep, err
	}

	// Per-b-tree page sizes. Every b-tree (table, index, shadow table)
	// appears under its own name; fold into the owning table.
	bytesByOwner := map[string]int64{}
	rows, err := database.QueryContext(ctx, `SELECT name, SUM(pgsize) FROM dbstat GROUP BY name`)
	if err != nil {
		return rep, fmt.Errorf("db.StorageStats: dbstat: %w", err)
	}
	for rows.Next() {
		var name string
		var size int64
		if err := rows.Scan(&name, &size); err != nil {
			rows.Close()
			return rep, fmt.Errorf("db.StorageStats: dbstat scan: %w", err)
		}
		bytesByOwner[resolveOwner(name, owners, ftsTables)] += size
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return rep, fmt.Errorf("db.StorageStats: dbstat rows: %w", err)
	}
	rows.Close()

	for owner, size := range bytesByOwner {
		t := StorageTable{Name: owner, Bytes: size, Rows: -1}
		if owner != sqliteInternalGroup {
			// COUNT works for both regular and FTS5 virtual tables.
			// Identifiers come from sqlite_schema, not user input; quote
			// defensively anyway.
			var n int64
			if err := database.QueryRowContext(ctx,
				fmt.Sprintf("SELECT COUNT(*) FROM %q", owner)).Scan(&n); err == nil {
				t.Rows = n
			}
		}
		rep.Tables = append(rep.Tables, t)
	}
	sort.Slice(rep.Tables, func(i, j int) bool {
		if rep.Tables[i].Bytes != rep.Tables[j].Bytes {
			return rep.Tables[i].Bytes > rep.Tables[j].Bytes
		}
		return rep.Tables[i].Name < rep.Tables[j].Name
	})
	return rep, nil
}

// sqliteInternalGroup is the display bucket for sqlite_* b-trees
// (schema table, sequence table, auto-indexes without a resolvable
// owner).
const sqliteInternalGroup = "(sqlite internals)"

// schemaOwners maps every named schema object to the table it belongs
// to, and reports which tables are FTS5 virtual tables (their shadow
// tables fold into them by name prefix).
func schemaOwners(ctx context.Context, database *sql.DB) (owners map[string]string, ftsTables []string, err error) {
	owners = map[string]string{}
	rows, err := database.QueryContext(ctx,
		`SELECT name, tbl_name, type, COALESCE(sql, '') FROM sqlite_schema`)
	if err != nil {
		return nil, nil, fmt.Errorf("db.StorageStats: schema: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, tbl, typ, sqlText string
		if err := rows.Scan(&name, &tbl, &typ, &sqlText); err != nil {
			return nil, nil, fmt.Errorf("db.StorageStats: schema scan: %w", err)
		}
		owners[name] = tbl
		if typ == "table" && strings.Contains(strings.ToLower(sqlText), "using fts5") {
			ftsTables = append(ftsTables, name)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("db.StorageStats: schema rows: %w", err)
	}
	// Longest prefix first so e.g. "a_b" wins over "a" for "a_b_data".
	sort.Slice(ftsTables, func(i, j int) bool { return len(ftsTables[i]) > len(ftsTables[j]) })
	return owners, ftsTables, nil
}

// resolveOwner folds a dbstat b-tree name into its user-visible
// table: indexes via sqlite_schema's tbl_name, FTS5 shadow tables via
// the virtual table's name prefix, sqlite_* internals into one
// bucket.
func resolveOwner(name string, owners map[string]string, ftsTables []string) string {
	if tbl, ok := owners[name]; ok && tbl != "" && tbl != name {
		name = tbl
	}
	for _, fts := range ftsTables {
		if strings.HasPrefix(name, fts+"_") {
			return fts
		}
	}
	if strings.HasPrefix(name, "sqlite_") {
		return sqliteInternalGroup
	}
	return name
}

// Vacuum rebuilds the database file, returning the freed bytes
// (before − after, from page accounting). VACUUM needs the write lock
// and temporarily doubles disk usage; run it at a quiet moment.
func Vacuum(ctx context.Context, database *sql.DB) (freedBytes int64, err error) {
	before, err := fileBytes(ctx, database)
	if err != nil {
		return 0, err
	}
	if _, err := database.ExecContext(ctx, "VACUUM"); err != nil {
		return 0, fmt.Errorf("db.Vacuum: %w", err)
	}
	after, err := fileBytes(ctx, database)
	if err != nil {
		return 0, err
	}
	return before - after, nil
}

// BackupInto writes a consistent snapshot of the live database to
// dest via VACUUM INTO — online-safe under WAL (readers and writers
// keep going; the snapshot is transactionally consistent). The
// destination must not exist; parent directories are created.
func BackupInto(ctx context.Context, database *sql.DB, dest string) error {
	if dest == "" {
		return fmt.Errorf("db.BackupInto: destination path required")
	}
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("db.BackupInto: destination %s already exists", dest)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("db.BackupInto: create backup dir: %w", err)
	}
	if _, err := database.ExecContext(ctx, "VACUUM INTO ?", dest); err != nil {
		return fmt.Errorf("db.BackupInto: %w", err)
	}
	return nil
}

func fileBytes(ctx context.Context, database *sql.DB) (int64, error) {
	var pageSize, pageCount int64
	if err := database.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return 0, fmt.Errorf("db: page_size: %w", err)
	}
	if err := database.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pageCount); err != nil {
		return 0, fmt.Errorf("db: page_count: %w", err)
	}
	return pageSize * pageCount, nil
}
