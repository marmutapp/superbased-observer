package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/compression/indexing"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// newDBCmd implements `observer db` — database maintenance (usability
// arc P6.8): per-table storage stats, vacuum, and one-click backup.
// The dashboard's Storage section drives `db vacuum` / `db backup`
// through the shared job registry, so the CLI and the web button are
// the same code path.
func newDBCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "db",
		Short: "Database maintenance: storage stats, vacuum, backup, import",
	}
	cmd.AddCommand(newDBStatsCmd(), newDBVacuumCmd(), newDBBackupCmd(), newDBImportCmd())
	return cmd
}

func newDBStatsCmd() *cobra.Command {
	var (
		configPath string
		jsonOut    bool
	)
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Per-table size breakdown (reads every page — a moment on large DBs)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, database, cleanup, err := loadConfigAndDB(cmd.Context(), configPath)
			if err != nil {
				return err
			}
			defer cleanup()
			rep, err := db.StorageStats(cmd.Context(), database)
			if err != nil {
				return err
			}
			if jsonOut {
				body, _ := json.MarshalIndent(rep, "", "  ")
				fmt.Fprintln(cmd.OutOrStdout(), string(body))
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s — %s total, ~%s reclaimable by vacuum\n\n",
				cfg.Observer.DBPath, fmtBytesIEC(rep.TotalBytes), fmtBytesIEC(rep.ReclaimableBytes))
			fmt.Fprintf(cmd.OutOrStdout(), "%-28s %12s %12s\n", "TABLE", "SIZE", "ROWS")
			for _, t := range rep.Tables {
				rows := "-"
				if t.Rows >= 0 {
					rows = fmt.Sprintf("%d", t.Rows)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%-28s %12s %12s\n", t.Name, fmtBytesIEC(t.Bytes), rows)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit JSON")
	return cmd
}

func newDBVacuumCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "vacuum",
		Short: "Rebuild the database file to reclaim free pages (needs the write lock; pick a quiet moment)",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, database, cleanup, err := loadConfigAndDB(cmd.Context(), configPath)
			if err != nil {
				return err
			}
			defer cleanup()
			fmt.Fprintln(cmd.OutOrStdout(), "vacuum: rebuilding — this can take a while on large databases…")
			freed, err := db.Vacuum(cmd.Context(), database)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "vacuum complete: %s reclaimed\n", fmtBytesIEC(freed))
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml")
	return cmd
}

func newDBBackupCmd() *cobra.Command {
	var (
		configPath string
		dest       string
	)
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Write a consistent snapshot of the live database (online-safe; VACUUM INTO)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, database, cleanup, err := loadConfigAndDB(cmd.Context(), configPath)
			if err != nil {
				return err
			}
			defer cleanup()
			if dest == "" {
				dest = defaultBackupPath(cfg.Observer.DBPath, time.Now().UTC())
			}
			if err := db.BackupInto(cmd.Context(), database, dest); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "backup written: %s\n\n", dest)
			fmt.Fprint(cmd.OutOrStdout(), restoreInstructions)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml")
	cmd.Flags().StringVar(&dest, "to", "", "Destination file (default <db dir>/backups/observer-<timestamp>.db)")
	return cmd
}

// newDBImportCmd implements `observer db import <path>` (usability
// arc P6.9) — merge a foreign observer.db into this install: a
// stranded second install (the split-brain era Windows-side DB), a
// pre-migration machine, or any old copy. Idempotent on the schema's
// (source_file, source_event_id) keys plus identity heuristics for
// keyless tables — running the same import twice inserts nothing.
func newDBImportCmd() *cobra.Command {
	var (
		configPath string
		dryRun     bool
		jsonOut    bool
	)
	cmd := &cobra.Command{
		Use:   "import <path-to-foreign-observer.db>",
		Short: "Merge another observer.db into this one (idempotent; source opened read-only after a schema upgrade)",
		Long: "Merges projects, sessions, actions, token usage, API turns, effort\n" +
			"rows, and failure context from another observer database into this\n" +
			"install, skipping anything already present. Node-local state (cache\n" +
			"tracking, PID bridge, parse cursors, file freshness, advisor state)\n" +
			"deliberately stays behind.\n\n" +
			"NOTE: the source file is first opened once through the normal open\n" +
			"path, which upgrades it to the current schema in place (the same\n" +
			"thing `observer start` would do against it). The merge itself then\n" +
			"reads it via a read-only attach. Take a copy first if you need the\n" +
			"source byte-identical.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			foreign, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			st, err := os.Stat(foreign)
			if err != nil {
				return fmt.Errorf("source database: %w", err)
			}
			if st.IsDir() || st.Size() == 0 {
				return fmt.Errorf("source database %s is %s — refusing (db.Open would silently create an empty one)",
					foreign, map[bool]string{true: "a directory", false: "empty"}[st.IsDir()])
			}

			cfg, database, cleanup, err := loadConfigAndDB(cmd.Context(), configPath)
			if err != nil {
				return err
			}
			defer cleanup()
			liveAbs, _ := filepath.Abs(cfg.Observer.DBPath)
			if liveAbs == foreign {
				return fmt.Errorf("source %s IS the live database — nothing to import", foreign)
			}

			// Migrate the foreign file to the current schema so the
			// set-based merge reads matching columns, then release it.
			fdb, err := db.Open(cmd.Context(), db.Options{Path: foreign})
			if err != nil {
				return fmt.Errorf("open source database: %w", err)
			}
			if err := fdb.Close(); err != nil {
				return fmt.Errorf("close source database: %w", err)
			}

			res, err := store.New(database).ImportFrom(cmd.Context(), foreign, store.ImportOptions{
				DryRun:  dryRun,
				Indexer: indexing.New(database, cfg.Compression.Indexing.MaxExcerptBytes),
			})
			if err != nil {
				return err
			}
			if jsonOut {
				body, _ := json.MarshalIndent(res, "", "  ")
				fmt.Fprintln(cmd.OutOrStdout(), string(body))
				return nil
			}
			verb := "imported"
			if dryRun {
				verb = "would import (dry-run — nothing written)"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s from %s:\n", verb, foreign)
			for _, row := range []struct {
				label string
				r     store.ImportTableResult
			}{
				{"projects", res.Projects},
				{"sessions", res.Sessions},
				{"actions", res.Actions},
				{"token_usage", res.TokenUsage},
				{"api_turns", res.APITurns},
				{"claudecode_effort", res.Effort},
				{"failure_context", res.FailureContext},
			} {
				fmt.Fprintf(cmd.OutOrStdout(), "  %-18s %6d new  (%d in source)\n", row.label, row.r.Inserted, row.r.Foreign)
			}
			if !dryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "  %-18s %6d indexed for search\n", "excerpts", res.ExcerptsIndexed)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Compute exact counts inside a rolled-back transaction; write nothing")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit JSON")
	return cmd
}

// defaultBackupPath puts timestamped snapshots in a backups/ dir next
// to the live database, so custom db_path installs keep their backups
// on the same volume.
func defaultBackupPath(dbPath string, now time.Time) string {
	return filepath.Join(filepath.Dir(dbPath), "backups",
		"observer-"+now.Format("20060102-150405")+".db")
}

// restoreInstructions is the canonical restore procedure — printed by
// `observer db backup` and shown in the dashboard's Storage section.
const restoreInstructions = `to restore from a backup:
  1. stop the daemon (ctrl-c the observer start process, or kill its PID)
  2. replace the live DB with the snapshot:
       cp <backup file> <db dir>/observer.db
     and delete observer.db-wal / observer.db-shm if present
  3. start the daemon again: observer start
`

// fmtBytesIEC renders byte counts in binary units (matches SQLite
// page accounting better than SI).
func fmtBytesIEC(n int64) string {
	const unit = 1024
	if n < unit && n > -unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit || v <= -unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
