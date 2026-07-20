// scrub.go — observer-org scrub-content subcommand. Operator-facing
// remediation for the v1.7.x → v1.8.0 privacy posture upgrade.
//
// Pre-v1.8.0 agents pushed raw `actions.target` (run_command bodies +
// task_complete prose), raw `actions.source_file`, raw
// sessions/api_turns/token_usage `project_root` + sessions
// `git_remote` to the org server, which persists them in plaintext
// (Issues 1+2 of `docs/teams-test-findings-2026-06-02.md`). v1.8.0
// fixes the seam going forward; this subcommand purges any already-
// landed content so the org dashboard / SQL queries / DB exfil all
// stop seeing the leaked values.
//
// Defaults to dry-run: prints per-column row counts and exits without
// modifying. --confirm must be passed to actually scrub. Always writes
// a backup copy of the DB file alongside the original (suffix
// ".scrubbed-<ts>.bak") before any UPDATE statements run, so an
// operator can roll back.

package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	orgdb "github.com/marmutapp/superbased-observer/internal/orgserver/db"
)

// scrubColumn describes one content-bearing column the scrub UPDATEs.
// The *_hash counterpart (populated by the v1.8.0 agent or by ingest's
// compute-on-the-fly fallback) stays intact, so dedup and rollup
// queries that key on the hash continue to function.
//
// blankTo selects the post-scrub value:
//   - "”" — empty string (used for NOT NULL PK columns like
//     actions.source_file and token_usage.source_file; matches what
//     v1.8.0 metadata-only agents ship, preserving dedup semantics).
//   - "NULL" — true NULL (used for everything else, the cleanest signal
//     that no raw value is held).
type scrubColumn struct {
	table   string
	column  string
	flag    string // CLI flag name controlling whether this column is scrubbed
	desc    string // one-line description for the dry-run report
	blankTo string // SQL literal to set the column to ("''" or "NULL")
}

var scrubColumns = []scrubColumn{
	{"actions", "target", "actions-target", "command bodies (run_command), assistant prose (task_complete), raw file paths", "NULL"},
	{"actions", "source_file", "actions-source-file", "raw JSONL paths (reveals home dir + project layout)", "''"},
	{"sessions", "project_root", "sessions-project-root", "raw filesystem paths", "NULL"},
	{"sessions", "git_remote", "sessions-git-remote", "raw git remote URLs (may include tokens)", "NULL"},
	{"api_turns", "project_root", "api-turns-project-root", "raw filesystem paths", "NULL"},
	{"token_usage", "project_root", "token-usage-project-root", "raw filesystem paths", "NULL"},
	{"token_usage", "source_file", "token-usage-source-file", "raw JSONL paths", "''"},
}

func newScrubContentCmd(configPath *string) *cobra.Command {
	var (
		confirm    bool
		all        bool
		skipBackup bool
		colFlags   = make(map[string]*bool, len(scrubColumns))
	)
	cmd := &cobra.Command{
		Use:   "scrub-content",
		Short: "Null content/path columns on already-landed rows (v1.8.0 privacy remediation)",
		Long: `Null content/path columns on rows that landed pre-v1.8.0 when raw values
shipped to the server (Issues 1+2 of the 2026-06-02 teams test). Per-column
*_hash counterparts (populated by v1.8.0 ingest's compute-on-the-fly fallback)
are preserved so dedup + rollup queries continue to function.

Default is dry-run: reports per-column candidate row counts and exits.
Pass --confirm to actually perform the UPDATEs. By default a backup copy
of the DB file is written to <db>.scrubbed-<utc-ts>.bak before any
modification; --skip-backup disables it (use only when an external
backup pipeline already covers the DB).

Per-column flags select what to scrub. --all is shorthand for every
column listed below. At least one --<column> flag (or --all) is required.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(*configPath)
			if err != nil {
				return err
			}
			ctx := cmd.Context()

			selected := selectedColumns(all, colFlags)
			if len(selected) == 0 {
				return errors.New("observer-org scrub-content: pick at least one --<column> flag, or --all")
			}

			db, err := orgdb.Open(ctx, orgdb.Options{Path: cfg.Server.DBPath})
			if err != nil {
				return err
			}
			defer db.Close()

			counts, err := scrubCounts(ctx, db, selected)
			if err != nil {
				return err
			}
			writeScrubReport(cmd.OutOrStdout(), counts, confirm)
			if !confirm {
				fmt.Fprintln(cmd.OutOrStdout(), "\nDRY RUN — pass --confirm to apply.")
				return nil
			}

			if !skipBackup {
				bak, err := backupDB(cfg.Server.DBPath)
				if err != nil {
					return fmt.Errorf("observer-org scrub-content: backup: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "backup written: %s\n", bak)
			}
			if err := scrubApply(ctx, db, selected); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "scrub applied.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&confirm, "confirm", false, "actually perform UPDATEs (omit for dry-run)")
	cmd.Flags().BoolVar(&all, "all", false, "scrub every content-bearing column listed below")
	cmd.Flags().BoolVar(&skipBackup, "skip-backup", false, "skip the alongside-DB backup copy (use when an external backup covers the DB)")
	for i, sc := range scrubColumns {
		f := false
		colFlags[sc.flag] = &f
		cmd.Flags().BoolVar(colFlags[sc.flag], sc.flag, false, fmt.Sprintf("scrub %s.%s — %s", sc.table, sc.column, sc.desc))
		_ = i
	}
	return cmd
}

// selectedColumns returns the subset of scrubColumns the operator asked
// for. --all overrides everything; otherwise per-flag picks.
func selectedColumns(all bool, flags map[string]*bool) []scrubColumn {
	if all {
		out := make([]scrubColumn, len(scrubColumns))
		copy(out, scrubColumns)
		return out
	}
	var out []scrubColumn
	for _, sc := range scrubColumns {
		if p, ok := flags[sc.flag]; ok && *p {
			out = append(out, sc)
		}
	}
	return out
}

// scrubCount is the per-column candidate-row count for the report.
type scrubCount struct {
	col   scrubColumn
	rows  int64
	total int64
}

func scrubCounts(ctx context.Context, db *sql.DB, selected []scrubColumn) ([]scrubCount, error) {
	out := make([]scrubCount, 0, len(selected))
	for _, sc := range selected {
		var rows, total int64
		// Rows currently holding a non-NULL, non-empty value (the
		// scrub-candidates). Table/column names are in-package literals
		// from the scrubColumns allowlist; gosec G201 is a false positive.
		q := fmt.Sprintf( //nolint:gosec // G201: in-package allowlist
			`SELECT COUNT(*) FROM %s WHERE %s IS NOT NULL AND %s != ''`, sc.table, sc.column, sc.column,
		)
		if err := db.QueryRowContext(ctx, q).Scan(&rows); err != nil {
			return nil, fmt.Errorf("observer-org scrub-content: count %s.%s: %w", sc.table, sc.column, err)
		}
		// Total rows in the table (for "X of Y" context in the report).
		qt := fmt.Sprintf( //nolint:gosec // G201: in-package allowlist
			`SELECT COUNT(*) FROM %s`, sc.table,
		)
		if err := db.QueryRowContext(ctx, qt).Scan(&total); err != nil {
			return nil, fmt.Errorf("observer-org scrub-content: total %s: %w", sc.table, err)
		}
		out = append(out, scrubCount{col: sc, rows: rows, total: total})
	}
	return out, nil
}

func scrubApply(ctx context.Context, db *sql.DB, selected []scrubColumn) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("observer-org scrub-content: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, sc := range selected {
		// blankTo is a fixed SQL literal from the in-package allowlist,
		// not user input, so direct interpolation is safe here.
		q := fmt.Sprintf( //nolint:gosec // G201: in-package allowlist + fixed SQL literal blankTo
			`UPDATE %s SET %s = %s WHERE %s IS NOT NULL AND %s != ''`,
			sc.table, sc.column, sc.blankTo, sc.column, sc.column,
		)
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("observer-org scrub-content: scrub %s.%s: %w", sc.table, sc.column, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("observer-org scrub-content: commit: %w", err)
	}
	return nil
}

// writeScrubReport prints the per-column candidate counts to w.
func writeScrubReport(w io.Writer, counts []scrubCount, confirm bool) {
	verb := "would scrub"
	if confirm {
		verb = "scrubbing"
	}
	fmt.Fprintln(w, "observer-org scrub-content:")
	for _, c := range counts {
		fmt.Fprintf(w, "  %-30s %s %d of %d rows  (%s)\n",
			fmt.Sprintf("%s.%s", c.col.table, c.col.column),
			verb, c.rows, c.total, c.col.desc)
	}
}

// backupDB copies <path> to <path>.scrubbed-<utc-ts>.bak so an operator
// can roll back a scrub. SQLite's online-backup API is not used here on
// purpose — for diff-and-restore the simple file-level copy is enough
// and survives WAL checkpoints. Caller already has the DB open via
// orgdb.Open; sqlite tolerates an external file copy while open
// (WAL-mode read-consistency).
func backupDB(path string) (string, error) {
	if path == "" {
		return "", errors.New("backupDB: db path is required")
	}
	ts := time.Now().UTC().Format("20060102T150405Z")
	dst := fmt.Sprintf("%s.scrubbed-%s.bak", path, ts)
	src, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer src.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	defer out.Close()
	if _, err := io.Copy(out, src); err != nil {
		_ = os.Remove(dst)
		return "", err
	}
	return dst, nil
}
