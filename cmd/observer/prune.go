package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/retention"
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
		Short: "Run retention now: delete old actions, orphaned sessions, stale logs",
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
			fmt.Fprintf(cmd.OutOrStdout(),
				"prune complete in %dms: actions=%d sessions=%d logs=%d file_state=%d size_passes=%d (db %d → %d bytes)\n",
				res.DurationMs,
				res.ActionsDeleted,
				res.OrphanedSessionsDeleted,
				res.LogEntriesDeleted,
				res.FileStateDeleted,
				res.SizePassesRun,
				res.DBSizeBytesBefore,
				res.DBSizeBytesAfter,
			)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit JSON")
	return cmd
}

// runRetention executes one retention pass with options drawn from config.
// Reused by `prune` and by the startup hook in watch / start.
func runRetention(ctx context.Context, cfg config.Config, database *sql.DB) (retention.Result, error) {
	p := retention.New(database)
	return p.Run(ctx, retention.Options{
		MaxAgeDays:            cfg.Observer.Retention.MaxAgeDays,
		MaxDBSizeMB:           cfg.Observer.Retention.MaxDBSizeMB,
		ObserverLogMaxAgeDays: cfg.Observer.Retention.ObserverLogMaxAgeDays,
		DBPath:                cfg.Observer.DBPath,
	})
}
