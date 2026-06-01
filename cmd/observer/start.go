package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/diag"
	otelexp "github.com/marmutapp/superbased-observer/internal/exporter/otel"
	"github.com/marmutapp/superbased-observer/internal/hook"
	"github.com/marmutapp/superbased-observer/internal/intelligence/cost"
	"github.com/marmutapp/superbased-observer/internal/intelligence/dashboard"
	"github.com/marmutapp/superbased-observer/internal/orgclient"
)

// newStartCmd implements `observer start` — the full-mode entrypoint that
// runs the proxy + watcher + dashboard in one process. Each component
// runs in its own goroutine with a shared context; any one's error
// cancels the others.
//
// What `start` auto-wires on launch:
//   - HOOKS for every detected AI tool, idempotently — see
//     [autoRegisterHooks]. Gated by `[observer.hooks] auto_register`,
//     default true. This is the on-launch self-heal path; users no
//     longer need to remember `observer init` for hook capture.
//
// What `start` deliberately does NOT touch:
//   - MCP REGISTRATION. The MCP stdio server cannot run alongside the
//     other goroutines (it owns stdin/stdout), and each AI client is
//     expected to spawn its own `observer serve` subprocess. MCP
//     registration writes per-client config (`~/.claude.json`,
//     `~/.cursor/mcp.json`, `~/.codex/config.toml`) that we treat as
//     explicit user opt-in — `observer init` (or `init --skip-hooks`
//     for MCP-only) is the dedicated path.
//   - PER-TOOL PROXY-ROUTING config (e.g. codex `base_url`). That stays
//     in `observer init` behind `--skip-proxy-route`. The proxy itself
//     still binds on `start`; the AI client routes into it via env
//     vars (`ANTHROPIC_BASE_URL`, `OPENAI_BASE_URL`) or per-tool config.
//
// So a vanilla `observer start` produces an install with: live capture
// (watcher + hooks) + proxy listener + dashboard, but no MCP overhead
// in any AI client unless the user separately ran `observer init`.
func newStartCmd() *cobra.Command {
	var (
		configPath  string
		recipeName  string
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

			// Write a per-PID lockfile in the DB dir so concurrent
			// daemons can be detected by `observer doctor`. Two
			// observers writing the same SQLite file race on cursor
			// state and have silently corrupted backfill in the past.
			cfgForLock, lockErr := config.Load(config.LoadOptions{GlobalPath: configPath})
			if lockErr == nil {
				binary, _ := absoluteBinaryPath()
				lockPath, lerr := diag.WriteLock(filepath.Dir(cfgForLock.Observer.DBPath), diag.LockInfo{
					PID:        os.Getpid(),
					StartedAt:  time.Now().UTC(),
					DBPath:     cfgForLock.Observer.DBPath,
					BinaryPath: binary,
				})
				if lerr == nil {
					defer func() { _ = diag.RemoveLock(lockPath) }()
				}
			}

			// Auto-register hooks for every detected tool, idempotently.
			// Already-registered entries are no-ops; stale-args entries
			// get refreshed silently. Conflicts with non-observer
			// entries log a warning and skip — never overwrite
			// user-authored configuration. Set
			// [observer.hooks] auto_register = false to opt out.
			//
			// Defaults to true even when config load failed (lockErr !=
			// nil) because the most common reason for a fresh install
			// having no working config is "the user just installed and
			// hasn't run init yet" — exactly the case we want to
			// auto-heal.
			autoRegister := true
			if lockErr == nil {
				autoRegister = cfgForLock.Observer.Hooks.AutoRegister
			}
			if autoRegister {
				autoRegisterHooks(cmd.OutOrStdout(), cmd.ErrOrStderr(), configPath)
			}

			p, pCleanup, addr, err := buildProxy(ctx, configPath, recipeName, port, bindAddr)
			if err != nil {
				return err
			}
			defer pCleanup()

			w, wCleanup, err := buildWatcher(ctx, configPath, recipeName)
			if err != nil {
				return err
			}
			defer wCleanup()

			// Org push client (Teams) — constructed only when [org_client]
			// enabled = true, so a solo-local install never probes the OS
			// keychain or opens an org code path. Shared with the dashboard
			// (its enrolment endpoints) and driven by the push loop below.
			// A construction failure is WARN-only: org mode must never block
			// the daemon (P1).
			var orgClient *orgclient.Client
			if lockErr == nil && cfgForLock.OrgClient.Enabled {
				c, orgCleanup, _, oerr := buildOrgClient(ctx, configPath)
				if oerr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "org push disabled — client init failed: %v\n", oerr)
				} else {
					orgClient = c
					defer orgCleanup()
				}
			}

			// OTel exporter (Teams M4) — constructed only when [exporter.otel]
			// enabled = true, so a solo-local install builds no OTLP client and
			// makes zero exporter network calls. It tails api_turns
			// independently of the org server (M0 only). A construction failure
			// is WARN-only: the exporter must never block the daemon (P1).
			var otelExporter *otelexp.Exporter
			if lockErr == nil && cfgForLock.Exporter.OTel.Enabled {
				exp, otelCleanup, _, oerr := buildOTelExporter(ctx, configPath)
				if oerr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "otel exporter disabled — init failed: %v\n", oerr)
				} else {
					otelExporter = exp
					defer otelCleanup()
				}
			}

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
				opts := dashboard.Options{
					DB:                    database,
					DBPath:                cfg.Observer.DBPath,
					CostEngine:            cost.NewEngine(cfg.Intelligence),
					MonthlyBudgetUSD:      cfg.Intelligence.MonthlyBudgetUSD,
					ConfigPath:            resolvedConfigPath,
					RecognizesSessionFile: recognizesSessionFile(),
					ProxyPort:             cfg.Proxy.Port,
				}
				// Assign the concrete client only when present so Options.OrgClient
				// stays a nil interface (not a non-nil interface holding a nil
				// pointer) on a solo-local install — the enrolment handlers key
				// off that nil to report not-enrolled.
				if orgClient != nil {
					opts.OrgClient = orgClient
				}
				dashboardServer, err = dashboard.New(opts)
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
			if orgClient != nil {
				// The org push loop never propagates an error: a stuck/failing
				// push must never cancel the proxy, watcher, or dashboard (P1).
				// PushLoop already WARN-logs failures and stops cleanly on an
				// auth failure or ctx-cancel.
				g.Go(func() error {
					_ = orgClient.PushLoop(gctx)
					return nil
				})
			}
			if otelExporter != nil {
				// The OTel exporter never propagates an error: a failing or
				// unreachable collector must never cancel the proxy, watcher,
				// dashboard, or push loop (P1). Start returns when gctx-cancel
				// closes the row tail; then flush remaining spans with a
				// detached, bounded context so shutdown doesn't drop them.
				g.Go(func() error {
					if err := otelExporter.Start(gctx); err != nil && !errors.Is(err, context.Canceled) {
						fmt.Fprintf(cmd.ErrOrStderr(), "otel exporter stopped: %v\n", err)
					}
					sctx, scancel := context.WithTimeout(context.WithoutCancel(gctx), 5*time.Second)
					_ = otelExporter.Stop(sctx)
					scancel()
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
	cmd.Flags().StringVar(&recipeName, "recipe", "", "Embedded compression recipe to apply between defaults and the config file ("+strings.Join(config.RecipeNames(), ", ")+"). Empty = no recipe; user config still wins.")
	cmd.Flags().IntVar(&port, "port", 0, "Override [proxy].port")
	cmd.Flags().StringVar(&bindAddr, "bind", "127.0.0.1", "Proxy bind address (default localhost only)")
	cmd.Flags().StringVar(&dashAddr, "dashboard-addr", "", "Dashboard listen address (default 127.0.0.1:8081)")
	cmd.Flags().BoolVar(&noDashboard, "no-dashboard", false, "Skip the dashboard goroutine")
	return cmd
}

// autoRegisterHooks installs hooks for every tool the hook registry
// detects on the current host, idempotently. Output goes to stdout
// for installs, stderr for warnings; tools whose hooks are already
// up-to-date stay silent so steady-state restarts are quiet.
//
// This is the on-launch self-heal path: a fresh install (or a Cursor
// install that happens AFTER the daemon was first started, like the
// WSL+Windows-Cursor case) gets its hooks wired without the user
// having to discover and run `observer init`. The same idempotent
// register* implementations init.go calls are reused, so explicit
// `observer init` and on-launch auto-register cannot disagree.
//
// Force is left false: a non-observer entry in someone's hooks file
// is a clear signal that the user (or a different tool) put it
// there, and silently overwriting that on every observer start would
// be far worse than not auto-registering.
func autoRegisterHooks(stdout, stderr io.Writer, configPath string) {
	binary, err := absoluteBinaryPath()
	if err != nil {
		fmt.Fprintf(stderr, "auto-register: cannot resolve binary path: %v\n", err)
		return
	}
	resolvedConfig, _ := config.ResolveGlobalPath(configPath)
	reg, err := hook.NewRegistry(hook.Options{
		BinaryPath: binary,
		ConfigPath: resolvedConfig,
		WSLDistro:  os.Getenv("WSL_DISTRO_NAME"),
	})
	if err != nil {
		fmt.Fprintf(stderr, "auto-register: %v\n", err)
		return
	}
	for _, tool := range reg.Installed() {
		if !hookSupported(tool) {
			continue
		}
		res := reg.Register(tool)
		switch {
		case res.Error != nil:
			fmt.Fprintf(stderr,
				"auto-register %s: %v (run `observer init --force` to overwrite)\n",
				tool, res.Error)
		case len(res.HooksAdded) > 0:
			fmt.Fprintf(stdout,
				"auto-register %s: installed %d hook(s) at %s\n",
				tool, len(res.HooksAdded), res.ConfigPath)
		}
	}
}
