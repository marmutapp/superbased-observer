package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/intelligence/cost"
	"github.com/marmutapp/superbased-observer/internal/intelligence/dashboard"
)

// newDashboardCmd wires `observer dashboard` — embedded HTML + /api/* JSON
// endpoints served on http://localhost:<port> (spec §15.6).
func newDashboardCmd() *cobra.Command {
	var (
		configPath string
		port       int
		addr       string
	)
	cmd := &cobra.Command{
		Use:   "dashboard",
		Short: "Serve the local analytics dashboard (embedded HTML + /api/* JSON)",
		Long: "Runs a single-file HTML dashboard served by net/http against the\n" +
			"observer DB. Endpoints: /api/status, /api/cost, /api/sessions,\n" +
			"/api/discover, /api/patterns. Content-Type-agnostic clients (e.g.\n" +
			"curl) can consume the /api/* JSON directly.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, database, cleanup, err := loadConfigAndDB(cmd.Context(), configPath)
			if err != nil {
				return err
			}
			defer cleanup()

			resolvedConfigPath, _ := config.ResolveGlobalPath(configPath)
			server, err := dashboard.New(dashboard.Options{
				DB:                    database,
				DBPath:                cfg.Observer.DBPath,
				CostEngine:            cost.NewEngine(cfg.Intelligence),
				MonthlyBudgetUSD:      cfg.Intelligence.MonthlyBudgetUSD,
				ConfigPath:            resolvedConfigPath,
				RecognizesSessionFile: recognizesSessionFile(),
			})
			if err != nil {
				return err
			}
			listen := addr
			if listen == "" {
				listen = fmt.Sprintf("127.0.0.1:%d", port)
			}
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			fmt.Fprintf(cmd.OutOrStdout(),
				"dashboard listening on http://%s — ctrl-c to stop\n", listen)
			if err := server.ListenAndServe(ctx, listen); err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml")
	cmd.Flags().IntVar(&port, "port", 8080, "Port to listen on (ignored if --addr is set)")
	cmd.Flags().StringVar(&addr, "addr", "", "Listen address (e.g. 127.0.0.1:8080)")
	return cmd
}
