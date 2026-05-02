package dashboard

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/diag"
	"github.com/marmutapp/superbased-observer/internal/intelligence/cost"
	"github.com/marmutapp/superbased-observer/internal/intelligence/discover"
)

//go:embed static
var assets embed.FS

// Options configures a Server.
type Options struct {
	// DB is the observer database.
	DB *sql.DB
	// DBPath is displayed in the header; not used to open anything.
	DBPath string
	// CostEngine prices token summaries. Defaults to baked-in pricing.
	CostEngine *cost.Engine
	// Logger receives operational messages.
	Logger *slog.Logger
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

	// execBackfill spawns the backfill subprocess. Default points at
	// realExecBackfill which os/exec's the observer binary. Tests
	// override with a fake to avoid requiring the binary in PATH.
	execBackfill backfillExecFn
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
	return &Server{
		opts:         opts,
		backfillJobs: map[string]*backfillJob{},
		execBackfill: realExecBackfill,
	}, nil
}

// Handler returns the dashboard's http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(assets, "static")
	mux.Handle("/", http.FileServer(http.FS(sub)))
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/cost", s.handleCost)
	mux.HandleFunc("/api/discover", s.handleDiscover)
	mux.HandleFunc("/api/sessions", s.handleSessions)
	mux.HandleFunc("/api/session/", s.handleSessionDetail) // /api/session/<id>
	mux.HandleFunc("/api/actions", s.handleActions)
	mux.HandleFunc("/api/patterns", s.handlePatterns)
	mux.HandleFunc("/api/timeseries/cost", s.handleTimeseriesCost)
	mux.HandleFunc("/api/timeseries/tokens-by-model", s.handleTimeseriesTokensByModel)
	mux.HandleFunc("/api/timeseries/actions", s.handleTimeseriesActions)
	mux.HandleFunc("/api/models", s.handleModels)
	mux.HandleFunc("/api/tools", s.handleTools)
	mux.HandleFunc("/api/tools/breakdown", s.handleToolsBreakdown)
	mux.HandleFunc("/api/compression/events", s.handleCompressionEvents)
	mux.HandleFunc("/api/compression/timeseries", s.handleCompressionTimeseries)
	mux.HandleFunc("/api/projects", s.handleProjects)
	mux.HandleFunc("/api/export.xlsx", s.handleExportXLSX)
	mux.HandleFunc("/api/analysis/headline", s.handleAnalysisHeadline)
	mux.HandleFunc("/api/analysis/trend", s.handleAnalysisTrend)
	mux.HandleFunc("/api/analysis/movers", s.handleAnalysisMovers)
	mux.HandleFunc("/api/analysis/top-sessions", s.handleAnalysisTopSessions)
	mux.HandleFunc("/api/analysis/routing-suggestions", s.handleAnalysisRoutingSuggestions)
	mux.HandleFunc("/api/config", s.handleConfig)                                 // GET full config
	mux.HandleFunc("/api/config/pricing", s.handleConfigPricing)                  // PUT pricing overrides (hot-reload)
	mux.HandleFunc("/api/config/pricing/defaults", s.handleConfigPricingDefaults) // GET baked-in defaults
	mux.HandleFunc("/api/config/section/", s.handleConfigSection)                 // PUT /api/config/section/<name>
	mux.HandleFunc("/api/admin/restart", s.handleAdminRestart)                    // POST → os.Exit(0)
	mux.HandleFunc("/api/backfill/status", s.handleBackfillStatus)                // GET candidate counts
	mux.HandleFunc("/api/backfill/run", s.handleBackfillRun)                      // POST {mode}
	mux.HandleFunc("/api/backfill/jobs/", s.handleBackfillJob)                    // GET /jobs/<id>
	mux.HandleFunc("/api/health/watcher", s.handleWatcherHealth)                  // GET watcher cursor vs file size
	return mux
}

// ListenAndServe runs the dashboard on addr until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
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

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	snap, err := diag.Snapshot(r.Context(), s.opts.DB, s.opts.DBPath)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, snap)
}

func (s *Server) handleCost(w http.ResponseWriter, r *http.Request) {
	days := intArg(r, "days", 30, 1, 36500)
	groupBy := r.URL.Query().Get("group_by")
	if groupBy == "" {
		groupBy = "model"
	}
	proj := r.URL.Query().Get("project")
	source := r.URL.Query().Get("source")
	if source == "" {
		source = "auto"
	}
	summary, err := s.opts.CostEngine.Summary(r.Context(), s.opts.DB, cost.Options{
		Days:        days,
		GroupBy:     cost.GroupBy(groupBy),
		Source:      cost.Source(source),
		ProjectRoot: proj,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, summary)
}

// handleDiscover serves /api/discover. Paginates the stale_reads and
// repeated_commands panels independently — stale_page/stale_limit and
// repeated_page/repeated_limit query params, defaulting to 20 rows per
// page. Backend caps total results at 500 per panel (discover SQL runs
// once per request and the dashboard surfaces top-N anyway); both
// panels expose stale_total / repeated_total for the pager UI.
func (s *Server) handleDiscover(w http.ResponseWriter, r *http.Request) {
	days := intArg(r, "days", 30, 1, 36500)
	stalePage := intArg(r, "stale_page", 1, 1, 1_000_000)
	staleLimit := intArg(r, "stale_limit", 20, 1, 500)
	repeatedPage := intArg(r, "repeated_page", 1, 1, 1_000_000)
	repeatedLimit := intArg(r, "repeated_limit", 20, 1, 500)
	proj := r.URL.Query().Get("project")

	// Cap the per-panel SQL limit at 500 — generous enough for realistic
	// dashboards while keeping a single discover.Run cheap.
	report, err := discover.New(s.opts.DB).Run(r.Context(), discover.Options{
		ProjectRoot: proj, Days: days, Limit: 500,
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
	blendedRate, err := s.opts.CostEngine.BlendedInputRate(r.Context(), s.opts.DB, 30)
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
		since := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
		where = append(where, "s.started_at >= ?")
		args = append(args, since.Format(time.RFC3339Nano))
	}
	whereClause := ""
	if len(where) > 0 {
		whereClause = "WHERE " + strings.Join(where, " AND ")
	}

	// Total row count for pagination. Must share the same WHERE as the
	// data query so page math stays coherent.
	var total int
	countArgs := append([]any{}, args...)
	if err := s.opts.DB.QueryRowContext(r.Context(),
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
	_ = s.opts.DB.QueryRowContext(r.Context(),
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
	dataArgs = append(dataArgs, limit, offset)
	rows, err := s.opts.DB.QueryContext(r.Context(),
		`SELECT s.id, s.tool, COALESCE(p.root_path, ''), s.started_at,
		        (SELECT COUNT(*) FROM actions a WHERE a.session_id = s.id) AS total_actions,
		        (SELECT COUNT(*) FROM actions a WHERE a.session_id = s.id AND a.is_sidechain = 1) AS sidechain_actions,
		        s.quality_score, s.error_rate, s.redundancy_ratio
		 FROM sessions s
		 LEFT JOIN projects p ON p.id = s.project_id
		 `+whereClause+`
		 ORDER BY s.started_at DESC LIMIT ? OFFSET ?`, dataArgs...)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	type sessRow struct {
		ID           string `json:"id"`
		Tool         string `json:"tool"`
		Project      string `json:"project"`
		StartedAt    string `json:"started_at"`
		TotalActions int    `json:"total_actions"`
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
		// Tokens / Cost / Reliability are attached post-scan from the
		// cost engine's GroupBySession rollup so dedup (proxy preferred,
		// JSONL fallback) matches /api/cost exactly. Tokens is the
		// billable sum (input + output + cache_read + cache_creation);
		// dashboards can break it out from the detail modal.
		TotalTokens int64   `json:"total_tokens"`
		CostUSD     float64 `json:"cost_usd"`
		// CostReliability is the worst-case reliability across the
		// rows that fed this session's totals. Surfaces as a pill on
		// the Sessions table so users know which numbers to trust.
		CostReliability string `json:"cost_reliability,omitempty"`
	}
	var out []sessRow
	for rows.Next() {
		var sr sessRow
		var q, er, rr sql.NullFloat64
		if err := rows.Scan(&sr.ID, &sr.Tool, &sr.Project, &sr.StartedAt, &sr.TotalActions, &sr.SidechainActionCount, &q, &er, &rr); err != nil {
			writeErr(w, err)
			return
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
		out = append(out, sr)
	}
	if err := rows.Err(); err != nil {
		writeErr(w, err)
		return
	}
	if out == nil {
		out = []sessRow{}
	}

	// Attach per-session token totals + cost from the cost engine. We
	// query GroupBySession over the longest realistic window (365d)
	// since the sessions list paginates over arbitrary history; without
	// a wide window, older sessions on later pages would render as
	// 0 tokens. The per-session map is then joined back onto the
	// page slice — O(rowsPerPage) lookups, no extra DB roundtrip per
	// session.
	if len(out) > 0 {
		costSummary, err := s.opts.CostEngine.Summary(r.Context(), s.opts.DB, cost.Options{
			Days:        365,
			GroupBy:     cost.GroupBySession,
			Source:      cost.SourceAuto,
			ProjectRoot: project,
			Limit:       100_000,
		})
		if err != nil {
			s.opts.Logger.Warn("sessions: per-session cost rollup failed", "err", err)
		} else {
			byID := make(map[string]cost.Row, len(costSummary.Rows))
			for _, row := range costSummary.Rows {
				byID[row.Key] = row
			}
			for i, sr := range out {
				row, ok := byID[sr.ID]
				if !ok {
					continue
				}
				out[i].TotalTokens = row.Tokens.Input + row.Tokens.Output +
					row.Tokens.CacheRead + row.Tokens.CacheCreation
				out[i].CostUSD = row.CostUSD
				out[i].CostReliability = row.Reliability
			}
		}
	}

	writeJSON(w, map[string]any{
		"rows":         out,
		"page":         page,
		"limit":        limit,
		"total":        total,
		"scored_count": scoredCount,
		"days":         days,
	})
}

func (s *Server) handleActions(w http.ResponseWriter, r *http.Request) {
	limit := intArg(r, "limit", 50, 1, 500)
	page := intArg(r, "page", 1, 1, 1_000_000)
	offset := (page - 1) * limit
	tool := r.URL.Query().Get("tool")
	sessionID := r.URL.Query().Get("session_id")
	actionType := r.URL.Query().Get("action_type")
	project := r.URL.Query().Get("project")

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
	whereClause := ""
	if len(where) > 0 {
		whereClause = "WHERE " + strings.Join(where, " AND ")
	}

	var total int
	countArgs := append([]any{}, args...)
	if err := s.opts.DB.QueryRowContext(r.Context(),
		"SELECT COUNT(*) FROM actions a "+whereClause, countArgs...,
	).Scan(&total); err != nil {
		writeErr(w, err)
		return
	}

	dataArgs := append([]any{}, args...)
	dataArgs = append(dataArgs, limit, offset)
	rows, err := s.opts.DB.QueryContext(r.Context(),
		`SELECT a.id, a.timestamp, a.tool, a.session_id,
		        COALESCE(p.root_path, ''), a.action_type,
		        COALESCE(a.raw_tool_name, ''), COALESCE(a.target, ''),
		        COALESCE(a.success, 1), COALESCE(a.error_message, ''),
		        COALESCE(a.message_id, '')
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
	}
	var out []actionRow
	for rows.Next() {
		var ar actionRow
		if err := rows.Scan(&ar.ID, &ar.Timestamp, &ar.Tool, &ar.SessionID, &ar.Project,
			&ar.ActionType, &ar.RawToolName, &ar.Target, &ar.Success, &ar.ErrorMessage,
			&ar.MessageID); err != nil {
			writeErr(w, err)
			return
		}
		out = append(out, ar)
	}
	if err := rows.Err(); err != nil {
		writeErr(w, err)
		return
	}
	if out == nil {
		out = []actionRow{}
	}
	writeJSON(w, map[string]any{
		"rows":  out,
		"page":  page,
		"limit": limit,
		"total": total,
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
	if id == "" {
		http.Error(w, "missing session id", http.StatusBadRequest)
		return
	}
	type modelBucket struct {
		Model         string  `json:"model"`
		Input         int64   `json:"input"`
		Output        int64   `json:"output"`
		CacheRead     int64   `json:"cache_read"`
		CacheCreation int64   `json:"cache_creation"`
		TurnCount     int64   `json:"turn_count"`
		CostUSD       float64 `json:"cost_usd"`
	}
	type sessionDetail struct {
		ID              string           `json:"id"`
		Tool            string           `json:"tool"`
		Project         string           `json:"project"`
		Model           string           `json:"model,omitempty"`
		StartedAt       string           `json:"started_at"`
		EndedAt         *string          `json:"ended_at,omitempty"`
		TotalActions    int              `json:"total_actions"`
		SuccessActions  int              `json:"success_actions"`
		FailureActions  int              `json:"failure_actions"`
		QualityScore    *float64         `json:"quality_score,omitempty"`
		ErrorRate       *float64         `json:"error_rate,omitempty"`
		RedundancyRatio *float64         `json:"redundancy_ratio,omitempty"`
		Tokens          map[string]int64 `json:"tokens"`
		// PerModel breaks the deduped tokens + cost out by model so the
		// session detail modal shows haiku and opus separately when a
		// session uses both (Claude Code's main vs sub-agent split, etc.).
		PerModel      []modelBucket  `json:"per_model"`
		CostUSD       float64        `json:"cost_usd"`
		ToolBreakdown []actionBucket `json:"tool_breakdown"`
	}

	var d sessionDetail
	d.ID = id
	var endedAt sql.NullString
	var q, er, rr sql.NullFloat64
	var model sql.NullString
	if err := s.opts.DB.QueryRowContext(r.Context(),
		`SELECT s.tool, COALESCE(p.root_path, ''), s.model, s.started_at,
		        s.ended_at, s.quality_score, s.error_rate, s.redundancy_ratio
		 FROM sessions s LEFT JOIN projects p ON p.id = s.project_id
		 WHERE s.id = ?`, id,
	).Scan(&d.Tool, &d.Project, &model, &d.StartedAt, &endedAt, &q, &er, &rr); err != nil {
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

	// Action aggregates and tool breakdown.
	if err := s.opts.DB.QueryRowContext(r.Context(),
		`SELECT COUNT(*),
		        SUM(CASE WHEN success = 0 THEN 0 ELSE 1 END),
		        SUM(CASE WHEN success = 0 THEN 1 ELSE 0 END)
		 FROM actions WHERE session_id = ?`, id,
	).Scan(&d.TotalActions, &d.SuccessActions, &d.FailureActions); err != nil {
		writeErr(w, err)
		return
	}
	brRows, err := s.opts.DB.QueryContext(r.Context(),
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
	const dedupedRowsCTE = `WITH proxy_turn_ids AS (
		SELECT request_id FROM api_turns
		 WHERE session_id = ? AND request_id IS NOT NULL AND request_id != ''
	),
	combined AS (
		SELECT model, input_tokens, output_tokens, cache_read_tokens,
		       cache_creation_tokens, cache_creation_1h_tokens, cost_usd
		FROM api_turns WHERE session_id = ?
		UNION ALL
		SELECT model, input_tokens, output_tokens, cache_read_tokens,
		       cache_creation_tokens, cache_creation_1h_tokens, estimated_cost_usd
		FROM token_usage
		WHERE session_id = ?
		  AND (source_event_id IS NULL OR source_event_id = ''
		       OR source_event_id NOT IN (SELECT request_id FROM proxy_turn_ids))
	)`

	sessionModel := d.Model
	rows, err := s.opts.DB.QueryContext(r.Context(),
		dedupedRowsCTE+`
		SELECT COALESCE(NULLIF(model, ''), ?),
		       COALESCE(input_tokens, 0),
		       COALESCE(output_tokens, 0),
		       COALESCE(cache_read_tokens, 0),
		       COALESCE(cache_creation_tokens, 0),
		       COALESCE(cache_creation_1h_tokens, 0),
		       COALESCE(cost_usd, 0)
		FROM combined`,
		id, id, id, sessionModel)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()

	bucketByModel := map[string]*modelBucket{}
	bucketOrder := []string{}
	var totalIn, totalOut, totalCR, totalCC int64
	for rows.Next() {
		var modelKey string
		var bundle cost.TokenBundle
		var recorded float64
		if err := rows.Scan(&modelKey,
			&bundle.Input, &bundle.Output,
			&bundle.CacheRead, &bundle.CacheCreation, &bundle.CacheCreation1h,
			&recorded); err != nil {
			writeErr(w, err)
			return
		}
		// Per-row cost: prefer recorded estimated_cost_usd / cost_usd
		// when non-zero (only OpenCode + Pi adapters set it today; api_turns
		// never carries it because no provider returns it). Otherwise
		// compute from pricing — Compute reads the bundle's prompt size
		// to dispatch LC vs standard rates.
		var rowCost float64
		if recorded > 0 {
			rowCost = recorded
		} else if computed, ok := s.opts.CostEngine.Compute(modelKey, bundle); ok {
			rowCost = computed
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
		mb.TurnCount++
		mb.CostUSD += rowCost

		d.CostUSD += rowCost
		totalIn += bundle.Input
		totalOut += bundle.Output
		totalCR += bundle.CacheRead
		totalCC += bundle.CacheCreation
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
	}
	d.PerModel = perModel

	writeJSON(w, d)
}

type actionBucket struct {
	ActionType string `json:"action_type"`
	Count      int    `json:"count"`
	Failures   int    `json:"failures"`
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
	_ = s.opts.DB.QueryRowContext(r.Context(),
		`SELECT COALESCE(model, '') FROM sessions WHERE id = ?`, sessionID,
	).Scan(&sessionModel)
	type toolCallRow struct {
		ActionType   string `json:"action_type"`
		RawToolName  string `json:"raw_tool_name"`
		Target       string `json:"target"`
		Success      bool   `json:"success"`
		ErrorMessage string `json:"error_message,omitempty"`
		Timestamp    string `json:"timestamp"`
		// DurationMs is the per-tool-call wall-clock duration in ms
		// (sourced from actions.duration_ms). Adapters populate this
		// where the source data carries timing — codex via the
		// function_call→output timestamp gap, claude-code via
		// tool_use→tool_result gap, copilot via elapsedMs. Zero when
		// the source provided no timing signal or the row predates
		// the v1.4.28 capture work.
		DurationMs int64 `json:"duration_ms,omitempty"`
	}
	type messageRow struct {
		MessageID     string        `json:"message_id"`
		Timestamp     string        `json:"timestamp"`
		Role          string        `json:"role"`
		Model         string        `json:"model,omitempty"`
		Input         int64         `json:"input"`
		Output        int64         `json:"output"`
		CacheRead     int64         `json:"cache_read"`
		CacheCreation int64         `json:"cache_creation"`
		CacheCw1h     int64         `json:"cache_creation_1h"`
		CostUSD       float64       `json:"cost_usd"`
		// ElapsedMs is the wall-clock gap between this message's
		// timestamp and the next message's. For user rows it
		// approximates "time the assistant took to respond"; for
		// assistant rows it approximates "time the user took before
		// sending the next prompt". null on the last message in the
		// session (no successor to subtract from). Computed
		// post-sort, after pagination boundaries are decided.
		ElapsedMs     *int64        `json:"elapsed_ms,omitempty"`
		// ToolDurationMs is the sum of contained tool_calls'
		// duration_ms — the assistant's tool-execution time for
		// this turn. Differs from ElapsedMs (which spans the entire
		// gap to the next message, including the model's reasoning
		// time and the user's typing time). Zero when no contained
		// tool_call carries duration_ms.
		ToolDurationMs int64         `json:"tool_duration_ms,omitempty"`
		ToolCallCount  int           `json:"tool_call_count"`
		ToolCalls      []toolCallRow `json:"tool_calls"`
	}

	// 1. Per-turn-deduped token rows grouped by message_id (or
	// source_event_id when message_id is NULL — pre-backfill rows).
	// api_turns doesn't have a separate message_id column —
	// request_id IS the upstream message.id (set by the proxy's
	// Anthropic response parser), so use it directly.
	const dedupedRowsCTE = `WITH proxy_turn_ids AS (
		SELECT request_id FROM api_turns
		 WHERE session_id = ? AND request_id IS NOT NULL AND request_id != ''
	),
	combined AS (
		SELECT COALESCE(NULLIF(request_id, ''), '') AS msg_key,
		       model, timestamp,
		       input_tokens, output_tokens, cache_read_tokens,
		       cache_creation_tokens, cache_creation_1h_tokens,
		       cost_usd
		FROM api_turns WHERE session_id = ?
		UNION ALL
		SELECT COALESCE(message_id, source_event_id, '') AS msg_key,
		       model, timestamp,
		       input_tokens, output_tokens, cache_read_tokens,
		       cache_creation_tokens, cache_creation_1h_tokens,
		       estimated_cost_usd
		FROM token_usage
		WHERE session_id = ?
		  AND (source_event_id IS NULL OR source_event_id = ''
		       OR source_event_id NOT IN (SELECT request_id FROM proxy_turn_ids))
	)`
	rows, err := s.opts.DB.QueryContext(r.Context(),
		dedupedRowsCTE+`
		SELECT msg_key,
		       MIN(timestamp),
		       COALESCE(NULLIF(MAX(model), ''), ?),
		       COALESCE(SUM(input_tokens), 0),
		       COALESCE(SUM(output_tokens), 0),
		       COALESCE(SUM(cache_read_tokens), 0),
		       COALESCE(SUM(cache_creation_tokens), 0),
		       COALESCE(SUM(cache_creation_1h_tokens), 0),
		       COALESCE(SUM(cost_usd), 0)
		FROM combined
		WHERE msg_key IS NOT NULL AND msg_key != ''
		GROUP BY msg_key
		ORDER BY MIN(timestamp) ASC`,
		sessionID, sessionID, sessionID, sessionModel)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()

	byKey := map[string]*messageRow{}
	out := []*messageRow{}
	for rows.Next() {
		var key, ts, model string
		var bundle cost.TokenBundle
		var recorded float64
		if err := rows.Scan(&key, &ts, &model,
			&bundle.Input, &bundle.Output,
			&bundle.CacheRead, &bundle.CacheCreation, &bundle.CacheCreation1h,
			&recorded); err != nil {
			writeErr(w, err)
			return
		}
		var costUSD float64
		if recorded > 0 {
			costUSD = recorded
		} else if computed, ok := s.opts.CostEngine.Compute(model, bundle); ok {
			costUSD = computed
		}
		mr := &messageRow{
			MessageID:     key,
			Timestamp:     ts,
			Role:          "assistant",
			Model:         model,
			Input:         bundle.Input,
			Output:        bundle.Output,
			CacheRead:     bundle.CacheRead,
			CacheCreation: bundle.CacheCreation,
			CacheCw1h:     bundle.CacheCreation1h,
			CostUSD:       costUSD,
			ToolCalls:     []toolCallRow{},
		}
		byKey[key] = mr
		out = append(out, mr)
	}
	if err := rows.Err(); err != nil {
		writeErr(w, err)
		return
	}

	// 2. Tool calls — grouped by message_id (or source_event_id as
	// fallback for pre-backfill rows). Append into each message's
	// ToolCalls; create synthetic message rows for actions whose
	// message_id doesn't have a token row (typically user_prompt).
	actRows, err := s.opts.DB.QueryContext(r.Context(),
		`SELECT COALESCE(message_id, source_event_id) AS msg_key,
		        action_type, COALESCE(raw_tool_name, ''),
		        COALESCE(target, ''), COALESCE(success, 1),
		        COALESCE(error_message, ''), timestamp,
		        COALESCE(duration_ms, 0)
		 FROM actions WHERE session_id = ?
		 ORDER BY timestamp ASC`, sessionID)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer actRows.Close()
	for actRows.Next() {
		var key, actionType, rawTool, target, errMsg, ts string
		var success int
		var durationMs int64
		if err := actRows.Scan(&key, &actionType, &rawTool, &target, &success, &errMsg, &ts, &durationMs); err != nil {
			writeErr(w, err)
			return
		}
		tc := toolCallRow{
			ActionType:   actionType,
			RawToolName:  rawTool,
			Target:       target,
			Success:      success != 0,
			ErrorMessage: errMsg,
			Timestamp:    ts,
			DurationMs:   durationMs,
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
		mr.ToolCallCount++
		mr.ToolDurationMs += tc.DurationMs
	}
	if err := actRows.Err(); err != nil {
		writeErr(w, err)
		return
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

// handlePatterns serves /api/patterns?page=N&limit=M. Returns a paged
// {rows, page, limit, total} envelope mirroring /api/sessions and
// /api/actions. Patterns are ordered by confidence DESC (the user's
// "what's most reliable to act on first" view).
func (s *Server) handlePatterns(w http.ResponseWriter, r *http.Request) {
	limit := intArg(r, "limit", 20, 1, 200)
	page := intArg(r, "page", 1, 1, 1_000_000)
	offset := (page - 1) * limit

	var total int
	if err := s.opts.DB.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM project_patterns`).Scan(&total); err != nil {
		writeErr(w, err)
		return
	}
	rows, err := s.opts.DB.QueryContext(r.Context(),
		`SELECT COALESCE(p.root_path, ''), pattern_type, pattern_data,
		        COALESCE(confidence, 0), COALESCE(observation_count, 0)
		 FROM project_patterns pp
		 LEFT JOIN projects p ON p.id = pp.project_id
		 ORDER BY confidence DESC
		 LIMIT ? OFFSET ?`, limit, offset)
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

	type point struct {
		Bucket           string  `json:"bucket"`
		Input            int64   `json:"input"`
		Output           int64   `json:"output"`
		CacheRead        int64   `json:"cache_read"`
		CacheCreation    int64   `json:"cache_creation"`
		CostUSD          float64 `json:"cost_usd"`
		TurnCount        int     `json:"turn_count"`
		CompBytesSaved   int64   `json:"compression_bytes_saved"`
		CompTokensSaved  int64   `json:"compression_tokens_saved_est"`
		CompCostUSDSaved float64 `json:"compression_cost_saved_usd_est"`
		CompTurns        int     `json:"compression_turns"`
	}

	if bucket == "day" {
		// Day-bucket: lean on the cost engine so pricing stays consistent
		// with /api/cost.
		summary, err := s.opts.CostEngine.Summary(r.Context(), s.opts.DB, cost.Options{
			Days: days, GroupBy: cost.GroupByDay, Source: cost.SourceAuto, Limit: 365,
		})
		if err != nil {
			writeErr(w, err)
			return
		}
		series := make([]point, 0, len(summary.Rows))
		for _, row := range summary.Rows {
			series = append(series, point{
				Bucket:           row.Key,
				Input:            row.Tokens.Input,
				Output:           row.Tokens.Output,
				CacheRead:        row.Tokens.CacheRead,
				CacheCreation:    row.Tokens.CacheCreation,
				CostUSD:          row.CostUSD,
				TurnCount:        row.TurnCount,
				CompBytesSaved:   row.Compression.SavedBytesSigned(),
				CompTokensSaved:  row.Compression.TokensSavedEst,
				CompCostUSDSaved: row.Compression.CostSavedUSDEst,
				CompTurns:        row.Compression.Turns,
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
	rows, err := s.opts.DB.QueryContext(r.Context(),
		`SELECT strftime('%Y-%m-%dT%H:00:00Z', timestamp) AS bucket,
		        COALESCE(SUM(input_tokens), 0),
		        COALESCE(SUM(output_tokens), 0),
		        COALESCE(SUM(cache_read_tokens), 0),
		        COALESCE(SUM(cache_creation_tokens), 0),
		        COUNT(*)
		 FROM api_turns
		 WHERE timestamp >= ?
		 GROUP BY bucket
		 ORDER BY bucket`, since.Format(time.RFC3339Nano))
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

	summary, err := s.opts.CostEngine.Summary(r.Context(), s.opts.DB, cost.Options{
		Days:        days,
		GroupBy:     cost.GroupByDayModel,
		Source:      cost.SourceAuto,
		ProjectRoot: projectFilter,
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
	args := []any{fmtSpec, since.Format(time.RFC3339Nano)}
	projectClause := ""
	if project != "" {
		projectClause = " AND project_id = (SELECT id FROM projects WHERE root_path = ?)"
		args = append(args, project)
	}
	rows, err := s.opts.DB.QueryContext(r.Context(),
		`SELECT strftime(?, timestamp) AS bucket, tool,
		        COUNT(*),
		        SUM(CASE WHEN success = 0 THEN 1 ELSE 0 END)
		 FROM actions
		 WHERE timestamp >= ?`+projectClause+`
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
	summary, err := s.opts.CostEngine.Summary(r.Context(), s.opts.DB, cost.Options{
		Days: days, GroupBy: cost.GroupByModel, Source: cost.SourceAuto, Limit: 50,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, summary)
}

// handleTools serves /api/tools?days=N — per-tool action volume + success
// rate over the window. Source: actions table.
func (s *Server) handleTools(w http.ResponseWriter, r *http.Request) {
	days := intArg(r, "days", 30, 1, 36500)
	since := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
	rows, err := s.opts.DB.QueryContext(r.Context(),
		`SELECT tool, COUNT(*),
		        SUM(CASE WHEN success = 0 THEN 1 ELSE 0 END),
		        COUNT(DISTINCT session_id),
		        MIN(timestamp), MAX(timestamp)
		 FROM actions
		 WHERE timestamp >= ?
		 GROUP BY tool
		 ORDER BY COUNT(*) DESC`,
		since.Format(time.RFC3339Nano))
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
	q := `SELECT tool, action_type, COUNT(*)
	      FROM actions
	      WHERE ` + strings.Join(where, " AND ") + `
	      GROUP BY tool, action_type
	      ORDER BY tool, COUNT(*) DESC`
	rows, err := s.opts.DB.QueryContext(r.Context(), q, args...)
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
	whereClause := "WHERE " + strings.Join(where, " AND ")

	var total int
	if err := s.opts.DB.QueryRowContext(r.Context(),
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
	rows, err := s.opts.DB.QueryContext(r.Context(),
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
		OriginalTokensEst   int64   `json:"original_tokens_est"`
		CompressedTokensEst int64   `json:"compressed_tokens_est"`
		SavedTokensEst      int64   `json:"saved_tokens_est"`
		MsgIndex            int     `json:"msg_index"`
		ImportanceScore     float64 `json:"importance_score"`
		Model               string  `json:"model"`
		SessionID           string  `json:"session_id"`
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

// handleCompressionTimeseries serves /api/compression/timeseries?bucket=day&days=N
// — per-day savings split by mechanism for the new "Savings by
// mechanism" chart. Returns one point per day with by_mechanism map of
// {mechanism: {count, original_bytes, compressed_bytes, saved_bytes}}.
func (s *Server) handleCompressionTimeseries(w http.ResponseWriter, r *http.Request) {
	days := intArg(r, "days", 30, 1, 36500)
	since := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
	rows, err := s.opts.DB.QueryContext(r.Context(),
		`SELECT strftime('%Y-%m-%d', timestamp) AS bucket,
		        mechanism,
		        COUNT(*),
		        COALESCE(SUM(original_bytes), 0),
		        COALESCE(SUM(compressed_bytes), 0)
		 FROM compression_events
		 WHERE timestamp >= ?
		 GROUP BY bucket, mechanism
		 ORDER BY bucket, mechanism`,
		since.Format(time.RFC3339Nano))
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	type mechStats struct {
		Count           int   `json:"count"`
		OriginalBytes   int64 `json:"original_bytes"`
		CompressedBytes int64 `json:"compressed_bytes"`
		SavedBytes      int64 `json:"saved_bytes"`
	}
	type point struct {
		Bucket      string                `json:"bucket"`
		ByMechanism map[string]*mechStats `json:"by_mechanism"`
		TotalSaved  int64                 `json:"total_saved_bytes"`
		TotalCount  int                   `json:"total_count"`
	}
	idx := map[string]*point{}
	order := []string{}
	for rows.Next() {
		var b, mech string
		var n int
		var orig, comp int64
		if err := rows.Scan(&b, &mech, &n, &orig, &comp); err != nil {
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
		p.ByMechanism[mech] = &mechStats{
			Count: n, OriginalBytes: orig, CompressedBytes: comp, SavedBytes: saved,
		}
		p.TotalSaved += saved
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
	rows, err := s.opts.DB.QueryContext(r.Context(),
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
