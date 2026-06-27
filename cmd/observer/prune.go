package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/retention"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// newPruneCmd implements `observer prune` — manual retention pass.
// `start` and `watch` also call retention.Run on boot when prune_on_startup
// is true; this command exists for ad-hoc cleanup outside that path.
func newPruneCmd() *cobra.Command {
	var (
		configPath string
		jsonOut    bool
	)
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Run retention now: delete old actions, orphaned sessions, stale logs, stale cache_* rows",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, database, cleanup, err := loadConfigAndDB(cmd.Context(), configPath)
			if err != nil {
				return err
			}
			defer cleanup()

			res, err := runRetention(cmd.Context(), cfg, database)
			if err != nil {
				return err
			}
			if jsonOut {
				body, _ := json.MarshalIndent(res, "", "  ")
				fmt.Fprintln(cmd.OutOrStdout(), string(body))
				return nil
			}
			fmt.Fprintf(
				cmd.OutOrStdout(),
				"prune complete in %dms: actions=%d sessions=%d logs=%d file_state=%d cache_rows=%d guard_rows=%d process_rows=%d size_passes=%d (db %d → %d bytes)\n",
				res.DurationMs,
				res.ActionsDeleted,
				res.OrphanedSessionsDeleted,
				res.LogEntriesDeleted,
				res.FileStateDeleted,
				res.CacheRowsDeleted,
				res.GuardRowsDeleted,
				res.ProcessRowsDeleted,
				res.SizePassesRun,
				res.DBSizeBytesBefore,
				res.DBSizeBytesAfter,
			)
			if res.SizeCapUnmet {
				fmt.Fprintf(cmd.OutOrStdout(),
					"WARNING: DB still exceeds max_db_size_mb after shedding aged actions — the bulk is in other tables (token_usage / cache_*). Recent actions were protected. Raise [observer.retention].max_db_size_mb.\n")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit JSON")
	return cmd
}

// runRetention executes one retention pass with options drawn from config.
// Reused by `prune` and by the startup hook in watch / start. Composes the
// retention package (sql.DB-only by design) with store.PruneCacheRows
// (cachetrack §9 sweep) — the retention package itself doesn't touch
// cache_* tables, so the orchestration lives here.
//
// Each call is idempotent: a second run within the same horizon is a no-op
// for both passes. Pinned by retention package's existing tests + the new
// TestPruneCacheRows_SecondRunNoop.
func runRetention(ctx context.Context, cfg config.Config, database *sql.DB) (retention.Result, error) {
	p := retention.New(database)
	res, err := p.Run(ctx, retention.Options{
		MaxAgeDays:            cfg.Observer.Retention.MaxAgeDays,
		MaxDBSizeMB:           cfg.Observer.Retention.MaxDBSizeMB,
		ObserverLogMaxAgeDays: cfg.Observer.Retention.ObserverLogMaxAgeDays,
		DBPath:                cfg.Observer.DBPath,
	})
	if err != nil {
		return res, err
	}
	// Cachetrack §9 sweep — wired here because the retention package
	// stays sql.DB-only (no internal/store import). cfg.CacheTrack.
	// RetentionDays ≤ 0 short-circuits inside PruneCacheRows.
	if cfg.CacheTrack.RetentionDays > 0 {
		s := store.New(database)
		n, perr := s.PruneCacheRows(ctx, cfg.CacheTrack.RetentionDays)
		if perr != nil {
			return res, fmt.Errorf("runRetention: prune cache rows: %w", perr)
		}
		res.CacheRowsDeleted = n
	}
	// Routing decision-log sweep ([routing].decision_log_retention_days,
	// model-routing spec §R21) — same orchestration rationale as the
	// cachetrack sweep. ≤ 0 short-circuits inside PruneRouterDecisions.
	if cfg.Routing.DecisionLogRetentionDays > 0 {
		s := store.New(database)
		n, perr := s.PruneRouterDecisions(ctx, cfg.Routing.DecisionLogRetentionDays)
		if perr != nil {
			return res, fmt.Errorf("runRetention: prune router decisions: %w", perr)
		}
		res.RouterDecisionsDeleted = n
	}
	// Guard §10.3 sweep — same composition rationale. The prefix-prune
	// writes the chain checkpoint so `guard verify-audit` keeps
	// anchoring across retention; ≤ 0 short-circuits inside.
	if cfg.Guard.RetentionDays > 0 {
		s := store.New(database)
		n, perr := s.PruneGuardRows(ctx, cfg.Guard.RetentionDays)
		if perr != nil {
			return res, fmt.Errorf("runRetention: prune guard rows: %w", perr)
		}
		res.GuardRowsDeleted = n
	}
	// Process-observability sweep ([observer.process].retention_days,
	// docs/process-observability.md §11) — same composition rationale.
	// ≤ 0 (incl. the feature being disabled with the default 30 but no
	// rows) short-circuits inside PruneProcessRows.
	if cfg.Observer.Process.RetentionDays > 0 {
		s := store.New(database)
		n, perr := s.PruneProcessRows(ctx, cfg.Observer.Process.RetentionDays)
		if perr != nil {
			return res, fmt.Errorf("runRetention: prune process rows: %w", perr)
		}
		res.ProcessRowsDeleted = n
	}
	return res, nil
}
