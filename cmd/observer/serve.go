package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/intelligence/cost"
	"github.com/marmutapp/superbased-observer/internal/mcp"
)

// newServeCmd implements `observer serve` — the stdio MCP server
// entrypoint that AI tools spawn via their MCP configuration. Logs go to
// stderr; stdout is reserved for JSON-RPC.
func newServeCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the MCP server on stdio (spawned by AI tools)",
		Long: "Starts the Model Context Protocol server over stdio. Meant to be\n" +
			"launched by an AI tool per its MCP configuration, NOT run\n" +
			"interactively. Use `observer init` to register this command with\n" +
			"the tools you use.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			cfg, err := config.Load(config.LoadOptions{GlobalPath: configPath})
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			if err := os.MkdirAll(filepath.Dir(cfg.Observer.DBPath), 0o755); err != nil {
				return fmt.Errorf("ensure db dir: %w", err)
			}
			database, err := db.Open(ctx, db.Options{Path: cfg.Observer.DBPath})
			if err != nil {
				return fmt.Errorf("open db %s: %w", cfg.Observer.DBPath, err)
			}
			defer database.Close()

			// Stderr logger — stdout belongs to JSON-RPC.
			logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
				Level: logLevelFromString(cfg.Observer.LogLevel),
			}))
			s, err := mcp.New(mcp.Options{
				DB:            database,
				Logger:        logger,
				ServerName:    "observer",
				ServerVersion: version,
				CostEngine:    cost.NewEngine(cfg.Intelligence),
			})
			if err != nil {
				return err
			}
			if err := s.Run(ctx, os.Stdin, os.Stdout); err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml)")
	return cmd
}

func logLevelFromString(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
