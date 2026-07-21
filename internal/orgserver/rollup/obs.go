// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 SuperBased

package rollup

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// obs.go aggregates the org-tier observability tables (server migrations
// 012–015) into the admin observability surfaces (obs-org-tier plan §5.3). It
// is the org analogue of the node Trajectories/Analytics pages: content-free
// cost/token/latency/error rollups over the obs_summaries aggregate the agent
// pushes under [org_client.share] obs_summary. Like Routing/Telemetry it is
// admin-only (the handler gates with requireAdmin) and reads only the closed,
// content-free columns — no model id is sensitive (the org "Top models" surface
// already shows model ids) and there is no body column on obs_summaries.

// ObsAnalyticsResult is the GET /api/org/obs/analytics body: window totals +
// the per-day trend + top-N distributions by model / project / source. Every
// field is content-free. Configured=false when no node has shared a summary.
type ObsAnalyticsResult struct {
	WindowDays int  `json:"window_days"`
	Configured bool `json:"configured"`

	TotalTraces     int64   `json:"total_traces"`
	TotalSpans      int64   `json:"total_spans"`
	TotalTokens     int64   `json:"total_tokens"`
	InputTokens     int64   `json:"input_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	CacheReadTokens int64   `json:"cache_read_tokens"`
	ReasoningTokens int64   `json:"reasoning_tokens"`
	TotalCostUSD    float64 `json:"total_cost_usd"`
	ErrorTraces     int64   `json:"error_traces"`
	ErrorRate       float64 `json:"error_rate"`      // error_traces / total_traces
	AvgDurationMs   float64 `json:"avg_duration_ms"` // duration_ms_sum / duration_ms_count

	ByDay     []ObsDayPoint `json:"by_day"`
	ByModel   []ObsDimCount `json:"by_model"`
	ByProject []ObsDimCount `json:"by_project"`
	BySource  []ObsDimCount `json:"by_source"`

	// Latency depth (OP5) — computed over the T2 obs_spans structure when a
	// node has shared it; zero/empty when only the T1 aggregate is shared.
	// LatencyConfigured distinguishes "no spans shared" from "all 0ms".
	LatencyConfigured bool             `json:"latency_configured"`
	LatencyP50Ms      int64            `json:"latency_p50_ms"`
	LatencyP95Ms      int64            `json:"latency_p95_ms"`
	LatencyP99Ms      int64            `json:"latency_p99_ms"`
	ByKind            []ObsKindLatency `json:"by_kind"`
	ErrorCauses       []ObsDimCount    `json:"error_causes"`
}

// ObsKindLatency is per-span-kind volume + latency (OP5).
type ObsKindLatency struct {
	Kind  string `json:"kind"`
	Spans int64  `json:"spans"`
	P50Ms int64  `json:"p50_ms"`
	P95Ms int64  `json:"p95_ms"`
	AvgMs int64  `json:"avg_ms"`
}

// ObsDayPoint is one day of the trend (ascending by date).
type ObsDayPoint struct {
	Date        string  `json:"date"`
	Traces      int64   `json:"traces"`
	Tokens      int64   `json:"tokens"`
	CostUSD     float64 `json:"cost_usd"`
	ErrorTraces int64   `json:"error_traces"`
}

// ObsDimCount is one distribution bucket (descending by tokens). Key is a
// content-free dimension value: a model id, a project_hash, or a source tag.
type ObsDimCount struct {
	Key     string  `json:"key"`
	Traces  int64   `json:"traces"`
	Tokens  int64   `json:"tokens"`
	CostUSD float64 `json:"cost_usd"`
}

// ObsAnalytics aggregates obs_summaries for the window into ObsAnalyticsResult.
func ObsAnalytics(ctx context.Context, db *sql.DB, w Window, now time.Time) (ObsAnalyticsResult, error) {
	sinceDay := now.UTC().AddDate(0, 0, -w.days()).Format("2006-01-02")
	res := ObsAnalyticsResult{
		WindowDays: w.days(),
		ByDay:      []ObsDayPoint{},
		ByModel:    []ObsDimCount{},
		ByProject:  []ObsDimCount{},
		BySource:   []ObsDimCount{},
	}
	byDay := map[string]*ObsDayPoint{}
	byModel := map[string]*ObsDimCount{}
	byProject := map[string]*ObsDimCount{}
	bySource := map[string]*ObsDimCount{}
	var durSum, durCount int64
	seen := false

	q := `
SELECT day, model, project_hash, source,
       COALESCE(SUM(traces),0), COALESCE(SUM(spans),0),
       COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
       COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(reasoning_tokens),0),
       COALESCE(SUM(total_tokens),0), COALESCE(SUM(cost_usd),0),
       COALESCE(SUM(error_traces),0), COALESCE(SUM(duration_ms_sum),0),
       COALESCE(SUM(duration_ms_count),0)
  FROM obs_summaries
 WHERE day >= ?
 GROUP BY day, model, project_hash, source`
	if err := eachRow(ctx, db, q, []any{sinceDay}, func(rows *sql.Rows) error {
		var day, model, project, source string
		var traces, spans, in, out, cacheR, reason, total, errTraces, dSum, dCount int64
		var cost float64
		if err := rows.Scan(&day, &model, &project, &source,
			&traces, &spans, &in, &out, &cacheR, &reason, &total, &cost, &errTraces, &dSum, &dCount); err != nil {
			return err
		}
		seen = true
		res.TotalTraces += traces
		res.TotalSpans += spans
		res.InputTokens += in
		res.OutputTokens += out
		res.CacheReadTokens += cacheR
		res.ReasoningTokens += reason
		res.TotalTokens += total
		res.TotalCostUSD += cost
		res.ErrorTraces += errTraces
		durSum += dSum
		durCount += dCount

		d := byDay[day]
		if d == nil {
			d = &ObsDayPoint{Date: day}
			byDay[day] = d
		}
		d.Traces += traces
		d.Tokens += total
		d.CostUSD += cost
		d.ErrorTraces += errTraces

		accumObsDim(byModel, model, traces, total, cost)
		accumObsDim(byProject, project, traces, total, cost)
		accumObsDim(bySource, source, traces, total, cost)
		return nil
	}); err != nil {
		return ObsAnalyticsResult{}, fmt.Errorf("rollup.ObsAnalytics: %w", err)
	}

	res.Configured = seen
	if res.TotalTraces > 0 {
		res.ErrorRate = float64(res.ErrorTraces) / float64(res.TotalTraces)
	}
	if durCount > 0 {
		res.AvgDurationMs = float64(durSum) / float64(durCount)
	}
	res.ByDay = sortedObsDays(byDay)
	res.ByModel = sortedObsDims(byModel)
	res.ByProject = sortedObsDims(byProject)
	res.BySource = sortedObsDims(bySource)

	// OP5 latency depth over the T2 structure (best-effort; the T1 floor stays
	// valid even when no spans are shared).
	if err := fillObsLatencyDepth(ctx, db, sinceDay, &res); err != nil {
		return ObsAnalyticsResult{}, err
	}
	return res, nil
}

// fillObsLatencyDepth computes overall + per-kind latency percentiles and the
// error-cause breakdown over the shared obs_spans (T2). It is a no-op leaving
// LatencyConfigured=false when no span structure has been shared. Percentiles
// are computed in Go over the window's durations (custom-app span volumes are
// modest; capped to keep the load bounded).
func fillObsLatencyDepth(ctx context.Context, db *sql.DB, sinceDay string, res *ObsAnalyticsResult) error {
	const cap = 200000
	// Overall + per-kind durations.
	byKind := map[string][]int64{}
	var all []int64
	//nolint:gosec // bound LIMIT, parameterized.
	q := `SELECT kind, duration_ms FROM obs_spans
	       WHERE started_at >= ? AND duration_ms > 0 ORDER BY started_at DESC LIMIT ?`
	if err := eachRow(ctx, db, q, []any{sinceDay + "T00:00:00Z", cap}, func(rows *sql.Rows) error {
		var kind string
		var dur int64
		if err := rows.Scan(&kind, &dur); err != nil {
			return err
		}
		all = append(all, dur)
		byKind[kind] = append(byKind[kind], dur)
		return nil
	}); err != nil {
		return fmt.Errorf("rollup.ObsAnalytics: latency: %w", err)
	}
	res.ByKind = []ObsKindLatency{}
	if len(all) == 0 {
		return nil
	}
	res.LatencyConfigured = true
	res.LatencyP50Ms = percentile(all, 0.50)
	res.LatencyP95Ms = percentile(all, 0.95)
	res.LatencyP99Ms = percentile(all, 0.99)
	for kind, ds := range byKind {
		var sum int64
		for _, d := range ds {
			sum += d
		}
		res.ByKind = append(res.ByKind, ObsKindLatency{
			Kind:  kind,
			Spans: int64(len(ds)),
			P50Ms: percentile(ds, 0.50),
			P95Ms: percentile(ds, 0.95),
			AvgMs: sum / int64(len(ds)),
		})
	}
	sort.SliceStable(res.ByKind, func(i, j int) bool { return res.ByKind[i].Spans > res.ByKind[j].Spans })

	// Error causes: error spans grouped by name (the operation that failed).
	res.ErrorCauses = []ObsDimCount{}
	causes := map[string]*ObsDimCount{}
	eq := `SELECT COALESCE(NULLIF(name,''),'(unnamed)'), COUNT(*) FROM obs_spans
	        WHERE started_at >= ? AND status = 'error' GROUP BY name`
	if err := eachRow(ctx, db, eq, []any{sinceDay + "T00:00:00Z"}, func(rows *sql.Rows) error {
		var name string
		var n int64
		if err := rows.Scan(&name, &n); err != nil {
			return err
		}
		causes[name] = &ObsDimCount{Key: name, Traces: n}
		return nil
	}); err != nil {
		return fmt.Errorf("rollup.ObsAnalytics: error causes: %w", err)
	}
	for _, c := range causes {
		res.ErrorCauses = append(res.ErrorCauses, *c)
	}
	sort.SliceStable(res.ErrorCauses, func(i, j int) bool { return res.ErrorCauses[i].Traces > res.ErrorCauses[j].Traces })
	return nil
}

// percentile returns the p-quantile (0..1) of vals using the nearest-rank
// method. vals is sorted in place. Returns 0 for an empty slice.
func percentile(vals []int64, p float64) int64 {
	if len(vals) == 0 {
		return 0
	}
	sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
	idx := int(p * float64(len(vals)-1))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(vals) {
		idx = len(vals) - 1
	}
	return vals[idx]
}

func accumObsDim(m map[string]*ObsDimCount, key string, traces, tokens int64, cost float64) {
	if key == "" {
		key = "(none)"
	}
	d := m[key]
	if d == nil {
		d = &ObsDimCount{Key: key}
		m[key] = d
	}
	d.Traces += traces
	d.Tokens += tokens
	d.CostUSD += cost
}

func sortedObsDays(m map[string]*ObsDayPoint) []ObsDayPoint {
	out := make([]ObsDayPoint, 0, len(m))
	for _, d := range m {
		out = append(out, *d)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Date < out[j].Date })
	return out
}

func sortedObsDims(m map[string]*ObsDimCount) []ObsDimCount {
	out := make([]ObsDimCount, 0, len(m))
	for _, d := range m {
		out = append(out, *d)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Tokens != out[j].Tokens {
			return out[i].Tokens > out[j].Tokens
		}
		return out[i].Key < out[j].Key
	})
	return out
}
