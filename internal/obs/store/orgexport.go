// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 SuperBased

package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/marmutapp/superbased-observer/internal/orgcontract"
)

// orgexport.go is the obs side of the org-tier observability bridge
// (docs/plans/obs-org-tier-plan-2026-06-29.md §2). obs OWNS these reads of its
// own obs_* tables and returns PLAIN orgcontract rows; the host binds them as
// the ObsOrgProviders seam at the single obs wiring point so internal/store
// never imports internal/obs and orgpush.go never names an obs_* table (the
// privacy sentinel stays green). Every row is content-free here EXCEPT
// ContentForOrg, which carries raw bodies that the host strips unless the node
// shares full content — the content_hash always rides.

// hashProject returns the content-free project key (hex SHA-256 of the raw
// project_root), mirroring the ProjectRootHash posture sessions/api_turns ship.
// Empty in → empty out (an unset project stays an empty dimension).
func hashProject(root string) string {
	if root == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(root))
	return hex.EncodeToString(sum[:])
}

// AggregateForOrg implements the T1 provider: per (day, model, provider,
// project_hash, source) counts + token/cost/latency sums over the recent
// window. CONTENT-FREE — the raw project_root is hashed here and never leaves.
func (s *Store) AggregateForOrg(ctx context.Context, windowDays int) ([]orgcontract.ObsSummaryRow, error) {
	since := time.Now().UTC().AddDate(0, 0, -windowDays).Format("2006-01-02")
	rows, err := s.db.QueryContext(ctx, `
SELECT substr(t.started_at,1,10) AS day,
       COALESCE(sp.model,''), COALESCE(sp.provider,''),
       COALESCE(t.project_root,''), COALESCE(t.source,''),
       COUNT(DISTINCT t.trace_id),
       COUNT(sp.span_id),
       COALESCE(SUM(sp.input_tokens),0), COALESCE(SUM(sp.output_tokens),0),
       COALESCE(SUM(sp.cache_read_tokens),0), COALESCE(SUM(sp.cache_write_tokens),0),
       COALESCE(SUM(sp.reasoning_tokens),0), COALESCE(SUM(sp.total_tokens),0),
       COALESCE(SUM(sp.cost_usd),0),
       COUNT(DISTINCT CASE WHEN t.status NOT IN ('ok','unset','') THEN t.trace_id END)
  FROM obs_traces t
  LEFT JOIN obs_spans sp ON sp.trace_id = t.trace_id
 WHERE t.started_at >= ?
 GROUP BY day, sp.model, sp.provider, t.project_root, t.source`, since)
	if err != nil {
		return nil, fmt.Errorf("obs/store.AggregateForOrg: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []orgcontract.ObsSummaryRow{}
	for rows.Next() {
		var r orgcontract.ObsSummaryRow
		var projectRoot string
		if err := rows.Scan(&r.Day, &r.Model, &r.Provider, &projectRoot, &r.Source,
			&r.Traces, &r.Spans,
			&r.InputTokens, &r.OutputTokens, &r.CacheReadTokens, &r.CacheWriteTokens,
			&r.ReasoningTokens, &r.TotalTokens, &r.CostUSD, &r.ErrorTraces); err != nil {
			return nil, fmt.Errorf("obs/store.AggregateForOrg: scan: %w", err)
		}
		r.ProjectHash = hashProject(projectRoot)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Duration sum/count is a second pass (per-span ended-started, ms) over the
	// same window keyed identically, folded onto the rows by their cube key.
	if err := s.fillDurations(ctx, since, out); err != nil {
		return nil, err
	}
	return out, nil
}

// fillDurations folds per-(day,model,provider,project,source) span duration
// sum+count onto the aggregate rows (server derives mean). Done as a separate
// scan to keep the main GROUP BY readable; the duration is computed in Go from
// started_at/ended_at because they are RFC3339 text.
func (s *Store) fillDurations(ctx context.Context, since string, rows []orgcontract.ObsSummaryRow) error {
	type key struct{ day, model, provider, projectHash, source string }
	idx := make(map[key]int, len(rows))
	for i := range rows {
		idx[key{rows[i].Day, rows[i].Model, rows[i].Provider, rows[i].ProjectHash, rows[i].Source}] = i
	}
	q, err := s.db.QueryContext(ctx, `
SELECT substr(t.started_at,1,10) AS day, COALESCE(sp.model,''), COALESCE(sp.provider,''),
       COALESCE(t.project_root,''), COALESCE(t.source,''), sp.started_at, COALESCE(sp.ended_at,'')
  FROM obs_traces t JOIN obs_spans sp ON sp.trace_id = t.trace_id
 WHERE t.started_at >= ? AND sp.ended_at IS NOT NULL AND sp.ended_at != ''`, since)
	if err != nil {
		return fmt.Errorf("obs/store.fillDurations: %w", err)
	}
	defer func() { _ = q.Close() }()
	for q.Next() {
		var day, model, provider, projectRoot, source, startedAt, endedAt string
		if err := q.Scan(&day, &model, &provider, &projectRoot, &source, &startedAt, &endedAt); err != nil {
			return fmt.Errorf("obs/store.fillDurations: scan: %w", err)
		}
		i, ok := idx[key{day, model, provider, hashProject(projectRoot), source}]
		if !ok {
			continue
		}
		if ms := durationMsText(startedAt, endedAt); ms >= 0 {
			rows[i].DurationMsSum += ms
			rows[i].DurationMsCount++
		}
	}
	return q.Err()
}

// SpansForOrg implements the T2 provider: trace + span + event STRUCTURE within
// the window (hashes only — no bodies). The content-free request_id rides so
// the server can do the proxy-exact wedge join (obs_spans × api_turns). Capped
// at max spans (windowed-recompute v1; server upserts by composite key).
func (s *Store) SpansForOrg(ctx context.Context, cur orgcontract.ObsCursor, max int) (orgcontract.ObsSpanBatch, error) {
	since := cur.SinceDay
	if since == "" {
		since = time.Now().UTC().AddDate(0, 0, -7).Format("2006-01-02")
	}
	batch := orgcontract.ObsSpanBatch{Traces: []orgcontract.ObsTraceRow{}, Spans: []orgcontract.ObsSpanRow{}, Events: []orgcontract.ObsSpanEventRow{}, Cursor: cur}

	// Traces (with per-trace span_count + cost + total_tokens aggregate).
	trows, err := s.db.QueryContext(ctx, `
SELECT t.trace_id, COALESCE(t.session_id,''), COALESCE(t.thread_id,''), COALESCE(t.source,''),
       COALESCE(t.status,''), t.started_at, COALESCE(t.ended_at,''), COALESCE(t.project_root,''),
       COALESCE(t.root_span_id,''), COUNT(sp.span_id), COALESCE(SUM(sp.cost_usd),0),
       COALESCE(SUM(sp.total_tokens),0)
  FROM obs_traces t
  LEFT JOIN obs_spans sp ON sp.trace_id = t.trace_id
 WHERE t.started_at >= ?
 GROUP BY t.trace_id
 ORDER BY t.started_at DESC
 LIMIT ?`, since, max)
	if err != nil {
		return orgcontract.ObsSpanBatch{}, fmt.Errorf("obs/store.SpansForOrg: traces: %w", err)
	}
	traceIDs := []string{}
	for trows.Next() {
		var r orgcontract.ObsTraceRow
		if err := trows.Scan(&r.TraceID, &r.SessionID, &r.ThreadID, &r.Source, &r.Status,
			&r.StartedAt, &r.EndedAt, &r.ProjectRoot, &r.RootSpanID, &r.SpanCount, &r.CostUSD, &r.TotalTokens); err != nil {
			_ = trows.Close()
			return orgcontract.ObsSpanBatch{}, fmt.Errorf("obs/store.SpansForOrg: trace scan: %w", err)
		}
		r.ProjectHash = hashProject(r.ProjectRoot)
		batch.Traces = append(batch.Traces, r)
		traceIDs = append(traceIDs, r.TraceID)
	}
	if err := trows.Err(); err != nil {
		_ = trows.Close()
		return orgcontract.ObsSpanBatch{}, err
	}
	_ = trows.Close()
	if len(traceIDs) == 0 {
		return batch, nil
	}

	// Spans for those traces.
	in, args := inClause(traceIDs)
	//nolint:gosec // G202: `in` is a generated `?,?,…` placeholder list; trace ids bind via args.
	srows, err := s.db.QueryContext(ctx, `
SELECT trace_id, span_id, COALESCE(parent_span_id,''), kind, COALESCE(name,''),
       started_at, COALESCE(ended_at,''), COALESCE(status,''), COALESCE(model,''), COALESCE(provider,''),
       COALESCE(input_tokens,0), COALESCE(output_tokens,0), COALESCE(cache_read_tokens,0),
       COALESCE(cache_write_tokens,0), COALESCE(reasoning_tokens,0), COALESCE(total_tokens,0),
       COALESCE(cost_usd,0), COALESCE(cost_source,''), COALESCE(request_id,''), COALESCE(tool_call_id,'')
  FROM obs_spans WHERE trace_id IN (`+in+`)`, args...)
	if err != nil {
		return orgcontract.ObsSpanBatch{}, fmt.Errorf("obs/store.SpansForOrg: spans: %w", err)
	}
	for srows.Next() {
		var r orgcontract.ObsSpanRow
		var startedAt, endedAt string
		if err := srows.Scan(&r.TraceID, &r.SpanID, &r.ParentSpanID, &r.Kind, &r.Name,
			&startedAt, &endedAt, &r.Status, &r.Model, &r.Provider,
			&r.InputTokens, &r.OutputTokens, &r.CacheReadTokens, &r.CacheWriteTokens,
			&r.ReasoningTokens, &r.TotalTokens, &r.CostUSD, &r.CostSource, &r.RequestID, &r.ToolCallID); err != nil {
			_ = srows.Close()
			return orgcontract.ObsSpanBatch{}, fmt.Errorf("obs/store.SpansForOrg: span scan: %w", err)
		}
		r.StartedAt, r.EndedAt = startedAt, endedAt
		if ms := durationMsText(startedAt, endedAt); ms >= 0 {
			r.DurationMs = ms
		}
		batch.Spans = append(batch.Spans, r)
	}
	if err := srows.Err(); err != nil {
		_ = srows.Close()
		return orgcontract.ObsSpanBatch{}, err
	}
	_ = srows.Close()

	// Span events (metadata only — name + time, no attribute bodies).
	//nolint:gosec // G202: `in` is a generated `?,?,…` placeholder list; trace ids bind via args.
	erows, err := s.db.QueryContext(ctx, `
SELECT sp.trace_id, e.span_id, e.time, e.name
  FROM obs_span_events e JOIN obs_spans sp ON sp.span_id = e.span_id
 WHERE sp.trace_id IN (`+in+`)`, args...)
	if err != nil {
		return orgcontract.ObsSpanBatch{}, fmt.Errorf("obs/store.SpansForOrg: events: %w", err)
	}
	defer func() { _ = erows.Close() }()
	for erows.Next() {
		var r orgcontract.ObsSpanEventRow
		if err := erows.Scan(&r.TraceID, &r.SpanID, &r.Time, &r.Name); err != nil {
			return orgcontract.ObsSpanBatch{}, fmt.Errorf("obs/store.SpansForOrg: event scan: %w", err)
		}
		batch.Events = append(batch.Events, r)
	}
	return batch, erows.Err()
}

// ContentForOrg implements the T3 provider: raw span bodies within the window.
// The content_hash always rides; the host strips Content unless the node shares
// full content. (We return the raw body here; gating happens at the host strip
// site, exactly like otel_content.)
func (s *Store) ContentForOrg(ctx context.Context, cur orgcontract.ObsCursor, max int) ([]orgcontract.ObsContentRow, error) {
	since := cur.SinceDay
	if since == "" {
		since = time.Now().UTC().AddDate(0, 0, -7).Format("2006-01-02")
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT COALESCE(c.trace_id,''), c.span_id, c.kind, c.content_hash, COALESCE(c.content,''), c.time
  FROM obs_span_content c
  JOIN obs_spans sp ON sp.span_id = c.span_id
  JOIN obs_traces t ON t.trace_id = sp.trace_id
 WHERE t.started_at >= ?
 ORDER BY c.time DESC
 LIMIT ?`, since, max)
	if err != nil {
		return nil, fmt.Errorf("obs/store.ContentForOrg: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []orgcontract.ObsContentRow{}
	for rows.Next() {
		var r orgcontract.ObsContentRow
		if err := rows.Scan(&r.TraceID, &r.SpanID, &r.Kind, &r.ContentHash, &r.Content, &r.Timestamp); err != nil {
			return nil, fmt.Errorf("obs/store.ContentForOrg: scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// EvalRunsForOrg implements the T4 provider: eval-run health summaries within
// the window (content-free — run/dataset/scorer names + score aggregates).
// One row per (day, dataset, run, scorer): mean/min score + pass counts.
func (s *Store) EvalRunsForOrg(ctx context.Context, windowDays int) ([]orgcontract.ObsEvalRow, error) {
	since := time.Now().UTC().AddDate(0, 0, -windowDays).Format("2006-01-02")
	rows, err := s.db.QueryContext(ctx, `
SELECT substr(r.started_at,1,10) AS day, COALESCE(d.name,''), COALESCE(r.name,''),
       sc.scorer, COUNT(*), COALESCE(SUM(sc.passed),0),
       COALESCE(AVG(sc.score),0), COALESCE(MIN(sc.score),0), COALESCE(sc.source,'run')
  FROM obs_eval_scores sc
  JOIN obs_eval_runs r ON r.id = sc.run_id
  LEFT JOIN obs_datasets d ON d.id = r.dataset_id
 WHERE r.started_at >= ? AND sc.run_id IS NOT NULL
 GROUP BY day, d.name, r.name, sc.scorer, sc.source`, since)
	if err != nil {
		return nil, fmt.Errorf("obs/store.EvalRunsForOrg: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []orgcontract.ObsEvalRow{}
	for rows.Next() {
		var r orgcontract.ObsEvalRow
		if err := rows.Scan(&r.Day, &r.DatasetName, &r.RunName, &r.ScorerName,
			&r.Total, &r.Passed, &r.MeanScore, &r.MinScore, &r.Source); err != nil {
			return nil, fmt.Errorf("obs/store.EvalRunsForOrg: scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// durationMsText returns the millisecond delta between two RFC3339 timestamps,
// or -1 when either is empty/unparseable (caller skips). Mirrors the read-layer
// durationMS but returns int64 ms for the aggregate sums.
func durationMsText(startedAt, endedAt string) int64 {
	if startedAt == "" || endedAt == "" {
		return -1
	}
	st, err1 := time.Parse(time.RFC3339Nano, startedAt)
	en, err2 := time.Parse(time.RFC3339Nano, endedAt)
	if err1 != nil || err2 != nil {
		return -1
	}
	ms := en.Sub(st).Milliseconds()
	if ms < 0 {
		return -1
	}
	return ms
}

// inClause builds a `?,?,…` placeholder list + the matching args slice for a
// string IN (…) query.
func inClause(vals []string) (string, []any) {
	if len(vals) == 0 {
		return "", nil
	}
	ph := make([]byte, 0, len(vals)*2)
	args := make([]any, len(vals))
	for i, v := range vals {
		if i > 0 {
			ph = append(ph, ',')
		}
		ph = append(ph, '?')
		args[i] = v
	}
	return string(ph), args
}
