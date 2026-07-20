package dashboard

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"net/http"
	"sort"

	"github.com/marmutapp/superbased-observer/internal/intelligence/cost"
)

// CoworkReconcileRow is one Cowork session's cost-reconciliation
// snapshot. Cowork emits an authoritative `total_cost_usd` per
// `result` record (its own computation); Observer derives a cost from
// `token_usage` rows × the pricing-table rates. The two should agree
// within rounding; significant drift signals pricing-table staleness.
type CoworkReconcileRow struct {
	SessionID       string  `json:"session_id"`
	Title           string  `json:"title,omitempty"`
	ProcessName     string  `json:"process_name,omitempty"`
	CoworkCostUSD   float64 `json:"cowork_cost_usd"`
	DerivedCostUSD  float64 `json:"derived_cost_usd"`
	DriftUSD        float64 `json:"drift_usd"`
	DriftPercent    float64 `json:"drift_percent"`
	OverThreshold   bool    `json:"over_threshold"`
	UnknownModel    bool    `json:"unknown_model,omitempty"`
	UnknownModelKey string  `json:"unknown_model_key,omitempty"`
}

// CoworkReconcileResult is the response envelope. Summary stats let
// the dashboard render a tile without iterating Rows client-side.
type CoworkReconcileResult struct {
	Rows              []CoworkReconcileRow `json:"rows"`
	SessionsTotal     int                  `json:"sessions_total"`
	SessionsOver      int                  `json:"sessions_over_threshold"`
	CoworkTotalUSD    float64              `json:"cowork_total_usd"`
	DerivedTotalUSD   float64              `json:"derived_total_usd"`
	OverallDriftUSD   float64              `json:"overall_drift_usd"`
	OverallDriftPct   float64              `json:"overall_drift_percent"`
	DriftThresholdPct float64              `json:"drift_threshold_percent"`
}

// coworkDriftThreshold is the |drift %| above which a row is flagged
// "over threshold." 5% matches the v1.4.50 antigravity audit's
// pricing-mismatch alarm threshold; sessions above it deserve the
// dashboard tile's attention badge.
const coworkDriftThreshold = 5.0

// computeCoworkReconciliation walks every Cowork session's
// task_complete row (carrying `total_cost_usd` from the audit-log
// `result` record) and its token_usage rows, deriving a cost via
// the pricing-table engine and reporting per-session + aggregate
// drift.
//
// Returns (zero-value CoworkReconcileResult, nil) when no Cowork
// sessions exist — clean empty state for fresh installs.
func computeCoworkReconciliation(ctx context.Context, database *sql.DB, engine *cost.Engine) (CoworkReconcileResult, error) {
	out := CoworkReconcileResult{
		DriftThresholdPct: coworkDriftThreshold,
	}
	if database == nil {
		return out, fmt.Errorf("computeCoworkReconciliation: nil db")
	}

	// Pull every cowork task_complete row whose metadata carries a
	// non-zero total_cost_usd — that's our Cowork-authoritative
	// per-session figure. Multiple result rows can land in one
	// session (Cowork emits one per turn batch); take the SUM for
	// the session's total.
	coworkRows, err := database.QueryContext(ctx, `
		SELECT
			a.session_id,
			COALESCE(SUM(CAST(json_extract(a.metadata, '$.total_cost_usd') AS REAL)), 0),
			COALESCE(MAX(json_extract(a.metadata, '$.cowork_title')), '') AS title,
			COALESCE(MAX(json_extract(a.metadata, '$.cowork_process_name')), '') AS process_name
		FROM actions a
		WHERE a.tool = 'cowork'
		  AND a.raw_tool_name = 'cowork.result'
		  AND a.metadata IS NOT NULL
		GROUP BY a.session_id
	`)
	if err != nil {
		return out, fmt.Errorf("computeCoworkReconciliation: query cowork rows: %w", err)
	}
	defer coworkRows.Close()
	per := map[string]*CoworkReconcileRow{}
	for coworkRows.Next() {
		var r CoworkReconcileRow
		if err := coworkRows.Scan(&r.SessionID, &r.CoworkCostUSD, &r.Title, &r.ProcessName); err != nil {
			return out, fmt.Errorf("scan cowork row: %w", err)
		}
		per[r.SessionID] = &r
	}
	if err := coworkRows.Err(); err != nil {
		return out, err
	}
	if len(per) == 0 {
		return out, nil // no cowork data yet — clean empty
	}

	// Sum token_usage per (session, model) and compute derived cost
	// through the pricing engine. web_search_requests is summed too so
	// the engine can apply Anthropic's $10/1000 server-side-tool fee on
	// top of per-token costs (Phase 2 of the v1.4.53 cost-drift fix).
	tokenRows, err := database.QueryContext(ctx, `
		SELECT
			session_id,
			COALESCE(model, ''),
			COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(cache_read_tokens), 0),
			COALESCE(SUM(cache_creation_tokens), 0),
			COALESCE(SUM(cache_creation_1h_tokens), 0),
			COALESCE(SUM(reasoning_tokens), 0),
			COALESCE(SUM(web_search_requests), 0)
		FROM token_usage
		WHERE tool = 'cowork'
		GROUP BY session_id, model
	`)
	if err != nil {
		return out, fmt.Errorf("query token_usage: %w", err)
	}
	defer tokenRows.Close()
	for tokenRows.Next() {
		var sessID, model string
		var bundle cost.TokenBundle
		if err := tokenRows.Scan(
			&sessID, &model,
			&bundle.Input, &bundle.Output,
			&bundle.CacheRead, &bundle.CacheCreation, &bundle.CacheCreation1h,
			&bundle.Reasoning,
			&bundle.WebSearchRequests,
		); err != nil {
			return out, fmt.Errorf("scan token row: %w", err)
		}
		row, ok := per[sessID]
		if !ok {
			// token_usage row for a session without a result row — skip.
			// Could happen mid-stream when the result event hasn't
			// landed yet.
			continue
		}
		usd, known := engine.Compute(model, bundle)
		if !known {
			row.UnknownModel = true
			row.UnknownModelKey = model
			continue
		}
		row.DerivedCostUSD += usd
	}
	if err := tokenRows.Err(); err != nil {
		return out, err
	}

	rows := make([]CoworkReconcileRow, 0, len(per))
	for _, r := range per {
		r.DriftUSD = r.DerivedCostUSD - r.CoworkCostUSD
		if r.CoworkCostUSD > 0 {
			r.DriftPercent = math.Abs(r.DriftUSD) / r.CoworkCostUSD * 100
		}
		r.OverThreshold = r.DriftPercent >= coworkDriftThreshold && r.CoworkCostUSD > 0
		out.CoworkTotalUSD += r.CoworkCostUSD
		out.DerivedTotalUSD += r.DerivedCostUSD
		rows = append(rows, *r)
	}
	out.OverallDriftUSD = out.DerivedTotalUSD - out.CoworkTotalUSD
	if out.CoworkTotalUSD > 0 {
		out.OverallDriftPct = math.Abs(out.OverallDriftUSD) / out.CoworkTotalUSD * 100
	}
	out.SessionsTotal = len(rows)
	for _, r := range rows {
		if r.OverThreshold {
			out.SessionsOver++
		}
	}

	// Sort largest absolute drift first — that's what the dashboard
	// surfaces.
	sort.Slice(rows, func(i, j int) bool {
		return math.Abs(rows[i].DriftUSD) > math.Abs(rows[j].DriftUSD)
	})
	out.Rows = rows
	return out, nil
}

// handleCoworkReconcile serves GET /api/cowork/reconcile. No query
// params — the result is whole-install scope (rare-enough that
// pagination isn't yet warranted).
func (s *Server) handleCoworkReconcile(w http.ResponseWriter, r *http.Request) {
	// s.opts.DB, not s.db(): reconciliation is a maintenance view over
	// the real install's cowork rows even while demo mode is active.
	res, err := computeCoworkReconciliation(r.Context(), s.opts.DB, s.opts.CostEngine)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, res)
}
