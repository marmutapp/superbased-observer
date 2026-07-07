package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// TraceListRow is one row of the trajectory list (the /api/obs/traces surface).
// Token fields are per-trace sums across the trace's spans (Gap C): surfaced in
// the hero band + list so cache/reasoning/total aren't buried in the span drawer.
type TraceListRow struct {
	TraceID          string  `json:"trace_id"`
	RootName         string  `json:"root_name"`
	Source           string  `json:"source"`
	SessionID        string  `json:"session_id"`
	Status           string  `json:"status"`
	StartedAt        string  `json:"started_at"`
	EndedAt          string  `json:"ended_at"`
	DurationMS       int64   `json:"duration_ms"`
	SpanCount        int64   `json:"span_count"`
	CostUSD          float64 `json:"cost_usd"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	ReasoningTokens  int64   `json:"reasoning_tokens"`
	TotalTokens      int64   `json:"total_tokens"` // reported total where present, else input+output
}

// SpanRow is one span in the trajectory detail. Nullable token/cost are
// pointers so the UI can distinguish "0" from "not reported".
type SpanRow struct {
	SpanID           string          `json:"span_id"`
	ParentSpanID     string          `json:"parent_span_id"`
	Kind             string          `json:"kind"`
	Name             string          `json:"name"`
	Status           string          `json:"status"`
	StartedAt        string          `json:"started_at"`
	EndedAt          string          `json:"ended_at"`
	DurationMS       int64           `json:"duration_ms"`
	Model            string          `json:"model"`
	Provider         string          `json:"provider"`
	InputTokens      *int64          `json:"input_tokens"`
	OutputTokens     *int64          `json:"output_tokens"`
	CacheReadTokens  *int64          `json:"cache_read_tokens"`
	CacheWriteTokens *int64          `json:"cache_write_tokens"`
	ReasoningTokens  *int64          `json:"reasoning_tokens"`
	CostUSD          *float64        `json:"cost_usd"`
	CostSource       string          `json:"cost_source"`           // "reported" | "computed" | ""
	CostDetail       json.RawMessage `json:"cost_detail,omitempty"` // {input,output,cache_read,...} USD
	RequestID        string          `json:"request_id"`
}

// SpanEventRow / SpanLinkRow are the detail's events + cross-trace links.
type SpanEventRow struct {
	SpanID     string `json:"span_id"`
	Time       string `json:"time"`
	Name       string `json:"name"`
	Attributes string `json:"attributes"`
}

type SpanLinkRow struct {
	SpanID      string `json:"span_id"`
	LinkedTrace string `json:"linked_trace"`
	LinkedSpan  string `json:"linked_span"`
}

// SpanContentRow is one captured prompt/response/tool-io body for a span.
// Content is the raw body, present only when the node's content posture allowed
// it (else ""); ContentHash is always set so the gated-off case still proves a
// body existed.
type SpanContentRow struct {
	SpanID      string `json:"span_id"`
	Kind        string `json:"kind"`
	Content     string `json:"content"`
	ContentHash string `json:"content_hash"`
	Time        string `json:"time"`
}

// TraceDetail is the full payload for one trace's trajectory view.
type TraceDetail struct {
	Trace   TraceListRow     `json:"trace"`
	Spans   []SpanRow        `json:"spans"`
	Events  []SpanEventRow   `json:"events"`
	Links   []SpanLinkRow    `json:"links"`
	Content []SpanContentRow `json:"content"`
}

// ListTraces returns the most-recent traces (by start time), with per-trace
// span count + summed cost + root-span name. limit is clamped to [1,500].
func (s *Store) ListTraces(ctx context.Context, limit, offset int) ([]TraceListRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT t.trace_id, t.source, COALESCE(t.session_id,''), t.status,
       t.started_at, COALESCE(t.ended_at,''),
       COALESCE(rs.name,''), COUNT(sp.span_id), COALESCE(SUM(sp.cost_usd),0),
       `+traceUsageSums+`
  FROM obs_traces t
  LEFT JOIN obs_spans sp ON sp.trace_id = t.trace_id
  LEFT JOIN obs_spans rs ON rs.span_id = t.root_span_id
 GROUP BY t.trace_id
 ORDER BY t.started_at DESC
 LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("obs/store.ListTraces: %w", err)
	}
	defer rows.Close()
	var out []TraceListRow
	for rows.Next() {
		var r TraceListRow
		var totalTok int64
		if err := rows.Scan(&r.TraceID, &r.Source, &r.SessionID, &r.Status,
			&r.StartedAt, &r.EndedAt, &r.RootName, &r.SpanCount, &r.CostUSD,
			&r.InputTokens, &r.OutputTokens, &r.CacheReadTokens, &r.CacheWriteTokens, &r.ReasoningTokens, &totalTok); err != nil {
			return nil, fmt.Errorf("obs/store.ListTraces: scan: %w", err)
		}
		finalizeTraceRow(&r, totalTok)
		out = append(out, r)
	}
	return out, rows.Err()
}

// traceUsageSums is the shared per-trace token-sum SELECT fragment (Gap C),
// used by both ListTraces and GetTrace so the list and detail agree.
const traceUsageSums = `COALESCE(SUM(sp.input_tokens),0), COALESCE(SUM(sp.output_tokens),0),
       COALESCE(SUM(sp.cache_read_tokens),0), COALESCE(SUM(sp.cache_write_tokens),0),
       COALESCE(SUM(sp.reasoning_tokens),0), COALESCE(SUM(sp.total_tokens),0)`

// finalizeTraceRow fills the derived fields: duration, and a TotalTokens that
// prefers the instrumentor-reported total but falls back to input+output when
// none was reported (total_tokens is sparse).
func finalizeTraceRow(r *TraceListRow, reportedTotal int64) {
	r.DurationMS = durationMS(r.StartedAt, r.EndedAt)
	if reportedTotal > 0 {
		r.TotalTokens = reportedTotal
	} else {
		r.TotalTokens = r.InputTokens + r.OutputTokens
	}
}

// GetTrace returns the full trajectory for one trace, or found=false when the
// trace_id is unknown.
func (s *Store) GetTrace(ctx context.Context, traceID string) (TraceDetail, bool, error) {
	var d TraceDetail
	var totalTok int64
	err := s.db.QueryRowContext(ctx, `
SELECT t.trace_id, t.source, COALESCE(t.session_id,''), t.status,
       t.started_at, COALESCE(t.ended_at,''),
       COALESCE(rs.name,''), COUNT(sp.span_id), COALESCE(SUM(sp.cost_usd),0),
       `+traceUsageSums+`
  FROM obs_traces t
  LEFT JOIN obs_spans sp ON sp.trace_id = t.trace_id
  LEFT JOIN obs_spans rs ON rs.span_id = t.root_span_id
 WHERE t.trace_id = ?
 GROUP BY t.trace_id`, traceID).Scan(
		&d.Trace.TraceID, &d.Trace.Source, &d.Trace.SessionID, &d.Trace.Status,
		&d.Trace.StartedAt, &d.Trace.EndedAt, &d.Trace.RootName, &d.Trace.SpanCount, &d.Trace.CostUSD,
		&d.Trace.InputTokens, &d.Trace.OutputTokens, &d.Trace.CacheReadTokens, &d.Trace.CacheWriteTokens, &d.Trace.ReasoningTokens, &totalTok,
	)
	if err != nil {
		// No row (COUNT makes this rare) → treat as not-found via a probe.
		var exists int
		if e2 := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM obs_traces WHERE trace_id=?`, traceID).Scan(&exists); e2 != nil || exists == 0 {
			return TraceDetail{}, false, nil
		}
		return TraceDetail{}, false, fmt.Errorf("obs/store.GetTrace: %w", err)
	}
	finalizeTraceRow(&d.Trace, totalTok)

	d.Spans = []SpanRow{}
	srows, err := s.db.QueryContext(ctx, `
SELECT span_id, COALESCE(parent_span_id,''), kind, COALESCE(name,''), status,
       started_at, COALESCE(ended_at,''), COALESCE(model,''), COALESCE(provider,''),
       input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens,
       cost_usd, COALESCE(cost_source,''), COALESCE(cost_detail,''), COALESCE(request_id,'')
  FROM obs_spans WHERE trace_id = ? ORDER BY started_at`, traceID)
	if err != nil {
		return TraceDetail{}, false, fmt.Errorf("obs/store.GetTrace: spans: %w", err)
	}
	defer srows.Close()
	for srows.Next() {
		var r SpanRow
		var costDetail string
		if err := srows.Scan(&r.SpanID, &r.ParentSpanID, &r.Kind, &r.Name, &r.Status,
			&r.StartedAt, &r.EndedAt, &r.Model, &r.Provider,
			&r.InputTokens, &r.OutputTokens, &r.CacheReadTokens, &r.CacheWriteTokens, &r.ReasoningTokens,
			&r.CostUSD, &r.CostSource, &costDetail, &r.RequestID); err != nil {
			return TraceDetail{}, false, fmt.Errorf("obs/store.GetTrace: span scan: %w", err)
		}
		if costDetail != "" {
			r.CostDetail = json.RawMessage(costDetail)
		}
		r.DurationMS = durationMS(r.StartedAt, r.EndedAt)
		d.Spans = append(d.Spans, r)
	}
	if err := srows.Err(); err != nil {
		return TraceDetail{}, false, err
	}

	d.Events = []SpanEventRow{}
	erows, err := s.db.QueryContext(ctx, `
SELECT e.span_id, e.time, e.name, COALESCE(e.attributes,'')
  FROM obs_span_events e JOIN obs_spans sp ON sp.span_id = e.span_id
 WHERE sp.trace_id = ? ORDER BY e.time`, traceID)
	if err != nil {
		return TraceDetail{}, false, fmt.Errorf("obs/store.GetTrace: events: %w", err)
	}
	defer erows.Close()
	for erows.Next() {
		var r SpanEventRow
		if err := erows.Scan(&r.SpanID, &r.Time, &r.Name, &r.Attributes); err != nil {
			return TraceDetail{}, false, fmt.Errorf("obs/store.GetTrace: event scan: %w", err)
		}
		d.Events = append(d.Events, r)
	}
	if err := erows.Err(); err != nil {
		return TraceDetail{}, false, err
	}

	d.Links = []SpanLinkRow{}
	lrows, err := s.db.QueryContext(ctx, `
SELECT l.span_id, l.linked_trace, COALESCE(l.linked_span,'')
  FROM obs_span_links l JOIN obs_spans sp ON sp.span_id = l.span_id
 WHERE sp.trace_id = ?`, traceID)
	if err != nil {
		return TraceDetail{}, false, fmt.Errorf("obs/store.GetTrace: links: %w", err)
	}
	defer lrows.Close()
	for lrows.Next() {
		var r SpanLinkRow
		if err := lrows.Scan(&r.SpanID, &r.LinkedTrace, &r.LinkedSpan); err != nil {
			return TraceDetail{}, false, fmt.Errorf("obs/store.GetTrace: link scan: %w", err)
		}
		d.Links = append(d.Links, r)
	}
	if err := lrows.Err(); err != nil {
		return TraceDetail{}, false, err
	}

	d.Content = []SpanContentRow{}
	crows, err := s.db.QueryContext(ctx, `
SELECT c.span_id, c.kind, COALESCE(c.content,''), c.content_hash, c.time
  FROM obs_span_content c JOIN obs_spans sp ON sp.span_id = c.span_id
 WHERE sp.trace_id = ? ORDER BY c.span_id, c.id`, traceID)
	if err != nil {
		return TraceDetail{}, false, fmt.Errorf("obs/store.GetTrace: content: %w", err)
	}
	defer crows.Close()
	for crows.Next() {
		var r SpanContentRow
		if err := crows.Scan(&r.SpanID, &r.Kind, &r.Content, &r.ContentHash, &r.Time); err != nil {
			return TraceDetail{}, false, fmt.Errorf("obs/store.GetTrace: content scan: %w", err)
		}
		d.Content = append(d.Content, r)
	}
	return d, true, crows.Err()
}

// durationMS computes milliseconds between two RFC3339 timestamps, 0 when
// either is empty/unparseable or the span is still open.
func durationMS(start, end string) int64 {
	if start == "" || end == "" {
		return 0
	}
	s, err1 := time.Parse(time.RFC3339Nano, start)
	e, err2 := time.Parse(time.RFC3339Nano, end)
	if err1 != nil || err2 != nil || e.Before(s) {
		return 0
	}
	return e.Sub(s).Milliseconds()
}
