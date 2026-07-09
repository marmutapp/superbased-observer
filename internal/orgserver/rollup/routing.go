package rollup

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// Routing aggregates the routing_summaries table (server migration 006) into an
// org-aggregate, content-free summary for the window — the model-routing org
// surface (§R19). The table receives the node-side §R19.4 AGGREGATE push only
// when the operator opts in with [org_client.share].routing_summary, so for the
// common (share-off) org this returns Configured=false.
//
// Like Telemetry this is ADMIN-ONLY (the handler gates with requireAdmin) and
// takes no Scope: routing is an org-governance surface and the query reads only
// the closed-enum dimensions (day/tier/reason/mode) plus numeric aggregates —
// never user_email or pushed_by_user_id, so no identity is disclosed and no
// model id or content column is read.
func Routing(ctx context.Context, db *sql.DB, w Window, now time.Time) (RoutingResult, error) {
	sinceDay := now.UTC().AddDate(0, 0, -w.days()).Format("2006-01-02")
	res := RoutingResult{
		WindowDays: w.days(),
		ByDay:      []RoutingDayPoint{},
		ByTier:     []RoutingDimCount{},
		ByReason:   []RoutingDimCount{},
	}

	byDay := map[string]*RoutingDayPoint{}
	byTier := map[string]*RoutingDimCount{}
	byReason := map[string]*RoutingDimCount{}
	seen := false

	q := `
SELECT day, tier, reason, mode,
       COALESCE(SUM(decisions),0), COALESCE(SUM(applied),0),
       COALESCE(SUM(est_savings_usd),0), COALESCE(SUM(cache_forfeit_usd),0)
  FROM routing_summaries
 WHERE day >= ?
 GROUP BY day, tier, reason, mode`
	if err := eachRow(ctx, db, q, []any{sinceDay}, func(rows *sql.Rows) error {
		var day, tier, reason, mode string
		var decisions, applied int64
		var est, forfeit float64
		if err := rows.Scan(&day, &tier, &reason, &mode, &decisions, &applied, &est, &forfeit); err != nil {
			return err
		}
		seen = true

		res.TotalDecisions += decisions
		res.TotalApplied += applied
		res.EstSavingsUSD += est
		res.CacheForfeitUSD += forfeit
		if mode == "enforce" {
			res.EnforceDecisions += decisions
		} else {
			// advise (or empty/legacy) counts as advisory.
			res.AdviseDecisions += decisions
		}

		d := byDay[day]
		if d == nil {
			d = &RoutingDayPoint{Date: day}
			byDay[day] = d
		}
		d.Decisions += decisions
		d.Applied += applied
		d.EstSavingsUSD += est

		if tier != "" {
			accumDim(byTier, tier, decisions, applied, est)
		}
		if reason != "" {
			accumDim(byReason, reason, decisions, applied, est)
		}
		return nil
	}); err != nil {
		return RoutingResult{}, fmt.Errorf("rollup.Routing: %w", err)
	}

	res.Configured = seen
	res.NetSavingsUSD = res.EstSavingsUSD - res.CacheForfeitUSD

	res.ByDay = sortedDays(byDay)
	res.ByTier = sortedDims(byTier)
	res.ByReason = sortedDims(byReason)
	return res, nil
}

// accumDim folds one row into a tier/reason bucket map.
func accumDim(m map[string]*RoutingDimCount, key string, decisions, applied int64, est float64) {
	d := m[key]
	if d == nil {
		d = &RoutingDimCount{Key: key}
		m[key] = d
	}
	d.Decisions += decisions
	d.Applied += applied
	d.EstSavingsUSD += est
}

// sortedDays returns the trend points ascending by date.
func sortedDays(m map[string]*RoutingDayPoint) []RoutingDayPoint {
	out := make([]RoutingDayPoint, 0, len(m))
	for _, d := range m {
		out = append(out, *d)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Date < out[j].Date })
	return out
}

// sortedDims returns the distribution buckets descending by decisions (ties
// broken by key for a stable order).
func sortedDims(m map[string]*RoutingDimCount) []RoutingDimCount {
	out := make([]RoutingDimCount, 0, len(m))
	for _, d := range m {
		out = append(out, *d)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Decisions != out[j].Decisions {
			return out[i].Decisions > out[j].Decisions
		}
		return out[i].Key < out[j].Key
	})
	return out
}
