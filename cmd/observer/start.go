package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/intelligence/cost"
	"github.com/marmutapp/superbased-observer/internal/intelligence/dashboard"
)

// newStartCmd implements `observer start` — the full-mode entrypoint that
// runs the proxy + watcher + dashboard in one process. Each component
// runs in its own goroutine with a shared context; any one's error
// cancels the others.
//
// MCP stdio cannot run alongside other commands on the same process — it
// owns stdin/stdout. MCP registration is intended to invoke a separate
// `observer serve` subprocess per AI tool, not be embedded here.
func newStartCmd() *cobra.Command {
	var (
		configPath  string
		port        int
		bindAddr    string
		dashAddr    string
		noDashboard bool
	)
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start proxy + watcher + dashboard in a single process",
		Long: "Runs the API reverse proxy, the session-log watcher, and the\n" +
			"local analytics dashboard concurrently. Use this when you want\n" +
			"one foreground process to capture everything and serve the\n" +
			"http://127.0.0.1:8081/ dashboard at the same time.\n\n" +
			"Pass --no-dashboard to skip the dashboard goroutine (useful when\n" +
			"you want a separate `observer dashboard` process bound to a\n" +
			"different address).\n\n" +
			"MCP stdio is registered separately via `observer init` — each\n" +
			"AI tool will launch its own `observer serve` subprocess.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			p, pCleanup, addr, err := buildProxy(ctx, configPath, port, bindAddr)
			if err != nil {
				return err
			}
			defer pCleanup()

			w, wCleanup, err := buildWatcher(ctx, configPath)
			if err != nil {
				return err
			}
			defer wCleanup()

			// Dashboard wiring — opt-out so users can run a separate
			// `observer dashboard` process bound to a non-loopback iface
			// or a different port without conflict.
			var dashboardServer *dashboard.Server
			var dashboardListen string
			if !noDashboard {
				cfg, database, dbCleanup, err := loadConfigAndDB(ctx, configPath)
				if err != nil {
					return err
				}
				defer dbCleanup()
				resolvedConfigPath, _ := config.ResolveGlobalPath(configPath)
				dashboardServer, err = dashboard.New(dashboard.Options{
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
				dashboardListen = dashAddr
				if dashboardListen == "" {
					dashboardListen = "127.0.0.1:8081"
				}
			}

			if dashboardServer != nil {
				fmt.Fprintf(cmd.OutOrStdout(),
					"start: proxy %s + watcher + dashboard http://%s — ctrl-c to stop\n",
					addr, dashboardListen)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(),
					"start: proxy %s + watcher (dashboard disabled via --no-dashboard) — ctrl-c to stop\n",
					addr)
			}

			g, gctx := errgroup.WithContext(ctx)
			g.Go(func() error {
				if err := p.ListenAndServe(gctx, addr); err != nil && !errors.Is(err, context.Canceled) {
					return fmt.Errorf("proxy: %w", err)
				}
				return nil
			})
			g.Go(func() error {
				if err := w.Watch(gctx); err != nil && !errors.Is(err, context.Canceled) {
					return fmt.Errorf("watcher: %w", err)
				}
				return nil
			})
			if dashboardServer != nil {
				g.Go(func() error {
					if err := dashboardServer.ListenAndServe(gctx, dashboardListen); err != nil && !errors.Is(err, context.Canceled) {
						return fmt.Errorf("dashboard: %w", err)
					}
					return nil
				})
			}
			if err := g.Wait(); err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml)")
	cmd.Flags().IntVar(&port, "port", 0, "Override [proxy].port")
	cmd.Flags().StringVar(&bindAddr, "bind", "127.0.0.1", "Proxy bind address (default localhost only)")
	cmd.Flags().StringVar(&dashAddr, "dashboard-addr", "", "Dashboard listen address (default 127.0.0.1:8081)")
	cmd.Flags().BoolVar(&noDashboard, "no-dashboard", false, "Skip the dashboard goroutine")
	return cmd
}
