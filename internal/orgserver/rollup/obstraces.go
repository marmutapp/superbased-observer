// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 SuperBased

package rollup

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// durationMsBetween returns the millisecond delta between two RFC3339
// timestamps, or 0 when either is empty/unparseable.
func durationMsBetween(startedAt, endedAt string) int64 {
	if startedAt == "" || endedAt == "" {
		return 0
	}
	st, err1 := time.Parse(time.RFC3339Nano, startedAt)
	en, err2 := time.Parse(time.RFC3339Nano, endedAt)
	if err1 != nil || err2 != nil {
		return 0
	}
	ms := en.Sub(st).Milliseconds()
	if ms < 0 {
		return 0
	}
	return ms
}

// obstraces.go is the OP2 org trajectory explorer (obs-org-tier plan §5.3): the
// trace list + one-trace span tree over the obs_traces/obs_spans STRUCTURE that
// member nodes push under [org_client.share].obs_traces. The headline is the
// proxy-exact WEDGE: each LLM span's content-free request_id is joined to
// api_turns (the proxy-verified cost/cache the node already pushes) — surfacing
// exact cost on org-scale agent spans, which no pure OTel backend can do.
//
// RBAC follows the people-scope posture: an admin sees the whole org; a team
// lead sees their team members' (and own) trajectories — scoped on
// pushed_by_user_id via peopleScopeSQL, exactly like Sessions.

// ObsTraceListRow is one trace in the explorer list.
type ObsTraceListRow struct {
	TraceID     string  `json:"trace_id"`
	RootName    string  `json:"root_name"`
	Source      string  `json:"source"`
	SessionID   string  `json:"session_id"`
	Status      string  `json:"status"`
	Email       string  `json:"email"`
	StartedAt   string  `json:"started_at"`
	DurationMs  int64   `json:"duration_ms"`
	SpanCount   int64   `json:"span_count"`
	TotalTokens int64   `json:"total_tokens"`
	CostUSD     float64 `json:"cost_usd"`
}

// ObsTrajectoriesResult is the GET /api/org/obs/trajectories body.
type ObsTrajectoriesResult struct {
	WindowDays int               `json:"window_days"`
	Configured bool              `json:"configured"`
	Traces     []ObsTraceListRow `json:"traces"`
}

// ObsProxyEnrichment is the proxy-exact wedge for one span (joined from
// api_turns by request_id). Found=false when the span had no matching proxy
// turn (a raw-instrumentor span, or a non-proxied call).
type ObsProxyEnrichment struct {
	Found               bool    `json:"found"`
	Provider            string  `json:"provider"`
	Model               string  `json:"model"`
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	CostUSD             float64 `json:"cost_usd"`
}

// ObsSpanDetail is one span in the tree, with its optional proxy enrichment.
type ObsSpanDetail struct {
	SpanID           string              `json:"span_id"`
	ParentSpanID     string              `json:"parent_span_id"`
	Kind             string              `json:"kind"`
	Name             string              `json:"name"`
	StartedAt        string              `json:"started_at"`
	EndedAt          string              `json:"ended_at"`
	DurationMs       int64               `json:"duration_ms"`
	Status           string              `json:"status"`
	Model            string              `json:"model"`
	Provider         string              `json:"provider"`
	InputTokens      int64               `json:"input_tokens"`
	OutputTokens     int64               `json:"output_tokens"`
	CacheReadTokens  int64               `json:"cache_read_tokens"`
	CacheWriteTokens int64               `json:"cache_write_tokens"`
	ReasoningTokens  int64               `json:"reasoning_tokens"`
	TotalTokens      int64               `json:"total_tokens"`
	CostUSD          float64             `json:"cost_usd"`
	CostSource       string              `json:"cost_source"`
	RequestID        string              `json:"request_id"`
	Enrichment       *ObsProxyEnrichment `json:"enrichment,omitempty"`
}

// ObsTraceDetailResult is the GET /api/org/obs/trace/{id} body.
type ObsTraceDetailResult struct {
	Trace ObsTraceListRow `json:"trace"`
	Spans []ObsSpanDetail `json:"spans"`
}

// ObsTrajectories lists traces in the window, scoped by RBAC, newest first.
func ObsTrajectories(ctx context.Context, db *sql.DB, w Window, scope Scope, selfUserID string, limit int, now time.Time) (ObsTrajectoriesResult, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	res := ObsTrajectoriesResult{WindowDays: w.days(), Traces: []ObsTraceListRow{}}
	uScope, uArgs := peopleScopeSQL("t.pushed_by_user_id", scope, selfUserID)
	if uScope == falseScope {
		return res, nil
	}
	sinceTs := since(w, now)
	//nolint:gosec // G201: uScope is a parameterized fragment; all values bind via ?.
	q := `
SELECT t.trace_id, COALESCE(rs.name,''), COALESCE(t.source,''), COALESCE(t.session_id,''),
       COALESCE(t.status,''), COALESCE(t.user_email,''), t.started_at, COALESCE(t.ended_at,''),
       t.span_count, t.total_tokens, t.cost_usd
  FROM obs_traces t
  LEFT JOIN obs_spans rs ON rs.org_id = t.org_id AND rs.trace_id = t.trace_id AND rs.span_id = t.root_span_id
 WHERE t.started_at >= ? AND ` + uScope + `
 ORDER BY t.started_at DESC
 LIMIT ?`
	args := append(append([]any{sinceTs}, uArgs...), limit)
	if err := eachRow(ctx, db, q, args, func(rows *sql.Rows) error {
		var r ObsTraceListRow
		var startedAt, endedAt string
		if err := rows.Scan(&r.TraceID, &r.RootName, &r.Source, &r.SessionID, &r.Status,
			&r.Email, &startedAt, &endedAt, &r.SpanCount, &r.TotalTokens, &r.CostUSD); err != nil {
			return err
		}
		r.StartedAt = startedAt
		r.DurationMs = durationMsBetween(startedAt, endedAt)
		res.Traces = append(res.Traces, r)
		return nil
	}); err != nil {
		return ObsTrajectoriesResult{}, fmt.Errorf("rollup.ObsTrajectories: %w", err)
	}
	res.Configured = len(res.Traces) > 0
	return res, nil
}

// ObsTraceDetail returns one trace's span tree + the per-span proxy wedge, or
// found=false when the trace_id is unknown or out of the caller's scope.
func ObsTraceDetail(ctx context.Context, db *sql.DB, id string, scope Scope, selfUserID string, now time.Time) (ObsTraceDetailResult, bool, error) {
	uScope, uArgs := peopleScopeSQL("pushed_by_user_id", scope, selfUserID)
	if uScope == falseScope {
		return ObsTraceDetailResult{}, false, nil
	}
	var out ObsTraceDetailResult
	var orgID, startedAt, endedAt string
	//nolint:gosec // G201: uScope is a parameterized fragment; all values bind via ?.
	headQ := `
SELECT org_id, trace_id, COALESCE(source,''), COALESCE(session_id,''), COALESCE(status,''),
       COALESCE(user_email,''), started_at, COALESCE(ended_at,''), span_count, total_tokens, cost_usd,
       COALESCE(root_span_id,'')
  FROM obs_traces
 WHERE trace_id = ? AND ` + uScope + `
 LIMIT 1`
	var rootSpanID string
	row := db.QueryRowContext(ctx, headQ, append([]any{id}, uArgs...)...)
	if err := row.Scan(&orgID, &out.Trace.TraceID, &out.Trace.Source, &out.Trace.SessionID, &out.Trace.Status,
		&out.Trace.Email, &startedAt, &endedAt, &out.Trace.SpanCount, &out.Trace.TotalTokens, &out.Trace.CostUSD, &rootSpanID); err != nil {
		if err == sql.ErrNoRows {
			return ObsTraceDetailResult{}, false, nil
		}
		return ObsTraceDetailResult{}, false, fmt.Errorf("rollup.ObsTraceDetail: head: %w", err)
	}
	out.Trace.StartedAt = startedAt
	out.Trace.DurationMs = durationMsBetween(startedAt, endedAt)
	out.Spans = []ObsSpanDetail{}

	spanQ := `
SELECT span_id, COALESCE(parent_span_id,''), kind, COALESCE(name,''), started_at, COALESCE(ended_at,''),
       duration_ms, COALESCE(status,''), COALESCE(model,''), COALESCE(provider,''),
       input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens,
       total_tokens, cost_usd, COALESCE(cost_source,''), COALESCE(request_id,'')
  FROM obs_spans
 WHERE org_id = ? AND trace_id = ?
 ORDER BY started_at`
	if err := eachRow(ctx, db, spanQ, []any{orgID, id}, func(rows *sql.Rows) error {
		var s ObsSpanDetail
		if err := rows.Scan(&s.SpanID, &s.ParentSpanID, &s.Kind, &s.Name, &s.StartedAt, &s.EndedAt,
			&s.DurationMs, &s.Status, &s.Model, &s.Provider,
			&s.InputTokens, &s.OutputTokens, &s.CacheReadTokens, &s.CacheWriteTokens, &s.ReasoningTokens,
			&s.TotalTokens, &s.CostUSD, &s.CostSource, &s.RequestID); err != nil {
			return err
		}
		out.Spans = append(out.Spans, s)
		return nil
	}); err != nil {
		return ObsTraceDetailResult{}, false, fmt.Errorf("rollup.ObsTraceDetail: spans: %w", err)
	}

	// The proxy-exact wedge: enrich each request_id-bearing span from api_turns
	// (the proxy-verified cost/cache the node already pushed), scoped to org.
	if err := enrichObsSpans(ctx, db, orgID, out.Spans); err != nil {
		return ObsTraceDetailResult{}, false, err
	}
	return out, true, nil
}

// enrichObsSpans joins each span's request_id to api_turns for this org and
// attaches the proxy-exact enrichment (the wedge). Spans without a request_id,
// or with no matching proxy turn, stay un-enriched (Found=false / nil).
func enrichObsSpans(ctx context.Context, db *sql.DB, orgID string, spans []ObsSpanDetail) error {
	idx := map[string]int{}
	for i := range spans {
		if spans[i].RequestID != "" {
			idx[spans[i].RequestID] = i
		}
	}
	if len(idx) == 0 {
		return nil
	}
	qargs := make([]any, 0, len(idx)+1)
	qargs = append(qargs, orgID)
	for id := range idx {
		qargs = append(qargs, id)
	}
	//nolint:gosec // G201: placeholders() is a generated `?,?,…` list; values bind via ?.
	q := `
SELECT request_id, COALESCE(provider,''), COALESCE(model,''),
       COALESCE(input_tokens,0), COALESCE(output_tokens,0),
       COALESCE(cache_read_tokens,0), COALESCE(cache_creation_tokens,0), COALESCE(cost_usd,0)
  FROM api_turns
 WHERE org_id = ? AND request_id IN (` + placeholders(len(idx)) + `)`
	rows, err := db.QueryContext(ctx, q, qargs...)
	if err != nil {
		return fmt.Errorf("rollup.ObsTraceDetail: wedge: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var reqID string
		var e ObsProxyEnrichment
		if err := rows.Scan(&reqID, &e.Provider, &e.Model, &e.InputTokens, &e.OutputTokens,
			&e.CacheReadTokens, &e.CacheCreationTokens, &e.CostUSD); err != nil {
			return fmt.Errorf("rollup.ObsTraceDetail: wedge scan: %w", err)
		}
		if i, ok := idx[reqID]; ok {
			e.Found = true
			spans[i].Enrichment = &e
		}
	}
	return rows.Err()
}
