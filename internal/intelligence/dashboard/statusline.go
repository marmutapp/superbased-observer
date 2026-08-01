package dashboard

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/marmutapp/superbased-observer/internal/intelligence/cost"
)

// StatuslineResponse is the wire shape of GET /api/statusline — a small,
// dedicated struct (NOT a subset of handleAnalysisHeadline's response)
// because the CLI's DaemonTile (cmd/observer/statusline.go, a later work
// package) mirrors these field names verbatim. SessionUSD /
// SessionCacheReadShare are pointers so "no session_id supplied" and
// "session_id supplied but unknown" both serialize as JSON null rather
// than a fabricated 0 — an honest omission, matching the render-side
// convention documented in the statusline plan §4.1 ("omitted, not
// $0.00").
type StatuslineResponse struct {
	TodayUSD              float64  `json:"today_usd"`
	SessionUSD            *float64 `json:"session_usd"`
	SessionCacheReadShare *float64 `json:"session_cache_read_share"`
	GeneratedAt           string   `json:"generated_at"`
}

// statuslineCacheEntry is one memoized StatuslineResponse plus the instant
// it stops being servable. GeneratedAt inside resp is left exactly as it
// was when the response was computed, so a cache hit still carries an
// honest (if slightly stale) staleness signal — a consumer diffing
// generated_at against wall-clock time can tell.
type statuslineCacheEntry struct {
	resp      StatuslineResponse
	expiresAt time.Time
}

// statuslineCacheKey scopes a cached entry to one Server instance (via
// pointer identity) AND one (day-window, session_id) pair. Server-instance
// scoping matters because this fix's file set is restricted to
// statusline.go/statusline_test.go — adding a cache field to the Server
// struct itself would require editing dashboard.go's type definition and
// New(), which is out of scope here — so the cache lives at package scope
// instead, and the *Server pointer in the key is what keeps two Server
// instances (e.g. two tests' fixtures, both querying "no session_id,
// today") from ever observing each other's cached rows.
type statuslineCacheKey struct {
	srv *Server
	key string
}

// statuslineTTL is how long a computed /api/statusline response is served
// from statuslineCache before the underlying query re-runs (F6 fix — see
// the handler doc comment below). 2s is chosen to be long enough to
// absorb a render-storm — a statusline that fires on every terminal
// render tick can fire on every keystroke, producing a burst of requests
// within the same second or two — while staying short enough that the
// number a human is watching still reads as live.
//
// It is a package-level var rather than a Server field for the same
// out-of-scope-file reason statuslineCacheKey embeds a pointer instead of
// a struct field: tests in this package override it directly (same
// package, no exported knob needed) and MUST restore the previous value
// via tb.Cleanup so one test's override never leaks into the next. Zero
// disables caching outright (every call is a miss) — BenchmarkHandleStatus
// lineTile uses this to keep measuring the uncached per-call query cost
// it exists to guard.
var statuslineTTL = 2 * time.Second

// statuslineCacheMu guards statuslineCache. It is held across the whole
// check-or-compute-and-store step in statuslineCachedOrCompute, not just
// the map access — a deliberate singleflight-lite choice. At this
// endpoint's expected concurrency (one local daemon, at most a handful of
// concurrent statusline pollers) it's cheaper to serialize a rare
// concurrent cache miss than to pull in a real singleflight dependency,
// and it guarantees two requests racing on the same key never both pay
// for the query.
var (
	statuslineCacheMu sync.Mutex
	statuslineCache   = map[statuslineCacheKey]statuslineCacheEntry{}
)

// statuslineCachedOrCompute serves the memoized response for cacheKey when
// one is still live, or computes + (when caching is enabled) stores a
// fresh one otherwise.
func statuslineCachedOrCompute(ctx context.Context, s *Server, cacheKey statuslineCacheKey, sessionID string, dayStart, now time.Time) (StatuslineResponse, error) {
	statuslineCacheMu.Lock()
	defer statuslineCacheMu.Unlock()

	if statuslineTTL > 0 {
		if entry, ok := statuslineCache[cacheKey]; ok && now.Before(entry.expiresAt) {
			return entry.resp, nil
		}
	}

	today, err := statuslineQueryRows(ctx, s.db(), s.opts.CostEngine, dayStart.Format(time.RFC3339Nano), "")
	if err != nil {
		return StatuslineResponse{}, err
	}

	resp := StatuslineResponse{
		TodayUSD:    today.CostUSD,
		GeneratedAt: now.Format(time.RFC3339),
	}

	if sessionID != "" {
		// Session-scoped total is intentionally ALL-TIME for that
		// session (no day window) — a session is the natural unit,
		// and it may have started before today (plan §2, "session_usd
		// + session_cache_read_share: ... all-time for that session,
		// not day-windowed").
		session, err := statuslineQueryRows(ctx, s.db(), s.opts.CostEngine, "", sessionID)
		if err != nil {
			return StatuslineResponse{}, err
		}
		if session.RowCount > 0 {
			usd := session.CostUSD
			resp.SessionUSD = &usd
			var share float64
			if session.PromptTokens > 0 {
				share = float64(session.CacheReadTokens) / float64(session.PromptTokens)
			}
			resp.SessionCacheReadShare = &share
		}
		// session.RowCount == 0 (unknown/empty session_id) leaves both
		// pointers nil → JSON null, per the honest-omission contract.
	}

	if statuslineTTL > 0 {
		statuslineCache[cacheKey] = statuslineCacheEntry{resp: resp, expiresAt: now.Add(statuslineTTL)}
	}
	return resp, nil
}

// handleStatuslineTile serves GET /api/statusline?session_id=<id> — the
// daemon-path data source for `observer statusline` (§2 of
// docs/plans/observer-statusline-plan-2026-07-30.md). It returns two
// numbers: today's (UTC calendar day) total spend, and — only when the
// caller supplies session_id — that session's all-time total plus its
// cache-read share of the prompt window.
//
// Deliberately narrow. handleAnalysisHeadline (analysis.go) is a 30-day,
// multi-tile scan built for the dashboard's Analysis KPI band — month
// projection, LC-tier surcharge decomposition, cache-savings
// counterfactual, burn rate, top-model concentration. WP0 measured that
// handler at 1.2-2.2s per call against a 15GB corpus. A statusline fired
// on every terminal render tick budgets <100ms end-to-end INCLUDING the
// loopback round trip (plan §2.3), so this handler reuses only the part
// of handleAnalysisHeadline's approach that's load-bearing — the per-turn
// -deduped api_turns∪token_usage scan + the recorded-cost-wins pricing
// rule (see statuslineQueryRows) — windowed to one day (or one session),
// with NONE of the extra tiles.
//
// REGRESSION GUARD: do not grow this handler back toward
// handleAnalysisHeadline's breadth. If a future change needs the month
// projection / LC tiles / burn rate / top-model here, that's a sign the
// change belongs on a *different* endpoint the statusline command
// doesn't call on its render hot path. BenchmarkHandleStatuslineTile
// pins the cost bound this comment describes; a regression that
// reintroduces analysis-headline-style scope will show up there first.
//
// Pricing correction (WP0, 2026-07-30): querying
// SUM(estimated_cost_usd) over today's token_usage rows measured $0.0
// on 1,151 real rows — that column is unpopulated for major sources
// (claude-code prices its turns at query time via the cost engine, not
// at ingest time). This handler MUST price every row without a
// positive recorded cost through the cost engine (cost.Compute against
// the pricing table), exactly like handleAnalysisHeadline does, rather
// than summing the raw column.
//
// Caching (F6 fix, adversarial review): "fires on every render tick"
// means, in practice, on every keystroke of an interactive terminal
// session — a render storm the query cost above was never meant to
// absorb per-tick. statuslineCachedOrCompute memoizes the computed
// response for statuslineTTL (2s) keyed on (this Server, day-window,
// session_id), so a burst of ticks within that window shares one
// computation. See statuslineTTL's doc comment for why the cache lives
// at package scope instead of on the Server struct.
func (s *Server) handleStatuslineTile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sessionID := r.URL.Query().Get("session_id")
	now := s.now()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	cacheKey := statuslineCacheKey{srv: s, key: dayStart.Format("2006-01-02") + "|" + sessionID}

	resp, err := statuslineCachedOrCompute(ctx, s, cacheKey, sessionID, dayStart, now)
	if err != nil {
		writeErr(w, err)
		return
	}

	writeJSON(w, resp)
}

// statuslineTotals accumulates the priced total plus the cache-read /
// prompt-token aggregates statuslineQueryRows needs for
// session_cache_read_share, over whichever row set the caller's WHERE
// clause selects. RowCount lets the caller distinguish "matched zero
// rows" (unknown session_id → render null) from "matched rows that
// happen to sum to $0".
type statuslineTotals struct {
	CostUSD         float64
	CacheReadTokens int64
	PromptTokens    int64 // input + cache_read + cache_creation, across matched rows
	RowCount        int
}

// statuslineQueryRows runs a per-turn-deduped scan over
// api_turns∪token_usage and prices every row through the cost engine.
// It mirrors the structural shape of handleAnalysisHeadline's `combined`
// CTE in analysis.go (same proxy_turn_ids exclusion trick so a JSONL
// row already covered by a proxy-recorded api_turns row isn't
// double-counted) but carries NONE of that handler's extra tiles —
// see the handleStatuslineTile doc comment for why that's a hard
// constraint, not an oversight.
//
// sinceRFC3339 (when non-empty) bounds rows to timestamp >= that value
// — used for the "today" query. sessionID (when non-empty) bounds rows
// to that session — used for the "session" query. The two callers of
// this function each supply exactly one of the two (never both, never
// neither), but the function itself tolerates any combination.
//
// Pricing rule per row (the "recorded-cost-wins" rule, identical to
// handleAnalysisHeadline): a row with a positive recorded cost
// (api_turns.cost_usd from the proxy, or token_usage.estimated_cost_usd
// from JSONL backfill) uses that ground-truth figure as-is. A row with
// no recorded cost (zero or NULL) is priced via cost.Compute against
// the engine's pricing table when the model is known; an unknown model
// contributes $0 (no fabricated cost). Unlike handleAnalysisHeadline,
// this function does NOT decompose long-context surcharge or a
// cache-savings counterfactual — the statusline has no use for either,
// and computing them would mean pricing every row twice for nothing.
func statuslineQueryRows(ctx context.Context, db *sql.DB, engine *cost.Engine, sinceRFC3339 string, sessionID string) (statuslineTotals, error) {
	var totals statuslineTotals
	if db == nil {
		return totals, nil
	}

	proxyExtra, proxyArgs := statuslineWhereClause("", sinceRFC3339, sessionID)
	atExtra, atArgs := statuslineWhereClause("at.", sinceRFC3339, sessionID)
	tuExtra, tuArgs := statuslineWhereClause("tu.", sinceRFC3339, sessionID)

	//nolint:gosec // G202: SQL structure (proxyExtra/atExtra/tuExtra) is built
	// only from the fixed " AND col >= ?" / " AND col = ?" fragments in
	// statuslineWhereClause — never from raw request input; all values are
	// bound via ? args.
	q := `WITH proxy_turn_ids AS (
		SELECT request_id FROM api_turns
		 WHERE request_id IS NOT NULL AND request_id != ''` + proxyExtra + `
	),
	combined AS (
		SELECT at.model, at.input_tokens, at.output_tokens, at.cache_read_tokens,
		       at.cache_creation_tokens, at.cache_creation_1h_tokens,
		       0 AS reasoning_tokens, at.web_search_requests,
		       at.cost_usd AS recorded_cost, at.fast
		FROM api_turns at
		WHERE 1=1` + atExtra + `
		UNION ALL
		SELECT tu.model, tu.input_tokens, tu.output_tokens, tu.cache_read_tokens,
		       tu.cache_creation_tokens, tu.cache_creation_1h_tokens,
		       tu.reasoning_tokens, tu.web_search_requests,
		       tu.estimated_cost_usd AS recorded_cost, tu.fast
		FROM token_usage tu
		WHERE 1=1` + tuExtra + `
		  AND (tu.source_event_id IS NULL OR tu.source_event_id = ''
		       OR tu.source_event_id NOT IN (SELECT request_id FROM proxy_turn_ids))
	)
	SELECT COALESCE(model, ''),
	       COALESCE(input_tokens, 0), COALESCE(output_tokens, 0),
	       COALESCE(cache_read_tokens, 0), COALESCE(cache_creation_tokens, 0),
	       COALESCE(cache_creation_1h_tokens, 0),
	       COALESCE(reasoning_tokens, 0), COALESCE(web_search_requests, 0),
	       COALESCE(recorded_cost, 0), COALESCE(fast, 0)
	FROM combined`

	args := make([]any, 0, len(proxyArgs)+len(atArgs)+len(tuArgs))
	args = append(args, proxyArgs...)
	args = append(args, atArgs...)
	args = append(args, tuArgs...)

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return totals, fmt.Errorf("dashboard.statuslineQueryRows: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			model    string
			bundle   cost.TokenBundle
			recorded float64
			fastInt  int
		)
		if err := rows.Scan(&model,
			&bundle.Input, &bundle.Output,
			&bundle.CacheRead, &bundle.CacheCreation, &bundle.CacheCreation1h,
			&bundle.Reasoning, &bundle.WebSearchRequests,
			&recorded, &fastInt); err != nil {
			return totals, fmt.Errorf("dashboard.statuslineQueryRows: scan: %w", err)
		}
		bundle.Fast = fastInt != 0

		var rowCost float64
		switch {
		case recorded > 0:
			rowCost = recorded
		default:
			if p, ok := engine.Lookup(model); ok {
				rowCost = cost.Compute(p, bundle)
			}
		}

		totals.CostUSD += rowCost
		totals.CacheReadTokens += bundle.CacheRead
		totals.PromptTokens += bundle.Input + bundle.CacheRead + bundle.CacheCreation
		totals.RowCount++
	}
	if err := rows.Err(); err != nil {
		return totals, fmt.Errorf("dashboard.statuslineQueryRows: rows: %w", err)
	}
	return totals, nil
}

// statuslineWhereClause builds the AND-joined WHERE fragment (plus its
// bound args, in the same order) for an optional time-floor and/or
// optional session-id filter, against columns referenced with the
// given alias prefix ("" for the unaliased proxy_turn_ids subquery,
// "at."/"tu." for the combined CTE's two legs). Returns ("", nil) when
// neither filter applies.
func statuslineWhereClause(prefix, sinceRFC3339, sessionID string) (clause string, args []any) {
	var parts []string
	if sinceRFC3339 != "" {
		parts = append(parts, prefix+"timestamp >= ?")
		args = append(args, sinceRFC3339)
	}
	if sessionID != "" {
		parts = append(parts, prefix+"session_id = ?")
		args = append(args, sessionID)
	}
	if len(parts) == 0 {
		return "", nil
	}
	return " AND " + strings.Join(parts, " AND "), args
}
