package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	otlpingest "github.com/marmutapp/superbased-observer/internal/ingest/otlp"
	"github.com/marmutapp/superbased-observer/internal/intelligence/advisor"
	"github.com/marmutapp/superbased-observer/internal/intelligence/cost"
	"github.com/marmutapp/superbased-observer/internal/intelligence/dashboard"
	"github.com/marmutapp/superbased-observer/internal/orgclient"
	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
	"github.com/marmutapp/superbased-observer/internal/store"
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
		noOpen      bool
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

			// Config schema auto-migration (daemon-only write owner):
			// rewrite deprecated keys (e.g. [compression.code_graph] →
			// [codeintel]) in place before anything Loads the file, so
			// this run and every short-lived subprocess afterwards see
			// the current schema and stop emitting deprecation warnings.
			// Runs here — NOT in config.Load — because Load fires in many
			// concurrent subprocesses and only the daemon should write.
			// Fail-open: a migration error never blocks startup.
			if migPath, perr := config.ResolveGlobalPath(configPath); perr == nil {
				if mres, merr := config.MigrateFile(migPath); merr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "WARN config auto-migrate failed for %s: %v (continuing)\n", migPath, merr)
				} else if mres.Skipped {
					fmt.Fprintf(cmd.ErrOrStderr(), "WARN config auto-migrate skipped %s: %s\n", migPath, mres.SkipReason)
				} else if mres.Migrated {
					fmt.Fprintf(cmd.ErrOrStderr(), "migrated config %s (schema %d → %d, %d change(s)); backup at %s.bak\n",
						migPath, mres.FromVersion, mres.ToVersion, len(mres.Changes), migPath)
				}
			}

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

				// Cross-env split guardrail: warn if a foreign-OS
				// observer.db exists that this daemon won't read (a second
				// daemon, or rows stranded by a hook that wrote a sibling DB
				// instead of running in this daemon's context). Cross-OS hook
				// capture is handled by registering the AI tool's hooks as a
				// wsl.exe bridge so they execute in the daemon's context; this
				// guardrail surfaces an otherwise-silent split (e.g. a stale
				// native-binary registration writing the wrong DB).
				for _, sib := range diag.DetectCrossEnvSiblingDBs(cfgForLock.Observer.DBPath, crossmount.AllHomes()) {
					fmt.Fprintf(cmd.ErrOrStderr(),
						"WARN cross-env observer.db at %s (%s): this daemon reads %s. "+
							"Rows already in that sibling DB won't appear here — re-register that tool's hooks "+
							"via `observer init` so they run in this daemon's context.\n",
						sib.Path, sib.Origin, cfgForLock.Observer.DBPath)
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

			p, pCleanup, addr, profilesReload, routingDemotions, err := buildProxy(ctx, configPath, recipeName, port, bindAddr)
			if err != nil {
				return err
			}
			defer pCleanup()

			w, wCleanup, err := buildWatcher(ctx, configPath)
			if err != nil {
				return err
			}
			defer wCleanup()

			// Share the proxy's cachetrack engine with the watcher's store
			// so Tier-2 (transcript) cache observations advance the SAME
			// CacheModel state the proxy's Tier-1 path does. Without this,
			// the watcher store's engine is nil and every transcript cache
			// observation is dropped, so sessions never routed through the
			// proxy show no cache warm/cold status at all. No-op when cache
			// tracking is disabled ([cachetrack].enabled = false → nil).
			if eng := p.CacheEngine(); eng != nil {
				w.SetCacheEngine(eng)
			}

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
				launchMgr, launchClose, err := newLaunchManager(cfg, slog.Default())
				if err != nil {
					return err
				}
				defer launchClose()
				opts := dashboard.Options{
					DB:                    database,
					DBPath:                cfg.Observer.DBPath,
					CostEngine:            cost.NewEngine(cfg.Intelligence),
					Predict:               cfg.Predict,
					CacheWarm:             cfg.CacheWarm,
					MonthlyBudgetUSD:      cfg.Intelligence.MonthlyBudgetUSD,
					ConfigPath:            resolvedConfigPath,
					RecognizesSessionFile: recognizesSessionFile(),
					ToolCatalog:           toolCatalog(),
					ProxyPort:             cfg.Proxy.Port,
					StashDir:              cfg.Compression.Conversation.Stash.Dir,
					GuardEnabled:          cfg.Guard.Enabled,
					GuardMode:             cfg.Guard.Mode,
					GuardStrict:           cfg.Guard.Strict,
					Version:               version,
					// Session handoff (docs/session-handoff.md P2): the
					// shared handoffsvc runner behind
					// /api/session/<id>/handoff*.
					BuildHandoff: handoffRunner(cfg, database),
					// P2.5: config saves hot-reload the proxy's compression
					// profile router — new sessions resolve against the
					// updated [profiles] table / compression params without
					// a restart. Nil when compression is off at boot (the
					// master switch stays restart-gated).
					OnConfigSaved: profilesReload,
					// P6.7: demo mode — temp DB seeded from embedded
					// fixtures; never touches the real observer.db.
					DemoSeeder: demoSeeder(slog.Default()),
					// R2.4: the live router's §R18.3 demotion set —
					// in-memory state only this process can answer for.
					// Nil when routing isn't wired; the status endpoint
					// reports demotions_live accordingly.
					RoutingDemotions: routingDemotions,
					// D4: the generalized-observability subsystem registers
					// its /api/obs/* trajectory endpoints here (the single
					// host->obs seam; nil when disabled or under no_obs).
					ExtraRoutes: obsDashboardRoutes(ctx, cfg, database, slog.Default()),
					// Embedded web-terminal launcher (docs/session-handoff.md
					// launch section). Nil when [handoff].allow_dashboard_launch
					// is false → endpoints 503, button hidden.
					LaunchManager: launchMgr,
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
				dashURL := "http://" + dashboardListen
				fmt.Fprintf(cmd.OutOrStdout(),
					"start: proxy %s + watcher + dashboard — ctrl-c to stop\n", addr)
				fmt.Fprintf(cmd.OutOrStdout(), "  dashboard → %s\n", dashURL)
				// Auto-open on interactive launches only (P1.14): a TTY
				// stdout means a human just typed `observer start`;
				// daemonized launches (setsid / systemd / redirected
				// logs) never pop a browser. Best-effort + delayed so
				// the listener is up first.
				if !noOpen && stdoutIsTerminal() {
					go func() {
						select {
						case <-time.After(700 * time.Millisecond):
							openBrowser(dashURL)
						case <-ctx.Done():
						}
					}()
				}
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
			// Code-intelligence index-on-start (ADR-0002: index time only,
			// never the proxy hot path). Best-effort like the org loops —
			// a codeintel failure NEVER cancels proxy/watcher/dashboard.
			g.Go(func() error {
				runCodeIntelOnStart(gctx, configPath)
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
				// Org policy-bundle poll (guard spec §14.2): fetch once at
				// start + every policy_poll_interval; verified bundles
				// land in the local cache the guard's org layer loads
				// from, rejections emit R-205 through the runner. Gated
				// on the guard being on (a guard-off agent ignores the
				// channel — the §14.2 compat posture) and P1 like every
				// sibling: never propagates an error.
				g.Go(func() error {
					pcfg, pdb, pcleanup, perr := loadConfigAndDB(gctx, configPath)
					if perr != nil {
						return nil
					}
					defer pcleanup()
					if !pcfg.Guard.Enabled || pcfg.Guard.Mode == "off" {
						return nil
					}
					cachePath := orgBundleCachePath(pcfg)
					if cachePath == "" {
						return nil
					}
					plogger := newLogger(pcfg.Observer.LogLevel)
					runner := newPolicyBundleRunner(pcfg, store.New(pdb), plogger,
						strings.TrimRight(pcfg.OrgClient.OrgServerURL, "/"))
					_ = orgClient.PolicyPollLoop(gctx, cachePath, runner.onResult)
					return nil
				})
			}
			// Guard cloud tier (guard spec §15, D1 explicit opt-in):
			// daemon-resident dispatcher sweeping the guard_events tail
			// to the configured webhooks + LLM judge through the single
			// notify.Egress worker. Fail-soft and P1 like every sibling:
			// the goroutine never propagates an error, and a cloud
			// outage degrades to core local behaviour. Gated on guard
			// on + [guard.cloud].enabled + at least one event-fed
			// feature configured (reputation alone is the on-demand
			// CLI surface).
			g.Go(func() error {
				ccfg, cdb, ccleanup, cerr := loadConfigAndDB(gctx, configPath)
				if cerr != nil {
					return nil
				}
				defer ccleanup()
				if !ccfg.Guard.Enabled || ccfg.Guard.Mode == "off" || !ccfg.Guard.Cloud.Enabled {
					return nil
				}
				clogger := newLogger(ccfg.Observer.LogLevel)
				d := newCloudDispatcher(ccfg.Guard.Cloud, store.New(cdb), clogger, nil)
				if !d.hasEventFeatures() {
					d.egress.Close()
					return nil
				}
				clogger.Info("guard cloud: dispatcher running",
					"webhooks", len(ccfg.Guard.Cloud.Webhooks),
					"llm_judge", ccfg.Guard.Cloud.LLMJudge.Enabled)
				d.run(gctx)
				return nil
			})
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
				// Guard-event OTel feed (guard spec §11.4, G16): sweeps the
				// guard_events tail into the same exporter rail. Doubly
				// gated — the exporter exists only under [exporter.otel]
				// enabled, and the tail additionally requires guard on +
				// [guard.export].otel (both default off). Fail-soft and P1
				// like every sibling; the exporter's Stop above flushes
				// whatever this tail emitted.
				g.Go(func() error {
					tcfg, tdb, tcleanup, terr := loadConfigAndDB(gctx, configPath)
					if terr != nil {
						return nil
					}
					defer tcleanup()
					if !tcfg.Guard.Enabled || tcfg.Guard.Mode == "off" || !tcfg.Guard.Export.OTel {
						return nil
					}
					tlogger := newLogger(tcfg.Observer.LogLevel)
					tail := &guardOTelTail{
						st: store.New(tdb), sink: otelExporter, logger: tlogger,
						interval: time.Duration(tcfg.Exporter.OTel.PollIntervalSeconds) * time.Second,
					}
					tlogger.Info("guard otel: guard-event tail running",
						"endpoint", tcfg.Exporter.OTel.Endpoint)
					tail.run(gctx)
					return nil
				})
			}
			// Native OTLP logs receiver (native-console integration, Phase 2b):
			// ingests a coding assistant's native telemetry (Claude Code with
			// CLAUDE_CODE_ENABLE_TELEMETRY=1) straight into the store, deduped
			// by request_id against proxy/JSONL. Gated by [ingest.otel] enabled
			// (default off → no listener, solo-local UX unchanged), loopback-
			// only unless allow_non_loopback. Fail-soft + P1: a bind failure
			// logs and returns nil, never cancelling siblings.
			g.Go(func() error {
				rcfg, rdb, rcleanup, rerr := loadConfigAndDB(gctx, configPath)
				if rerr != nil {
					return nil
				}
				defer rcleanup()
				// The shared OTLP receiver serves logs when [ingest.otel]
				// is on and traces when [observability] is on (the obs
				// subsystem registers its trace handler here — the single
				// host->obs seam, build-tagged). Off only when both are off.
				if !rcfg.Ingest.OTel.Enabled && !rcfg.Observability.Enabled {
					return nil
				}
				rlogger := newLogger(rcfg.Observer.LogLevel)
				opts := otlpingest.Options{
					GRPCAddr:         rcfg.Ingest.OTel.GRPCAddr,
					HTTPAddr:         rcfg.Ingest.OTel.HTTPAddr,
					AllowNonLoopback: rcfg.Ingest.OTel.AllowNonLoopback,
					Logger:           rlogger,
				}
				if rcfg.Ingest.OTel.Enabled {
					opts.Handler = otlpLogsHandler(store.New(rdb), rlogger, rcfg.Ingest.OTel.CapturesContent())
				}
				opts.TraceHandler = newObsTraceHandler(gctx, rcfg, rdb, rlogger)
				recv, err := otlpingest.New(opts)
				if err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "otlp receiver disabled — init failed: %v\n", err)
					return nil
				}
				recv.Start()
				<-gctx.Done()
				sctx, scancel := context.WithTimeout(context.WithoutCancel(gctx), 5*time.Second)
				_ = recv.Shutdown(sctx)
				scancel()
				return nil
			})
			// Advisor digest refresher (plan Phase 3): keeps the one-row
			// advisor_digest snapshot warm so the session-start hook and
			// the MCP get_suggestions tool point-read instead of
			// computing. Self-contained config+DB handle; best-effort
			// (P1) — failures log and never cancel siblings.
			g.Go(func() error {
				acfg, adb, acleanup, aerr := loadConfigAndDB(gctx, configPath)
				if aerr != nil || !acfg.Advisor.Enabled {
					return nil
				}
				defer acleanup()
				refresh := func() {
					guardMode, routingMode, shadow := advisorPostureInputs(gctx, acfg, store.New(adb), acfg.Advisor.WindowDays)
					rep, rerr := advisor.Run(gctx, adb, advisor.Options{
						WindowDays:    acfg.Advisor.WindowDays,
						MinConfidence: acfg.Advisor.MinConfidence,
						MinSavingsUSD: acfg.Advisor.MinSavingsUSD,
						CostEngine:    cost.NewEngine(acfg.Intelligence),
						GuardMode:     guardMode,
						RoutingMode:   routingMode,
						RoutingShadow: shadow,
					})
					if rerr == nil {
						rerr = advisor.SaveDigest(gctx, adb, rep, 5)
					}
					if rerr != nil && !errors.Is(rerr, context.Canceled) {
						fmt.Fprintf(cmd.ErrOrStderr(), "advisor digest refresh: %v\n", rerr)
					}
				}
				refresh()
				every := acfg.Advisor.DigestRefreshMinutes
				if every <= 0 {
					every = 30
				}
				ticker := time.NewTicker(time.Duration(every) * time.Minute)
				defer ticker.Stop()
				for {
					select {
					case <-gctx.Done():
						return nil
					case <-ticker.C:
						refresh()
					}
				}
			})
			// Guard baseline scans (spec §9.2 + §13.2): pin every
			// configured MCP server on daemon start so first-sight
			// baselines exist before any session traffic (changes made
			// while the daemon was down surface as R-301/305 now), and
			// re-verify pinned native-dialect artifacts against the
			// current policy (drift while down surfaces as R-204 now).
			// One pass each, best-effort (P1) — failures log inside the
			// runners and never cancel siblings. Live config edits
			// re-check via the shared watcher trigger (guardwire.go).
			g.Go(func() error {
				mcfg, mdb, mcleanup, merr := loadConfigAndDB(gctx, configPath)
				if merr != nil {
					return nil
				}
				defer mcleanup()
				if !mcfg.Guard.Enabled || mcfg.Guard.Mode == "off" {
					return nil
				}
				if !mcfg.Guard.MCP.Pinning && !mcfg.Guard.Dialects.Compile {
					return nil
				}
				logger := newLogger(mcfg.Observer.LogLevel)
				st := store.New(mdb)
				gd := acquireProcessGuard(gctx, mcfg, st, logger)
				if gd == nil {
					return nil
				}
				if mcfg.Guard.MCP.Pinning {
					runner := newMCPSecRunner(configGuardMCP{
						Pinning:             mcfg.Guard.MCP.Pinning,
						PoisoningHeuristics: mcfg.Guard.MCP.PoisoningHeuristics,
					}, st, gd, logger)
					sum := runner.ScanConfigs(gctx)
					if sum.Servers > 0 || len(sum.Findings) > 0 {
						logger.Info("guard mcp: baseline scan",
							"servers", sum.Servers, "new_pins", sum.NewPins, "findings", len(sum.Findings))
					}
				}
				if mcfg.Guard.Dialects.Compile {
					if dr := newDialectRunner(configGuardDialects{
						Compile: mcfg.Guard.Dialects.Compile,
						Targets: mcfg.Guard.Dialects.Targets,
					}, st, gd, logger); dr != nil {
						sum := dr.CheckDrift(gctx)
						if sum.Checked > 0 || len(sum.Drifted) > 0 {
							logger.Info("guard compile: baseline drift check",
								"pinned", sum.Checked, "drifted", len(sum.Drifted), "events", sum.EventsRecorded)
						}
					}
				}
				return nil
			})
			// Process Observability (docs/process-observability.md) — opt-in,
			// daemon-resident OS-level process capture. Gated on
			// [observer.process].enabled (default off) and fail-open: a
			// missing/unsupported backend or any runtime error degrades to a
			// WARN and never cancels the proxy/watcher/dashboard.
			g.Go(func() error {
				return runProcessObserver(gctx, configPath)
			})
			if err := g.Wait(); err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml)")
	cmd.Flags().StringVar(&recipeName, "recipe", "", "DEPRECATED: run with the named compression profile ("+strings.Join(config.RecipeNames(), ", ")+") as the default for ALL proxy traffic this run. Use [profiles] in config.toml instead.")
	_ = cmd.Flags().MarkDeprecated("recipe", "recipes are now compression profiles resolved per provider — this run maps the name onto [profiles].default for all proxy traffic; set [profiles] in config.toml for a durable assignment (watcher-side [compression.shell]/[compression.indexing] recipe keys no longer apply)")
	cmd.Flags().IntVar(&port, "port", 0, "Override [proxy].port")
	cmd.Flags().StringVar(&bindAddr, "bind", "127.0.0.1", "Proxy bind address (default localhost only)")
	cmd.Flags().StringVar(&dashAddr, "dashboard-addr", "", "Dashboard listen address (default 127.0.0.1:8081)")
	cmd.Flags().BoolVar(&noDashboard, "no-dashboard", false, "Skip the dashboard goroutine")
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "Don't auto-open the dashboard in a browser on interactive launches")
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
