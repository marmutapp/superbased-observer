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

// EndUserSpendForOrg implements the T5 provider: per (day, end-user) spend +
// token + trace-count aggregate within the window, attributed through the
// node-local obs_traces.user column (the hosted-app end-user id, NOT the
// developer). PII — the host composes it only under ObsSummary &&
// shipsRawContent() (org-budget guardrails plan §2.1). Unattributed traces
// (empty user) are excluded; the raw project_root never leaves (not selected).
// Mirrors the node-local per-end-user read in admissionbudget.go::UserSpend.
func (s *Store) EndUserSpendForOrg(ctx context.Context, windowDays int) ([]orgcontract.ObsEndUserSpendRow, error) {
	since := time.Now().UTC().AddDate(0, 0, -windowDays).Format("2006-01-02")
	rows, err := s.db.QueryContext(ctx, `
SELECT substr(t.started_at,1,10) AS day, COALESCE(t.user,'') AS eu,
       COALESCE(SUM(sp.cost_usd),0), COUNT(DISTINCT t.trace_id),
       COALESCE(SUM(sp.total_tokens),0)
  FROM obs_traces t
  LEFT JOIN obs_spans sp ON sp.trace_id = t.trace_id
 WHERE t.started_at >= ? AND COALESCE(t.user,'') != ''
 GROUP BY day, eu`, since)
	if err != nil {
		return nil, fmt.Errorf("obs/store.EndUserSpendForOrg: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []orgcontract.ObsEndUserSpendRow{}
	for rows.Next() {
		var r orgcontract.ObsEndUserSpendRow
		if err := rows.Scan(&r.Day, &r.EndUser, &r.CostUSD, &r.Traces, &r.TotalTokens); err != nil {
			return nil, fmt.Errorf("obs/store.EndUserSpendForOrg: scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AdmissionForOrg implements the T6 provider (Plane-A admission org tier,
// gap-audit 2026-07-10 §2.1 / #1a): the window's input-admission verdict events
// (obs_admission_events, newest first, capped at max) plus the policy snapshots
// (obs_admission_policy_versions) those verdicts reference. The raw request
// text is NEVER stored on the node — only message_hash — so the verdict
// metadata is content-free; the three content-bearing columns (tenant, user,
// reason_excerpt) ride raw out of this provider and the HOST strips them unless
// the node shares full content (composeObsTiers), exactly like ContentForOrg's
// body. Naming the obs_admission_* tables HERE is correct — this is the
// obs-owned file; orgpush.go never names them.
//
// Policies are selected by policy_hash membership in the returned window's
// events (referential completeness: every shipped verdict's policy travels with
// it), which the obs_admission_policy_versions PK supports as a clean IN-list;
// a policy that judged no shared traffic in the window simply doesn't ship.
func (s *Store) AdmissionForOrg(ctx context.Context, cur orgcontract.ObsCursor, max int) (orgcontract.ObsAdmissionBatch, error) {
	since := cur.SinceDay
	if since == "" {
		since = time.Now().UTC().AddDate(0, 0, -7).Format("2006-01-02")
	}
	batch := orgcontract.ObsAdmissionBatch{
		Events:   []orgcontract.ObsAdmissionRow{},
		Policies: []orgcontract.ObsAdmissionPolicyRow{},
		Cursor:   cur,
	}

	// Verdict events (newest first; ts is RFC3339 text so a "2006-01-02" since
	// day compares lexicographically). judge_used is INTEGER → scanned to bool.
	erows, err := s.db.QueryContext(ctx, `
SELECT ts, mode, decision, severity, COALESCE(criterion_id,''), policy_hash,
       judge_used, COALESCE(judge_hosting,''), COALESCE(degraded,''), latency_ms,
       COALESCE(trace_id,''), COALESCE(session_id,''), COALESCE(request_id,''),
       message_hash, COALESCE(prev_hash,''), row_hash,
       COALESCE(tenant,''), COALESCE(user,''), COALESCE(reason_excerpt,'')
  FROM obs_admission_events
 WHERE ts >= ?
 ORDER BY id DESC
 LIMIT ?`, since, max)
	if err != nil {
		return orgcontract.ObsAdmissionBatch{}, fmt.Errorf("obs/store.AdmissionForOrg: events: %w", err)
	}
	policyHashes := map[string]struct{}{}
	for erows.Next() {
		var r orgcontract.ObsAdmissionRow
		var judgeUsed int64
		if err := erows.Scan(&r.TS, &r.Mode, &r.Decision, &r.Severity, &r.CriterionID, &r.PolicyHash,
			&judgeUsed, &r.JudgeHosting, &r.Degraded, &r.LatencyMS,
			&r.TraceID, &r.SessionID, &r.RequestID,
			&r.MessageHash, &r.PrevHash, &r.RowHash,
			&r.Tenant, &r.EndUser, &r.ReasonExcerpt); err != nil {
			_ = erows.Close()
			return orgcontract.ObsAdmissionBatch{}, fmt.Errorf("obs/store.AdmissionForOrg: event scan: %w", err)
		}
		r.JudgeUsed = judgeUsed != 0
		batch.Events = append(batch.Events, r)
		if r.PolicyHash != "" {
			policyHashes[r.PolicyHash] = struct{}{}
		}
	}
	if err := erows.Err(); err != nil {
		_ = erows.Close()
		return orgcontract.ObsAdmissionBatch{}, err
	}
	_ = erows.Close()
	if len(policyHashes) == 0 {
		return batch, nil
	}

	// Policy snapshots referenced by the window's verdicts. body is the ADMIN's
	// authored policy (config), always shipped by the host.
	hashes := make([]string, 0, len(policyHashes))
	for h := range policyHashes {
		hashes = append(hashes, h)
	}
	in, args := inClause(hashes)
	//nolint:gosec // G202: `in` is a generated `?,?,…` placeholder list; policy hashes bind via args.
	prows, err := s.db.QueryContext(ctx, `
SELECT policy_hash, created_at, mode, scope, criteria_count, COALESCE(body,'')
  FROM obs_admission_policy_versions
 WHERE policy_hash IN (`+in+`)`, args...)
	if err != nil {
		return orgcontract.ObsAdmissionBatch{}, fmt.Errorf("obs/store.AdmissionForOrg: policies: %w", err)
	}
	defer func() { _ = prows.Close() }()
	for prows.Next() {
		var r orgcontract.ObsAdmissionPolicyRow
		if err := prows.Scan(&r.PolicyHash, &r.CreatedAt, &r.Mode, &r.Scope, &r.CriteriaCount, &r.Body); err != nil {
			return orgcontract.ObsAdmissionBatch{}, fmt.Errorf("obs/store.AdmissionForOrg: policy scan: %w", err)
		}
		batch.Policies = append(batch.Policies, r)
	}
	return batch, prows.Err()
}

// maxEvalExcerptLen bounds the per-item content excerpts shipped in the T7
// per-item eval tier. The dataset item input/output can be large; the org
// surface only needs a bounded excerpt to render the drill-down, so the
// provider truncates to keep the push payload sane (the node's ContentGate has
// already decided whether ANY raw body is stored — this only bounds the size).
const maxEvalExcerptLen = 2000

// EvalItemsForOrg implements the T7 provider (Plane-A eval-run detail org tier,
// gap-audit 2026-07-10 §1 / §2.2 / §6): the window's per-item eval scores
// (obs_eval_scores, source='run') joined to their run (obs_eval_runs), dataset
// (obs_datasets), dataset item (obs_dataset_items — for content_hash + the
// input/reference/output snapshots) and scored span (obs_spans — for duration).
// The item content excerpts (input/expected/output/rationale) ride raw out of
// this provider and the HOST strips them unless the node shares full content
// (composeObsTiers), exactly like ContentForOrg's body; the content_hash always
// rides. Naming the obs_eval_* tables HERE is correct — this is the obs-owned
// file; orgpush.go never names them. Online-sampled scores (run_id NULL) are
// excluded (they carry no run identity to attach to). Windowed by run
// started_at, capped at max (windowed-recompute v1; server upserts by the
// run/item/scorer natural key).
func (s *Store) EvalItemsForOrg(ctx context.Context, cur orgcontract.ObsCursor, max int) (orgcontract.ObsEvalItemBatch, error) {
	since := cur.SinceDay
	if since == "" {
		since = time.Now().UTC().AddDate(0, 0, -7).Format("2006-01-02")
	}
	batch := orgcontract.ObsEvalItemBatch{Items: []orgcontract.ObsEvalItemRow{}, Cursor: cur}

	rows, err := s.db.QueryContext(ctx, `
SELECT r.id, COALESCE(r.name,''), r.dataset_id, COALESCE(d.name,''),
       COALESCE(sc.item_id,0), COALESCE(sc.span_id,''), COALESCE(i.trace_id,''),
       sc.scorer, sc.score, sc.passed, COALESCE(sc.source,'run'),
       COALESCE(sp.started_at,''), COALESCE(sp.ended_at,''),
       sc.created_at, COALESCE(i.content_hash,''),
       COALESCE(i.input,''), COALESCE(i.reference,''), COALESCE(i.output,''),
       COALESCE(sc.rationale,'')
  FROM obs_eval_scores sc
  JOIN obs_eval_runs r ON r.id = sc.run_id
  LEFT JOIN obs_datasets d ON d.id = r.dataset_id
  LEFT JOIN obs_dataset_items i ON i.id = sc.item_id
  LEFT JOIN obs_spans sp ON sp.span_id = sc.span_id
 WHERE sc.run_id IS NOT NULL AND r.started_at >= ?
 ORDER BY r.id DESC, sc.item_id, sc.scorer
 LIMIT ?`, since, max)
	if err != nil {
		return orgcontract.ObsEvalItemBatch{}, fmt.Errorf("obs/store.EvalItemsForOrg: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			r              orgcontract.ObsEvalItemRow
			passed         int64
			started, ended string
		)
		if err := rows.Scan(&r.RunID, &r.RunName, &r.DatasetID, &r.DatasetName,
			&r.ItemID, &r.SpanID, &r.TraceID,
			&r.Scorer, &r.Score, &passed, &r.Source,
			&started, &ended,
			&r.TS, &r.ContentHash,
			&r.InputExcerpt, &r.ExpectedExcerpt, &r.OutputExcerpt,
			&r.Rationale); err != nil {
			return orgcontract.ObsEvalItemBatch{}, fmt.Errorf("obs/store.EvalItemsForOrg: scan: %w", err)
		}
		r.Passed = passed != 0
		if ms := durationMsText(started, ended); ms >= 0 {
			r.DurationMs = ms
		}
		r.InputExcerpt = truncExcerpt(r.InputExcerpt)
		r.ExpectedExcerpt = truncExcerpt(r.ExpectedExcerpt)
		r.OutputExcerpt = truncExcerpt(r.OutputExcerpt)
		r.Rationale = truncExcerpt(r.Rationale)
		batch.Items = append(batch.Items, r)
	}
	return batch, rows.Err()
}

// truncExcerpt bounds a content excerpt to maxEvalExcerptLen runes, appending a
// single ellipsis when it truncates. Empty in → empty out.
func truncExcerpt(s string) string {
	if s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) <= maxEvalExcerptLen {
		return s
	}
	return string(r[:maxEvalExcerptLen]) + "…"
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
