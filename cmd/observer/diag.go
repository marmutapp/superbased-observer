package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/diag"
)

func newDoctorCmd() *cobra.Command {
	var (
		configPath string
		jsonOut    bool
	)
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Run health checks on the observer (DB, hooks, MCP)",
		Long: "Verifies database integrity, hook checksums, MCP registrations, and\n" +
			"the running binary's path against what was recorded by `observer init`.\n" +
			"Exits non-zero if any check fails.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, database, cleanup, err := loadConfigAndDB(cmd.Context(), configPath)
			if err != nil {
				return err
			}
			defer cleanup()

			binary, err := absoluteBinaryPath()
			if err != nil {
				return err
			}
			report := diag.Run(cmd.Context(), diag.DoctorOptions{
				Config:     cfg,
				DB:         database,
				BinaryPath: binary,
			})
			if jsonOut {
				body, _ := json.MarshalIndent(report, "", "  ")
				fmt.Fprintln(cmd.OutOrStdout(), string(body))
			} else {
				printReport(cmd.OutOrStdout(), report)
			}
			if report.Failed() {
				return errors.New("one or more checks failed")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON instead of formatted output")
	return cmd
}

func newStatusCmd() *cobra.Command {
	var (
		configPath string
		jsonOut    bool
	)
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show observer DB stats and recent activity",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, database, cleanup, err := loadConfigAndDB(cmd.Context(), configPath)
			if err != nil {
				return err
			}
			defer cleanup()

			snap, err := diag.Snapshot(cmd.Context(), database, cfg.Observer.DBPath)
			if err != nil {
				return err
			}
			if jsonOut {
				body, _ := json.MarshalIndent(snap, "", "  ")
				fmt.Fprintln(cmd.OutOrStdout(), string(body))
				return nil
			}
			fmt.Fprint(cmd.OutOrStdout(), diag.FormatStatus(snap))
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	return cmd
}

func newTailCmd() *cobra.Command {
	var (
		configPath   string
		intervalSecs int
		pageSize     int
		sinceMinutes int
	)
	cmd := &cobra.Command{
		Use:   "tail",
		Short: "Live-stream captured actions to the terminal",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			_, database, cleanup, err := loadConfigAndDB(ctx, configPath)
			if err != nil {
				return err
			}
			defer cleanup()

			opts := diag.TailOptions{
				Interval: time.Duration(intervalSecs) * time.Second,
				PageSize: pageSize,
			}
			if sinceMinutes > 0 {
				opts.Since = time.Now().UTC().Add(-time.Duration(sinceMinutes) * time.Minute)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "tail running — ctrl-c to stop")
			if err := diag.Tail(ctx, database, cmd.OutOrStdout(), opts); err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml")
	cmd.Flags().IntVar(&intervalSecs, "interval", 1, "Poll interval in seconds")
	cmd.Flags().IntVar(&pageSize, "page-size", 100, "Max rows to read per poll")
	cmd.Flags().IntVar(&sinceMinutes, "since", 0, "Replay actions from the last N minutes before tailing live")
	return cmd
}

// loadConfigAndDB centralizes the config + DB open boilerplate the three
// diag commands all need. Caller must invoke cleanup() to close the DB.
func loadConfigAndDB(ctx context.Context, configPath string) (config.Config, *sql.DB, func(), error) {
	cfg, err := config.Load(config.LoadOptions{GlobalPath: configPath})
	if err != nil {
		return config.Config{}, nil, func() {}, fmt.Errorf("load config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Observer.DBPath), 0o755); err != nil {
		return config.Config{}, nil, func() {}, fmt.Errorf("ensure db dir: %w", err)
	}
	database, err := db.Open(ctx, db.Options{Path: cfg.Observer.DBPath})
	if err != nil {
		return config.Config{}, nil, func() {}, fmt.Errorf("open db %s: %w", cfg.Observer.DBPath, err)
	}
	cleanup := func() { _ = database.Close() }
	return cfg, database, cleanup, nil
}

// printReport renders a diag.Report as one line per check, with optional
// indented detail lines for warn/fail entries.
func printReport(w io.Writer, report diag.Report) {
	for _, c := range report.Checks {
		symbol := "✓"
		switch c.Status {
		case diag.StatusWarn:
			symbol = "⚠"
		case diag.StatusFail:
			symbol = "✗"
		}
		fmt.Fprintf(w, "%s %-20s %s\n", symbol, c.Name, c.Message)
		for _, d := range c.Details {
			fmt.Fprintf(w, "    %s\n", d)
		}
	}
	ok, warn, fail := report.Counts()
	fmt.Fprintf(w, "\n%d ok, %d warn, %d fail\n", ok, warn, fail)
}
