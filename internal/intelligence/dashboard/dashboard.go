package dashboard

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/diag"
	"github.com/marmutapp/superbased-observer/internal/handoffsvc"
	"github.com/marmutapp/superbased-observer/internal/intelligence/advisor"
	"github.com/marmutapp/superbased-observer/internal/intelligence/cost"
	"github.com/marmutapp/superbased-observer/internal/intelligence/dashboard/webapp"
	"github.com/marmutapp/superbased-observer/internal/intelligence/discover"
	"github.com/marmutapp/superbased-observer/internal/intelligence/learn"
	"github.com/marmutapp/superbased-observer/internal/intelligence/suggest"
	"github.com/marmutapp/superbased-observer/internal/orgclient"
	"github.com/marmutapp/superbased-observer/internal/scrub"
	"github.com/marmutapp/superbased-observer/internal/stash"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// Options configures a Server.
// ExtraRoute is one (pattern, handler) pair a separable subsystem registers
// into the dashboard's shared mux via Options.ExtraRoutes. The handler is a
// plain http.HandlerFunc so the dashboard carries no dependency on the
// subsystem (decision D4 / the obs reverse-import boundary).
type ExtraRoute struct {
	Pattern string
	Handler http.HandlerFunc
}

type Options struct {
	// DB is the observer database.
	DB *sql.DB
	// DBPath is displayed in the header; not used to open anything.
	DBPath string
	// CostEngine prices token summaries. Defaults to baked-in pricing.
	CostEngine *cost.Engine
	// Predict carries the [predict] tunables (Next-Message Cost & Limit
	// Predictor). Zero value → the handler falls back to built-in
	// defaults, so tests and read-only callers need not set it.
	Predict config.PredictConfig
	// CacheWarm carries the [cachewarm] tunables (cache-expiry warning +
	// smart keep-warm). Zero value disables the cache-status surface
	// (Enabled=false), so tests and read-only callers need not set it.
	CacheWarm config.CacheWarmConfig
	// Logger receives operational messages.
	Logger *slog.Logger
	// ExtraRoutes are additional (pattern, handler) pairs a separable
	// subsystem registers into the dashboard mux at the wiring point —
	// e.g. internal/obs's /api/obs/* trajectory endpoints. Generic
	// http.HandlerFunc only; this package never learns the subsystem's
	// types (decision D4). Empty/nil by default.
	ExtraRoutes []ExtraRoute
	// MonthlyBudgetUSD surfaces on the Analysis tab as a spend-budget
	// progress tile. Zero hides the budget readout. Sourced from
	// `intelligence.monthly_budget_usd` in config.toml.
	MonthlyBudgetUSD float64
	// ConfigPath is the resolved path to config.toml — required by the
	// Settings page's GET /api/config + PUT /api/config/pricing
	// endpoints. Empty disables the Settings save path (read-only).
	ConfigPath string
	// RecognizesSessionFile, when non-nil, filters parse_cursors rows
	// in /api/health/watcher: paths NOT recognised by any current
	// adapter are tagged orphan_unmatched and excluded from the
	// "behind" count. Without this, parse_cursors entries from older
	// adapter versions (whose IsSessionFile criteria have since
	// tightened) show in the banner forever — the recovery flow
	// (Rescan / Run All) only re-walks paths a current adapter
	// matches, so it can never close those rows.
	RecognizesSessionFile func(path string) bool
	// ProxyPort is the resolved observer-proxy port (cfg.Proxy.Port).
	// Used by /api/setup/codex to compute the desired
	// ~/.codex/config.toml base_url. Zero falls back to 8820.
	ProxyPort int
	// StashDir is the proxy-side SROD (Stash & Retrieve on Demand) stash
	// directory (cfg.Compression.Conversation.Stash.Dir). When set, the
	// /api/compression/retrieval surface reads short content snippets of
	// what was stashed so the panel shows readable previews instead of
	// opaque SHAs. Empty disables snippets (the panel degrades to counts).
	StashDir string
	// GuardEnabled / GuardMode / GuardStrict surface the [guard]
	// posture on the Security page header (guard spec §11.2).
	// Display-only — the dashboard never constructs a guard.
	GuardEnabled bool
	GuardMode    string
	GuardStrict  bool
	// OrgClient backs the /api/enrolment/* endpoints (Teams). It is non-nil
	// only when [org_client] is enabled; when nil those endpoints report
	// not-enrolled and the web UI hides the org surface — preserving the
	// byte-identical solo-local experience.
	OrgClient EnrolmentService
	// Version is the running binary's version string (e.g. "1.8.2").
	// Stamped at build time via -ldflags="-X main.version=…" and
	// surfaced on /api/status so the dashboard can compare it against
	// the latest published release. "dev" is treated as "no compare".
	Version string
	// OnConfigSaved, when non-nil, fires after every successful
	// config.toml write (section PUT, pricing PUT, backup restore).
	// The daemon wires it to consumers that can re-read config at
	// runtime — P2.5: the proxy's compression profile router, so
	// profile/assignment edits apply to NEW sessions without a
	// restart. Called synchronously after the write lands and before
	// the HTTP response; implementations must be quick and never
	// panic (mirror the CostEngine.Reload hot-path contract).
	OnConfigSaved func()
	// ToolCatalog lists every supported adapter (stable tool name +
	// canonical watch paths) for the Connected-tools panel (P4.1).
	// Injected by cmd — the same seam pattern as
	// RecognizesSessionFile, keeping adapter packages out of the
	// dashboard's import graph. Empty = the panel reports only tools
	// with DB activity.
	ToolCatalog []ToolCatalogEntry
	// BuildHandoff, when non-nil, runs one session-handoff request through
	// internal/handoffsvc (docs/session-handoff.md). Injected by cmd — the
	// same seam pattern as ToolCatalog, keeping the concrete adapter set
	// out of the dashboard's import graph. Nil disables the
	// /api/session/<id>/handoff endpoints (503 with an honest message).
	BuildHandoff func(ctx context.Context, req handoffsvc.Request) (handoffsvc.Result, error)
	// DemoSeeder, when non-nil, enables demo mode (P6.7): it builds a
	// TEMPORARY database seeded from embedded synthetic fixtures and
	// returns the open handle plus a cleanup that closes it and
	// removes its directory. Injected by cmd (the same seam pattern as
	// ToolCatalog — the dashboard never imports the demo or adapter
	// packages). Nil keeps the /api/demo endpoints honest about
	// unavailability. The real observer.db is never read or written on
	// any demo path.
	DemoSeeder func(ctx context.Context) (*sql.DB, func() error, error)
	// RoutingDemotions, when non-nil, returns the live router's §R18.3
	// calibration demotion set (rule name → reason) — in-memory state
	// only the daemon process hosting the router can answer for
	// (R2.4). Injected by `observer start` from the wired router; nil
	// in a standalone `observer dashboard` process, when routing is
	// disabled, and in tests — /api/routing/status reports
	// demotions_live=false so the UI never confuses "can't see" with
	// "none demoted".
	RoutingDemotions func() map[string]string
	// LaunchManager, when non-nil, backs the embedded web-terminal launch
	// surface (POST /api/session/<id>/launch + GET /ws/launch/<handle>).
	// Injected by cmd only when [handoff].allow_dashboard_launch is true;
	// nil makes the endpoints 503 and hides the "Launch <tool> here" action
	// (a nil seam IS the disabled state — no separate flag needed). The seam
	// pattern mirrors BuildHandoff: the dashboard never imports
	// internal/termsession.
	LaunchManager LaunchManager
}

// ToolCatalogEntry is one supported tool in Options.ToolCatalog:
// the adapter's stable name (the `tool` column value) plus the
// canonical directories it watches — returned regardless of installed
// state, so their existence doubles as the install probe.
type ToolCatalogEntry struct {
	Tool       string
	WatchPaths []string
}

// EnrolmentService is the subset of *orgclient.Client the enrolment endpoints
// need. Keeping it an interface lets the dashboard avoid a hard dependency on
// the concrete client in tests and keeps the org surface optional.
type EnrolmentService interface {
	Status(ctx context.Context) (orgclient.EnrolmentState, error)
	Unenroll(ctx context.Context) error
	LastPayload(ctx context.Context) ([]byte, error)
}

// Server wires the /api/* endpoints and static file handler.
type Server struct {
	opts Options

	// Backfill job registry — tracks subprocesses spawned by the
	// Backfill section's Run-Now buttons. Keyed by random hex id;
	// populated in handleBackfillRun, drained by handleBackfillJob.
	// In-memory only; daemon restart drops the registry.
	backfillMu   sync.Mutex
	backfillJobs map[string]*backfillJob
	// backfillSeq hands each job a creation-ordered sequence number
	// under backfillMu — the jobs list sorts on it (StartedAt ties on
	// coarse clocks; see backfillJob.seq).
	backfillSeq int64

	// execBackfill spawns the backfill subprocess. Default points at
	// realExecBackfill which os/exec's the observer binary. Tests
	// override with a fake to avoid requiring the binary in PATH.
	execBackfill backfillExecFn

	// now returns the current UTC time. Defaults to time.Now().UTC();
	// tests override to pin date-sensitive handlers (e.g. the analysis
	// headline's prior-month-same-day window) so CI doesn't flake when
	// the wall clock crosses a calendar boundary the handler treats
	// specially.
	now func() time.Time

	// correlateMu guards lastCorrelate, the per-session timestamp of the last
	// process-correlation pass run from handleSessionProcesses. The Processes
	// drawer polls that endpoint every few seconds while open; without this the
	// correlation WRITE passes (cross-OS + action links) re-ran on every poll,
	// scaling with the unattributed-row backlog and slowing the UI. We instead
	// run them at most once per correlateMinInterval per session — the tree
	// barely changes between polls — and serve the read fresh every time.
	correlateMu   sync.Mutex
	lastCorrelate map[string]time.Time

	// Demo mode (P6.7). demoDB holds the seeded temp database while
	// demo mode is active — data endpoints read it through Server.db()
	// (atomic: the getter sits on every data handler's path).
	// demoCleanup closes the handle and removes the temp directory;
	// both mutate only under demoMu.
	demoMu      sync.Mutex
	demoDB      atomic.Pointer[sql.DB]
	demoCleanup func() error
}

// db returns the database the data endpoints serve from: the seeded
// demo database while demo mode is active (P6.7), the real one
// otherwise. Data handlers MUST read through this getter rather than
// s.opts.DB so demo mode swaps every data surface at one seam.
// Operational surfaces (doctor, watcher health, backfill status,
// connected tools, cowork reconcile) deliberately keep reading
// s.opts.DB — they describe THIS install, not the data.
func (s *Server) db() *sql.DB {
	if d := s.demoDB.Load(); d != nil {
		return d
	}
	return s.opts.DB
}

// New returns a Server. DB is required.
func New(opts Options) (*Server, error) {
	if opts.DB == nil {
		return nil, errors.New("dashboard.New: DB is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.CostEngine == nil {
		opts.CostEngine = cost.NewEngine(config.IntelligenceConfig{})
	}
	if opts.ProxyPort <= 0 {
		opts.ProxyPort = 8820
	}
	return &Server{
		opts:          opts,
		backfillJobs:  map[string]*backfillJob{},
		execBackfill:  realExecBackfill,
		now:           func() time.Time { return time.Now().UTC() },
		lastCorrelate: map[string]time.Time{},
	}, nil
}

// Handler returns the dashboard's http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// React/Vite dashboard at root (Phase 8 cutover, 2026-05-16 —
	// promoted from /v2/). Returns the SPA shell for any non-API
	// path so React Router can render client-side routes.
	mux.Handle(webapp.MountPath, webapp.Handler())
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/status/scoped", s.handleStatusScoped)
	mux.HandleFunc("/api/codex/support", s.handleCodexSupport)
	mux.HandleFunc("/api/cowork/reconcile", s.handleCoworkReconcile)
	mux.HandleFunc("/api/setup/codex", s.handleSetupCodex)
	mux.HandleFunc("/api/setup/codex-hooks", s.handleSetupCodexHooks)
	mux.HandleFunc("/api/setup/claude", s.handleSetupClaude)
	mux.HandleFunc("/api/cost", s.handleCost)
	mux.HandleFunc("/api/discover", s.handleDiscover)
	mux.HandleFunc("/api/suggestions", s.handleSuggestions)                 // advisor engine (spec §15.7)
	mux.HandleFunc("/api/suggestions/state", s.handleSuggestionState)       // dismiss / snooze / acted
	mux.HandleFunc("/api/routing/status", s.handleRoutingStatus)            // model-routing §R17.1: live policy + rule table
	mux.HandleFunc("/api/routing/decisions", s.handleRoutingDecisions)      // decisions feed w/ reason filters (§R17.1/17.2)
	mux.HandleFunc("/api/routing/savings", s.handleRoutingSavings)          // realized vs would-have (§R17.3)
	mux.HandleFunc("/api/routing/tiers", s.handleRoutingTiers)              // tier map + calibration overlays
	mux.HandleFunc("/api/routing/health", s.handleRoutingHealth)            // observed model health board
	mux.HandleFunc("/api/routing/shadow", s.handleRoutingShadow)            // §R18.2 advise-shadow promotion surface
	mux.HandleFunc("/api/routing/simulate", s.handleRoutingSimulate)        // §R18.1 counterfactual replay (R1.2; POST, read-only)
	mux.HandleFunc("/api/routing/apply", s.handleRoutingApply)              // §R10 Channel A: GET dry-run preview / POST one-file consent write (R2.1)
	mux.HandleFunc("/api/routing/apply/revert", s.handleRoutingApplyRevert) // per-file or all=true restore from observer backups (R2.1/R2.5)
	mux.HandleFunc("/api/routing/apply/ledger", s.handleRoutingApplyLedger) // "what did observer change?" audit trail (R2.5)
	mux.HandleFunc("/api/routing/policy", s.handleRoutingPolicy)            // [[routing.rules]] editor payload — read-only; the write rides the config section seam (R2.2)
	mux.HandleFunc("/api/routing/policy/lint", s.handleRoutingPolicyLint)   // fragment validate button = the save gate's exact check (R2.2)
	mux.HandleFunc("/api/verbosity/aggregate", s.handleVerbosityAggregate)  // Output Composition cross-session rollup (by model|project|day)
	mux.HandleFunc("/api/sessions", s.handleSessions)
	mux.HandleFunc("/api/sessions/calendar", s.handleSessionsCalendar) // per-day rollup over the window
	mux.HandleFunc("/api/session/", s.handleSessionDetail)             // /api/session/<id>
	mux.HandleFunc("/ws/launch/", s.handleLaunchWS)                    // embedded web-terminal PTY bridge (/ws/launch/<handle>)
	mux.HandleFunc("/api/launch/", s.handleLaunchAdmin)                // list live terminal sessions / DELETE <handle> to reap
	mux.HandleFunc("/api/process/findings", s.handleProcessFindings)   // §14 observe-only process-finding rollup (recent, cross-session)
	mux.HandleFunc("/api/actions", s.handleActions)
	mux.HandleFunc("/api/live", s.handleLive)                           // "now playing" — active sessions + feeds (P6.1)
	mux.HandleFunc("/api/search", s.handleSearch)                       // FTS5 global search over action excerpts (P6.2)
	mux.HandleFunc("/api/budget", s.handleBudget)                       // advisory budget guardrails — global + per-project (P6.3)
	mux.HandleFunc("/api/experiments", s.handleExperiments)             // GET list / POST start (P6.4)
	mux.HandleFunc("/api/experiments/stop", s.handleExperimentStop)     // POST {name}
	mux.HandleFunc("/api/experiments/report", s.handleExperimentReport) // GET ?name=
	mux.HandleFunc("/api/privacy/scrub-test", s.handlePrivacyScrubTest) // POST — live scrub tester, in-memory only (P6.5)
	mux.HandleFunc("/api/demo", s.handleDemo)                           // demo mode state (P6.7)
	mux.HandleFunc("/api/demo/start", s.handleDemoStart)                // POST — seed temp DB from embedded fixtures
	mux.HandleFunc("/api/demo/stop", s.handleDemoStop)                  // POST — one-click clear
	mux.HandleFunc("/api/storage", s.handleStorage)                     // per-table sizes + backups (P6.8)
	mux.HandleFunc("/api/storage/vacuum", s.handleStorageVacuum)        // POST — `db vacuum` via job registry
	mux.HandleFunc("/api/storage/backup", s.handleStorageBackup)        // POST — `db backup` via job registry
	mux.HandleFunc("/api/report/monthly", s.handleReportMonthly)        // GET ?month=YYYY-MM&project= — printable statement data (P6.6)
	mux.HandleFunc("/api/actions/day-counts", s.handleActionsDayCounts) // per-day action counts for Timeline day strip
	mux.HandleFunc("/api/action/", s.handleActionDetail)                // /api/action/<id>/full_text — on-demand full raw_tool_input + raw_tool_output
	mux.HandleFunc("/api/file/state", s.handleFileState)                // /api/file/state?path=<abs> — per-file freshness for the VS Code FileDecorationProvider (M5)
	mux.HandleFunc("/api/patterns", s.handlePatterns)
	mux.HandleFunc("/api/patterns/timeseries", s.handlePatternsTimeseries)
	mux.HandleFunc("/api/suggest", s.handleSuggestPreview)
	mux.HandleFunc("/api/suggest/write", s.handleSuggestWrite)
	mux.HandleFunc("/api/timeseries/cost", s.handleTimeseriesCost)
	mux.HandleFunc("/api/timeseries/tokens-by-model", s.handleTimeseriesTokensByModel)
	mux.HandleFunc("/api/timeseries/actions", s.handleTimeseriesActions)
	mux.HandleFunc("/api/models", s.handleModels)
	mux.HandleFunc("/api/tools", s.handleTools)
	mux.HandleFunc("/api/tools/breakdown", s.handleToolsBreakdown)
	mux.HandleFunc("/api/compression/events", s.handleCompressionEvents)
	mux.HandleFunc("/api/compression/timeseries", s.handleCompressionTimeseries)
	mux.HandleFunc("/api/compression/by-model", s.handleCompressionByModel)
	mux.HandleFunc("/api/compression/retrieval", s.handleCompressionRetrieval)
	mux.HandleFunc("/api/compression/rolling-cost", s.handleCompressionRollingCost)
	mux.HandleFunc("/api/compaction/events", s.handleCompactionEvents)
	mux.HandleFunc("/api/guard/summary", s.handleGuardSummary)                    // Security page header (guard spec §11.2)
	mux.HandleFunc("/api/guard/events", s.handleGuardEvents)                      // verdict timeline (+rule_id/session_id filters, G1.4)
	mux.HandleFunc("/api/guard/conformance", s.handleGuardConformance)            // §6.5 coverage matrix
	mux.HandleFunc("/api/guard/rules", s.handleGuardRules)                        // built-in rule catalog (rule_id → definition)
	mux.HandleFunc("/api/guard/simulate", s.handleGuardSimulate)                  // pre-enforce evidence replay (G1.2)
	mux.HandleFunc("/api/guard/approvals", s.handleGuardApprovals)                // §6.3 exception register: GET list + POST grant (G1.3)
	mux.HandleFunc("/api/guard/approvals/", s.handleGuardApprovalDelete)          // DELETE /api/guard/approvals/{id} (G1.3)
	mux.HandleFunc("/api/guard/mcp", s.handleGuardMCP)                            // §9 MCP pin inventory (G1.6)
	mux.HandleFunc("/api/guard/mcp/approve", s.handleGuardMCPApprove)             // pin approval (G1.6)
	mux.HandleFunc("/api/guard/policy", s.handleGuardPolicy)                      // layers view + USER-layer editor — the one write path (G2.2)
	mux.HandleFunc("/api/guard/policy/lint", s.handleGuardPolicyLint)             // lint-before-save gate (G2.2)
	mux.HandleFunc("/api/guard/policy/backup", s.handleGuardPolicyBackup)         // .bak view / swap-undo (G2.2)
	mux.HandleFunc("/api/guard/evidence", s.handleGuardEvidence)                  // report/export/verify-audit jobs via the shared registry (G2.3)
	mux.HandleFunc("/api/guard/evidence/download", s.handleGuardEvidenceDownload) // completed-job artifact (G2.3)
	mux.HandleFunc("/api/guard/budget", s.handleGuardBudget)                      // budget thresholds + observed-spend basis (G2.4)
	mux.HandleFunc("/api/cache/status", s.handleCacheStatus)
	mux.HandleFunc("/api/cache/overview", s.handleCacheOverview)
	mux.HandleFunc("/api/cache/timeseries", s.handleCacheTimeseries)
	mux.HandleFunc("/api/cache/health", s.handleCacheHealth)
	mux.HandleFunc("/api/cache/events", s.handleCacheEvents)
	mux.HandleFunc("/api/cache/entry-states", s.handleCacheEntryStates)
	mux.HandleFunc("/api/projects", s.handleProjects)
	mux.HandleFunc("/api/export.xlsx", s.handleExportXLSX)
	mux.HandleFunc("/api/analysis/headline", s.handleAnalysisHeadline)
	mux.HandleFunc("/api/analysis/trend", s.handleAnalysisTrend)
	mux.HandleFunc("/api/analysis/movers", s.handleAnalysisMovers)
	mux.HandleFunc("/api/analysis/top-sessions", s.handleAnalysisTopSessions)
	mux.HandleFunc("/api/analysis/routing-suggestions", s.handleAnalysisRoutingSuggestions)
	mux.HandleFunc("/api/analysis/cost-by-hour", s.handleAnalysisCostByHour)
	mux.HandleFunc("/api/analysis/cost-by-dow-hour", s.handleAnalysisCostByDowHour)
	mux.HandleFunc("/api/analysis/cache-savings-trend", s.handleAnalysisCacheSavingsTrend)
	mux.HandleFunc("/api/config", s.handleConfig)                                  // GET full config
	mux.HandleFunc("/api/config/pricing", s.handleConfigPricing)                   // PUT pricing overrides (hot-reload)
	mux.HandleFunc("/api/config/pricing/defaults", s.handleConfigPricingDefaults)  // GET baked-in defaults
	mux.HandleFunc("/api/config/section/", s.handleConfigSection)                  // PUT /api/config/section/<name>
	mux.HandleFunc("/api/config/backup", s.handleConfigBackup)                     // GET view / POST restore config.toml.bak (P1.15)
	mux.HandleFunc("/api/config/reload", s.handleConfigReload)                     // POST re-read config for hot-reloadable consumers (P2.6)
	mux.HandleFunc("/api/config/profiles", s.handleConfigProfiles)                 // POST create user profile (P3.4/D11)
	mux.HandleFunc("/api/config/profiles/", s.handleConfigProfile)                 // GET show / PATCH set-key / DELETE per profile
	mux.HandleFunc("/api/tools/status", s.handleToolsStatus)                       // GET connected-tools matrix (P4.1)
	mux.HandleFunc("/api/tools/launch", s.handleToolsLaunch)                       // POST best-effort terminal launch (P4.6)
	mux.HandleFunc("/api/setup/hooks", s.handleSetupHooks)                         // POST register hooks for one tool (P4.2 wizard)
	mux.HandleFunc("/api/setup/mcp", s.handleSetupMCP)                             // POST register MCP for one tool (P4.2 wizard)
	mux.HandleFunc("/api/health/doctor", s.handleHealthDoctor)                     // GET doctor checks (P4.8)
	mux.HandleFunc("/api/health/failures", s.handleHealthFailures)                 // GET recent failures, recovered-vs-not (P4.11)
	mux.HandleFunc("/api/mcp/value", s.handleMCPValue)                             // GET MCP value meter (P4.10)
	mux.HandleFunc("/api/admin/restart", s.handleAdminRestart)                     // POST → os.Exit(0)
	mux.HandleFunc("/api/admin/antigravity-bridge.exe", s.handleAntigravityBridge) // GET → download bin/antigravity-bridge.exe
	mux.HandleFunc("/api/scan/run", s.handleScanRun)                               // POST full rescan via job runner (P4.13)
	mux.HandleFunc("/api/backfill/status", s.handleBackfillStatus)                 // GET candidate counts
	mux.HandleFunc("/api/backfill/run", s.handleBackfillRun)                       // POST {mode}
	mux.HandleFunc("/api/prune/run", s.handlePruneRun)                             // POST — on-demand retention sweep (P1.10)
	mux.HandleFunc("/api/backfill/jobs", s.handleBackfillJobsList)                 // GET in-flight + recent (newest first)
	mux.HandleFunc("/api/backfill/jobs/", s.handleBackfillJob)                     // GET /jobs/<id>
	mux.HandleFunc("/api/health/watcher", s.handleWatcherHealth)                   // GET watcher cursor vs file size
	mux.HandleFunc("/api/enrolment/status", s.handleEnrolmentStatus)               // GET Teams enrolment status + last push
	mux.HandleFunc("/api/enrolment/last-payload", s.handleEnrolmentLastPayload)    // GET byte-for-byte last shared payload
	mux.HandleFunc("/api/enrolment/unenroll", s.handleEnrolmentUnenroll)           // POST leave the org
	// ExtraRoutes lets a separable subsystem (e.g. internal/obs's
	// trajectory UI) register its own /api/* handlers into this shared
	// mux at the wiring point WITHOUT this package importing it — the
	// dependency stays one-way (decision D4). Empty by default.
	for _, rt := range s.opts.ExtraRoutes {
		if rt.Pattern != "" && rt.Handler != nil {
			mux.HandleFunc(rt.Pattern, rt.Handler)
		}
	}
	return mux
}

// browserGuard defends the dashboard's loopback single-user deployment against
// two attacks that need no network access, only a web page the operator
// visits: CSRF (a malicious page drives a no-preflight POST to a state-changing
// endpoint — restart, config write, tool launch) and DNS rebinding (an attacker
// domain re-pointed at 127.0.0.1 becomes same-origin with the dashboard). It
// wraps the mux at ListenAndServe, where the bind address is known:
//   - unsafe methods (POST/PUT/DELETE/PATCH): the Origin (or Referer) header,
//     when present, must be same-host as the request Host, else 403. A
//     browser always sends Origin on a cross-origin POST, so this blocks CSRF;
//     non-browser clients (curl, the VS Code extension) send no Origin and are
//     unaffected.
//   - on a loopback bind (the default), EVERY request's Host header must name a
//     loopback host, else 403 — this defeats DNS rebinding, whose request
//     carries the attacker's own Host. A deliberately non-loopback bind can't
//     know its intended external host, so the Host allow-list is relaxed there
//     (start-up emits a WARN); the Origin/CSRF check still applies.
func browserGuard(next http.Handler, bindAddr string) http.Handler {
	loopbackBind := hostIsLoopback(hostnameOnly(bindAddr))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqHost := hostnameOnly(r.Host)
		if loopbackBind && !hostIsLoopback(reqHost) {
			http.Error(w, "forbidden: unrecognized Host header", http.StatusForbidden)
			return
		}
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch:
			if origin := requestOriginHost(r); origin != "" && !strings.EqualFold(origin, reqHost) {
				http.Error(w, "forbidden: cross-origin request", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// hostnameOnly strips the port (and IPv6 brackets) from a host[:port] value,
// returning the bare hostname/IP. It tolerates a missing port.
func hostnameOnly(hostport string) string {
	if hostport == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.Trim(hostport, "[]")
}

// hostIsLoopback reports whether host is a loopback name or IP (localhost,
// 127.0.0.0/8, ::1).
func hostIsLoopback(host string) bool {
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// requestOriginHost returns the hostname of the request's Origin header, or
// its Referer host as a fallback. Empty when neither is present (a
// same-origin/non-browser request); "null" opaque origins return a non-empty
// sentinel so an unsafe method from a sandboxed context is treated as
// cross-origin.
func requestOriginHost(r *http.Request) string {
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = r.Header.Get("Referer")
	}
	if origin == "" {
		return ""
	}
	if origin == "null" {
		return "null"
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return "null"
	}
	return hostnameOnly(u.Host)
}

// ListenAndServe runs the dashboard on addr until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	if !hostIsLoopback(hostnameOnly(addr)) {
		s.opts.Logger.Warn("dashboard bound to a non-loopback address — it exposes state-changing endpoints (config writes, tool launch, restart) to anything that can reach it; the DNS-rebind Host allow-list is relaxed on a non-loopback bind", "addr", addr)
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           browserGuard(s.Handler(), addr),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// processStartedAt approximates the daemon's start time — the dashboard
// server is constructed once inside `observer start` / `observer
// dashboard`, so package-init time is the serving process's start. The
// restart-pending banner compares config-save timestamps against this
// to auto-clear once the operator has restarted.
var processStartedAt = time.Now().UTC()

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	snap, err := diag.Snapshot(r.Context(), s.db(), s.opts.DBPath)
	if err != nil {
		writeErr(w, err)
		return
	}
	snap.Version = s.opts.Version
	snap.StartedAt = processStartedAt
	snap.UptimeSeconds = int64(time.Since(processStartedAt).Seconds())
	writeJSON(w, snap)
}

// handleStatusScoped serves /api/status/scoped?days=&tool=&project= — the
// window/tool/project-scoped equivalent of /api/status's `counts` block.
// Drives the Overview + Analysis headline tiles that previously sourced
// from the global lifetime counts and showed the same number regardless
// of filter — a "window 30d" chip over an all-time value.
//
// Returned counts:
//   - sessions: distinct session IDs touched in the window
//   - api_turns: api_turns rows in the window (the proxy-accurate source)
//   - token_usage: token_usage rows in the window (the JSONL fallback)
//   - actions: actions rows in the window
//
// All counts honor the same `tool` + `project` filters as the rest of
// the dashboard so the surface stays internally consistent.
func (s *Server) handleStatusScoped(w http.ResponseWriter, r *http.Request) {
	days := intArg(r, "days", 30, 1, 36500)
	tool := r.URL.Query().Get("tool")
	project := r.URL.Query().Get("project")

	since := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour).Format(time.RFC3339Nano)

	// Helper: append the tool+project filter to a query whose primary
	// table is aliased `x` and has `x.session_id` + `x.project_id`
	// (api_turns) — or to one that has only `x.session_id` so we walk
	// through sessions for project (token_usage / sessions / actions).
	scoped := func(viaSession bool) (extra string, args []any) {
		if project != "" {
			// Identical whether reached via session_id or project_id: both
			// query paths expose project_id on the aliased table x.
			extra += " AND project_id IN (SELECT id FROM projects WHERE root_path = ?)"
			args = append(args, project)
		}
		if tool != "" {
			if viaSession {
				extra += " AND tool = ?"
			} else {
				extra += " AND session_id IN (SELECT id FROM sessions WHERE tool = ?)"
			}
			args = append(args, tool)
		}
		return
	}

	type counts struct {
		Days       int   `json:"days"`
		Sessions   int64 `json:"sessions"`
		APITurns   int64 `json:"api_turns"`
		TokenUsage int64 `json:"token_usage"`
		Actions    int64 `json:"actions"`
	}
	out := counts{Days: days}

	// sessions: started_at in window, session-table direct. Marker-only
	// probe sessions are excluded so this stat agrees with the Sessions list.
	sExtra, sArgs := scoped(true) // sessions has tool + project_id directly
	sQ := `SELECT COUNT(*) FROM sessions WHERE started_at >= ? AND ` +
		nonEmptySessionPredicateSessions + sExtra
	_ = s.db().QueryRowContext(r.Context(), sQ, append([]any{since}, sArgs...)...).Scan(&out.Sessions)

	// api_turns
	atExtra, atArgs := scoped(false) // api_turns: project_id direct, tool via session
	atQ := `SELECT COUNT(*) FROM api_turns WHERE timestamp >= ?` + atExtra
	_ = s.db().QueryRowContext(r.Context(), atQ, append([]any{since}, atArgs...)...).Scan(&out.APITurns)

	// token_usage: no project_id column → walk through sessions for project
	tuExtra := ""
	tuArgs := []any{}
	if project != "" {
		tuExtra += " AND session_id IN (SELECT id FROM sessions WHERE project_id = (SELECT id FROM projects WHERE root_path = ?))"
		tuArgs = append(tuArgs, project)
	}
	if tool != "" {
		tuExtra += " AND session_id IN (SELECT id FROM sessions WHERE tool = ?)"
		tuArgs = append(tuArgs, tool)
	}
	tuQ := `SELECT COUNT(*) FROM token_usage WHERE timestamp >= ?` + tuExtra
	_ = s.db().QueryRowContext(r.Context(), tuQ, append([]any{since}, tuArgs...)...).Scan(&out.TokenUsage)

	// actions: project_id direct, tool direct
	aExtra := ""
	aArgs := []any{}
	if project != "" {
		aExtra += " AND project_id = (SELECT id FROM projects WHERE root_path = ?)"
		aArgs = append(aArgs, project)
	}
	if tool != "" {
		aExtra += " AND tool = ?"
		aArgs = append(aArgs, tool)
	}
	aQ := `SELECT COUNT(*) FROM actions WHERE timestamp >= ?` + aExtra
	_ = s.db().QueryRowContext(r.Context(), aQ, append([]any{since}, aArgs...)...).Scan(&out.Actions)

	writeJSON(w, out)
}

func (s *Server) handleCost(w http.ResponseWriter, r *http.Request) {
	days := intArg(r, "days", 30, 1, 36500)
	groupBy := r.URL.Query().Get("group_by")
	if groupBy == "" {
		groupBy = "model"
	}
	proj := r.URL.Query().Get("project")
	tool := r.URL.Query().Get("tool")
	source := r.URL.Query().Get("source")
	if source == "" {
		source = "auto"
	}
	summary, err := s.opts.CostEngine.Summary(r.Context(), s.db(), cost.Options{
		Days:        days,
		GroupBy:     cost.GroupBy(groupBy),
		Source:      cost.Source(source),
		ProjectRoot: proj,
		Tool:        tool,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	// Spec §13 cost-view annotation: when the rollup keys on a
	// dimension the cache engine indexes (session_id or model),
	// attach per-row cache annotations via the shared
	// loadCacheAnnotationsByKey helper. Empty for other groupings
	// (project / tool) — cache_events doesn't carry those columns
	// natively; the /api/cache/overview endpoint serves the cross-
	// session per-project rollup operators need.
	if column := costAnnotationColumn(groupBy); column != "" {
		keys := make([]string, 0, len(summary.Rows))
		for _, row := range summary.Rows {
			keys = append(keys, row.Key)
		}
		if ann, derr := loadCacheAnnotationsByKey(r.Context(), s.db(), column, keys); derr == nil && len(ann) > 0 {
			writeJSON(w, costSummaryWithCache{Summary: summary, CacheByKey: ann})
			return
		}
	}
	writeJSON(w, summary)
}

// costSummaryWithCache wraps cost.Summary with the per-row cache
// annotation map. The embedded Summary keeps the existing JSON
// shape intact (backward-compat for clients that don't read
// cache_by_key); cache_by_key is the new field that the Cost
// page reads to render the per-row cache pill.
type costSummaryWithCache struct {
	cost.Summary
	CacheByKey map[string]*SessionCacheAnnotation `json:"cache_by_key,omitempty"`
}

// costAnnotationColumn maps cost group_by values to the
// cache_events column they correspond to. Returns "" when the
// grouping isn't directly indexable by cache_events (project /
// tool / pricing_source — for those, the Cache overview page
// serves the cross-session rollup operators need).
func costAnnotationColumn(groupBy string) string {
	switch groupBy {
	case "session":
		return "session_id"
	case "model":
		return "model"
	}
	return ""
}

func (s *Server) handleCodexSupport(w http.ResponseWriter, r *http.Request) {
	days := intArg(r, "days", 0, 0, 36500)
	project := r.URL.Query().Get("project")
	snap, err := buildCodexSupportSnapshot(r.Context(), s.db(), days, project)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, snap)
}

// handleDiscover serves /api/discover. Paginates the stale_reads and
// repeated_commands panels independently — stale_page/stale_limit and
// repeated_page/repeated_limit query params, defaulting to 20 rows per
// page. Backend caps total results at 500 per panel (discover SQL runs
// once per request and the dashboard surfaces top-N anyway); both
// panels expose stale_total / repeated_total for the pager UI.
// handleSuggestions — GET /api/suggestions?days=N&project=R
// [&category=cost|latency|quality|hygiene][&severity=…][&detector=…]
// [&page=P&limit=L]. Computed on read by the advisor engine (spec §15.7;
// thresholds per docs/plans/advisor-calibration-2026-06-10.md). Filters
// and pagination are presentation concerns and live here, not in the
// engine; totals and the per-category rollup reflect the FILTERED set
// (pre-pagination) so the header reconciles with what the list shows.
func (s *Server) handleSuggestions(w http.ResponseWriter, r *http.Request) {
	// Max 36500 (~100y) so the global "all" window (36500 days) isn't
	// clamped to a year — matches the other dashboard endpoints' all-time.
	days := intArg(r, "days", 14, 1, 36500)
	page := intArg(r, "page", 1, 1, 1_000_000)
	limit := intArg(r, "limit", 20, 1, 200)
	proj := r.URL.Query().Get("project")
	tool := r.URL.Query().Get("tool")
	category := r.URL.Query().Get("category")
	severity := r.URL.Query().Get("severity")
	detector := r.URL.Query().Get("detector")

	// X3.1 posture inputs: effective modes from the on-disk config plus
	// the §R22 shadow signal through the one gate owner. Best-effort —
	// neither a config-load nor a shadow-read failure may take down the
	// suggestions report.
	guardMode, routingMode := "off", "off"
	var shadow *advisor.ShadowSignal
	if cfg, cerr := loadConfigForDashboard(s.opts.ConfigPath); cerr == nil {
		if cfg.Guard.Enabled {
			guardMode = cfg.Guard.Mode
		}
		if cfg.Routing.Enabled {
			routingMode = cfg.Routing.Mode
		}
		st := store.New(s.opts.DB)
		if sh, serr := st.AdviseShadowSignal(r.Context(), days, enginePriceFn(s.opts.CostEngine)); serr == nil {
			shadow = &advisor.ShadowSignal{
				AdviseDecisions: sh.AdviseDecisions,
				WouldReroute:    sh.WouldReroute,
				WouldSaveUSD:    sh.WouldSaveUSD,
				QualityFlags:    sh.QualityFlags,
				MinDecisions:    sh.MinDecisions,
				Ready:           sh.ReadyToPromote,
			}
		}
	}
	rep, err := advisor.Run(r.Context(), s.db(), advisor.Options{
		WindowDays:    days,
		ProjectRoot:   proj,
		Tool:          tool,
		CostEngine:    s.opts.CostEngine,
		GuardMode:     guardMode,
		RoutingMode:   routingMode,
		RoutingShadow: shadow,
	})
	if err != nil {
		writeErr(w, err)
		return
	}

	filtered := rep.Suggestions[:0:0]
	byCategory := map[string]float64{}
	byDetector := map[string]int{}
	var totUSD, totMin float64
	for _, sg := range rep.Suggestions {
		if (category != "" && sg.Category != category) ||
			(severity != "" && sg.Severity != severity) ||
			(detector != "" && sg.Detector != detector) {
			continue
		}
		filtered = append(filtered, sg)
		byCategory[sg.Category] += sg.SavingsUSD
		byDetector[sg.Detector]++
		totUSD += sg.SavingsUSD
		totMin += sg.SavingsMin
	}
	total := len(filtered)
	start := (page - 1) * limit
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}

	writeJSON(w, map[string]any{
		"suggestions":       filtered[start:end],
		"total_count":       total,
		"page":              page,
		"limit":             limit,
		"total_savings_usd": round2f(totUSD),
		"total_savings_min": round2f(totMin),
		"by_category":       byCategory,
		"by_detector":       byDetector,
		"window_days":       rep.WindowDays,
		"generated_at":      rep.GeneratedAt,
		"sessions_scanned":  rep.SessionsScanned,
	})
}

func round2f(v float64) float64 { return float64(int64(v*100+0.5)) / 100 }

// handleSuggestionState — POST /api/suggestions/state with JSON
// {dedup_key, status: dismissed|snoozed|acted, snooze_days?}. State is
// node-local (advisor_state, migration 039; never org-pushed).
func (s *Server) handleSuggestionState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		DedupKey   string `json:"dedup_key"`
		Status     string `json:"status"`
		SnoozeDays int    `json:"snooze_days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.DedupKey == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	now := time.Now().UTC()
	var until time.Time
	if req.Status == advisor.StatusSnoozed {
		d := req.SnoozeDays
		if d <= 0 {
			d = 7
		}
		until = now.AddDate(0, 0, d)
	}
	if err := advisor.SetState(r.Context(), s.db(), req.DedupKey, req.Status, until, now); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleDiscover(w http.ResponseWriter, r *http.Request) {
	days := intArg(r, "days", 30, 1, 36500)
	stalePage := intArg(r, "stale_page", 1, 1, 1_000_000)
	staleLimit := intArg(r, "stale_limit", 20, 1, 500)
	repeatedPage := intArg(r, "repeated_page", 1, 1, 1_000_000)
	repeatedLimit := intArg(r, "repeated_limit", 20, 1, 500)
	proj := r.URL.Query().Get("project")
	tool := r.URL.Query().Get("tool")

	// Cap the per-panel SQL limit at 500 — generous enough for realistic
	// dashboards while keeping a single discover.Run cheap.
	report, err := discover.New(s.db()).Run(r.Context(), discover.Options{
		ProjectRoot: proj, Tool: tool, Days: days, Limit: 500,
	})
	if err != nil {
		writeErr(w, err)
		return
	}

	staleTotal := len(report.StaleReads)
	staleStart := (stalePage - 1) * staleLimit
	staleEnd := staleStart + staleLimit
	if staleStart > staleTotal {
		staleStart = staleTotal
	}
	if staleEnd > staleTotal {
		staleEnd = staleTotal
	}
	staleSlice := report.StaleReads[staleStart:staleEnd]

	repTotal := len(report.RepeatedCommands)
	repStart := (repeatedPage - 1) * repeatedLimit
	repEnd := repStart + repeatedLimit
	if repStart > repTotal {
		repStart = repTotal
	}
	if repEnd > repTotal {
		repEnd = repTotal
	}
	repSlice := report.RepeatedCommands[repStart:repEnd]

	// Blended input rate — derived from the user's actual last-30d
	// api_turns (per-model prompt-token volume × per-model rate) so
	// the ~$ wasted KPI tile reflects real model mix rather than a
	// hardcoded representative rate. Falls back to the default
	// (claude-sonnet-4 input rate) when no proxy data is available.
	blendedRate, err := s.opts.CostEngine.BlendedInputRate(r.Context(), s.db(), 30)
	if err != nil {
		s.opts.Logger.Warn("discover: blended input rate", "err", err)
		blendedRate = cost.DefaultBlendedInputRate
	}

	writeJSON(w, map[string]any{
		"stale_reads":                    staleSlice,
		"stale_total":                    staleTotal,
		"stale_page":                     stalePage,
		"stale_limit":                    staleLimit,
		"repeated_commands":              repSlice,
		"repeated_total":                 repTotal,
		"repeated_page":                  repeatedPage,
		"repeated_limit":                 repeatedLimit,
		"cross_tool_files":               report.CrossToolFiles,
		"native_vs_bash":                 report.NativeVsBash,
		"summary":                        report.Summary,
		"blended_input_rate_per_million": blendedRate,
	})
}

// emptySessionMarkers are the lifecycle / meta action types that a hook
// fires for an empty Windows-CC probe session (CLAUDE.md loaded, settings
// touched, session opened then closed) WITHOUT any real work. A session
// whose only rows are these — and which has no token_usage / api_turns —
// is contentless and must not surface on the dashboard. They slip past
// store.Ingest's session_end bootstrap guard because they can legitimately
// PRECEDE a real session (instructions_loaded / config_change fire at
// session start, before the watcher parses the transcript), so dropping
// them at ingestion would strip them from real sessions too. Filtering at
// the read layer — where a session's emptiness is finally knowable — keeps
// the markers on real sessions while hiding the probes.
const emptySessionMarkers = `'instructions_loaded','config_change','session_start','session_end','setup','notification'`

// nonEmptySessionPredicate{S,Sessions} are SQL boolean expressions (no
// bound args) that are true only for sessions with real content: at least
// one substantive action, or any token_usage / api_turns row. They are
// compile-time constants (string-literal + const concat) so they fold to
// a single constant and never trip gosec's G202 SQL-concat taint check.
// Two variants because the enclosing queries use different sessions-table
// aliases: "s" for handleSessions' joined query, "sessions" for the
// unaliased overview / calendar counts.
const nonEmptySessionPredicateS = `(EXISTS (SELECT 1 FROM actions a WHERE a.session_id = s.id` +
	` AND a.action_type NOT IN (` + emptySessionMarkers + `))` +
	` OR EXISTS (SELECT 1 FROM token_usage tu WHERE tu.session_id = s.id)` +
	` OR EXISTS (SELECT 1 FROM api_turns t WHERE t.session_id = s.id))`

const nonEmptySessionPredicateSessions = `(EXISTS (SELECT 1 FROM actions a WHERE a.session_id = sessions.id` +
	` AND a.action_type NOT IN (` + emptySessionMarkers + `))` +
	` OR EXISTS (SELECT 1 FROM token_usage tu WHERE tu.session_id = sessions.id)` +
	` OR EXISTS (SELECT 1 FROM api_turns t WHERE t.session_id = sessions.id))`

// parseSessionsSortParams reads sort_by / sort_dir from the request, clamping
// sort_by to a fixed allow-list (defaulting to started_at) so the value is
// never interpolated unchecked into SQL. Direction defaults to descending; only
// the literal "asc" flips it.
func parseSessionsSortParams(r *http.Request) (sortBy string, desc bool) {
	sortBy = strings.ToLower(strings.TrimSpace(r.URL.Query().Get("sort_by")))
	switch sortBy {
	case "session", "tool", "project", "started_at", "elapsed", "actions",
		"input", "cache_r", "cache_w", "output", "cost", "quality",
		"errors", "redundancy":
	default:
		sortBy = "started_at"
	}
	desc = strings.ToLower(strings.TrimSpace(r.URL.Query().Get("sort_dir"))) != "asc"
	return sortBy, desc
}

// sessionsSortRequiresInMemory reports whether a sort column is computed in Go
// (elapsed) or via the cost engine (token/cost buckets) and therefore needs the
// full filtered set loaded before paging, rather than an SQL ORDER BY + LIMIT.
func sessionsSortRequiresInMemory(sortBy string) bool {
	switch sortBy {
	case "elapsed", "input", "cache_r", "cache_w", "output", "cost":
		return true
	}
	return false
}

// sessionsSortRequiresCost reports whether the sort column needs the cost
// engine rollup attached before sorting (the token/cost buckets). "elapsed" is
// in-memory but does NOT need cost, so it is excluded here.
func sessionsSortRequiresCost(sortBy string) bool {
	switch sortBy {
	case "input", "cache_r", "cache_w", "output", "cost":
		return true
	}
	return false
}

// sessionsSQLOrderClause maps an allow-listed sort column to a SQL ORDER BY
// fragment (used only for the cheap, SQL-sortable columns). The sort column is
// never interpolated directly — it selects a fixed expression. A stable
// tiebreak on started_at DESC, id ASC keeps pagination deterministic.
func sessionsSQLOrderClause(sortBy string, desc bool) string {
	dir := "ASC"
	if desc {
		dir = "DESC"
	}
	expr := "s.started_at"
	switch sortBy {
	case "session":
		expr = "s.id"
	case "tool":
		expr = "s.tool"
	case "project":
		expr = "COALESCE(p.root_path, '')"
	case "actions":
		expr = "total_actions"
	case "quality":
		expr = "COALESCE(s.quality_score, -1.0)"
	case "errors":
		expr = "COALESCE(s.error_rate, -1.0)"
	case "redundancy":
		expr = "COALESCE(s.redundancy_ratio, -1.0)"
	}
	return expr + " " + dir + ", s.started_at DESC, s.id ASC"
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	limit := intArg(r, "limit", 20, 1, 500)
	page := intArg(r, "page", 1, 1, 1_000_000)
	offset := (page - 1) * limit
	tool := r.URL.Query().Get("tool")
	project := r.URL.Query().Get("project")
	// days=0 (or missing) means "no time filter" — preserves the prior
	// behaviour for callers that haven't been updated. Frontend always
	// passes the global window; CLI / older API consumers may not.
	days := intArg(r, "days", 0, 0, 36500)
	// from_date / to_date — YYYY-MM-DD prefix filter against
	// substr(s.started_at, 1, 10). Mirrors the /api/actions params so
	// the Sessions Calendar day-click can server-side scope to that
	// day's sessions instead of substring-filtering the loaded page
	// (which silently dropped any day outside the page-50 slice and
	// produced a misleading "No sessions match" empty state for any
	// day older than the loaded rows).
	fromDate := r.URL.Query().Get("from_date")
	toDate := r.URL.Query().Get("to_date")

	// Server-side sort. Cheap columns sort in SQL; columns computed in Go
	// (elapsed) or via the cost engine (token/cost buckets) need the full
	// filtered set attached + sorted before paging — otherwise the page-local
	// client sort surfaces "the most expensive of the visible 20", not the
	// most expensive session overall.
	sortBy, sortDesc := parseSessionsSortParams(r)
	inMemorySort := sessionsSortRequiresInMemory(sortBy)
	costSort := sessionsSortRequiresCost(sortBy)

	// Build optional WHERE clause over sessions + a project-id lookup.
	var where []string
	var args []any
	if tool != "" {
		where = append(where, "s.tool = ?")
		args = append(args, tool)
	}
	if project != "" {
		where = append(where, "s.project_id = (SELECT id FROM projects WHERE root_path = ?)")
		args = append(args, project)
	}
	if days > 0 {
		since := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour).Format(time.RFC3339Nano)
		// Reconcile the Sessions window with the cost engine. /api/cost and
		// /api/models window by turn/token TIMESTAMP (cost/summary.go), so a
		// long-running session that STARTED before the window but has RECENT
		// turns contributes there. Windowing on started_at alone would exclude
		// it here, making the Sessions tab under-count vs the Cost tab. Include
		// a session if it started in-window OR has any in-window activity
		// (action / api_turn / token_usage). The shared `where` slice carries
		// this predicate into the pagination `total` COUNT and scored_count too,
		// so the page math stays coherent. (A row dated >N days ago can thus
		// surface in an N-day view; its metadata reflects the whole session,
		// its cost is windowed — consistent with /api/cost.)
		where = append(where, `(s.started_at >= ?
			OR EXISTS (SELECT 1 FROM actions a2 WHERE a2.session_id = s.id AND a2.timestamp >= ?)
			OR EXISTS (SELECT 1 FROM api_turns at2 WHERE at2.session_id = s.id AND at2.timestamp >= ?)
			OR EXISTS (SELECT 1 FROM token_usage tu2 WHERE tu2.session_id = s.id AND tu2.timestamp >= ?))`)
		args = append(args, since, since, since, since)
	}
	if fromDate != "" {
		where = append(where, "substr(s.started_at, 1, 10) >= ?")
		args = append(args, fromDate)
	}
	if toDate != "" {
		where = append(where, "substr(s.started_at, 1, 10) <= ?")
		args = append(args, toDate)
	}
	// Hide contentless probe sessions (marker-only, no tokens/turns). Added
	// to the shared `where` so the data query, the pagination `total`, and
	// the `scored_count` all agree on which sessions exist.
	where = append(where, nonEmptySessionPredicateS)
	whereClause := ""
	if len(where) > 0 {
		whereClause = "WHERE " + strings.Join(where, " AND ")
	}

	// Total row count for pagination. Must share the same WHERE as the
	// data query so page math stays coherent.
	var total int
	countArgs := append([]any{}, args...)
	if err := s.db().QueryRowContext(
		r.Context(),
		"SELECT COUNT(*) FROM sessions s "+whereClause, countArgs...,
	).Scan(&total); err != nil {
		writeErr(w, err)
		return
	}

	// scored_count tells the frontend whether to render the Quality /
	// Errors / Redundancy columns. None of those fields are populated
	// unless `observer score` has run — pre-fix the columns rendered
	// dashes for every row, wasting horizontal space and misleading
	// users into thinking scoring is unsupported. Same WHERE as `total`
	// so the count is consistent with the visible filter.
	var scoredCount int
	_ = s.db().QueryRowContext(
		r.Context(),
		"SELECT COUNT(*) FROM sessions s "+whereClause+
			func() string {
				if whereClause == "" {
					return "WHERE s.quality_score IS NOT NULL"
				}
				return " AND s.quality_score IS NOT NULL"
			}(),
		countArgs...,
	).Scan(&scoredCount)

	// total_actions is computed live; the sessions.total_actions stored
	// column is never advanced past 0 by any writer (UpsertSession's MAX
	// merge keeps it at whatever the first batch wrote, scoring computes
	// len(actions) only into a transient struct). Subquery is cheap at
	// LIMIT 20 and avoids a stale-column class of bug.
	dataArgs := append([]any{}, args...)
	// Cheap columns sort + page in SQL. In-memory columns (elapsed / token /
	// cost buckets) load the FULL filtered set ordered by started_at and are
	// sorted + paginated in Go below, after the per-session cost/tokens attach.
	orderAndLimit := "ORDER BY " + sessionsSQLOrderClause(sortBy, sortDesc) + " LIMIT ? OFFSET ?"
	if inMemorySort {
		orderAndLimit = "ORDER BY s.started_at DESC, s.id ASC"
	} else {
		dataArgs = append(dataArgs, limit, offset)
	}
	// last_seen falls back to MAX(actions.timestamp) when ended_at is
	// NULL (open session) so DurationSeconds is still meaningful for
	// in-flight sessions. Subqueries are cheap at LIMIT 20.
	rows, err := s.db().QueryContext(r.Context(),
		//nolint:gosec // G202: SQL structure (WHERE/JOIN/scope fragments, ORDER BY column from a fixed allow-list, and any IN placeholder list) is built from code constants; all values are bound via ? args.
		`SELECT s.id, s.tool, COALESCE(p.root_path, ''), s.started_at,
		        COALESCE(s.ended_at,
		                 (SELECT MAX(a.timestamp) FROM actions a WHERE a.session_id = s.id),
		                 '') AS last_seen_at,
		        (SELECT COUNT(*) FROM actions a WHERE a.session_id = s.id) AS total_actions,
		        (SELECT COUNT(*) FROM actions a WHERE a.session_id = s.id AND a.is_sidechain = 1) AS sidechain_actions,
		        s.quality_score, s.error_rate, s.redundancy_ratio,
		        s.redundancy_ratio_wasteful, s.stale_reads_wasteful, s.stale_reads_necessary
		 FROM sessions s
		 LEFT JOIN projects p ON p.id = s.project_id
		 `+whereClause+` `+orderAndLimit, dataArgs...)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	type sessRow struct {
		ID        string `json:"id"`
		Tool      string `json:"tool"`
		Project   string `json:"project"`
		StartedAt string `json:"started_at"`
		// LastSeenAt is COALESCE(ended_at, MAX(actions.timestamp)). Drives
		// DurationSeconds for both closed and still-open sessions.
		LastSeenAt string `json:"last_seen_at,omitempty"`
		// DurationSeconds = LastSeenAt - StartedAt, computed server-side
		// so the frontend formatter doesn't need to parse timestamps.
		// Zero when LastSeenAt is empty (no actions yet) or when start
		// is unparseable. Surfaced as "Elapsed" in the Sessions table.
		DurationSeconds int64 `json:"duration_seconds"`
		TotalActions    int   `json:"total_actions"`
		// SidechainActionCount is the count of actions emitted inside
		// any sub-agent runtime spawned by this session (Claude Code's
		// `Agent` tool). Sub-agents share the parent's session_id;
		// this is the only structural marker. > 0 implies the session
		// fanned out work to sub-agents — surfaced as a "sidechain N"
		// pill on the Sessions tab.
		SidechainActionCount int      `json:"sidechain_action_count"`
		QualityScore         *float64 `json:"quality_score,omitempty"`
		ErrorRate            *float64 `json:"error_rate,omitempty"`
		RedundancyRatio      *float64 `json:"redundancy_ratio,omitempty"`
		// Spec §14.1 wasteful-subset (nil when the session has
		// no cache_events).
		RedundancyRatioWasteful *float64 `json:"redundancy_ratio_wasteful,omitempty"`
		StaleReadsWasteful      *int     `json:"stale_reads_wasteful,omitempty"`
		StaleReadsNecessary     *int     `json:"stale_reads_necessary,omitempty"`
		// Token breakdown — attached post-scan from the cost engine's
		// GroupBySession rollup so dedup (proxy preferred, JSONL
		// fallback) matches /api/cost exactly. v1.4.51 surfaces all
		// four billable buckets separately so the Sessions table can
		// show Input / Cache R / Cache W / Output as distinct columns.
		// TotalTokens is the sum for backwards compatibility with
		// older callers; the Sessions table doesn't render it as of
		// v1.4.51.
		InputTokens         int64 `json:"input_tokens"`
		OutputTokens        int64 `json:"output_tokens"`
		CacheReadTokens     int64 `json:"cache_read_tokens"`
		CacheCreationTokens int64 `json:"cache_creation_tokens"`
		// CacheCreation1hTokens is the 1h-ephemeral-tier subset of
		// CacheCreationTokens (the rest is implicitly 5m-tier).
		// Surfaced so the Sessions table's "Cache W" column can show
		// the per-row 5m/1h split as a hover tooltip — visualisation
		// of why a session billed at the higher-tier cache-write rate.
		// Anthropic-only field; non-Anthropic providers always emit 0.
		CacheCreation1hTokens int64 `json:"cache_creation_1h_tokens"`
		ReasoningTokens       int64 `json:"reasoning_tokens,omitempty"`
		WebSearchRequests     int64 `json:"web_search_requests,omitempty"`
		TotalTokens           int64 `json:"total_tokens"`
		// CostUSD is the legacy total; AICostUSD + ToolCostUSD split
		// it so the Sessions table can show "API cost vs tool cost vs
		// total" separately. CostUSD == AICostUSD + ToolCostUSD.
		CostUSD     float64 `json:"cost_usd"`
		AICostUSD   float64 `json:"ai_cost_usd"`
		ToolCostUSD float64 `json:"tool_cost_usd"`
		// CostReliability is the worst-case reliability across the
		// rows that fed this session's totals. Surfaces as a pill on
		// the Sessions table so users know which numbers to trust.
		CostReliability string `json:"cost_reliability,omitempty"`
		// Models is the distinct set of model identifiers seen across
		// this session's api_turns + token_usage rows, ordered by turn
		// count (heaviest first). Enables the Sessions table's Model(s)
		// column to render a primary chip + "+N more" affordance and
		// the Overview Recent sessions list to show which model the
		// session leaned on. Empty when no proxy/JSONL rows captured
		// a model (rare; usually means scraped fallback).
		Models []string `json:"models,omitempty"`
	}
	var out []sessRow
	for rows.Next() {
		var sr sessRow
		var q, er, rr sql.NullFloat64
		var rrWasteful sql.NullFloat64
		var stWasteful, stNecessary sql.NullInt64
		if err := rows.Scan(&sr.ID, &sr.Tool, &sr.Project, &sr.StartedAt, &sr.LastSeenAt,
			&sr.TotalActions, &sr.SidechainActionCount, &q, &er, &rr,
			&rrWasteful, &stWasteful, &stNecessary); err != nil {
			writeErr(w, err)
			return
		}
		if sr.LastSeenAt != "" {
			start, sErr := time.Parse(time.RFC3339Nano, sr.StartedAt)
			end, eErr := time.Parse(time.RFC3339Nano, sr.LastSeenAt)
			if sErr == nil && eErr == nil && end.After(start) {
				sr.DurationSeconds = int64(end.Sub(start).Seconds())
			}
		}
		if q.Valid {
			v := q.Float64
			sr.QualityScore = &v
		}
		if er.Valid {
			v := er.Float64
			sr.ErrorRate = &v
		}
		if rr.Valid {
			v := rr.Float64
			sr.RedundancyRatio = &v
		}
		if rrWasteful.Valid {
			v := rrWasteful.Float64
			sr.RedundancyRatioWasteful = &v
		}
		if stWasteful.Valid {
			v := int(stWasteful.Int64)
			sr.StaleReadsWasteful = &v
		}
		if stNecessary.Valid {
			v := int(stNecessary.Int64)
			sr.StaleReadsNecessary = &v
		}
		out = append(out, sr)
	}
	if err := rows.Err(); err != nil {
		writeErr(w, err)
		return
	}
	if out == nil {
		out = []sessRow{}
	}

	// Attach per-session token totals + cost from the cost engine, then (for
	// in-memory sort columns) sort the full filtered set and slice to the page.
	//
	// The window matches the days query param so the Sessions-page per-session
	// cost sum equals the Cost-page /api/models total (the v1.6.3 → v1.6.4
	// reconciliation fix). When days=0 (no time filter) the cost rollup spans
	// full history (Days=36500), keeping CLI callers correct.
	costDays := days
	if costDays == 0 {
		costDays = 36500
	}

	// attachCost rolls up per-session cost + tokens and stamps them onto rows.
	// scopeIDs limits the engine to those session_ids (the cheap page-scoped
	// path); pass nil to roll up the whole window — needed when a cost/token
	// sort column requires the full filtered set BEFORE paging, since the byID
	// map then filters the rollup back to the rows we hold.
	attachCost := func(rows []sessRow, scopeIDs []string) {
		if len(rows) == 0 {
			return
		}
		costSummary, err := s.opts.CostEngine.Summary(r.Context(), s.db(), cost.Options{
			Days:        costDays,
			GroupBy:     cost.GroupBySession,
			Source:      cost.SourceAuto,
			ProjectRoot: project,
			Tool:        tool,
			SessionIDs:  scopeIDs,
			Limit:       1_000_000,
		})
		if err != nil {
			s.opts.Logger.Warn("sessions: per-session cost rollup failed", "err", err)
			return
		}
		byID := make(map[string]cost.Row, len(costSummary.Rows))
		for _, row := range costSummary.Rows {
			byID[row.Key] = row
		}
		for i := range rows {
			row, ok := byID[rows[i].ID]
			if !ok {
				continue
			}
			rows[i].InputTokens = row.Tokens.Input
			rows[i].OutputTokens = row.Tokens.Output
			rows[i].CacheReadTokens = row.Tokens.CacheRead
			rows[i].CacheCreationTokens = row.Tokens.CacheCreation
			rows[i].CacheCreation1hTokens = row.Tokens.CacheCreation1h
			rows[i].ReasoningTokens = row.Tokens.Reasoning
			rows[i].WebSearchRequests = row.Tokens.WebSearchRequests
			rows[i].TotalTokens = row.Tokens.Input + row.Tokens.Output +
				row.Tokens.CacheRead + row.Tokens.CacheCreation
			rows[i].CostUSD = row.CostUSD
			rows[i].AICostUSD = row.AICostUSD
			rows[i].ToolCostUSD = row.ToolCostUSD
			rows[i].CostReliability = row.Reliability
		}
	}

	costAttached := false
	if inMemorySort {
		if costSort {
			// Cost/token columns: roll up the WHOLE filtered set so the sort is
			// global, not page-local.
			attachCost(out, nil)
			costAttached = true
		}
		// Sort the full filtered set in Go. Numeric key per column; stable
		// tiebreak on started_at DESC, id ASC mirrors the SQL clause so paging
		// is deterministic.
		sortKey := func(sr sessRow) float64 {
			switch sortBy {
			case "elapsed":
				return float64(sr.DurationSeconds)
			case "input":
				return float64(sr.InputTokens)
			case "cache_r":
				return float64(sr.CacheReadTokens)
			case "cache_w":
				return float64(sr.CacheCreationTokens)
			case "output":
				return float64(sr.OutputTokens)
			case "cost":
				return sr.CostUSD
			}
			return 0
		}
		sort.SliceStable(out, func(i, j int) bool {
			ki, kj := sortKey(out[i]), sortKey(out[j])
			if ki != kj {
				if sortDesc {
					return ki > kj
				}
				return ki < kj
			}
			if out[i].StartedAt != out[j].StartedAt {
				return out[i].StartedAt > out[j].StartedAt
			}
			return out[i].ID < out[j].ID
		})
		// Slice to the requested page.
		start := offset
		if start > len(out) {
			start = len(out)
		}
		end := start + limit
		if end > len(out) {
			end = len(out)
		}
		out = out[start:end]
	}

	pageIDs := make([]string, len(out))
	for i, sr := range out {
		pageIDs[i] = sr.ID
	}
	if !costAttached {
		// Cheap-SQL-sort and elapsed-sort pages: attach cost scoped to the
		// page's session_ids (no whole-window rollup needed).
		attachCost(out, pageIDs)
	}

	// Attach per-session model list for the page — one query batches across the
	// page's session IDs and unions api_turns + token_usage. Models are ordered
	// by turn count desc so out[i].Models[0] is the session's "primary" model
	// (heaviest by count). Both source tables index session_id; cheap at
	// LIMIT ≤ 500.
	if len(out) > 0 {
		ids := make([]any, 0, len(pageIDs))
		for _, id := range pageIDs {
			ids = append(ids, id)
		}
		placeholders := strings.Repeat("?,", len(ids))
		placeholders = strings.TrimRight(placeholders, ",")
		modelArgs := append(append([]any{}, ids...), ids...)
		modelRows, mErr := s.db().QueryContext(r.Context(),
			//nolint:gosec // G202: SQL structure (WHERE/JOIN/scope fragments and any IN placeholder list) is built from code constants; all values are bound via ? args.
			`SELECT session_id, model, SUM(c) AS turns FROM (
				SELECT session_id, COALESCE(model, '') AS model, COUNT(*) AS c
				 FROM api_turns
				 WHERE session_id IN (`+placeholders+`) AND COALESCE(model, '') != ''
				 GROUP BY session_id, model
				UNION ALL
				SELECT session_id, COALESCE(model, '') AS model, COUNT(*) AS c
				 FROM token_usage
				 WHERE session_id IN (`+placeholders+`) AND COALESCE(model, '') != ''
				 GROUP BY session_id, model
			) GROUP BY session_id, model ORDER BY session_id, turns DESC, model ASC`,
			modelArgs...)
		if mErr == nil {
			modelsBySession := make(map[string][]string, len(out))
			for modelRows.Next() {
				var sid, model string
				var turns int64
				if err := modelRows.Scan(&sid, &model, &turns); err != nil {
					continue
				}
				modelsBySession[sid] = append(modelsBySession[sid], model)
			}
			_ = modelRows.Close()
			for i, sr := range out {
				if ms, ok := modelsBySession[sr.ID]; ok {
					out[i].Models = ms
				}
			}
		} else {
			s.opts.Logger.Warn("sessions: per-session model list failed", "err", mErr)
		}
	}

	// Page footer totals so the frontend footer reconciles with the visible
	// rows even when the global sort surfaced a different slice.
	var pageCost, pageAICost, pageToolCost float64
	for _, sr := range out {
		pageCost += sr.CostUSD
		pageAICost += sr.AICostUSD
		pageToolCost += sr.ToolCostUSD
	}
	sortDir := "asc"
	if sortDesc {
		sortDir = "desc"
	}

	writeJSON(w, map[string]any{
		"rows":               out,
		"page":               page,
		"limit":              limit,
		"total":              total,
		"scored_count":       scoredCount,
		"days":               days,
		"sort_by":            sortBy,
		"sort_dir":           sortDir,
		"page_cost_usd":      pageCost,
		"page_ai_cost_usd":   pageAICost,
		"page_tool_cost_usd": pageToolCost,
	})
}

// handleSessionsCalendar — GET /api/sessions/calendar?days=N
//
// Returns one row per day across the window: {day, session_count,
// cost_usd}. Dashboard's Calendar view consumes this so the grid
// spans the full Window with real per-day distribution instead of
// the most recent page-50 slice. Session counts come from a GROUP
// BY date(started_at) over sessions; costs come from the cost engine
// rolled up GroupByDay over the same window.
func (s *Server) handleSessionsCalendar(w http.ResponseWriter, r *http.Request) {
	days := intArg(r, "days", 30, 1, 365)
	tool := r.URL.Query().Get("tool")
	project := r.URL.Query().Get("project")
	since := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)

	// Session count per day.
	var where []string
	args := []any{since.Format(time.RFC3339Nano)}
	where = append(where, "started_at >= ?")
	if tool != "" {
		where = append(where, "tool = ?")
		args = append(args, tool)
	}
	if project != "" {
		where = append(where, "project_id = (SELECT id FROM projects WHERE root_path = ?)")
		args = append(args, project)
	}
	rows, err := s.db().QueryContext(r.Context(),
		//nolint:gosec // G202: SQL structure (WHERE/JOIN/scope fragments and any IN placeholder list) is built from code constants; all values are bound via ? args.
		`SELECT substr(started_at, 1, 10) AS day, COUNT(*) AS n
		   FROM sessions
		  WHERE `+strings.Join(where, " AND ")+` AND `+nonEmptySessionPredicateSessions+`
		  GROUP BY day
		  ORDER BY day`, args...)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()

	type cell struct {
		Day          string  `json:"day"`
		SessionCount int     `json:"session_count"`
		CostUSD      float64 `json:"cost_usd"`
	}
	byDay := map[string]*cell{}
	order := []string{}
	for rows.Next() {
		var day string
		var n int
		if err := rows.Scan(&day, &n); err != nil {
			writeErr(w, err)
			return
		}
		byDay[day] = &cell{Day: day, SessionCount: n}
		order = append(order, day)
	}
	if err := rows.Err(); err != nil {
		writeErr(w, err)
		return
	}

	// Cost per day — cost.Summary with GroupByDay covers turn-date
	// bucketing across the same window, joined back onto the session
	// day map. A session that ran across midnight will have its turns
	// land on multiple days; that's expected behaviour and matches
	// the daily cost shown on /cost.
	costSummary, err := s.opts.CostEngine.Summary(r.Context(), s.db(), cost.Options{
		Days:        days,
		GroupBy:     cost.GroupByDay,
		Source:      cost.SourceAuto,
		ProjectRoot: project,
		Tool:        tool, // align per-day cost with the tool-filtered session count above
		Limit:       365,
	})
	if err == nil {
		for _, row := range costSummary.Rows {
			c, ok := byDay[row.Key]
			if !ok {
				c = &cell{Day: row.Key}
				byDay[row.Key] = c
				order = append(order, row.Key)
			}
			c.CostUSD = row.CostUSD
		}
	} else {
		s.opts.Logger.Warn("sessions calendar: cost rollup failed", "err", err)
	}

	out := make([]cell, 0, len(order))
	seen := map[string]bool{}
	for _, k := range order {
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, *byDay[k])
	}
	// Stable sort by day ascending so the frontend can iterate the
	// returned slice in order regardless of insertion order.
	sort.Slice(out, func(i, j int) bool { return out[i].Day < out[j].Day })
	writeJSON(w, map[string]any{
		"days":  days,
		"cells": out,
	})
}

// loadActionExcerpts fetches the first action_excerpts.excerpt for each
// id, truncated to maxBytes when > 0. Returns map[action_id] -> excerpt.
//
// action_excerpts is an FTS5 virtual table with action_id declared
// UNINDEXED, so there's no b-tree on action_id and SQLite must fall back
// to a full virtual-table SCAN for every (action_id = ?) probe. A
// correlated subquery in the SELECT list or a LEFT JOIN therefore costs
// O(N rows × M excerpts) — empirically ~22s for 500 rows on an 81k-action
// DB, and ~136s for the 1772-action session messages view. The batch IN
// form below pays one ~50ms scan regardless of |ids|, then filters
// in-memory. The map's "first wins" semantic preserves the
// `LIMIT 1`/`COALESCE(ae.excerpt, ”)` behavior of the original queries
// (action_excerpts can hold multiple rows per action_id when the same
// action was re-indexed).
func loadActionExcerpts(ctx context.Context, db *sql.DB, ids []int64, maxBytes int) (map[int64]string, error) {
	out := make(map[int64]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	var q string
	if maxBytes > 0 {
		q = fmt.Sprintf("SELECT action_id, substr(excerpt, 1, %d) FROM action_excerpts WHERE action_id IN (%s)", maxBytes, placeholders)
	} else {
		q = "SELECT action_id, excerpt FROM action_excerpts WHERE action_id IN (" + placeholders + ")"
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var excerpt string
		if err := rows.Scan(&id, &excerpt); err != nil {
			return nil, err
		}
		if _, ok := out[id]; !ok {
			out[id] = excerpt
		}
	}
	return out, rows.Err()
}

func (s *Server) handleActions(w http.ResponseWriter, r *http.Request) {
	limit := intArg(r, "limit", 50, 1, 500)
	page := intArg(r, "page", 1, 1, 1_000_000)
	offset := (page - 1) * limit
	tool := r.URL.Query().Get("tool")
	sessionID := r.URL.Query().Get("session_id")
	actionType := r.URL.Query().Get("action_type")
	project := r.URL.Query().Get("project")
	// v1.4.48: metadata filters land on the migration-017 actions.metadata
	// JSON column via SQLite's json_extract (no JSON1 dependency added —
	// modernc.org/sqlite ships it). Empty params skip the filter entirely
	// so the legacy /api/actions surface is unchanged for callers that
	// don't pass them.
	effortLevel := r.URL.Query().Get("effort_level")
	permissionMode := r.URL.Query().Get("permission_mode")
	isInterrupt := r.URL.Query().Get("is_interrupt")
	// v1.4.49: assistant_text filter surfaces "what did the AI say to the
	// user?" rows from any adapter. The multi-pattern OR-chain
	// accommodates the RawToolName convention drift documented in
	// docs/handover-v1.4.49 — new wirings use `<source>.assistant_text`,
	// legacy precedents stay as-is (Pi's `message.assistant.<stopReason>`,
	// Copilot's `agent_response`, Antigravity's `structured.assistant_text`,
	// openclaw's `message.assistant.stop`).
	assistantText := r.URL.Query().Get("assistant_text")
	// Date filters — accept YYYY-MM-DD prefix matching against
	// substr(a.timestamp, 1, 10). The Timeline view passes from_date
	// = to_date when the user picks a single day from the day strip.
	fromDate := r.URL.Query().Get("from_date")
	toDate := r.URL.Query().Get("to_date")

	var where []string
	var args []any
	if tool != "" {
		where = append(where, "a.tool = ?")
		args = append(args, tool)
	}
	if sessionID != "" {
		where = append(where, "a.session_id = ?")
		args = append(args, sessionID)
	}
	if actionType != "" {
		where = append(where, "a.action_type = ?")
		args = append(args, actionType)
	}
	if project != "" {
		where = append(where, "a.project_id = (SELECT id FROM projects WHERE root_path = ?)")
		args = append(args, project)
	}
	if fromDate != "" {
		where = append(where, "substr(a.timestamp, 1, 10) >= ?")
		args = append(args, fromDate)
	}
	if toDate != "" {
		where = append(where, "substr(a.timestamp, 1, 10) <= ?")
		args = append(args, toDate)
	}
	if effortLevel != "" {
		where = append(where, "json_extract(a.metadata, '$.effort_level') = ?")
		args = append(args, effortLevel)
	}
	if permissionMode != "" {
		where = append(where, "json_extract(a.metadata, '$.permission_mode') = ?")
		args = append(args, permissionMode)
	}
	if isInterrupt == "1" {
		// SQLite's json_extract on a JSON boolean returns 1/0 (integer)
		// — compare against 1 not "true". Rows where metadata is NULL or
		// is_interrupt is absent return NULL from json_extract, which
		// fails the equality and is correctly excluded.
		where = append(where, "json_extract(a.metadata, '$.is_interrupt') = 1")
	}
	if assistantText == "1" {
		// `<source>.assistant_text` covers new wirings (codex / cline /
		// roo-code / claudecode / cursor / gemini / opencode / openclaw).
		// `structured.assistant_text` is Antigravity's pre-existing
		// RawToolName. `message.assistant.<stopReason>` is Pi.
		// `message.assistant.stop` is OpenClaw's legacy marker row.
		// `agent_response` is Copilot's pre-existing RawToolName. All
		// four legacy names are left alone per the v1.4.49 convention
		// decision to avoid SourceEventID dedup churn.
		where = append(where, `(
			a.raw_tool_name LIKE '%.assistant_text'
			OR a.raw_tool_name LIKE 'message.assistant.%'
			OR a.raw_tool_name = 'agent_response'
		)`)
	}
	whereClause := ""
	if len(where) > 0 {
		whereClause = "WHERE " + strings.Join(where, " AND ")
	}

	var total int
	countArgs := append([]any{}, args...)
	if err := s.db().QueryRowContext(
		r.Context(),
		"SELECT COUNT(*) FROM actions a "+whereClause, countArgs...,
	).Scan(&total); err != nil {
		writeErr(w, err)
		return
	}

	dataArgs := append([]any{}, args...)
	dataArgs = append(dataArgs, limit, offset)
	// Excerpt is loaded in a second batch query — see loadActionExcerpts
	// for why an inline subquery is O(N×M) on the FTS5 action_excerpts
	// table.
	rows, err := s.db().QueryContext(r.Context(),
		//nolint:gosec // G202: SQL structure (WHERE/JOIN/scope fragments and any IN placeholder list) is built from code constants; all values are bound via ? args.
		`SELECT a.id, a.timestamp, a.tool, a.session_id,
		        COALESCE(p.root_path, ''), a.action_type,
		        COALESCE(a.raw_tool_name, ''), COALESCE(a.target, ''),
		        COALESCE(a.success, 1), COALESCE(a.error_message, ''),
		        COALESCE(a.message_id, ''),
		        COALESCE(json_extract(a.metadata, '$.permission_mode'), '') AS permission_mode,
		        COALESCE(json_extract(a.metadata, '$.effort_level'), '') AS effort_level,
		        COALESCE(json_extract(a.metadata, '$.is_interrupt'), 0) AS is_interrupt,
		        COALESCE(json_extract(a.metadata, '$.stop_reason'), '') AS stop_reason,
		        COALESCE(json_extract(a.metadata, '$.service_tier'), '') AS service_tier,
		        COALESCE(a.source_file, ''),
		        COALESCE(a.source_event_id, '')
		 FROM actions a
		 LEFT JOIN projects p ON p.id = a.project_id
		 `+whereClause+`
		 ORDER BY a.timestamp DESC, a.id DESC LIMIT ? OFFSET ?`, dataArgs...)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	type actionRow struct {
		ID           int64  `json:"id"`
		Timestamp    string `json:"timestamp"`
		Tool         string `json:"tool"`
		SessionID    string `json:"session_id"`
		Project      string `json:"project"`
		ActionType   string `json:"action_type"`
		RawToolName  string `json:"raw_tool_name"`
		Target       string `json:"target"`
		Success      bool   `json:"success"`
		ErrorMessage string `json:"error_message,omitempty"`
		// MessageID is the upstream Anthropic msg_xxx id for the API
		// turn that produced this action (populated by the claudecode
		// adapter, the message-id backfill, and the api_error path).
		// For user_prompt rows it carries the synthesized "user:<id>"
		// form; for tool_use rows the parent assistant message's id.
		// Lets the Actions tab link a row back to the per-message
		// timeline modal via the same id surfaced on the Compression
		// events table.
		MessageID string `json:"message_id"`
		// Per-event metadata extracted from actions.metadata JSON
		// (migration 017). Empty / false when the row pre-dates
		// the migration or the source adapter didn't emit the
		// field. omitempty keeps the response payload lean.
		PermissionMode string `json:"permission_mode,omitempty"`
		EffortLevel    string `json:"effort_level,omitempty"`
		IsInterrupt    bool   `json:"is_interrupt,omitempty"`
		// StopReason — why the assistant turn ended (end_turn / max_tokens
		// / tool_use / stop_sequence / refusal). ServiceTier — the served
		// capacity tier (standard / priority / batch). Both per-message,
		// captured from the transcript (claude-code, cowork). Empty for
		// rows that pre-date capture or adapters that don't emit them.
		StopReason  string `json:"stop_reason,omitempty"`
		ServiceTier string `json:"service_tier,omitempty"`
		// SourceFile / SourceEventID — provenance for this row. Tells
		// the user which JSONL or proxy capture produced the event.
		// SourceFile may be empty for synthesized rows (e.g. hook
		// closures) where the adapter doesn't track a file origin.
		SourceFile    string `json:"source_file,omitempty"`
		SourceEventID string `json:"source_event_id,omitempty"`
		// Excerpt — first 280 chars of the action's indexed body from
		// action_excerpts. Lets the Actions table surface "what did
		// the tool actually do" inline without a row-expand click;
		// the full text remains retrievable via /api/actions/<id> when
		// that endpoint lands.
		Excerpt string `json:"excerpt,omitempty"`
	}
	var out []actionRow
	for rows.Next() {
		var ar actionRow
		var isInterrupt int
		if err := rows.Scan(&ar.ID, &ar.Timestamp, &ar.Tool, &ar.SessionID, &ar.Project,
			&ar.ActionType, &ar.RawToolName, &ar.Target, &ar.Success, &ar.ErrorMessage,
			&ar.MessageID, &ar.PermissionMode, &ar.EffortLevel, &isInterrupt,
			&ar.StopReason, &ar.ServiceTier,
			&ar.SourceFile, &ar.SourceEventID); err != nil {
			writeErr(w, err)
			return
		}
		ar.IsInterrupt = isInterrupt != 0
		out = append(out, ar)
	}
	if err := rows.Err(); err != nil {
		writeErr(w, err)
		return
	}
	if out == nil {
		out = []actionRow{}
	}
	ids := make([]int64, len(out))
	for i, r := range out {
		ids[i] = r.ID
	}
	excerpts, err := loadActionExcerpts(r.Context(), s.db(), ids, 280)
	if err != nil {
		writeErr(w, err)
		return
	}
	for i := range out {
		out[i].Excerpt = excerpts[out[i].ID]
	}
	writeJSON(w, map[string]any{
		"rows":  out,
		"page":  page,
		"limit": limit,
		"total": total,
	})
}

// handleActionsDayCounts — GET /api/actions/day-counts?days=N
//
// fullTextInlineMax is the per-row preview cap embedded in
// /api/session/<id>/messages. Anything longer is truncated to this
// length and surfaced with full_text_elided=true so the frontend
// knows to fetch the untruncated body via /api/action/<id>/full_text
// only when the operator actually clicks copy / view. Keeps the
// timeline payload bounded regardless of how large any single row's
// raw_tool_input grows post-migration-027.
const fullTextInlineMax = 4000

// handleActionDetail handles /api/action/<id>/<sub>. The only currently
// supported sub-resource is `full_text`, which returns the untruncated
// raw_tool_input + raw_tool_output for an action so the dashboard's
// copy and view-full-text buttons can fetch on demand instead of
// embedding multi-MB blobs in every /messages response.
//
// Bounded by the adapter-side internal/contentcap.DefaultMaxBytes
// (1 MiB per column); rows that overflowed adapter capture carry the
// trailing "…(content truncated at N bytes)…" marker so the operator
// can tell the served body is itself a truncation.
func (s *Server) handleActionDetail(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/action/")
	if !strings.HasSuffix(rest, "/full_text") {
		http.Error(w, "unsupported action sub-resource", http.StatusNotFound)
		return
	}
	idStr := strings.TrimSuffix(rest, "/full_text")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "missing or invalid action id", http.StatusBadRequest)
		return
	}
	var (
		actionType string
		target     string
		rawInput   sql.NullString
		rawOutput  sql.NullString
	)
	err = s.db().QueryRowContext(
		r.Context(),
		`SELECT action_type, COALESCE(target, ''), raw_tool_input, raw_tool_output
		   FROM actions WHERE id = ?`, id,
	).Scan(&actionType, &target, &rawInput, &rawOutput)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "action not found", http.StatusNotFound)
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	fullInput := rawInput.String
	if actionType == "run_command" && fullInput != "" {
		fullInput = decodeCommandInput(fullInput)
	}
	type resp struct {
		ActionID      int64  `json:"action_id"`
		ActionType    string `json:"action_type"`
		Target        string `json:"target,omitempty"`
		RawToolInput  string `json:"raw_tool_input,omitempty"`
		RawToolOutput string `json:"raw_tool_output,omitempty"`
	}
	writeJSON(w, resp{
		ActionID:      id,
		ActionType:    actionType,
		Target:        target,
		RawToolInput:  fullInput,
		RawToolOutput: rawOutput.String,
	})
}

// Returns one row per day in the window: {day, count}. Drives the
// Actions Timeline view's day strip so every day in the configured
// Window is selectable even when it lies outside the most-recent
// page-500 slice. Honors the same tool/project filters as
// /api/actions so the strip aligns with whatever's filtered.
func (s *Server) handleActionsDayCounts(w http.ResponseWriter, r *http.Request) {
	days := intArg(r, "days", 30, 1, 365)
	tool := r.URL.Query().Get("tool")
	project := r.URL.Query().Get("project")
	since := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)

	where := []string{"a.timestamp >= ?"}
	args := []any{since.Format(time.RFC3339Nano)}
	if tool != "" {
		where = append(where, "a.tool = ?")
		args = append(args, tool)
	}
	if project != "" {
		where = append(where, "a.project_id = (SELECT id FROM projects WHERE root_path = ?)")
		args = append(args, project)
	}
	rows, err := s.db().QueryContext(r.Context(),
		//nolint:gosec // G202: SQL structure (WHERE/JOIN/scope fragments and any IN placeholder list) is built from code constants; all values are bound via ? args.
		`SELECT substr(a.timestamp, 1, 10) AS day, COUNT(*) AS n
		   FROM actions a
		  WHERE `+strings.Join(where, " AND ")+`
		  GROUP BY day
		  ORDER BY day`, args...)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()

	type cell struct {
		Day   string `json:"day"`
		Count int    `json:"count"`
	}
	out := []cell{}
	for rows.Next() {
		var c cell
		if err := rows.Scan(&c.Day, &c.Count); err != nil {
			writeErr(w, err)
			return
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{
		"days":  days,
		"cells": out,
	})
}

// handleSessionDetail handles /api/session/<id>. Returns session metadata
// plus aggregate roll-ups (action counts, tool breakdown, token totals,
// per-model usage). Action list is NOT inlined — the frontend should
// follow-up with /api/actions?session_id=<id>&page=… for the paginated
// stream.
func (s *Server) handleSessionDetail(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/session/")
	// Sub-route: /api/session/<id>/messages → per-message timeline
	// (one row per upstream Anthropic message). Returns the deduped
	// per-turn breakdown with grouped tool calls. Used by the
	// session modal's Messages panel.
	if strings.HasSuffix(id, "/messages") {
		id = strings.TrimSuffix(id, "/messages")
		s.handleSessionMessages(w, r, id)
		return
	}
	// Sub-route: /api/session/<id>/cache/forecast → model-switch
	// cost forecaster (spec §14.2). Pure read-side math over the
	// existing tables — P from cache_entries, S/gaps from
	// cache_events, O/T/fast from api_turns, rates from cost.Table.
	// Returns the headline switch_cost + break_even_turns + per-
	// turn delta + closed-set warning list. Must precede the
	// /cache match below since "/cache/forecast" suffixes
	// "/cache" too.
	if strings.HasSuffix(id, "/cache/forecast") {
		id = strings.TrimSuffix(id, "/cache/forecast")
		s.handleSessionCacheForecast(w, r, id)
		return
	}
	// Sub-route: /api/session/<id>/cache → cachetrack panel
	// payload (spec §13 / C13). Tier + entries + events +
	// efficiency rollup + timeline (baseline rolled to a single
	// count entry; anomalies itemized). Drives SessionDetailPanel's
	// Cache tab.
	if strings.HasSuffix(id, "/cache") {
		id = strings.TrimSuffix(id, "/cache")
		s.handleSessionCache(w, r, id)
		return
	}
	// Sub-route: /api/session/<id>/processes → the Process Observability
	// session tree (docs/process-observability.md §13.1). Attributed
	// fork/exec/exit lineage with the spawning command per subtree. Empty
	// unless [observer.process] is enabled; drives the Processes drawer.
	if strings.HasSuffix(id, "/processes") {
		id = strings.TrimSuffix(id, "/processes")
		s.handleSessionProcesses(w, r, id)
		return
	}
	// Sub-route: /api/session/<id>/handoff/estimate → session-handoff
	// carry-mode price table + fork-picker boundaries (dry run, nothing
	// written). Must precede the /handoff match below.
	if strings.HasSuffix(id, "/handoff/estimate") {
		id = strings.TrimSuffix(id, "/handoff/estimate")
		s.handleSessionHandoffEstimate(w, r, id)
		return
	}
	// Sub-route: /api/session/<id>/handoff → perform the handoff (POST):
	// writes HANDOFF-<shortid>.md + one node-local handoffs row.
	if strings.HasSuffix(id, "/handoff") {
		id = strings.TrimSuffix(id, "/handoff")
		s.handleSessionHandoffCreate(w, r, id)
		return
	}
	// Sub-route: /api/session/<id>/launch → start the tool in the embedded
	// web terminal (POST). Mints an opaque session handle; the browser then
	// opens GET /ws/launch/<handle> to drive the PTY. Gated by the
	// LaunchManager seam (503 when the launcher is disabled).
	if strings.HasSuffix(id, "/launch") {
		id = strings.TrimSuffix(id, "/launch")
		s.handleSessionLaunch(w, r, id)
		return
	}
	// Sub-route: /api/session/<id>/predict → the Next-Message Cost &
	// Limit Predictor (docs/plans/next-message-cost-predictor-plan-...).
	// Cost estimate is pure read-side math over token_usage; the limit
	// gauge is proxy-gated (Phase C capture) and renders "needs proxy"
	// until snapshots exist.
	if strings.HasSuffix(id, "/predict") {
		id = strings.TrimSuffix(id, "/predict")
		s.handleSessionPredict(w, r, id)
		return
	}
	// Sub-route: /api/session/<id>/verbosity → the Output Composition
	// (Verbosity) card. Read-side: visible narrative/artifact bytes
	// segmented from assistant text + authored code (writes + shell
	// commands) from actions.content_bytes (migration 054).
	if strings.HasSuffix(id, "/verbosity") {
		id = strings.TrimSuffix(id, "/verbosity")
		s.handleSessionVerbosity(w, r, id)
		return
	}
	if id == "" {
		http.Error(w, "missing session id", http.StatusBadRequest)
		return
	}
	type modelBucket struct {
		Model             string  `json:"model"`
		Input             int64   `json:"input"`
		Output            int64   `json:"output"`
		CacheRead         int64   `json:"cache_read"`
		CacheCreation     int64   `json:"cache_creation"`
		Reasoning         int64   `json:"reasoning,omitempty"`
		WebSearchRequests int64   `json:"web_search_requests,omitempty"`
		TurnCount         int64   `json:"turn_count"`
		CostUSD           float64 `json:"cost_usd"`
		AICostUSD         float64 `json:"ai_cost_usd"`
		ToolCostUSD       float64 `json:"tool_cost_usd"`
		// Per-bucket AICost components (v1.6.13) — sums equal AICostUSD.
		// Feed the session-detail Models Used panel's cost-by-bucket
		// stacked bar. Zero values stay in the response so the frontend
		// can render a 4-segment bar uniformly even for cache-only models.
		InputCostUSD         float64 `json:"input_cost_usd"`
		OutputCostUSD        float64 `json:"output_cost_usd"`
		CacheReadCostUSD     float64 `json:"cache_read_cost_usd"`
		CacheCreationCostUSD float64 `json:"cache_creation_cost_usd"`
	}
	type sessionDetail struct {
		ID        string  `json:"id"`
		Tool      string  `json:"tool"`
		Project   string  `json:"project"`
		Model     string  `json:"model,omitempty"`
		StartedAt string  `json:"started_at"`
		EndedAt   *string `json:"ended_at,omitempty"`
		// LastActivityAt is COALESCE(ended_at, MAX(actions.timestamp)) — the
		// real end of activity for both closed and open/never-closed
		// sessions. The frontend uses it for the Elapsed figure so a session
		// that stopped without a clean ended_at (rollup never finalized)
		// shows start→last-activity instead of start→now (the 583h bug).
		// Mirrors the sessions-list endpoint's COALESCE (see the list query).
		// Empty only when the session has neither ended_at nor any action.
		LastActivityAt  string   `json:"last_activity_at,omitempty"`
		TotalActions    int      `json:"total_actions"`
		SuccessActions  int      `json:"success_actions"`
		FailureActions  int      `json:"failure_actions"`
		QualityScore    *float64 `json:"quality_score,omitempty"`
		ErrorRate       *float64 `json:"error_rate,omitempty"`
		RedundancyRatio *float64 `json:"redundancy_ratio,omitempty"`
		// Spec §14.1 freshness/stale-read split — populated only
		// when the session has cache_events (Tier 3 / pre-backfill
		// sessions leave these nil, no fake zeros).
		StaleReadsWasteful      *int             `json:"stale_reads_wasteful,omitempty"`
		StaleReadsNecessary     *int             `json:"stale_reads_necessary,omitempty"`
		RedundancyRatioWasteful *float64         `json:"redundancy_ratio_wasteful,omitempty"`
		Tokens                  map[string]int64 `json:"tokens"`
		// ContextBudgetTokens + TokensNote are the honest fallback for a
		// session with NO billed token usage. Cursor isn't proxied (no
		// api_turns) and only reports usage on its `stop` hook, so a
		// cancelled/interrupted turn records zero tokens even though it
		// carried a real context. ContextBudgetTokens is the carried context
		// budget (the prompt_context section counts — part of a turn's input
		// but never billed on their own), surfaced as an ESTIMATE separate
		// from cost_usd; TokensNote explains why the billed figure is empty.
		// Both omitted when the session has real billed usage.
		ContextBudgetTokens int64  `json:"context_budget_tokens,omitempty"`
		TokensNote          string `json:"tokens_note,omitempty"`
		// PerModel breaks the deduped tokens + cost out by model so the
		// session detail modal shows haiku and opus separately when a
		// session uses both (Claude Code's main vs sub-agent split, etc.).
		PerModel []modelBucket `json:"per_model"`
		// CostUSD is the legacy total; AICostUSD + ToolCostUSD split
		// the same number so callers can render API spend vs tool
		// fees separately. Total == AI + Tool always.
		CostUSD       float64        `json:"cost_usd"`
		AICostUSD     float64        `json:"ai_cost_usd"`
		ToolCostUSD   float64        `json:"tool_cost_usd"`
		ToolBreakdown []actionBucket `json:"tool_breakdown"`
		// CacheSummary is the C15 cost-view cache annotation —
		// a compact glance-view of cache health for this session
		// (tier, event/hit/write/rewrite counts, token rollup,
		// ratio). Sits next to PerModel + ToolBreakdown so the
		// session detail modal shows API spend, tool spend, and
		// cache efficiency side by side. The full Cache tab
		// continues to load /api/session/<id>/cache for the
		// timeline + entries detail.
		CacheSummary *SessionCacheAnnotation `json:"cache_summary,omitempty"`
	}

	var d sessionDetail
	d.ID = id
	var endedAt sql.NullString
	var q, er, rr sql.NullFloat64
	var rrWasteful sql.NullFloat64
	var stWasteful, stNecessary sql.NullInt64
	var model sql.NullString
	if err := s.db().QueryRowContext(
		r.Context(),
		`SELECT s.tool, COALESCE(p.root_path, ''), s.model, s.started_at,
		        s.ended_at, s.quality_score, s.error_rate, s.redundancy_ratio,
		        s.stale_reads_wasteful, s.stale_reads_necessary, s.redundancy_ratio_wasteful
		 FROM sessions s LEFT JOIN projects p ON p.id = s.project_id
		 WHERE s.id = ?`, id,
	).Scan(&d.Tool, &d.Project, &model, &d.StartedAt, &endedAt, &q, &er, &rr,
		&stWasteful, &stNecessary, &rrWasteful); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		writeErr(w, err)
		return
	}
	if model.Valid {
		d.Model = model.String
	}
	if endedAt.Valid {
		v := endedAt.String
		d.EndedAt = &v
	}
	if q.Valid {
		v := q.Float64
		d.QualityScore = &v
	}
	if er.Valid {
		v := er.Float64
		d.ErrorRate = &v
	}
	if rr.Valid {
		v := rr.Float64
		d.RedundancyRatio = &v
	}
	if stWasteful.Valid {
		v := int(stWasteful.Int64)
		d.StaleReadsWasteful = &v
	}
	if stNecessary.Valid {
		v := int(stNecessary.Int64)
		d.StaleReadsNecessary = &v
	}
	if rrWasteful.Valid {
		v := rrWasteful.Float64
		d.RedundancyRatioWasteful = &v
	}

	// Action aggregates and tool breakdown. COALESCE the SUMs: a session
	// whose actions were all pruned by retention but whose token_usage rows
	// survive (token_usage isn't pruned) has zero matching rows here, and
	// SUM over an empty set is NULL — scanning that into int fails ("NULL to
	// int is unsupported") and 500s the whole endpoint. This became routine
	// once the FK-787 retention fix let actions actually prune; the detail
	// view must still render (tokens/model/cost) for such sessions. COUNT(*)
	// is never NULL.
	var maxActionTs sql.NullString
	if err := s.db().QueryRowContext(
		r.Context(),
		`SELECT COUNT(*),
		        COALESCE(SUM(CASE WHEN success = 0 THEN 0 ELSE 1 END), 0),
		        COALESCE(SUM(CASE WHEN success = 0 THEN 1 ELSE 0 END), 0),
		        MAX(timestamp)
		 FROM actions WHERE session_id = ?`, id,
	).Scan(&d.TotalActions, &d.SuccessActions, &d.FailureActions, &maxActionTs); err != nil {
		writeErr(w, err)
		return
	}
	// last_activity_at = ended_at when the session was cleanly closed, else
	// the last action's timestamp. Drives the frontend Elapsed figure so a
	// never-closed session doesn't measure start→now.
	switch {
	case endedAt.Valid && endedAt.String != "":
		d.LastActivityAt = endedAt.String
	case maxActionTs.Valid:
		d.LastActivityAt = maxActionTs.String
	}
	brRows, err := s.db().QueryContext(r.Context(),
		`SELECT action_type, COUNT(*),
		        SUM(CASE WHEN success = 0 THEN 1 ELSE 0 END)
		 FROM actions WHERE session_id = ?
		 GROUP BY action_type
		 ORDER BY COUNT(*) DESC`, id)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer brRows.Close()
	for brRows.Next() {
		var ab actionBucket
		if err := brRows.Scan(&ab.ActionType, &ab.Count, &ab.Failures); err != nil {
			writeErr(w, err)
			return
		}
		d.ToolBreakdown = append(d.ToolBreakdown, ab)
	}
	if d.ToolBreakdown == nil {
		d.ToolBreakdown = []actionBucket{}
	}

	// C15 cache annotation. Best-effort: a query failure leaves
	// CacheSummary nil and the modal hides the cache pill row.
	// Nil-summary also when the session has no cache_events
	// (pre-cachetrack history, non-Anthropic provider).
	if summary, err := loadSessionCacheAnnotation(r.Context(), s.db(), id); err == nil && summary != nil {
		d.CacheSummary = summary
	}

	// Token totals + per-model breakdown — both come from the same
	// per-turn-deduped CTE. Pre-2026-04-29 this endpoint had the same
	// bug as the cost engine: "if api_turns has ANY row for this
	// session, drop ALL token_usage rows" — so a session where the
	// proxy intercepted only some turns would show pure-proxy totals
	// even though most of the work went direct (b9bd459d had 3% of
	// input tokens captured by the proxy; the rest came from JSONL
	// and was silently dropped). The fix mirrors the cost engine's
	// per-turn dedup (api_turns.request_id ↔ token_usage.source_event_id):
	// proxy wins for turns it intercepted, JSONL fills the gaps.
	//
	// Single SQL CTE keeps the rollup atomic and avoids two passes
	// over the same dataset. cost.Options doesn't expose a session_id
	// filter so we can't reuse cost.Engine.Summary directly here.
	//
	// Per-row pricing (no SQL GROUP BY): the cost engine's long-context
	// dispatch reprices entire turns whose prompt window exceeds a
	// threshold (Sonnet 4 / 4.5 at 200K, gpt-5.4 / 5.5 at 272K, Gemini
	// Pro at 200K). LC is a per-request property — aggregating tokens
	// across many turns first would false-positive the threshold check
	// whenever a session's summed prompt exceeded it even if no single
	// turn did. So we pull individual rows and bucket per-model in Go.
	// Per-session token aggregation has TWO dedup gates against the
	// proxy api_turns rows:
	//
	//   1. source_event_id NOT IN (api_turns.request_id) — when the
	//      JSONL adapter mirrors the upstream message id verbatim
	//      (Claude Code stores Anthropic's msg_xxx; the proxy's
	//      request_id captures the same). Per-turn exact match.
	//
	//   2. NOT EXISTS (api_turn with same model + token shape) —
	//      fallback for adapters whose source_event_id format does
	//      NOT mirror the proxy's request_id. Codex's JSONL adapter
	//      writes a synthetic "tk:<file>:L<line>" id while the proxy
	//      stores OpenAI's "resp_<hex>" id; the shape match is the
	//      only way to recognise them as the same turn. Deliberately
	//      NO minute bucket: codex's rollout flush lands ~10s after
	//      the proxy logs the request, so ~15% of turns near a minute
	//      boundary would escape a minute-bucketed match and
	//      double-count (audit F1). False-positive risk: two distinct
	//      same-session calls with byte-identical token shapes
	//      collapse — effectively impossible since cache_read grows
	//      monotonically across a session; same trade
	//      Engine.loadRows::sessionShapeKey accepts in cost/summary.go.
	const dedupedRowsCTE = `WITH proxy_turn_ids AS (
		SELECT request_id FROM api_turns
		 WHERE session_id = ? AND request_id IS NOT NULL AND request_id != ''
	),
	combined AS (
		-- api_turns has no reasoning_tokens column (proxy folds it into
		-- output_tokens at capture); pad with 0 so the UNION schema
		-- matches and cost.Compute applies its reasoning × output_rate
		-- multiplier as 0 for proxy rows. fast = the proxy row's own tier;
		-- inherited_fast = a fast JSONL twin exists for this turn (codex,
		-- where the priority flag lives only on the JSONL/config path) —
		-- audit F1.
		SELECT at.model, at.input_tokens, at.output_tokens, at.cache_read_tokens,
		       at.cache_creation_tokens, at.cache_creation_1h_tokens,
		       0 AS reasoning_tokens,
		       at.web_search_requests, at.cost_usd,
		       COALESCE(at.fast, 0) AS fast,
		       CASE WHEN EXISTS (
		           SELECT 1 FROM token_usage tw
		           WHERE tw.session_id = at.session_id AND COALESCE(tw.fast, 0) = 1
		             AND COALESCE(tw.model, '') = COALESCE(at.model, '')
		             AND COALESCE(tw.input_tokens, 0) = COALESCE(at.input_tokens, 0)
		             AND COALESCE(tw.output_tokens, 0) = COALESCE(at.output_tokens, 0)
		             AND COALESCE(tw.cache_read_tokens, 0) = COALESCE(at.cache_read_tokens, 0)
		             AND COALESCE(tw.cache_creation_tokens, 0) = COALESCE(at.cache_creation_tokens, 0)
		       ) THEN 1 ELSE 0 END AS inherited_fast
		FROM api_turns at WHERE at.session_id = ?
		UNION ALL
		SELECT tu.model, tu.input_tokens, tu.output_tokens, tu.cache_read_tokens,
		       tu.cache_creation_tokens, tu.cache_creation_1h_tokens,
		       tu.reasoning_tokens,
		       tu.web_search_requests, tu.estimated_cost_usd,
		       COALESCE(tu.fast, 0) AS fast,
		       0 AS inherited_fast
		FROM token_usage tu
		WHERE tu.session_id = ?
		  AND (tu.source_event_id IS NULL OR tu.source_event_id = ''
		       OR tu.source_event_id NOT IN (SELECT request_id FROM proxy_turn_ids))
		  -- F1: also drop a JSONL row that duplicates a proxy turn by
		  -- token-bundle shape when the ids don't match (codex: tk:… vs
		  -- resp_…). COALESCE because codex leaves cache_creation NULL on
		  -- one side and 0 on the other. Anthropic proxy rows fold reasoning
		  -- into output, so their output_tokens differ from the JSONL row's
		  -- and never false-match (verified: 0 claude-code collisions live).
		  AND NOT EXISTS (
		      SELECT 1 FROM api_turns ap
		      WHERE ap.session_id = tu.session_id
		        AND COALESCE(ap.model, '') = COALESCE(tu.model, '')
		        AND COALESCE(ap.input_tokens, 0) = COALESCE(tu.input_tokens, 0)
		        AND COALESCE(ap.output_tokens, 0) = COALESCE(tu.output_tokens, 0)
		        AND COALESCE(ap.cache_read_tokens, 0) = COALESCE(tu.cache_read_tokens, 0)
		        AND COALESCE(ap.cache_creation_tokens, 0) = COALESCE(tu.cache_creation_tokens, 0)
		  )
		  -- Copilot family (copilot, copilot-cli) emits TWO token_usage rows per
		  -- turn: a full-usage row (Tier-1 process-log [DEBUG] usage block / the
		  -- request row) and an output-only "shadow" row (Tier-3 events.jsonl
		  -- assistant.message). The adapter set MessageID on both intending a
		  -- (session_id, message_id) merge, but the store upserts on
		  -- (source_file, source_event_id), so they never merge and the output
		  -- double-counts. Drop the output-only shadow when a full-usage sibling
		  -- carries the same output in this session. Scoped to the copilot tools
		  -- (the only adapters that emit >1 token row per turn) so nothing else
		  -- is affected.
		  AND NOT (
		      tu.tool IN ('copilot', 'copilot-cli')
		      AND COALESCE(tu.input_tokens, 0) = 0
		      AND COALESCE(tu.cache_read_tokens, 0) = 0
		      AND COALESCE(tu.cache_creation_tokens, 0) = 0
		      AND COALESCE(tu.output_tokens, 0) > 0
		      AND EXISTS (
		          SELECT 1 FROM token_usage tsh
		          WHERE tsh.session_id = tu.session_id
		            AND tsh.rowid != tu.rowid
		            AND COALESCE(tsh.output_tokens, 0) = COALESCE(tu.output_tokens, 0)
		            AND (COALESCE(tsh.input_tokens, 0) > 0
		                 OR COALESCE(tsh.cache_read_tokens, 0) > 0
		                 OR COALESCE(tsh.cache_creation_tokens, 0) > 0)
		      )
		  )
	)`

	sessionModel := d.Model
	rows, err := s.db().QueryContext(r.Context(),
		dedupedRowsCTE+`
		SELECT COALESCE(NULLIF(model, ''), ?),
		       COALESCE(input_tokens, 0),
		       COALESCE(output_tokens, 0),
		       COALESCE(cache_read_tokens, 0),
		       COALESCE(cache_creation_tokens, 0),
		       COALESCE(cache_creation_1h_tokens, 0),
		       COALESCE(reasoning_tokens, 0),
		       COALESCE(web_search_requests, 0),
		       COALESCE(cost_usd, 0),
		       COALESCE(fast, 0),
		       COALESCE(inherited_fast, 0)
		FROM combined`,
		id, id, id, sessionModel)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()

	bucketByModel := map[string]*modelBucket{}
	bucketOrder := []string{}
	var totalIn, totalOut, totalCR, totalCC, totalCC1h, totalReasoning int64
	for rows.Next() {
		var modelKey string
		var bundle cost.TokenBundle
		var recorded float64
		var fastInt, inheritedFastInt int
		if err := rows.Scan(&modelKey,
			&bundle.Input, &bundle.Output,
			&bundle.CacheRead, &bundle.CacheCreation, &bundle.CacheCreation1h,
			&bundle.Reasoning,
			&bundle.WebSearchRequests,
			&recorded, &fastInt, &inheritedFastInt); err != nil {
			writeErr(w, err)
			return
		}
		// Per-row cost: prefer recorded estimated_cost_usd / cost_usd
		// when non-zero (only OpenCode + Pi adapters set it today; api_turns
		// carries it for proxy rows). proxyAwareCost applies the F1 "keep
		// proxy, OR-in fast" rule — a codex proxy turn that inherited a fast
		// JSONL twin re-prices with the FastMultiplier premium (its recorded
		// cost was the standard wire tier). ComputeBreakdown returns the AI
		// vs tool split so we can show "API cost vs tool cost vs total"
		// separately; recorded costs land in AICost only (those adapters
		// don't model web_search billing). Recorded-cost rows leave the
		// per-bucket components zero so the frontend's "$ mode" stacked bar
		// renders as a single undifferentiated AI block.
		var rowCost, rowAICost, rowToolCost float64
		var rowInputCost, rowOutputCost, rowCacheReadCost, rowCacheCreationCost float64
		if cb, ok := proxyAwareCost(s.opts.CostEngine, modelKey, bundle, recorded, fastInt != 0, inheritedFastInt != 0); ok {
			rowCost = cb.Total
			rowAICost = cb.AICost
			rowToolCost = cb.ToolCost
			rowInputCost = cb.InputCost
			rowOutputCost = cb.OutputCost
			rowCacheReadCost = cb.CacheReadCost
			rowCacheCreationCost = cb.CacheCreationCost
		}

		mb, ok := bucketByModel[modelKey]
		if !ok {
			mb = &modelBucket{Model: modelKey}
			bucketByModel[modelKey] = mb
			bucketOrder = append(bucketOrder, modelKey)
		}
		mb.Input += bundle.Input
		mb.Output += bundle.Output
		mb.CacheRead += bundle.CacheRead
		mb.CacheCreation += bundle.CacheCreation
		mb.Reasoning += bundle.Reasoning
		mb.WebSearchRequests += bundle.WebSearchRequests
		mb.TurnCount++
		mb.CostUSD += rowCost
		mb.AICostUSD += rowAICost
		mb.ToolCostUSD += rowToolCost
		mb.InputCostUSD += rowInputCost
		mb.OutputCostUSD += rowOutputCost
		mb.CacheReadCostUSD += rowCacheReadCost
		mb.CacheCreationCostUSD += rowCacheCreationCost

		d.CostUSD += rowCost
		d.AICostUSD += rowAICost
		d.ToolCostUSD += rowToolCost
		totalIn += bundle.Input
		totalOut += bundle.Output
		totalCR += bundle.CacheRead
		totalCC += bundle.CacheCreation
		totalCC1h += bundle.CacheCreation1h
		totalReasoning += bundle.Reasoning
	}
	if err := rows.Err(); err != nil {
		writeErr(w, err)
		return
	}
	// Order buckets by token volume DESC (matches the prior SQL ORDER BY).
	sort.SliceStable(bucketOrder, func(i, j int) bool {
		bi, bj := bucketByModel[bucketOrder[i]], bucketByModel[bucketOrder[j]]
		ti := bi.Input + bi.Output + bi.CacheRead + bi.CacheCreation
		tj := bj.Input + bj.Output + bj.CacheRead + bj.CacheCreation
		return ti > tj
	})
	perModel := make([]modelBucket, 0, len(bucketOrder))
	for _, key := range bucketOrder {
		perModel = append(perModel, *bucketByModel[key])
	}
	d.Tokens = map[string]int64{
		"input": totalIn, "output": totalOut, "cache_read": totalCR, "cache_creation": totalCC,
		// cache_creation_1h is the 1h-ephemeral-tier subset of cache_creation
		// (the rest is 5m-tier). Surfaced separately so the session-detail
		// Token Buckets panel can split "Cache Write" into "Cache Write (5m)"
		// and "Cache Write (1h)" — different bill rates.
		"cache_creation_1h": totalCC1h,
		"reasoning":         totalReasoning,
	}
	d.PerModel = perModel

	// Context-budget fallback for a session with no billed token usage.
	// Cursor isn't proxied and only reports usage on its `stop` hook, so a
	// cancelled turn leaves zero billed tokens; surface the carried context
	// budget (prompt_context section counts) as an honest estimate + a note
	// explaining the empty bill. Never touches cost_usd.
	if totalIn+totalOut+totalCR+totalCC+totalCC1h+totalReasoning == 0 {
		d.ContextBudgetTokens = s.sessionContextBudget(r.Context(), id)
		d.TokensNote = tokensUnbilledNote(d.Tool, d.ContextBudgetTokens)
	}

	// Headline model fallback. sessions.model is populated only when an
	// ingested event carried a model, which is unreliable across adapters:
	// it's empty for nearly all claude-code sessions and every cursor session
	// (cursor's hooks don't deliver a model, and current cursor-agent emits
	// no stop-token row — see project_cursor_model_capture_gap). When it's
	// empty, surface the dominant model from the per-turn token_usage
	// breakdown, which IS the authoritative per-turn model source. perModel
	// is already sorted by token volume DESC, so the first non-empty bucket
	// is the model the session predominantly ran on.
	if d.Model == "" {
		for _, mb := range perModel {
			if mb.Model != "" {
				d.Model = mb.Model
				break
			}
		}
	}

	writeJSON(w, d)
}

// contextTokensRe extracts the token count from a cursor prompt_context
// action's target ("Rules — 15580 tokens, 61924 chars"). The format is
// adapter-controlled (cursor.promptSectionEvent) and stable.
var contextTokensRe = regexp.MustCompile(`(\d+)\s+tokens`)

// sessionContextBudget sums the carried context budget from a session's
// prompt_context actions (cursor's per-section prompt-token counts). These
// tokens are part of a turn's input but never billed on their own, so this is
// only ever surfaced as an estimate for a session with no billed usage — it
// must NOT feed cost. Returns 0 on any error or when no sections exist.
func (s *Server) sessionContextBudget(ctx context.Context, sessionID string) int64 {
	rows, err := s.db().QueryContext(ctx,
		`SELECT COALESCE(target, '') FROM actions
		  WHERE session_id = ? AND action_type = 'prompt_context'`, sessionID)
	if err != nil {
		return 0
	}
	defer rows.Close()
	var total int64
	for rows.Next() {
		var target string
		if err := rows.Scan(&target); err != nil {
			return total
		}
		if m := contextTokensRe.FindStringSubmatch(target); m != nil {
			if v, perr := strconv.ParseInt(m[1], 10, 64); perr == nil {
				total += v
			}
		}
	}
	return total
}

// tokensUnbilledNote explains why a session shows no billed token usage. For
// cursor it names the real cause (not proxied + usage only on the stop hook);
// otherwise a generic note. The budget clause is appended only when a carried
// context figure is available.
func tokensUnbilledNote(tool string, budget int64) string {
	if tool == "cursor" {
		n := "No billed token usage — Cursor isn't routed through the proxy and reports usage only on its stop hook, so a cancelled or interrupted turn records no tokens."
		if budget > 0 {
			n += " The figure shown is the carried context budget (estimated), not a bill."
		}
		return n
	}
	return "No billed token usage captured for this session."
}

type actionBucket struct {
	ActionType string `json:"action_type"`
	Count      int    `json:"count"`
	Failures   int    `json:"failures"`
}

// proxyAwareCost computes a deduped row's cost under the audit-F1 "keep
// proxy, OR-in fast" rule (docs/audits/audit-2026-06-08.md). The effective
// fast tier is the row's own fast OR a fast JSONL twin that was deduped
// against it (inheritedFast). A recorded proxy cost_usd is authoritative
// EXCEPT when the row inherited a fast flag it didn't carry itself — codex's
// proxy wire reports the standard tier, so its insert-time cost_usd was
// priced WITHOUT the FastMultiplier premium; in that one case we re-price
// from the table so the premium lands. Returns ok=false only when the model
// is unknown AND there's no recorded cost (caller leaves the row at $0,
// matching prior behavior).
func proxyAwareCost(engine *cost.Engine, model string, bundle cost.TokenBundle, recorded float64, ownFast, inheritedFast bool) (cost.Breakdown, bool) {
	bundle.Fast = ownFast || inheritedFast
	if recorded > 0 && !(inheritedFast && !ownFast) {
		// Recorded cost already reflects this row's own tier — use as-is.
		// Recorded-cost adapters (OpenCode, Pi) don't split AI vs tool, so
		// the whole amount lands on AICost, matching the prior code path.
		return cost.Breakdown{AICost: recorded, Total: recorded}, true
	}
	if engine == nil {
		return cost.Breakdown{}, false
	}
	return engine.ComputeBreakdown(model, bundle)
}

// handleSessionMessages serves /api/session/<id>/messages — one row
// per upstream Anthropic message id. Each row carries the message's
// own token usage and cost (per-turn deduped via the same
// proxy-preferred / JSONL-fallback logic as the session detail
// endpoint), plus the contained tool_calls grouped by message_id.
//
// Includes user-prompt rows synthesized from action_type='user_prompt'
// so the timeline shows "user said X → assistant did Y" together.
func (s *Server) handleSessionMessages(w http.ResponseWriter, r *http.Request, sessionID string) {
	if sessionID == "" {
		http.Error(w, "missing session id", http.StatusBadRequest)
		return
	}
	var sessionModel string
	_ = s.db().QueryRowContext(
		r.Context(),
		`SELECT COALESCE(model, '') FROM sessions WHERE id = ?`, sessionID,
	).Scan(&sessionModel)
	type toolCallRow struct {
		// ActionID is the actions.id primary key. Surfaced so the
		// frontend can call /api/action/<id>/full_text to fetch the
		// untruncated raw_tool_input + raw_tool_output on demand for
		// the copy and view-full-text buttons.
		ActionID    int64  `json:"action_id"`
		ActionType  string `json:"action_type"`
		RawToolName string `json:"raw_tool_name"`
		Target      string `json:"target"`
		FullText    string `json:"full_text,omitempty"`
		// FullTextElided marks rows whose raw_tool_input exceeded the
		// per-row inline cap (fullTextInlineMax) and was truncated for
		// the timeline payload. UI fetches the untruncated body via
		// /api/action/<id>/full_text when the operator clicks copy or
		// view-full-text.
		FullTextElided bool `json:"full_text_elided,omitempty"`
		// HasFullOutput is true when actions.raw_tool_output is
		// non-empty for this row — i.e. the adapter captured a
		// tool_result body that's available via the on-demand
		// /api/action/<id>/full_text endpoint. The inline Excerpt
		// stays 2 KiB (FTS5 cap) regardless; this flag tells the UI
		// there's a fuller version to offer.
		HasFullOutput bool   `json:"has_full_output,omitempty"`
		Excerpt       string `json:"excerpt,omitempty"`
		Success       bool   `json:"success"`
		ErrorMessage  string `json:"error_message,omitempty"`
		Timestamp     string `json:"timestamp"`
		// DurationMs is the per-tool-call wall-clock duration in ms
		// (sourced from actions.duration_ms). Adapters populate this
		// where the source data carries timing — codex via the
		// function_call→output timestamp gap, claude-code via
		// tool_use→tool_result gap, copilot via elapsedMs. Zero when
		// the source provided no timing signal or the row predates
		// the v1.4.28 capture work.
		DurationMs int64 `json:"duration_ms,omitempty"`
		// Per-event metadata extracted from actions.metadata JSON
		// (migration 017 + codex JSONL extension). Empty / false when
		// the source adapter didn't emit the field. omitempty keeps
		// the response payload lean.
		PermissionMode string `json:"permission_mode,omitempty"`
		EffortLevel    string `json:"effort_level,omitempty"`
		IsInterrupt    bool   `json:"is_interrupt,omitempty"`
		// StopReason — why the assistant turn ended; ServiceTier — served
		// capacity tier. Per-message metadata from the transcript.
		StopReason  string `json:"stop_reason,omitempty"`
		ServiceTier string `json:"service_tier,omitempty"`
	}
	type messageRow struct {
		MessageID         string `json:"message_id"`
		Timestamp         string `json:"timestamp"`
		Role              string `json:"role"`
		Model             string `json:"model,omitempty"`
		Input             int64  `json:"input"`
		Output            int64  `json:"output"`
		CacheRead         int64  `json:"cache_read"`
		CacheCreation     int64  `json:"cache_creation"`
		CacheCw1h         int64  `json:"cache_creation_1h"`
		Reasoning         int64  `json:"reasoning,omitempty"`
		WebSearchRequests int64  `json:"web_search_requests,omitempty"`
		// CostUSD is the legacy total; AICostUSD + ToolCostUSD split
		// it so the Messages table can render API / Tool / Total in
		// separate columns. CostUSD == AICostUSD + ToolCostUSD always.
		CostUSD     float64 `json:"cost_usd"`
		AICostUSD   float64 `json:"ai_cost_usd"`
		ToolCostUSD float64 `json:"tool_cost_usd"`
		// ElapsedMs is the wall-clock gap between this message's
		// timestamp and the next message's. For user rows it
		// approximates "time the assistant took to respond"; for
		// assistant rows it approximates "time the user took before
		// sending the next prompt". null on the last message in the
		// session (no successor to subtract from). Computed
		// post-sort, after pagination boundaries are decided.
		ElapsedMs *int64 `json:"elapsed_ms,omitempty"`
		// ToolDurationMs is the sum of contained tool_calls'
		// duration_ms — the assistant's tool-execution time for
		// this turn. Differs from ElapsedMs (which spans the entire
		// gap to the next message, including the model's reasoning
		// time and the user's typing time). Zero when no contained
		// tool_call carries duration_ms.
		ToolDurationMs int64 `json:"tool_duration_ms,omitempty"`
		ToolCallCount  int   `json:"tool_call_count"`
		// EffortLevel is the per-turn reasoning effort the adapter
		// captured for this message — sourced from
		// actions.metadata.$.effort_level on any action in the turn.
		// All actions in one message share the same effort_level
		// (codex collaboration_mode.settings.reasoning_effort is
		// per-turn, antigravity's effort is encoded in the SKU
		// itself — gemini-pro-agent, gemini-3.1-pro-low/medium/high
		// per [[project_antigravity_skus]]). First non-empty wins.
		// Empty when the adapter didn't emit it (Anthropic via
		// claude-code/cowork, copilot, etc. — Anthropic doesn't
		// expose a reasoning-effort knob).
		EffortLevel string `json:"effort_level,omitempty"`
		// StopReason is the assistant turn's terminal reason (end_turn /
		// max_tokens / tool_use / stop_sequence / refusal) and ServiceTier
		// the served capacity tier (standard / priority / batch), both from
		// the transcript (claude-code / cowork). Aggregated per message —
		// first non-empty among the turn's actions wins. Empty when the
		// adapter didn't emit them or the rows pre-date capture.
		StopReason  string `json:"stop_reason,omitempty"`
		ServiceTier string `json:"service_tier,omitempty"`
		// Fast is true when any token/turn row in this message bucket was
		// served in the provider's low-latency "fast" tier (Anthropic
		// Opus 4.8 speed:"fast", captured by the proxy). The timeline
		// renders a FAST badge on the row; CostUSD already reflects the
		// FastMultiplier premium. Zero/false for every standard turn.
		Fast      bool          `json:"fast,omitempty"`
		ToolCalls []toolCallRow `json:"tool_calls"`
		// TpsMs is the denominator the Tok/s column divides Output by, in
		// milliseconds — picked from the best available timing source per a
		// layered priority (see the post-merge block): (1) the proxy's
		// MEASURED total_response_ms when this bucket carries a proxy turn;
		// (2) the intra-turn generation span (MAX−MIN of a codex user-turn's
		// per-inference timestamps) when the bucket rolled up ≥2 timestamped
		// rows; (3) ElapsedMs (gap-to-next-message) for a single non-proxied
		// API call (claude-code). null when none applies (e.g. a
		// single-inference non-proxied codex turn, where gap-to-next would be
		// the meaningless inter-turn idle gap). TpsBasis names which source
		// was used, for the column's tooltip.
		TpsMs    *int64 `json:"tps_ms,omitempty"`
		TpsBasis string `json:"tps_basis,omitempty"`
		// respMs/firstT/lastT/tsCount/turnRollup are unexported per-bucket
		// accumulators feeding the TpsMs decision (set during merge,
		// resolved post-sort). Never serialized. respMs sums the proxy
		// total_response_ms of any api_turns sub-rows; firstT/lastT/tsCount
		// bound the intra-turn span; turnRollup marks a whole-turn bucket
		// (key from token_usage.turn_id — codex) so a single-inference codex
		// turn shows "—" rather than a gap-to-next rate.
		respMs     int64
		firstT     time.Time
		lastT      time.Time
		tsCount    int
		turnRollup bool
	}

	// 1. Token rows joined into per-message buckets. Two modes:
	//
	//   - Default (turn rollup): bucket by
	//     COALESCE(turn_id, message_id, source_event_id). For codex
	//     (v1.7.24+) turn_id groups multiple per-inference rows back
	//     into the user-turn; for claudecode and other Anthropic
	//     adapters turn_id is NULL and message_id (= the upstream
	//     msg_xxx) is the natural per-API-call grouping.
	//
	//   - ?detail=inference: bucket by
	//     COALESCE(message_id, source_event_id). For codex this
	//     produces one row per token_count event (per model inference);
	//     for claudecode it's identical to the default mode because
	//     turn_id is NULL.
	//
	// api_turns is always per-HTTP-request (proxy emits one row per
	// upstream call), so its request_id is already the right grouping
	// key in both modes.
	//nolint:gosec // G101: code-constant SQL grouping expression switched by query param. No credentials involved; gosec false-positives on the `_id` substring.
	tokenGroupExpr := `COALESCE(NULLIF(turn_id, ''), NULLIF(message_id, ''), source_event_id, '')`
	if r.URL.Query().Get("detail") == "inference" {
		tokenGroupExpr = `COALESCE(NULLIF(message_id, ''), source_event_id, '')` //nolint:gosec // G101: same false-positive as above; code-constant SQL fragment.
	}
	dedupedRowsCTE := `WITH proxy_turn_ids AS (
		SELECT request_id FROM api_turns
		 WHERE session_id = ? AND request_id IS NOT NULL AND request_id != ''
	),
	combined AS (
		-- api_turns has no reasoning_tokens column (proxy folds reasoning
		-- into output_tokens at capture); pad with 0 for UNION schema
		-- parity so cost.Compute treats proxy rows correctly. fast = the
		-- proxy row's own tier; inherited_fast = a fast JSONL twin exists
		-- for this turn (codex priority flag lives only on the JSONL/config
		-- path) — audit F1.
		SELECT COALESCE(NULLIF(at.request_id, ''), '') AS msg_key,
		       at.model, at.timestamp,
		       at.input_tokens, at.output_tokens, at.cache_read_tokens,
		       at.cache_creation_tokens, at.cache_creation_1h_tokens,
		       0 AS reasoning_tokens,
		       at.web_search_requests, at.cost_usd,
		       COALESCE(at.fast, 0) AS fast,
		       CASE WHEN EXISTS (
		           SELECT 1 FROM token_usage tw
		           WHERE tw.session_id = at.session_id AND COALESCE(tw.fast, 0) = 1
		             AND COALESCE(tw.model, '') = COALESCE(at.model, '')
		             AND COALESCE(tw.input_tokens, 0) = COALESCE(at.input_tokens, 0)
		             AND COALESCE(tw.output_tokens, 0) = COALESCE(at.output_tokens, 0)
		             AND COALESCE(tw.cache_read_tokens, 0) = COALESCE(at.cache_read_tokens, 0)
		             AND COALESCE(tw.cache_creation_tokens, 0) = COALESCE(at.cache_creation_tokens, 0)
		       ) THEN 1 ELSE 0 END AS inherited_fast,
		       '' AS turn_id,
		       COALESCE(at.total_response_ms, 0) AS total_response_ms
		FROM api_turns at WHERE at.session_id = ?
		UNION ALL
		SELECT ` + tokenGroupExpr + ` AS msg_key,
		       tu.model, tu.timestamp,
		       tu.input_tokens, tu.output_tokens, tu.cache_read_tokens,
		       tu.cache_creation_tokens, tu.cache_creation_1h_tokens,
		       tu.reasoning_tokens,
		       tu.web_search_requests, tu.estimated_cost_usd,
		       COALESCE(tu.fast, 0) AS fast,
		       0 AS inherited_fast,
		       COALESCE(tu.turn_id, '') AS turn_id,
		       0 AS total_response_ms
		FROM token_usage tu
		WHERE tu.session_id = ?
		  AND (tu.source_event_id IS NULL OR tu.source_event_id = ''
		       OR tu.source_event_id NOT IN (SELECT request_id FROM proxy_turn_ids))
		  -- F1: also drop a JSONL row that duplicates a proxy turn by
		  -- token-bundle shape when the ids don't match (codex: tk:… vs
		  -- resp_…). COALESCE because codex leaves cache_creation NULL on
		  -- one side, 0 on the other. Anthropic proxy rows fold reasoning
		  -- into output, so output_tokens differ and never false-match.
		  AND NOT EXISTS (
		      SELECT 1 FROM api_turns ap
		      WHERE ap.session_id = tu.session_id
		        AND COALESCE(ap.model, '') = COALESCE(tu.model, '')
		        AND COALESCE(ap.input_tokens, 0) = COALESCE(tu.input_tokens, 0)
		        AND COALESCE(ap.output_tokens, 0) = COALESCE(tu.output_tokens, 0)
		        AND COALESCE(ap.cache_read_tokens, 0) = COALESCE(tu.cache_read_tokens, 0)
		        AND COALESCE(ap.cache_creation_tokens, 0) = COALESCE(tu.cache_creation_tokens, 0)
		  )
		  -- Copilot family (copilot, copilot-cli) emits TWO token_usage rows per
		  -- turn: a full-usage row (Tier-1 process-log [DEBUG] usage block / the
		  -- request row) and an output-only "shadow" row (Tier-3 events.jsonl
		  -- assistant.message). The adapter set MessageID on both intending a
		  -- (session_id, message_id) merge, but the store upserts on
		  -- (source_file, source_event_id), so they never merge and the output
		  -- double-counts. Drop the output-only shadow when a full-usage sibling
		  -- carries the same output in this session. Scoped to the copilot tools
		  -- (the only adapters that emit >1 token row per turn) so nothing else
		  -- is affected.
		  AND NOT (
		      tu.tool IN ('copilot', 'copilot-cli')
		      AND COALESCE(tu.input_tokens, 0) = 0
		      AND COALESCE(tu.cache_read_tokens, 0) = 0
		      AND COALESCE(tu.cache_creation_tokens, 0) = 0
		      AND COALESCE(tu.output_tokens, 0) > 0
		      AND EXISTS (
		          SELECT 1 FROM token_usage tsh
		          WHERE tsh.session_id = tu.session_id
		            AND tsh.rowid != tu.rowid
		            AND COALESCE(tsh.output_tokens, 0) = COALESCE(tu.output_tokens, 0)
		            AND (COALESCE(tsh.input_tokens, 0) > 0
		                 OR COALESCE(tsh.cache_read_tokens, 0) > 0
		                 OR COALESCE(tsh.cache_creation_tokens, 0) > 0)
		      )
		  )
	)`
	rows, err := s.db().QueryContext(r.Context(),
		dedupedRowsCTE+`
		SELECT msg_key,
		       timestamp,
		       COALESCE(NULLIF(model, ''), ?),
		       COALESCE(input_tokens, 0),
		       COALESCE(output_tokens, 0),
		       COALESCE(cache_read_tokens, 0),
		       COALESCE(cache_creation_tokens, 0),
		       COALESCE(cache_creation_1h_tokens, 0),
		       COALESCE(reasoning_tokens, 0),
		       COALESCE(web_search_requests, 0),
		       COALESCE(cost_usd, 0),
		       COALESCE(fast, 0),
		       COALESCE(inherited_fast, 0),
		       COALESCE(turn_id, ''),
		       COALESCE(total_response_ms, 0)
		FROM combined
		WHERE msg_key IS NOT NULL AND msg_key != ''
		ORDER BY timestamp ASC`,
		sessionID, sessionID, sessionID, sessionModel)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()

	byKey := map[string]*messageRow{}
	out := []*messageRow{}
	for rows.Next() {
		var key, ts, model, turnID string
		var bundle cost.TokenBundle
		var recorded float64
		var fastInt, inheritedFastInt int
		var respMs int64
		if err := rows.Scan(&key, &ts, &model,
			&bundle.Input, &bundle.Output,
			&bundle.CacheRead, &bundle.CacheCreation, &bundle.CacheCreation1h,
			&bundle.Reasoning,
			&bundle.WebSearchRequests,
			&recorded, &fastInt, &inheritedFastInt, &turnID, &respMs); err != nil {
			writeErr(w, err)
			return
		}
		// F1 "keep proxy, OR-in fast": effective tier is the row's own fast
		// OR a fast JSONL twin's. proxyAwareCost re-prices a codex proxy turn
		// that inherited fast (its recorded cost was the standard wire tier).
		bundle.Fast = fastInt != 0 || inheritedFastInt != 0
		var costUSD, aiCostUSD, toolCostUSD float64
		if cb, ok := proxyAwareCost(s.opts.CostEngine, model, bundle, recorded, fastInt != 0, inheritedFastInt != 0); ok {
			costUSD = cb.Total
			aiCostUSD = cb.AICost
			toolCostUSD = cb.ToolCost
		}
		mr, ok := byKey[key]
		if !ok {
			mr = &messageRow{
				MessageID: key,
				Timestamp: ts,
				Role:      "assistant",
				Model:     model,
				ToolCalls: []toolCallRow{},
			}
			byKey[key] = mr
			out = append(out, mr)
		}
		if mr.Model == "" && model != "" {
			mr.Model = model
		}
		// A turn shows the ⚡ premium badge only when it was served fast AND
		// the model actually carries a fast-mode premium
		// (Pricing.FastMultiplier > 0). Codex sends service_tier:"priority"
		// globally, but only gpt-5.5 / gpt-5.4 have a documented Fast
		// premium — so mini/codex priority turns keep the service_tier pill
		// (captured separately on the action row) without an ⚡ that implies
		// a price bump they don't incur. Anthropic Opus 4.8 (FastMultiplier
		// 2) still lights up exactly as before.
		if bundle.Fast {
			if p, ok := s.opts.CostEngine.Lookup(model); ok && p.FastMultiplier > 0 {
				mr.Fast = true
			}
		}
		mr.Input += bundle.Input
		mr.Output += bundle.Output
		mr.CacheRead += bundle.CacheRead
		mr.CacheCreation += bundle.CacheCreation
		mr.CacheCw1h += bundle.CacheCreation1h
		mr.Reasoning += bundle.Reasoning
		mr.WebSearchRequests += bundle.WebSearchRequests
		mr.CostUSD += costUSD
		mr.AICostUSD += aiCostUSD
		mr.ToolCostUSD += toolCostUSD
		// Per-bucket timing accumulators feeding the TpsMs decision below.
		// respMs sums any proxy total_response_ms (the measured per-call
		// wall-clock — preferred source). turnRollup marks a whole-turn
		// bucket (key from turn_id — codex). firstT/lastT/tsCount bound the
		// intra-turn generation span for a codex user-turn (many token_count
		// inference rows rolled into one bucket).
		mr.respMs += respMs
		if turnID != "" {
			mr.turnRollup = true
		}
		if t, perr := time.Parse(time.RFC3339Nano, ts); perr == nil {
			if mr.tsCount == 0 || t.Before(mr.firstT) {
				mr.firstT = t
			}
			if mr.tsCount == 0 || t.After(mr.lastT) {
				mr.lastT = t
			}
			mr.tsCount++
		}
	}
	if err := rows.Err(); err != nil {
		writeErr(w, err)
		return
	}

	// 2. Tool calls — grouped by message_id (or source_event_id as
	// fallback for pre-backfill rows). Append into each message's
	// ToolCalls; create synthetic message rows for actions whose
	// message_id doesn't have a token row (typically user_prompt).
	//
	// Excerpts are loaded in a second batch query — see
	// loadActionExcerpts for why an inline LEFT JOIN on
	// action_excerpts is O(N×M) on FTS5 (~136s for a 1772-action
	// session before this change).
	actRows, err := s.db().QueryContext(r.Context(),
		`SELECT a.id, COALESCE(message_id, source_event_id) AS msg_key,
		        a.action_type, COALESCE(a.raw_tool_name, ''),
		        COALESCE(a.target, ''), COALESCE(a.raw_tool_input, ''),
		        LENGTH(COALESCE(a.raw_tool_output, '')) AS raw_output_len,
		        COALESCE(a.success, 1),
		        COALESCE(a.error_message, ''), a.timestamp,
		        COALESCE(a.duration_ms, 0),
		        COALESCE(json_extract(a.metadata, '$.permission_mode'), '') AS permission_mode,
		        COALESCE(json_extract(a.metadata, '$.effort_level'), '') AS effort_level,
		        COALESCE(json_extract(a.metadata, '$.is_interrupt'), 0) AS is_interrupt,
		        COALESCE(json_extract(a.metadata, '$.stop_reason'), '') AS stop_reason,
		        COALESCE(json_extract(a.metadata, '$.service_tier'), '') AS service_tier
		 FROM actions a
		 WHERE a.session_id = ?
		 ORDER BY a.timestamp ASC`, sessionID)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer actRows.Close()
	// pendingExcerpt records each tool-call's location so we can fill
	// its Excerpt field after the batch FTS5 lookup below. Indices into
	// mr.ToolCalls are stable once the scan loop ends.
	type pendingExcerpt struct {
		actionID int64
		mr       *messageRow
		idx      int
	}
	var pendings []pendingExcerpt
	var actionIDs []int64
	for actRows.Next() {
		var actionID int64
		var key, actionType, rawTool, target, rawInput, errMsg, ts string
		var permMode, effortLevel, stopReason, serviceTier string
		var success, isInterrupt int
		var durationMs, rawOutputLen int64
		if err := actRows.Scan(&actionID, &key, &actionType, &rawTool, &target, &rawInput, &rawOutputLen, &success, &errMsg, &ts, &durationMs, &permMode, &effortLevel, &isInterrupt, &stopReason, &serviceTier); err != nil {
			writeErr(w, err)
			return
		}
		fullText := target
		switch actionType {
		case "user_prompt", "system_prompt", "ask_user", "run_command":
			if rawInput != "" {
				fullText = rawInput
			}
		}
		if actionType == "run_command" {
			fullText = decodeCommandInput(fullText)
		}
		fullTextElided := false
		if len(fullText) > fullTextInlineMax {
			fullText = fullText[:fullTextInlineMax]
			fullTextElided = true
		}
		tc := toolCallRow{
			ActionID:       actionID,
			ActionType:     actionType,
			RawToolName:    rawTool,
			Target:         target,
			FullText:       fullText,
			FullTextElided: fullTextElided,
			HasFullOutput:  rawOutputLen > 0,
			Success:        success != 0,
			ErrorMessage:   errMsg,
			Timestamp:      ts,
			DurationMs:     durationMs,
			PermissionMode: permMode,
			EffortLevel:    effortLevel,
			IsInterrupt:    isInterrupt != 0,
			StopReason:     stopReason,
			ServiceTier:    serviceTier,
		}
		mr, ok := byKey[key]
		if !ok {
			// No matching token row — this is a user_prompt or
			// other action whose parent message doesn't carry token
			// usage (user messages don't bill). Synthesize a row
			// so the timeline still shows it.
			role := "user"
			if actionType != "user_prompt" {
				role = "assistant"
			}
			// Per-turn model resolution for synthesized rows. A user
			// prompt and its assistant turn share a request_id, so the
			// assistant's token row carries the canonical per-turn
			// model (e.g. claude-haiku-4-5-20251001). Falling back to
			// sessions.model would always show the FIRST turn's model
			// for every later turn — wrong whenever a session crosses
			// upstream models (Copilot Auto routing routinely picks
			// different models per turn).
			model := sessionModel
			if role == "user" && strings.HasPrefix(key, "user:") {
				peerKey := "assistant:" + strings.TrimPrefix(key, "user:")
				if peer, ok := byKey[peerKey]; ok && peer.Model != "" {
					model = peer.Model
				}
			}
			mr = &messageRow{
				MessageID: key,
				Timestamp: ts,
				Role:      role,
				Model:     model,
				ToolCalls: []toolCallRow{},
			}
			byKey[key] = mr
			out = append(out, mr)
		}
		mr.ToolCalls = append(mr.ToolCalls, tc)
		pendings = append(pendings, pendingExcerpt{actionID: actionID, mr: mr, idx: len(mr.ToolCalls) - 1})
		actionIDs = append(actionIDs, actionID)
		mr.ToolCallCount++
		mr.ToolDurationMs += tc.DurationMs
		if mr.EffortLevel == "" && tc.EffortLevel != "" {
			mr.EffortLevel = tc.EffortLevel
		}
		if mr.StopReason == "" && tc.StopReason != "" {
			mr.StopReason = tc.StopReason
		}
		if mr.ServiceTier == "" && tc.ServiceTier != "" {
			mr.ServiceTier = tc.ServiceTier
		}
	}
	if err := actRows.Err(); err != nil {
		writeErr(w, err)
		return
	}
	// Batch-fetch excerpts for every tool call (single FTS5 scan instead
	// of N×M); see loadActionExcerpts. maxBytes=0 preserves the original
	// full-text semantics for the messages view.
	excerptByID, err := loadActionExcerpts(r.Context(), s.db(), actionIDs, 0)
	if err != nil {
		writeErr(w, err)
		return
	}
	for _, p := range pendings {
		if ex := excerptByID[p.actionID]; ex != "" {
			p.mr.ToolCalls[p.idx].Excerpt = ex
		}
	}

	// Orphan-token stub injection — for agentic sessions (gemini /
	// antigravity tool-call-loop turns) where the upstream API stores
	// no extractable content for most LLM calls, surface a synthetic
	// row carrying the per-turn token totals so the dashboard's
	// expand-row view has SOMETHING to display instead of an empty
	// Tools column. Gated on orphan ratio > 0.5 so claude sessions
	// (where every turn already has narrative or a tool call) don't
	// grow noise stubs that obscure real content.
	var assistantTotal, assistantOrphan int
	for _, mr := range out {
		if mr.Role != "assistant" {
			continue
		}
		assistantTotal++
		if len(mr.ToolCalls) == 0 {
			assistantOrphan++
		}
	}
	if assistantTotal > 0 && float64(assistantOrphan)/float64(assistantTotal) > 0.5 {
		for _, mr := range out {
			if mr.Role != "assistant" || len(mr.ToolCalls) > 0 {
				continue
			}
			target := fmt.Sprintf("API call (no recovered text): %d in + %d cache_read + %d cache_create + %d out tokens",
				mr.Input, mr.CacheRead, mr.CacheCreation, mr.Output)
			mr.ToolCalls = append(mr.ToolCalls, toolCallRow{
				ActionType:  "llm_call",
				RawToolName: "synthetic.api_call",
				Target:      target,
				Success:     true,
				Timestamp:   mr.Timestamp,
			})
			mr.ToolCallCount++
		}
	}

	// Sort the merged list chronologically — token-row pass appended
	// in time order but the actions pass may have appended synthetic
	// rows out of order. On equal timestamps, prefer the user message:
	// the proxy or adapter often stamps a synthesized user_prompt with
	// the same wall-clock as the assistant turn it triggers, and the
	// timeline reads more naturally with "user said X → assistant did Y".
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Timestamp != out[j].Timestamp {
			return out[i].Timestamp < out[j].Timestamp
		}
		return out[i].Role == "user" && out[j].Role != "user"
	})

	// Per-message wall-clock duration: gap from this message's
	// timestamp to the NEXT message's. Computed across the full sorted
	// timeline (not the paginated slice) so a row near a page boundary
	// still gets the correct successor. Null on the final message —
	// no follower to subtract from. Adapter-captured DurationMs (codex
	// task_complete, copilot elapsedMs, …) lives on the contained
	// actions/tool_calls; this field is the orthogonal "wall-clock
	// between user and assistant turns" view.
	for i := 0; i < len(out)-1; i++ {
		t1, err1 := time.Parse(time.RFC3339Nano, out[i].Timestamp)
		t2, err2 := time.Parse(time.RFC3339Nano, out[i+1].Timestamp)
		if err1 != nil || err2 != nil {
			continue
		}
		ms := t2.Sub(t1).Milliseconds()
		if ms < 0 {
			continue
		}
		out[i].ElapsedMs = &ms
	}

	// Tok/s denominator (TpsMs) — layered, best-source-first. Runs after
	// the ElapsedMs loop because the "elapsed" tier consumes it.
	//   1. "measured"   — the proxy's total_response_ms (summed across the
	//                     bucket's api_turns sub-rows): the real per-call
	//                     wall-clock. Best when the session routed through
	//                     the proxy (claude-code AND codex).
	//   2. "intra-turn" — MAX−MIN of a codex user-turn's per-inference
	//                     timestamps (≥2 rows): a real generation+tool span
	//                     for non-proxied codex. Anchored to inference
	//                     completion timestamps, so it slightly excludes the
	//                     first inference's own window (negligible on long
	//                     turns).
	//   3. "elapsed"    — ElapsedMs (gap-to-next-message): valid only for a
	//                     single non-proxied API call (claude-code), where
	//                     it approximates response time. Skipped for a
	//                     turn-rollup bucket (codex), where gap-to-next is
	//                     the meaningless inter-turn idle gap.
	// Synthesized action rows (user_prompt, etc.) match no tier → "—".
	for _, mr := range out {
		switch {
		case mr.respMs > 0:
			ms := mr.respMs
			mr.TpsMs, mr.TpsBasis = &ms, "measured"
		case mr.tsCount >= 2 && mr.lastT.Sub(mr.firstT).Milliseconds() > 0:
			ms := mr.lastT.Sub(mr.firstT).Milliseconds()
			mr.TpsMs, mr.TpsBasis = &ms, "intra-turn"
		case !mr.turnRollup && mr.ElapsedMs != nil && *mr.ElapsedMs > 0:
			ms := *mr.ElapsedMs
			mr.TpsMs, mr.TpsBasis = &ms, "elapsed"
		}
	}

	// Pagination — added v1.4.24 because rendering 5000+ messages in
	// one go was crashing the dashboard browser tab. Default limit is
	// 100; pass limit=0 explicitly to opt into the pre-v1.4.24 "all
	// messages" behaviour. Server-side paginates AFTER the chronological
	// sort so the page boundaries are stable across re-fetches.
	limit, offset := 100, 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	// ?locate=<message_id>: snap offset to the page containing that message so
	// the caller (the Processes panel "jump to the message that spawned this
	// process" link) lands on the right page. Reuses this handler's own
	// chronological ordering — no fragile external ordinal. No-op if not found.
	if mid := r.URL.Query().Get("locate"); mid != "" && limit > 0 {
		for i := range out {
			if out[i].MessageID == mid {
				offset = (i / limit) * limit
				break
			}
		}
	}
	total := len(out)
	if offset > total {
		offset = total
	}
	page := out[offset:]
	if limit > 0 && len(page) > limit {
		page = page[:limit]
	}
	writeJSON(w, map[string]any{
		"session_id": sessionID,
		"messages":   page,
		"total":      total,
		"limit":      limit,
		"offset":     offset,
	})
}

func decodeCommandInput(raw string) string {
	if raw == "" || raw[0] != '[' {
		return raw
	}
	var argv []string
	if err := json.Unmarshal([]byte(raw), &argv); err != nil || len(argv) == 0 {
		return raw
	}
	return strings.Join(argv, " ")
}

// handlePatterns serves /api/patterns?page=N&limit=M. Returns a paged
// {rows, page, limit, total} envelope mirroring /api/sessions and
// /api/actions. Patterns are ordered by confidence DESC (the user's
// "what's most reliable to act on first" view).
func (s *Server) handlePatterns(w http.ResponseWriter, r *http.Request) {
	limit := intArg(r, "limit", 20, 1, 200)
	page := intArg(r, "page", 1, 1, 1_000_000)
	offset := (page - 1) * limit
	project := r.URL.Query().Get("project")
	tool := r.URL.Query().Get("tool")

	countArgs := []any{}
	listArgs := []any{}
	where := []string{}
	if project != "" {
		where = append(where, "pp.project_id = (SELECT id FROM projects WHERE root_path = ?)")
		countArgs = append(countArgs, project)
		listArgs = append(listArgs, project)
	}
	// Patterns are mined per-project; tool-scoping restricts to projects
	// whose actions table has at least one row for the requested tool.
	// IN with a derived DISTINCT set scans actions once and hash-joins —
	// avoids the EXISTS-per-pattern quadratic risk the v1.6.2 ship hit on
	// crossToolFiles (handover §4d).
	if tool != "" {
		where = append(where, "pp.project_id IN (SELECT DISTINCT project_id FROM actions WHERE tool = ?)")
		countArgs = append(countArgs, tool)
		listArgs = append(listArgs, tool)
	}
	whereSQL := ""
	if len(where) > 0 {
		whereSQL = " WHERE " + joinStrings(where, " AND ")
	}

	var total int
	if err := s.db().QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM project_patterns pp`+whereSQL, countArgs...).Scan(&total); err != nil {
		writeErr(w, err)
		return
	}
	listArgs = append(listArgs, limit, offset)
	rows, err := s.db().QueryContext(r.Context(),
		//nolint:gosec // G202: SQL structure (WHERE/JOIN/scope fragments and any IN placeholder list) is built from code constants; all values are bound via ? args.
		`SELECT COALESCE(p.root_path, ''), pattern_type, pattern_data,
		        COALESCE(confidence, 0), COALESCE(observation_count, 0)
		 FROM project_patterns pp
		 LEFT JOIN projects p ON p.id = pp.project_id`+whereSQL+`
		 ORDER BY confidence DESC
		 LIMIT ? OFFSET ?`, listArgs...)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	type patternRow struct {
		Project          string  `json:"project"`
		PatternType      string  `json:"pattern_type"`
		Data             string  `json:"data"`
		Confidence       float64 `json:"confidence"`
		ObservationCount int     `json:"observation_count"`
	}
	out := []patternRow{}
	for rows.Next() {
		var pr patternRow
		if err := rows.Scan(&pr.Project, &pr.PatternType, &pr.Data, &pr.Confidence, &pr.ObservationCount); err != nil {
			writeErr(w, err)
			return
		}
		out = append(out, pr)
	}
	writeJSON(w, map[string]any{
		"rows":  out,
		"page":  page,
		"limit": limit,
		"total": total,
	})
}

// handlePatternsTimeseries serves /api/patterns/timeseries?days=N — one
// bucket per calendar day in the window with the number of patterns
// reinforced that day, split by pattern_type. Drives the "Pattern
// discovery over time" chart on the Patterns tab.
//
// Aggregation uses last_reinforced_at (the column the patterns engine
// touches on every observation). Patterns whose last_reinforced_at is
// NULL (legacy rows) skip; they'd otherwise pile onto epoch.
func (s *Server) handlePatternsTimeseries(w http.ResponseWriter, r *http.Request) {
	days := intArg(r, "days", 30, 1, 36500)
	project := r.URL.Query().Get("project")
	tool := r.URL.Query().Get("tool")
	since := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)

	args := []any{since.Format(time.RFC3339Nano)}
	projClause := ""
	if project != "" {
		projClause = " AND project_id = (SELECT id FROM projects WHERE root_path = ?)"
		args = append(args, project)
	}
	if tool != "" {
		projClause += " AND project_id IN (SELECT DISTINCT project_id FROM actions WHERE tool = ?)"
		args = append(args, tool)
	}
	rows, err := s.db().QueryContext(r.Context(),
		//nolint:gosec // G202: SQL structure (WHERE/JOIN/scope fragments and any IN placeholder list) is built from code constants; all values are bound via ? args.
		`SELECT substr(last_reinforced_at, 1, 10) AS day, pattern_type, COUNT(*) AS c
		 FROM project_patterns
		 WHERE last_reinforced_at IS NOT NULL AND last_reinforced_at >= ?`+projClause+`
		 GROUP BY day, pattern_type
		 ORDER BY day ASC, pattern_type ASC`,
		args...)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()

	type point struct {
		Day    string         `json:"day"`
		Total  int            `json:"total"`
		ByType map[string]int `json:"by_type"`
	}
	byDay := make(map[string]*point)
	for rows.Next() {
		var day, pt string
		var c int
		if err := rows.Scan(&day, &pt, &c); err != nil {
			writeErr(w, err)
			return
		}
		p, ok := byDay[day]
		if !ok {
			p = &point{Day: day, ByType: map[string]int{}}
			byDay[day] = p
		}
		p.ByType[pt] += c
		p.Total += c
	}
	if err := rows.Err(); err != nil {
		writeErr(w, err)
		return
	}

	// Order by day ascending; emit a stable JSON shape.
	keys := make([]string, 0, len(byDay))
	for k := range byDay {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*point, 0, len(keys))
	for _, k := range keys {
		out = append(out, byDay[k])
	}
	writeJSON(w, map[string]any{
		"days":   days,
		"points": out,
	})
}

// handleSuggestPreview serves POST /api/suggest — given a project root
// + window, returns the rendered CLAUDE.md / AGENTS.md / .cursorrules
// bodies derived from the project's mined patterns. Does NOT write any
// files; preview only.
//
// Request body: {"project_root": string, "days": int}
// Response: {"markdown": "...", "cursorrules": "...", "input": Input}
func (s *Server) handleSuggestPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ProjectRoot string `json:"project_root"`
		Days        int    `json:"days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.ProjectRoot == "" {
		http.Error(w, "project_root required", http.StatusBadRequest)
		return
	}
	if req.Days <= 0 {
		req.Days = 30
	}
	in, err := suggest.Load(r.Context(), s.db(), suggest.Options{
		ProjectRoot: req.ProjectRoot,
		Days:        req.Days,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	now := time.Now().UTC()
	writeJSON(w, map[string]any{
		"project_root": req.ProjectRoot,
		"days":         req.Days,
		"markdown":     suggest.RenderMarkdown(in, now),
		"cursorrules":  suggest.RenderCursorRules(in, now),
		"input":        in,
	})
}

// handleSuggestWrite serves POST /api/suggest/write — same render
// pipeline as preview, then actually persists the result to a file
// in the project root. The target chooses between CLAUDE.md (default),
// AGENTS.md, and .cursorrules; the file is created if missing and
// over-written between observer-managed delimiters when present.
//
// Request body: {"project_root": string, "days": int, "target": "claude"|"agents"|"cursor"}
// Response: {"path": string, "changed": bool, "body": string}
func (s *Server) handleSuggestWrite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ProjectRoot string `json:"project_root"`
		Days        int    `json:"days"`
		Target      string `json:"target"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.ProjectRoot == "" {
		http.Error(w, "project_root required", http.StatusBadRequest)
		return
	}
	if req.Days <= 0 {
		req.Days = 30
	}
	target := req.Target
	if target == "" {
		target = "claude"
	}
	var (
		filename string
		body     string
	)
	in, err := suggest.Load(r.Context(), s.db(), suggest.Options{
		ProjectRoot: req.ProjectRoot,
		Days:        req.Days,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	now := time.Now().UTC()
	switch target {
	case "claude":
		filename = "CLAUDE.md"
		body = suggest.RenderMarkdown(in, now)
	case "agents":
		filename = "AGENTS.md"
		body = suggest.RenderMarkdown(in, now)
	case "cursor":
		filename = ".cursorrules"
		body = suggest.RenderCursorRules(in, now)
	default:
		http.Error(w, "target must be one of claude|agents|cursor", http.StatusBadRequest)
		return
	}
	path := req.ProjectRoot
	if !strings.HasSuffix(path, "/") {
		path += "/"
	}
	path += filename
	changed, err := suggest.Apply(path, body)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{
		"path":    path,
		"changed": changed,
		"target":  target,
		"body":    body,
	})
}

// handleTimeseriesCost serves /api/timeseries/cost?days=N&bucket=day|hour.
// Reuses the cost engine's GroupByDay aggregation; returns one point per
// bucket with token totals + cost. Bucket=hour walks api_turns directly
// since the engine doesn't support hour granularity.
func (s *Server) handleTimeseriesCost(w http.ResponseWriter, r *http.Request) {
	days := intArg(r, "days", 30, 1, 36500)
	bucket := r.URL.Query().Get("bucket")
	if bucket == "" {
		bucket = "day"
	}
	tool := r.URL.Query().Get("tool")
	project := r.URL.Query().Get("project")

	type point struct {
		Bucket                        string  `json:"bucket"`
		Input                         int64   `json:"input"`
		Output                        int64   `json:"output"`
		CacheRead                     int64   `json:"cache_read"`
		CacheCreation                 int64   `json:"cache_creation"`
		CostUSD                       float64 `json:"cost_usd"`
		TurnCount                     int     `json:"turn_count"`
		CompBytesSaved                int64   `json:"compression_bytes_saved"`
		CompTokensSaved               int64   `json:"compression_tokens_saved_est"`
		CompCostUSDSaved              float64 `json:"compression_cost_saved_usd_est"`
		CompCostUSDSavedInputTier     float64 `json:"compression_cost_saved_usd_est_input_tier"`
		CompCostUSDSavedCacheReadTier float64 `json:"compression_cost_saved_usd_est_cache_read_tier"`
		CompTurns                     int     `json:"compression_turns"`
	}

	if bucket == "day" {
		// Day-bucket: lean on the cost engine so pricing stays consistent
		// with /api/cost.
		summary, err := s.opts.CostEngine.Summary(r.Context(), s.db(), cost.Options{
			Days: days, GroupBy: cost.GroupByDay, Source: cost.SourceAuto, Limit: 365,
			Tool: tool, ProjectRoot: project,
		})
		if err != nil {
			writeErr(w, err)
			return
		}
		series := make([]point, 0, len(summary.Rows))
		for _, row := range summary.Rows {
			series = append(series, point{
				Bucket:                        row.Key,
				Input:                         row.Tokens.Input,
				Output:                        row.Tokens.Output,
				CacheRead:                     row.Tokens.CacheRead,
				CacheCreation:                 row.Tokens.CacheCreation,
				CostUSD:                       row.CostUSD,
				TurnCount:                     row.TurnCount,
				CompBytesSaved:                row.Compression.SavedBytesSigned(),
				CompTokensSaved:               row.Compression.TokensSavedEst,
				CompCostUSDSaved:              row.Compression.CostSavedUSDEst,
				CompCostUSDSavedInputTier:     row.Compression.CostSavedUSDEstInputTier,
				CompCostUSDSavedCacheReadTier: row.Compression.CostSavedUSDEstCacheReadTier,
				CompTurns:                     row.Compression.Turns,
			})
		}
		// cost.Engine.Summary sorts rows by cost_usd DESC for the
		// /api/cost top-N use case; re-sort here so the timeseries reads
		// chronologically (oldest left, newest right) on the chart axis.
		// ISO date strings sort correctly as strings.
		sort.SliceStable(series, func(i, j int) bool {
			return series[i].Bucket < series[j].Bucket
		})
		writeJSON(w, map[string]any{
			"metric": "cost",
			"bucket": "day",
			"days":   days,
			"series": series,
		})
		return
	}

	// Hour-bucket fallback — query api_turns directly. JSONL token_usage
	// rows are intentionally excluded from the hour view because their
	// timestamps aren't always when the API call happened (the JSONL
	// adapter parses files on disk; rows can land minutes after the
	// originating turn). Hour resolution only makes sense for the
	// proxy-sourced stream.
	since := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
	hourArgs := []any{since.Format(time.RFC3339Nano)}
	hourWhere := []string{"at.timestamp >= ?"}
	if project != "" {
		hourWhere = append(hourWhere, "p.root_path = ?")
		hourArgs = append(hourArgs, project)
	}
	if tool != "" {
		hourWhere = append(hourWhere, "s.tool = ?")
		hourArgs = append(hourArgs, tool)
	}
	//nolint:gosec // G202: SQL structure (WHERE/JOIN/scope fragments and any IN placeholder list) is built from code constants; all values are bound via ? args.
	hourQ := `SELECT strftime('%Y-%m-%dT%H:00:00Z', at.timestamp) AS bucket,
	                 COALESCE(SUM(at.input_tokens), 0),
	                 COALESCE(SUM(at.output_tokens), 0),
	                 COALESCE(SUM(at.cache_read_tokens), 0),
	                 COALESCE(SUM(at.cache_creation_tokens), 0),
	                 COUNT(*)
	          FROM api_turns at
	          LEFT JOIN projects p ON p.id = at.project_id
	          LEFT JOIN sessions s ON s.id = at.session_id
	          WHERE ` + strings.Join(hourWhere, " AND ") + `
	          GROUP BY bucket
	          ORDER BY bucket`
	rows, err := s.db().QueryContext(r.Context(), hourQ, hourArgs...)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	series := make([]point, 0)
	for rows.Next() {
		var p point
		if err := rows.Scan(&p.Bucket, &p.Input, &p.Output, &p.CacheRead, &p.CacheCreation, &p.TurnCount); err != nil {
			writeErr(w, err)
			return
		}
		series = append(series, p)
	}
	writeJSON(w, map[string]any{
		"metric": "cost",
		"bucket": "hour",
		"days":   days,
		"series": series,
	})
}

// handleTimeseriesTokensByModel serves /api/timeseries/tokens-by-model
// ?days=N&project=PATH. Returns one point per (day, model) pair so the
// Cost tab can render a stacked-bar chart of tokens per day with each
// model as its own series. Tokens, cost, and turn counts come from the
// cost engine in SourceAuto mode (proxy preferred, JSONL fallback) so
// the dedup/reliability semantics match /api/cost and
// /api/timeseries/cost exactly.
func (s *Server) handleTimeseriesTokensByModel(w http.ResponseWriter, r *http.Request) {
	days := intArg(r, "days", 30, 1, 36500)
	projectFilter := r.URL.Query().Get("project")
	toolFilter := r.URL.Query().Get("tool")

	type point struct {
		Bucket        string  `json:"bucket"`
		Model         string  `json:"model"`
		Input         int64   `json:"input"`
		Output        int64   `json:"output"`
		CacheRead     int64   `json:"cache_read"`
		CacheCreation int64   `json:"cache_creation"`
		TotalTokens   int64   `json:"total_tokens"`
		CostUSD       float64 `json:"cost_usd"`
		TurnCount     int     `json:"turn_count"`
	}

	summary, err := s.opts.CostEngine.Summary(r.Context(), s.db(), cost.Options{
		Days:        days,
		GroupBy:     cost.GroupByDayModel,
		Source:      cost.SourceAuto,
		ProjectRoot: projectFilter,
		Tool:        toolFilter,
		// Limit large enough to cover realistic windows: 365d × ~6 models
		// per day = 2190 buckets. Keep some headroom for pathological
		// many-model accounts.
		Limit: 5000,
	})
	if err != nil {
		writeErr(w, err)
		return
	}

	series := make([]point, 0, len(summary.Rows))
	for _, row := range summary.Rows {
		day, model := cost.SplitDayModelKey(row.Key)
		series = append(series, point{
			Bucket:        day,
			Model:         model,
			Input:         row.Tokens.Input,
			Output:        row.Tokens.Output,
			CacheRead:     row.Tokens.CacheRead,
			CacheCreation: row.Tokens.CacheCreation,
			TotalTokens:   row.Tokens.Input + row.Tokens.Output + row.Tokens.CacheRead + row.Tokens.CacheCreation,
			CostUSD:       row.CostUSD,
			TurnCount:     row.TurnCount,
		})
	}
	// Engine returns rows sorted by cost_usd DESC. Re-sort chronologically
	// (then by model for a stable stacking order within a day) so the
	// chart axis reads left-to-right.
	sort.SliceStable(series, func(i, j int) bool {
		if series[i].Bucket != series[j].Bucket {
			return series[i].Bucket < series[j].Bucket
		}
		return series[i].Model < series[j].Model
	})
	writeJSON(w, map[string]any{
		"metric": "tokens_by_model",
		"bucket": "day",
		"days":   days,
		"series": series,
	})
}

// handleTimeseriesActions serves /api/timeseries/actions?days=N&bucket=day|hour.
// Returns one point per bucket with action counts (total, successful,
// failed) and a per-tool breakdown so charts can stack by tool.
//
// Honors ?project=<root_path> to scope to a single project (mirrors the
// filter applied to /api/sessions and /api/actions). Without the
// filter, cross-project actions are summed.
func (s *Server) handleTimeseriesActions(w http.ResponseWriter, r *http.Request) {
	days := intArg(r, "days", 30, 1, 36500)
	bucket := r.URL.Query().Get("bucket")
	if bucket == "" {
		bucket = "day"
	}
	fmtSpec := "%Y-%m-%d"
	if bucket == "hour" {
		fmtSpec = "%Y-%m-%dT%H:00:00Z"
	}
	since := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
	project := r.URL.Query().Get("project")
	tool := r.URL.Query().Get("tool")
	args := []any{fmtSpec, since.Format(time.RFC3339Nano)}
	extra := ""
	if project != "" {
		extra += " AND project_id = (SELECT id FROM projects WHERE root_path = ?)"
		args = append(args, project)
	}
	if tool != "" {
		extra += " AND tool = ?"
		args = append(args, tool)
	}
	rows, err := s.db().QueryContext(r.Context(),
		//nolint:gosec // G202: SQL structure (WHERE/JOIN/scope fragments and any IN placeholder list) is built from code constants; all values are bound via ? args.
		`SELECT strftime(?, timestamp) AS bucket, tool,
		        COUNT(*),
		        SUM(CASE WHEN success = 0 THEN 1 ELSE 0 END)
		 FROM actions
		 WHERE timestamp >= ?`+extra+`
		 GROUP BY bucket, tool
		 ORDER BY bucket, tool`,
		args...)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()

	type point struct {
		Bucket   string         `json:"bucket"`
		Total    int            `json:"total"`
		Failures int            `json:"failures"`
		ByTool   map[string]int `json:"by_tool"`
	}
	byBucket := map[string]*point{}
	order := []string{}
	for rows.Next() {
		var b, tool string
		var n, fails int
		if err := rows.Scan(&b, &tool, &n, &fails); err != nil {
			writeErr(w, err)
			return
		}
		p, ok := byBucket[b]
		if !ok {
			p = &point{Bucket: b, ByTool: map[string]int{}}
			byBucket[b] = p
			order = append(order, b)
		}
		p.Total += n
		p.Failures += fails
		p.ByTool[tool] = n
	}
	series := make([]point, 0, len(order))
	for _, b := range order {
		series = append(series, *byBucket[b])
	}
	// Pin the contract: timeseries reads chronologically. The SQL
	// already orders by bucket ASC, but sort defensively so any future
	// upstream change can't silently flip chart axes.
	sort.SliceStable(series, func(i, j int) bool {
		return series[i].Bucket < series[j].Bucket
	})
	writeJSON(w, map[string]any{
		"metric": "actions",
		"bucket": bucket,
		"days":   days,
		"series": series,
	})
}

// handleModels serves /api/models?days=N — per-model breakdown over the
// window. Same shape as /api/cost but always group_by=model and JSON only.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	days := intArg(r, "days", 30, 1, 36500)
	tool := r.URL.Query().Get("tool")
	project := r.URL.Query().Get("project")
	summary, err := s.opts.CostEngine.Summary(r.Context(), s.db(), cost.Options{
		Days: days, GroupBy: cost.GroupByModel, Source: cost.SourceAuto, Limit: 50,
		Tool: tool, ProjectRoot: project,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	// Spec §13 cost-view annotation: attach per-model cache
	// annotations. Same shape as /api/cost?group_by=model so the
	// frontend's Cost page can render the same cache pill on
	// whichever endpoint it consumes.
	keys := make([]string, 0, len(summary.Rows))
	for _, row := range summary.Rows {
		keys = append(keys, row.Key)
	}
	if ann, derr := loadCacheAnnotationsByKey(r.Context(), s.db(), "model", keys); derr == nil && len(ann) > 0 {
		writeJSON(w, costSummaryWithCache{Summary: summary, CacheByKey: ann})
		return
	}
	writeJSON(w, summary)
}

// handleTools serves /api/tools?days=N — per-tool action volume + success
// rate over the window. Source: actions table.
func (s *Server) handleTools(w http.ResponseWriter, r *http.Request) {
	days := intArg(r, "days", 30, 1, 36500)
	project := r.URL.Query().Get("project")
	since := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
	args := []any{since.Format(time.RFC3339Nano)}
	where := []string{"timestamp >= ?"}
	if project != "" {
		where = append(where, "project_id = (SELECT id FROM projects WHERE root_path = ?)")
		args = append(args, project)
	}
	//nolint:gosec // G202: SQL structure (WHERE/JOIN/scope fragments and any IN placeholder list) is built from code constants; all values are bound via ? args.
	q := `SELECT tool, COUNT(*),
	             SUM(CASE WHEN success = 0 THEN 1 ELSE 0 END),
	             COUNT(DISTINCT session_id),
	             MIN(timestamp), MAX(timestamp)
	      FROM actions
	      WHERE ` + strings.Join(where, " AND ") + `
	      GROUP BY tool
	      ORDER BY COUNT(*) DESC`
	rows, err := s.db().QueryContext(r.Context(), q, args...)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	type toolRow struct {
		Tool         string  `json:"tool"`
		ActionCount  int     `json:"action_count"`
		FailureCount int     `json:"failure_count"`
		SuccessRate  float64 `json:"success_rate"`
		SessionCount int     `json:"session_count"`
		FirstSeen    string  `json:"first_seen"`
		LastSeen     string  `json:"last_seen"`
	}
	out := []toolRow{}
	for rows.Next() {
		var tr toolRow
		if err := rows.Scan(&tr.Tool, &tr.ActionCount, &tr.FailureCount,
			&tr.SessionCount, &tr.FirstSeen, &tr.LastSeen); err != nil {
			writeErr(w, err)
			return
		}
		if tr.ActionCount > 0 {
			tr.SuccessRate = 1 - float64(tr.FailureCount)/float64(tr.ActionCount)
		}
		out = append(out, tr)
	}
	writeJSON(w, map[string]any{
		"days":  days,
		"since": since.Format(time.RFC3339),
		"tools": out,
	})
}

// handleToolsBreakdown serves /api/tools/breakdown?days=N — per-tool
// action_type counts over the window. Powers the Tools tab's "what
// each AI client actually does" stacked bar (one row per tool, segments
// per action type). Honors ?project= and ?tool= filters.
func (s *Server) handleToolsBreakdown(w http.ResponseWriter, r *http.Request) {
	days := intArg(r, "days", 30, 1, 36500)
	since := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
	tool := r.URL.Query().Get("tool")
	project := r.URL.Query().Get("project")
	args := []any{since.Format(time.RFC3339Nano)}
	where := []string{"timestamp >= ?"}
	if tool != "" {
		where = append(where, "tool = ?")
		args = append(args, tool)
	}
	if project != "" {
		where = append(where, "project_id = (SELECT id FROM projects WHERE root_path = ?)")
		args = append(args, project)
	}
	//nolint:gosec // G202: SQL structure (WHERE/JOIN/scope fragments and any IN placeholder list) is built from code constants; all values are bound via ? args.
	q := `SELECT tool, action_type, COUNT(*)
	      FROM actions
	      WHERE ` + strings.Join(where, " AND ") + `
	      GROUP BY tool, action_type
	      ORDER BY tool, COUNT(*) DESC`
	rows, err := s.db().QueryContext(r.Context(), q, args...)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	type toolBreakdown struct {
		Tool   string         `json:"tool"`
		Total  int            `json:"total"`
		ByType map[string]int `json:"by_type"`
	}
	idx := map[string]*toolBreakdown{}
	order := []string{}
	for rows.Next() {
		var t, atype string
		var n int
		if err := rows.Scan(&t, &atype, &n); err != nil {
			writeErr(w, err)
			return
		}
		b, ok := idx[t]
		if !ok {
			b = &toolBreakdown{Tool: t, ByType: map[string]int{}}
			idx[t] = b
			order = append(order, t)
		}
		b.ByType[atype] = n
		b.Total += n
	}
	out := make([]toolBreakdown, 0, len(order))
	for _, t := range order {
		out = append(out, *idx[t])
	}
	// Sort by Total descending so the densest tool sits at the top of
	// the chart (matches user intuition).
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Total > out[j].Total
	})
	writeJSON(w, map[string]any{
		"days":  days,
		"tools": out,
	})
}

// handleProjects serves /api/projects — every project root the observer
// knows about, sorted by recent activity. Used by the dashboard toolbar
// to populate the project filter so users can scope Sessions / Actions /
// Cost / Discover queries to one project root.
// handleCompressionEvents serves /api/compression/events?days=N&page=&limit=
// — paginated per-event compression detail joined back to api_turns
// for model + session context. Driven by the compression_events table
// (migration 009). Mechanism is one of json/code/logs/text/diff/html
// (per-content-type compressor) or 'drop' (low-importance message
// replaced by a marker). Honors ?mechanism= and ?model= for narrowing.
func (s *Server) handleCompressionEvents(w http.ResponseWriter, r *http.Request) {
	days := intArg(r, "days", 30, 1, 36500)
	page := intArg(r, "page", 1, 1, 1_000_000)
	limit := intArg(r, "limit", 50, 1, 500)
	offset := (page - 1) * limit
	mechanism := r.URL.Query().Get("mechanism")
	model := r.URL.Query().Get("model")
	project := r.URL.Query().Get("project")
	tool := r.URL.Query().Get("tool")
	since := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)

	where := []string{"ce.timestamp >= ?"}
	args := []any{since.Format(time.RFC3339Nano)}
	if mechanism != "" {
		where = append(where, "ce.mechanism = ?")
		args = append(args, mechanism)
	}
	if model != "" {
		where = append(where, "at.model = ?")
		args = append(args, model)
	}
	if project != "" {
		where = append(where, "at.project_id = (SELECT id FROM projects WHERE root_path = ?)")
		args = append(args, project)
	}
	if tool != "" {
		where = append(where, "(SELECT tool FROM sessions WHERE id = at.session_id) = ?")
		args = append(args, tool)
	}
	whereClause := "WHERE " + strings.Join(where, " AND ")

	var total int
	if err := s.db().QueryRowContext(
		r.Context(),
		`SELECT COUNT(*) FROM compression_events ce
		 LEFT JOIN api_turns at ON at.id = ce.api_turn_id `+whereClause,
		args...,
	).Scan(&total); err != nil {
		writeErr(w, err)
		return
	}

	// is_subagent_runtime is derived per-row by correlating against
	// actions: an api_turn whose session_id has any sidechain (Agent
	// runtime) action within ±2 minutes of the turn's timestamp is
	// almost certainly a sub-agent's API call. EXISTS subquery on the
	// indexed (session_id, timestamp, is_sidechain) columns is fast
	// enough to compute inline at query time.
	rows, err := s.db().QueryContext(r.Context(),
		//nolint:gosec // G202: SQL structure (WHERE/JOIN/scope fragments and any IN placeholder list) is built from code constants; all values are bound via ? args.
		`SELECT ce.id, ce.api_turn_id, ce.timestamp, ce.mechanism,
		        ce.original_bytes, ce.compressed_bytes,
		        COALESCE(ce.msg_index, -1), COALESCE(ce.importance_score, 0),
		        COALESCE(at.model, ''), COALESCE(at.session_id, ''),
		        COALESCE(at.request_id, ''),
		        EXISTS (
		          SELECT 1 FROM actions a
		          WHERE a.session_id = at.session_id
		            AND a.is_sidechain = 1
		            AND ABS(strftime('%s', a.timestamp) - strftime('%s', ce.timestamp)) <= 120
		        ) AS is_subagent
		 FROM compression_events ce
		 LEFT JOIN api_turns at ON at.id = ce.api_turn_id
		 `+whereClause+`
		 ORDER BY ce.timestamp DESC, ce.id DESC
		 LIMIT ? OFFSET ?`,
		append(args, limit, offset)...)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()

	type eventRow struct {
		ID              int64  `json:"id"`
		APITurnID       int64  `json:"api_turn_id"`
		Timestamp       string `json:"timestamp"`
		Mechanism       string `json:"mechanism"`
		OriginalBytes   int64  `json:"original_bytes"`
		CompressedBytes int64  `json:"compressed_bytes"`
		SavedBytes      int64  `json:"saved_bytes"`
		// Token estimates derived from bytes via the 4 chars/token rule
		// of thumb (matches cost.CompressionStats.TokensSavedEst).
		// Same heuristic used by the cost engine's compression rollup
		// so the dashboard's per-event view stays consistent with the
		// summary numbers above the table.
		OriginalTokensEst   int64 `json:"original_tokens_est"`
		CompressedTokensEst int64 `json:"compressed_tokens_est"`
		SavedTokensEst      int64 `json:"saved_tokens_est"`
		// SavedUSDEst is saved_tokens_est × the row's model input rate.
		// Same formula as cost.Engine.Summary's per-row CostSavedUSDEst,
		// just applied per-event. Zero when the model is unrecognized.
		SavedUSDEst     float64 `json:"saved_usd_est"`
		MsgIndex        int     `json:"msg_index"`
		ImportanceScore float64 `json:"importance_score"`
		Model           string  `json:"model"`
		SessionID       string  `json:"session_id"`
		// MessageID is the upstream Anthropic msg_xxx id (sourced from
		// api_turns.request_id — same column the proxy populates). Lets
		// the UI link compression events to the same message thread on
		// the per-message timeline modal.
		MessageID string `json:"message_id"`
		// IsSubagentRuntime is true when the api_turn that produced
		// this event came from a sub-agent runtime — derived by
		// finding any sidechain action in the same session within
		// ±2 minutes of the turn's timestamp. Surfaces as a "Source"
		// pill on the events table so users can spot which mechanism
		// activity is attributable to delegated work.
		IsSubagentRuntime bool `json:"is_subagent_runtime"`
	}
	out := []eventRow{}
	for rows.Next() {
		var er eventRow
		var isSubInt int
		if err := rows.Scan(&er.ID, &er.APITurnID, &er.Timestamp, &er.Mechanism,
			&er.OriginalBytes, &er.CompressedBytes,
			&er.MsgIndex, &er.ImportanceScore,
			&er.Model, &er.SessionID, &er.MessageID, &isSubInt); err != nil {
			writeErr(w, err)
			return
		}
		er.SavedBytes = er.OriginalBytes - er.CompressedBytes
		er.OriginalTokensEst = er.OriginalBytes / 4
		er.CompressedTokensEst = er.CompressedBytes / 4
		er.SavedTokensEst = er.SavedBytes / 4
		if er.Model != "" {
			if pricing, ok := s.opts.CostEngine.Lookup(er.Model); ok && pricing.Input > 0 {
				er.SavedUSDEst = float64(er.SavedTokensEst) * pricing.Input / 1_000_000
			}
		}
		er.IsSubagentRuntime = isSubInt != 0
		out = append(out, er)
	}
	writeJSON(w, map[string]any{
		"rows":  out,
		"page":  page,
		"limit": limit,
		"total": total,
	})
}

// handleCompressionByModel serves /api/compression/by-model?days=N —
// per-model rollup of compression savings. One row per (model, mechanism)
// pair with event count, original/compressed bytes, saved bytes, and a
// best-effort $ estimate computed by pricing saved_bytes/4 tokens at the
// model's input rate (same convention as handleCompressionTimeseries).
//
// Drives the Compression tab's "Per-model breakdown" table (audit §3.7
// Cp11 / §4.7 dCp3). Sorted by saved_bytes DESC so the heaviest
// compressors lead.
func (s *Server) handleCompressionByModel(w http.ResponseWriter, r *http.Request) {
	days := intArg(r, "days", 30, 1, 36500)
	project := r.URL.Query().Get("project")
	tool := r.URL.Query().Get("tool")
	since := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
	where := []string{"ce.timestamp >= ?"}
	args := []any{since.Format(time.RFC3339Nano)}
	if project != "" {
		where = append(where, "at.project_id = (SELECT id FROM projects WHERE root_path = ?)")
		args = append(args, project)
	}
	if tool != "" {
		where = append(where, "(SELECT tool FROM sessions WHERE id = at.session_id) = ?")
		args = append(args, tool)
	}
	rows, err := s.db().QueryContext(r.Context(),
		//nolint:gosec // G202: SQL structure (WHERE/JOIN/scope fragments and any IN placeholder list) is built from code constants; all values are bound via ? args.
		`SELECT COALESCE(at.model, '(unknown)') AS model,
		        ce.mechanism,
		        COUNT(*) AS events,
		        SUM(ce.original_bytes) AS orig,
		        SUM(ce.compressed_bytes) AS comp
		 FROM compression_events ce
		 LEFT JOIN api_turns at ON at.id = ce.api_turn_id
		 WHERE `+strings.Join(where, " AND ")+`
		 GROUP BY model, ce.mechanism
		 ORDER BY (SUM(ce.original_bytes) - SUM(ce.compressed_bytes)) DESC`,
		args...)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()

	type row struct {
		Model           string  `json:"model"`
		Mechanism       string  `json:"mechanism"`
		Events          int     `json:"events"`
		OriginalBytes   int64   `json:"original_bytes"`
		CompressedBytes int64   `json:"compressed_bytes"`
		SavedBytes      int64   `json:"saved_bytes"`
		SavedTokensEst  int64   `json:"saved_tokens_est"`
		SavedUSDEst     float64 `json:"saved_usd_est"`
	}
	out := []row{}
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.Model, &r.Mechanism, &r.Events, &r.OriginalBytes, &r.CompressedBytes); err != nil {
			writeErr(w, err)
			return
		}
		r.SavedBytes = r.OriginalBytes - r.CompressedBytes
		// 4 bytes/token is the same lossy conversion handleCompression
		// Timeseries uses. Good enough for "savings" framing.
		r.SavedTokensEst = r.SavedBytes / 4
		if p, ok := s.opts.CostEngine.Lookup(r.Model); ok && r.SavedTokensEst > 0 {
			r.SavedUSDEst = (float64(r.SavedTokensEst) / 1_000_000) * p.Input
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{
		"days": days,
		"rows": out,
	})
}

// handleCompressionTimeseries serves /api/compression/timeseries?bucket=day&days=N
// — per-day savings split by mechanism for the "Savings by mechanism"
// chart. Returns one point per day with by_mechanism map of
// {mechanism: {count, original_bytes, compressed_bytes, saved_bytes,
// saved_usd_est}}.
//
// Per-mechanism $ is computed by joining compression_events back to
// api_turns for model context, looking up each model's input rate via
// the cost engine, and pricing (saved_bytes / 4) tokens at that rate.
// Models without pricing contribute to bytes/tokens but not to $ —
// matches the per-model breakdown in cost.Engine.Summary.
func (s *Server) handleCompressionTimeseries(w http.ResponseWriter, r *http.Request) {
	days := intArg(r, "days", 30, 1, 36500)
	project := r.URL.Query().Get("project")
	tool := r.URL.Query().Get("tool")
	since := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
	where := []string{"ce.timestamp >= ?"}
	args := []any{since.Format(time.RFC3339Nano)}
	if project != "" {
		where = append(where, "at.project_id = (SELECT id FROM projects WHERE root_path = ?)")
		args = append(args, project)
	}
	if tool != "" {
		where = append(where, "(SELECT tool FROM sessions WHERE id = at.session_id) = ?")
		args = append(args, tool)
	}
	rows, err := s.db().QueryContext(r.Context(),
		//nolint:gosec // G202: SQL structure (WHERE/JOIN/scope fragments and any IN placeholder list) is built from code constants; all values are bound via ? args.
		`SELECT strftime('%Y-%m-%d', ce.timestamp) AS bucket,
		        ce.mechanism,
		        COALESCE(at.model, '') AS model,
		        COUNT(*),
		        COALESCE(SUM(ce.original_bytes), 0),
		        COALESCE(SUM(ce.compressed_bytes), 0)
		 FROM compression_events ce
		 LEFT JOIN api_turns at ON at.id = ce.api_turn_id
		 WHERE `+strings.Join(where, " AND ")+`
		 GROUP BY bucket, ce.mechanism, model
		 ORDER BY bucket, ce.mechanism`,
		args...)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	type mechStats struct {
		Count           int     `json:"count"`
		OriginalBytes   int64   `json:"original_bytes"`
		CompressedBytes int64   `json:"compressed_bytes"`
		SavedBytes      int64   `json:"saved_bytes"`
		SavedUSDEst     float64 `json:"saved_usd_est"`
	}
	type point struct {
		Bucket      string                `json:"bucket"`
		ByMechanism map[string]*mechStats `json:"by_mechanism"`
		TotalSaved  int64                 `json:"total_saved_bytes"`
		TotalUSD    float64               `json:"total_saved_usd_est"`
		TotalCount  int                   `json:"total_count"`
	}
	idx := map[string]*point{}
	order := []string{}
	for rows.Next() {
		var b, mech, model string
		var n int
		var orig, comp int64
		if err := rows.Scan(&b, &mech, &model, &n, &orig, &comp); err != nil {
			writeErr(w, err)
			return
		}
		p, ok := idx[b]
		if !ok {
			p = &point{Bucket: b, ByMechanism: map[string]*mechStats{}}
			idx[b] = p
			order = append(order, b)
		}
		saved := orig - comp
		// Price savings at the model's input rate (matches
		// cost.Engine.Summary's CostSavedUSDEst formula). Unknown
		// models contribute 0 to $ but still show up in bytes/tokens.
		var savedUSD float64
		if model != "" {
			if pricing, ok := s.opts.CostEngine.Lookup(model); ok && pricing.Input > 0 {
				tokens := float64(saved) / 4
				savedUSD = tokens * pricing.Input / 1_000_000
			}
		}
		ms, exists := p.ByMechanism[mech]
		if !exists {
			ms = &mechStats{}
			p.ByMechanism[mech] = ms
		}
		ms.Count += n
		ms.OriginalBytes += orig
		ms.CompressedBytes += comp
		ms.SavedBytes += saved
		ms.SavedUSDEst += savedUSD
		p.TotalSaved += saved
		p.TotalUSD += savedUSD
		p.TotalCount += n
	}
	series := make([]point, 0, len(order))
	for _, b := range order {
		series = append(series, *idx[b])
	}
	sort.SliceStable(series, func(i, j int) bool {
		return series[i].Bucket < series[j].Bucket
	})
	writeJSON(w, map[string]any{
		"metric": "compression_events",
		"days":   days,
		"series": series,
	})
}

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db().QueryContext(r.Context(),
		`SELECT p.root_path,
		        (SELECT COUNT(*) FROM sessions s WHERE s.project_id = p.id) AS session_count,
		        (SELECT COUNT(*) FROM actions  a WHERE a.project_id = p.id) AS action_count,
		        (SELECT MAX(a.timestamp) FROM actions a WHERE a.project_id = p.id) AS last_seen
		 FROM projects p
		 ORDER BY last_seen DESC NULLS LAST, p.id DESC`)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	type projectRow struct {
		RootPath     string `json:"root_path"`
		SessionCount int    `json:"session_count"`
		ActionCount  int    `json:"action_count"`
		LastSeen     string `json:"last_seen,omitempty"`
	}
	out := []projectRow{}
	for rows.Next() {
		var pr projectRow
		var lastSeen sql.NullString
		if err := rows.Scan(&pr.RootPath, &pr.SessionCount, &pr.ActionCount, &lastSeen); err != nil {
			writeErr(w, err)
			return
		}
		if lastSeen.Valid {
			pr.LastSeen = lastSeen.String
		}
		out = append(out, pr)
	}
	if err := rows.Err(); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"rows": out})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func intArg(r *http.Request, key string, def, lo, hi int) int {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

// handleCompressionRetrieval serves /api/compression/retrieval?days=N —
// the K43 / Tier 3 self-learning feedback loop measurement: how many
// stashed bodies were actually retrieved and which shapes / actions
// the model returns to most often. Pairs with the G31 (CCR / stash)
// mechanism — `retrieve_rate` is the load-bearing dogfood metric for
// the strategic moat.
func (s *Server) handleCompressionRetrieval(w http.ResponseWriter, r *http.Request) {
	days := intArg(r, "days", 7, 1, 365)
	tool := r.URL.Query().Get("tool")
	project := r.URL.Query().Get("project")

	rep, err := learn.NewPatternMiner(s.db()).Report(r.Context(), learn.ReportOptions{
		Days: days, Tool: tool, Project: project,
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("retrieval report: %v", err), http.StatusInternalServerError)
		return
	}

	// Mirror the prior shape so existing JS consumers don't break.
	// retrieve_rate can exceed 1.0 when the model retrieves the same
	// sha multiple times — we surface the raw ratio and let the UI
	// render "% retrieves per stash" so > 100% reads naturally.
	type shaCount struct {
		Sha   string `json:"sha"`
		Count int    `json:"count"`
	}
	type actionCount struct {
		ActionID int64 `json:"action_id"`
		Count    int   `json:"count"`
	}
	out := struct {
		Days               int                   `json:"days"`
		StashRetrievals    int                   `json:"stash_retrievals"`
		SearchHits         int                   `json:"search_hits"`
		TotalStashes       int                   `json:"total_stashes"`
		RetrieveRate       float64               `json:"retrieve_rate"`
		TopRetrievedShas   []shaCount            `json:"top_retrieved_shas"`
		TopSearchedActions []actionCount         `json:"top_searched_actions"`
		StashedSamples     []stashedSample       `json:"stashed_samples"`
		Hints              []learn.ThresholdHint `json:"hints"`
	}{
		Days:               days,
		StashRetrievals:    rep.StashRetrievals,
		SearchHits:         rep.SearchHits,
		TotalStashes:       rep.TotalStashes,
		RetrieveRate:       rep.RetrieveRate,
		TopRetrievedShas:   make([]shaCount, 0, len(rep.TopRetrievedShas)),
		TopSearchedActions: make([]actionCount, 0, len(rep.TopSearchedActions)),
		StashedSamples:     s.loadStashedSamples(r.Context(), days, tool, project, rep.TopRetrievedShas),
		Hints:              rep.Hints,
	}
	if out.Hints == nil {
		out.Hints = []learn.ThresholdHint{}
	}
	for _, sc := range rep.TopRetrievedShas {
		out.TopRetrievedShas = append(out.TopRetrievedShas, shaCount{Sha: sc.Sha, Count: sc.Count})
	}
	for _, ac := range rep.TopSearchedActions {
		out.TopSearchedActions = append(out.TopSearchedActions, actionCount{ActionID: ac.ActionID, Count: ac.Count})
	}
	writeJSON(w, out)
}

// stashedSample is one entry in the "what's getting stashed" preview: a short
// scrubbed snippet of a stashed body, with how many times it was stashed and
// (when applicable) retrieved. Replaces the opaque "top retrieved SHAs" list.
type stashedSample struct {
	Sha            string `json:"sha"`
	Snippet        string `json:"snippet"`
	Bytes          int64  `json:"bytes"`
	Count          int    `json:"count"`
	RetrievedCount int    `json:"retrieved_count"`
}

// loadStashedSamples returns up to 8 recent distinct stashed bodies with a
// scrubbed content snippet, so the SROD panel shows what the proxy is actually
// offloading instead of raw SHAs. body_hash on a stash compression_event IS
// the stash sha, so we read each from the stash dir (best-effort — LRU-evicted
// shas are skipped). Returns an empty slice when no stash dir is wired or
// nothing readable remains, never an error (the panel degrades to counts).
func (s *Server) loadStashedSamples(ctx context.Context, days int, tool, project string, retrieved []learn.ShaCount) []stashedSample {
	empty := []stashedSample{}
	if s.opts.StashDir == "" {
		return empty
	}
	st, err := stash.New(stash.Options{Dir: s.opts.StashDir})
	if err != nil {
		return empty
	}
	retrievedCount := make(map[string]int, len(retrieved))
	for _, x := range retrieved {
		retrievedCount[x.Sha] = x.Count
	}

	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339Nano)
	join, where := "", ""
	args := []any{cutoff}
	if tool != "" || project != "" {
		join = ` LEFT JOIN api_turns at ON at.id = ce.api_turn_id
		         LEFT JOIN sessions ss ON ss.id = at.session_id
		         LEFT JOIN projects p ON p.id = ss.project_id`
		if tool != "" {
			where += " AND ss.tool = ?"
			args = append(args, tool)
		}
		if project != "" {
			where += " AND p.root_path = ?"
			args = append(args, project)
		}
	}
	//nolint:gosec // G202: join/where are code-constant fragments; all values are bound via ? args.
	q := `SELECT ce.body_hash, COUNT(*), MAX(COALESCE(ce.original_bytes, 0))
	        FROM compression_events ce` + join + `
	       WHERE ce.mechanism = 'stash' AND ce.body_hash IS NOT NULL AND ce.body_hash != ''
	         AND ce.timestamp >= ?` + where + `
	       GROUP BY ce.body_hash
	       ORDER BY MAX(ce.timestamp) DESC
	       LIMIT 24`
	rows, err := s.db().QueryContext(ctx, q, args...)
	if err != nil {
		return empty
	}
	defer rows.Close()

	scr := scrub.New()
	samples := empty
	for rows.Next() {
		var sha string
		var count int
		var bytes int64
		if err := rows.Scan(&sha, &count, &bytes); err != nil {
			return samples
		}
		content, rerr := st.Read(sha)
		if rerr != nil {
			continue // evicted / missing / corrupt → skip
		}
		snip := makeStashSnippet(scr, content, 160)
		if snip == "" {
			continue
		}
		samples = append(samples, stashedSample{
			Sha:            sha,
			Snippet:        snip,
			Bytes:          bytes,
			Count:          count,
			RetrievedCount: retrievedCount[sha],
		})
		if len(samples) >= 8 {
			break
		}
	}
	return samples
}

// makeStashSnippet scrubs a stashed body and renders a single-line preview of
// at most n runes. The body is the model's raw (pre-scrub) tool output, so the
// scrub pass is load-bearing for display safety even on the local dashboard.
func makeStashSnippet(scr *scrub.Scrubber, body []byte, n int) string {
	s := string(body)
	if len(s) > 4096 {
		s = s[:4096] // bound scrub work — we only need the head
	}
	if scr != nil {
		s = scr.String(s)
	}
	s = strings.Join(strings.Fields(s), " ") // collapse newlines/runs to single spaces
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// handleCompactionEvents serves /api/compaction/events?days=N — the
// D23 / Tier 3 compaction-survival visibility surface. Counts
// compaction_events rows (one per /compact in the window), surfaces
// how many had post-compact recovery context injected, and lists the
// recent events with session_id + ghost-files-after count parsed out
// of the JSON snapshot.
func (s *Server) handleCompactionEvents(w http.ResponseWriter, r *http.Request) {
	days := intArg(r, "days", 7, 1, 365)
	tool := r.URL.Query().Get("tool")
	project := r.URL.Query().Get("project")
	since := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339Nano)

	type eventRow struct {
		ID                int64  `json:"id"`
		SessionID         string `json:"session_id"`
		Timestamp         string `json:"timestamp"`
		Tool              string `json:"tool"`
		PreActionCount    int    `json:"pre_action_count"`
		InjectedAt        string `json:"injected_at,omitempty"`
		GhostFilesAfter   int    `json:"ghost_files_after_count"`
		FileSnapshotCount int    `json:"file_snapshot_count"`
	}
	out := struct {
		Days             int        `json:"days"`
		Count            int        `json:"count"`
		SessionsAffected int        `json:"sessions_affected"`
		InjectionsFired  int        `json:"injections_fired"`
		Events           []eventRow `json:"events"`
	}{Days: days, Events: []eventRow{}}

	// compaction_events has direct tool + project_id columns — no
	// joins needed for filtering. Project lookup via projects table.
	whereExtra := ""
	args := []any{since}
	if tool != "" {
		whereExtra += " AND tool = ?"
		args = append(args, tool)
	}
	if project != "" {
		whereExtra += " AND project_id = (SELECT id FROM projects WHERE root_path = ?)"
		args = append(args, project)
	}

	_ = s.db().QueryRowContext(r.Context(),
		`SELECT COUNT(*), COUNT(DISTINCT session_id),
		        COALESCE(SUM(CASE WHEN injected_at IS NOT NULL THEN 1 ELSE 0 END), 0)
		 FROM compaction_events WHERE timestamp >= ?`+whereExtra,
		args...).Scan(&out.Count, &out.SessionsAffected, &out.InjectionsFired)

	rows, err := s.db().QueryContext(r.Context(),
		//nolint:gosec // G202: SQL structure (WHERE/JOIN/scope fragments and any IN placeholder list) is built from code constants; all values are bound via ? args.
		`SELECT id, session_id, timestamp, COALESCE(tool, ''),
		        COALESCE(pre_action_count, 0),
		        COALESCE(injected_at, ''),
		        COALESCE(ghost_files_after, ''),
		        COALESCE(file_state_snapshot, '')
		 FROM compaction_events
		 WHERE timestamp >= ?`+whereExtra+`
		 ORDER BY timestamp DESC LIMIT 50`,
		args...)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var e eventRow
		var ghostsJSON, snapJSON string
		if err := rows.Scan(&e.ID, &e.SessionID, &e.Timestamp, &e.Tool,
			&e.PreActionCount, &e.InjectedAt, &ghostsJSON, &snapJSON); err != nil {
			writeErr(w, err)
			return
		}
		// Count ghost files (a JSON array of paths) without
		// unmarshalling — substring count of `","` + 1 if non-empty.
		// Cheap heuristic; defensible when the field is "[]" or empty.
		if ghostsJSON != "" && ghostsJSON != "[]" && ghostsJSON != "null" {
			var ghosts []string
			if err := json.Unmarshal([]byte(ghostsJSON), &ghosts); err == nil {
				e.GhostFilesAfter = len(ghosts)
			}
		}
		if snapJSON != "" && snapJSON != "null" {
			var snap struct {
				FileCount int `json:"file_count"`
			}
			if err := json.Unmarshal([]byte(snapJSON), &snap); err == nil {
				e.FileSnapshotCount = snap.FileCount
			}
		}
		out.Events = append(out.Events, e)
	}
	writeJSON(w, out)
}

// handleCompressionRollingCost serves /api/compression/rolling-cost?days=N
// — the D20 cost-net surface. Anthropic Haiku summary calls go directly
// to api.anthropic.com (NOT through our proxy), so api_turns doesn't
// see them. We instead read the dedicated `summary_calls` table
// populated by [messagesummary.AnthropicSummarizer] and join against
// `compression_events.mechanism = 'rolling_summary'` to estimate the
// net delta (savings on cache_creation - Haiku spend).
func (s *Server) handleCompressionRollingCost(w http.ResponseWriter, r *http.Request) {
	days := intArg(r, "days", 7, 1, 365)
	tool := r.URL.Query().Get("tool")
	project := r.URL.Query().Get("project")
	since := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339Nano)

	out := struct {
		Days                     int     `json:"days"`
		SummaryCalls             int     `json:"summary_calls"`
		SummaryInputTokens       int64   `json:"summary_input_tokens"`
		SummaryOutputTokens      int64   `json:"summary_output_tokens"`
		SummaryCostUSD           float64 `json:"summary_cost_usd"`
		RollingSavingsBytes      int64   `json:"rolling_savings_bytes"`
		RollingSavingsTokensEst  int64   `json:"rolling_savings_tokens_est"`
		RollingSavingsCostUSDEst float64 `json:"rolling_savings_cost_usd_est"`
		NetDeltaUSD              float64 `json:"net_delta_usd"`
	}{Days: days}

	// Build optional tool/project filter clauses for summary_calls
	// (joins through sessions → projects) and compression_events
	// (joins through api_turns → sessions → projects).
	scJoin, scWhere, scArgs := "", "", []any{since}
	if tool != "" || project != "" {
		scJoin = ` LEFT JOIN sessions s ON s.id = sc.session_id
		           LEFT JOIN projects p ON p.id = s.project_id`
		if tool != "" {
			scWhere += " AND s.tool = ?"
			scArgs = append(scArgs, tool)
		}
		if project != "" {
			scWhere += " AND p.root_path = ?"
			scArgs = append(scArgs, project)
		}
	}
	_ = s.db().QueryRowContext(r.Context(),
		`SELECT COUNT(*),
		        COALESCE(SUM(sc.input_tokens), 0),
		        COALESCE(SUM(sc.output_tokens), 0),
		        COALESCE(SUM(sc.cost_usd), 0)
		 FROM summary_calls sc`+scJoin+
			` WHERE sc.timestamp >= ?`+scWhere,
		scArgs...).Scan(&out.SummaryCalls, &out.SummaryInputTokens, &out.SummaryOutputTokens, &out.SummaryCostUSD)

	ceJoin, ceWhere, ceArgs := "", "", []any{since}
	if tool != "" || project != "" {
		ceJoin = ` LEFT JOIN sessions s ON s.id = at.session_id
		           LEFT JOIN projects p ON p.id = s.project_id`
		if tool != "" {
			ceWhere += " AND s.tool = ?"
			ceArgs = append(ceArgs, tool)
		}
		if project != "" {
			ceWhere += " AND p.root_path = ?"
			ceArgs = append(ceArgs, project)
		}
	}
	rows, err := s.db().QueryContext(r.Context(),
		//nolint:gosec // G202: SQL structure (WHERE/JOIN/scope fragments and any IN placeholder list) is built from code constants; all values are bound via ? args.
		`SELECT COALESCE(at.model, ''),
		        COALESCE(SUM(ce.original_bytes - ce.compressed_bytes), 0)
		 FROM compression_events ce
		 LEFT JOIN api_turns at ON at.id = ce.api_turn_id`+ceJoin+
			` WHERE ce.mechanism = 'rolling_summary' AND ce.timestamp >= ?`+ceWhere+`
		 GROUP BY at.model`,
		ceArgs...)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var model string
			var saved int64
			if err := rows.Scan(&model, &saved); err != nil {
				continue
			}
			out.RollingSavingsBytes += saved
			tokens := saved / 4
			out.RollingSavingsTokensEst += tokens
			if model != "" {
				if pricing, ok := s.opts.CostEngine.Lookup(model); ok && pricing.CacheCreation > 0 {
					// rolling_summary saves bytes that would otherwise
					// be cache_creation tokens (the conversation
					// prefix would have to be re-cached on the next
					// turn without the summary). Price at the
					// CacheCreation rate, not Input.
					out.RollingSavingsCostUSDEst += float64(tokens) * pricing.CacheCreation / 1_000_000
				}
			}
		}
	}
	out.NetDeltaUSD = out.RollingSavingsCostUSDEst - out.SummaryCostUSD
	writeJSON(w, out)
}
