// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 SuperBased

package rollup

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// obseval.go is the READ-ONLY org per-item eval surface (Plane-A eval-run
// detail org tier, gap-audit 2026-07-10 §1 / §2.2 / §6). It surfaces the
// per-item eval scores enrolled nodes push under
// [org_client.share.obs].eval_items: a run list, one run's per-item scores, a
// run-vs-run per-scorer comparison, and — as a DEEPER, server-audited
// disclosure — the item content excerpts. It complements ObsEvals (the T4
// content-free aggregate), which answers "how are runs trending"; this answers
// "which items regressed, and why".
//
// Run identity: obs_eval_items.run_id is node-local, so a run is keyed by
// (pushed_by_user_id, run_id) — the pushing node plus its own run id. The
// surface exposes that pair as an opaque composite ref (encodeRunRef); the bare
// integer never reaches the client. Scope is applied on pushed_by_user_id
// exactly like ObsAdmission / ObsTraceContent, so a lead can only reach runs in
// their scope even with a hand-crafted ref.

// evalRunRefSep separates the pushing user id from the run id in a run ref. The
// run id is always a trailing integer, so decodeRunRef splits on the LAST
// separator — the user id may itself contain the separator without ambiguity.
const evalRunRefSep = "~"

// encodeRunRef builds the opaque run reference (pushed_by_user_id + run_id).
func encodeRunRef(pushedByUserID string, runID int64) string {
	return pushedByUserID + evalRunRefSep + strconv.FormatInt(runID, 10)
}

// decodeRunRef parses a run ref back into (pushed_by_user_id, run_id). It splits
// on the LAST separator so a user id containing the separator still decodes
// (the run id is a trailing integer). ok=false on a malformed ref.
func decodeRunRef(ref string) (userID string, runID int64, ok bool) {
	i := strings.LastIndex(ref, evalRunRefSep)
	if i < 0 {
		return "", 0, false
	}
	n, err := strconv.ParseInt(ref[i+1:], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return ref[:i], n, true
}

// ObsEvalRunListRow is one run in the org Evals feed, aggregated from its
// per-item scores. Ref is the opaque (node, run_id) handle the detail/compare
// endpoints consume; the bare run_id is never exposed.
type ObsEvalRunListRow struct {
	Ref         string   `json:"ref"`
	UserEmail   string   `json:"user_email"` // the pushing operator (run author)
	RunName     string   `json:"run_name"`
	DatasetName string   `json:"dataset_name"`
	Items       int64    `json:"items"`      // distinct dataset items scored
	Scores      int64    `json:"scores"`     // total (item × scorer) score rows
	Passed      int64    `json:"passed"`     // score rows with passed=1
	PassRate    float64  `json:"pass_rate"`  // passed / scores (0 when no scores)
	MeanScore   float64  `json:"mean_score"` // mean over score rows
	StartedAt   string   `json:"started_at"` // MIN(ts)
	EndedAt     string   `json:"ended_at"`   // MAX(ts)
	Scorers     []string `json:"scorers"`
}

// ObsEvalRunsResult is the GET /api/org/obs/eval/runs body.
type ObsEvalRunsResult struct {
	WindowDays int                 `json:"window_days"`
	Configured bool                `json:"configured"`
	Runs       []ObsEvalRunListRow `json:"runs"`
}

// ObsEvalItemScoreRow is one per-item score on the detail view (content-free —
// the excerpts are the separate audited disclosure).
type ObsEvalItemScoreRow struct {
	ItemID     int64   `json:"item_id"`
	SpanID     string  `json:"span_id"`
	TraceID    string  `json:"trace_id"`
	Scorer     string  `json:"scorer"`
	Score      float64 `json:"score"`
	Passed     bool    `json:"passed"`
	DurationMs int64   `json:"duration_ms"`
	TS         string  `json:"ts"`
}

// ObsEvalRunDetailResult is the GET /api/org/obs/eval/run body: one run's
// summary + its per-item scores.
type ObsEvalRunDetailResult struct {
	WindowDays int                   `json:"window_days"`
	Run        ObsEvalRunListRow     `json:"run"`
	Scores     []ObsEvalItemScoreRow `json:"scores"`
}

// ObsEvalScorerDelta is one scorer's paired base-vs-compare aggregate.
type ObsEvalScorerDelta struct {
	Scorer          string  `json:"scorer"`
	BaseCount       int64   `json:"base_count"`
	CompareCount    int64   `json:"compare_count"`
	BaseMean        float64 `json:"base_mean"`
	CompareMean     float64 `json:"compare_mean"`
	MeanDelta       float64 `json:"mean_delta"`
	BasePassRate    float64 `json:"base_pass_rate"`
	ComparePassRate float64 `json:"compare_pass_rate"`
	PassRateDelta   float64 `json:"pass_rate_delta"`
}

// ObsEvalCompareResult is the GET /api/org/obs/eval/compare body: two run
// summaries + per-scorer deltas (compare − base).
type ObsEvalCompareResult struct {
	Base    ObsEvalRunListRow    `json:"base"`
	Compare ObsEvalRunListRow    `json:"compare"`
	Scorers []ObsEvalScorerDelta `json:"scorers"`
}

// ObsEvalItemContentRow is one item's content excerpts (a DEEPER, audited
// disclosure than the content-free scores). Excerpts arrive only from nodes
// that opted into full-content sharing; hash-only nodes contribute nothing.
type ObsEvalItemContentRow struct {
	ItemID          int64  `json:"item_id"`
	SpanID          string `json:"span_id"`
	Scorer          string `json:"scorer"`
	TS              string `json:"ts"`
	ContentHash     string `json:"content_hash"`
	InputExcerpt    string `json:"input_excerpt"`
	ExpectedExcerpt string `json:"expected_excerpt"`
	OutputExcerpt   string `json:"output_excerpt"`
	Rationale       string `json:"rationale"`
}

// ObsEvalItemContentResult is the GET /api/org/obs/eval/run/content body.
type ObsEvalItemContentResult struct {
	Items []ObsEvalItemContentRow `json:"items"`
}

// ObsEvalRuns lists the per-item eval runs shared in scope over the trailing
// window, aggregated from obs_eval_items (one row per run). configured is true
// when any run is visible in scope. Single-org-per-server convention.
func ObsEvalRuns(ctx context.Context, db *sql.DB, w Window, scope Scope, selfUserID string, now time.Time) (ObsEvalRunsResult, error) {
	res := ObsEvalRunsResult{WindowDays: w.days(), Runs: []ObsEvalRunListRow{}}
	uScope, uArgs := peopleScopeSQL("pushed_by_user_id", scope, selfUserID)
	if uScope == falseScope {
		return res, nil
	}
	sinceTS := since(w, now)
	//nolint:gosec // G201: uScope is a parameterized scope fragment; values bind via ?.
	q := `
SELECT pushed_by_user_id, run_id, MAX(run_name), MAX(dataset_name), MAX(user_email),
       COUNT(DISTINCT item_id), COUNT(*), COALESCE(SUM(passed),0), COALESCE(AVG(score),0),
       MIN(ts), MAX(ts), COALESCE(GROUP_CONCAT(DISTINCT scorer),'')
  FROM obs_eval_items
 WHERE ts >= ? AND ` + uScope + `
 GROUP BY pushed_by_user_id, run_id
 ORDER BY MAX(ts) DESC, run_id DESC`
	if err := eachRow(ctx, db, q, append([]any{sinceTS}, uArgs...), func(rows *sql.Rows) error {
		row, err := scanEvalRunSummary(rows)
		if err != nil {
			return err
		}
		res.Runs = append(res.Runs, row)
		return nil
	}); err != nil {
		return ObsEvalRunsResult{}, fmt.Errorf("rollup.ObsEvalRuns: %w", err)
	}
	res.Configured = len(res.Runs) > 0
	return res, nil
}

// ObsEvalRunDetail returns one run's summary + per-item scores, keyed by the
// opaque ref and re-scoped so a hand-crafted ref cannot escape the caller's
// scope. found=false when the ref is malformed or names no in-scope run.
func ObsEvalRunDetail(ctx context.Context, db *sql.DB, ref string, w Window, scope Scope, selfUserID string) (ObsEvalRunDetailResult, bool, error) {
	res := ObsEvalRunDetailResult{WindowDays: w.days(), Scores: []ObsEvalItemScoreRow{}}
	userID, runID, ok := decodeRunRef(ref)
	if !ok {
		return res, false, nil
	}
	uScope, uArgs := peopleScopeSQL("pushed_by_user_id", scope, selfUserID)
	if uScope == falseScope {
		return res, false, nil
	}
	summary, found, err := evalRunSummary(ctx, db, userID, runID, ref, uScope, uArgs)
	if err != nil {
		return ObsEvalRunDetailResult{}, false, fmt.Errorf("rollup.ObsEvalRunDetail: summary: %w", err)
	}
	if !found {
		return res, false, nil
	}
	res.Run = summary

	//nolint:gosec // G201: uScope is a parameterized scope fragment; values bind via ?.
	sq := `
SELECT item_id, span_id, trace_id, scorer, score, passed, duration_ms, ts
  FROM obs_eval_items
 WHERE pushed_by_user_id = ? AND run_id = ? AND ` + uScope + `
 ORDER BY item_id, scorer`
	args := append([]any{userID, runID}, uArgs...)
	if err := eachRow(ctx, db, sq, args, func(rows *sql.Rows) error {
		var s ObsEvalItemScoreRow
		var passed int64
		if err := rows.Scan(&s.ItemID, &s.SpanID, &s.TraceID, &s.Scorer, &s.Score, &passed, &s.DurationMs, &s.TS); err != nil {
			return err
		}
		s.Passed = passed != 0
		res.Scores = append(res.Scores, s)
		return nil
	}); err != nil {
		return ObsEvalRunDetailResult{}, false, fmt.Errorf("rollup.ObsEvalRunDetail: scores: %w", err)
	}
	return res, true, nil
}

// ObsEvalCompare returns two run summaries + the per-scorer paired deltas
// (compare − base). A scorer present in only one run still appears (the missing
// side reads zero count). Both refs are re-scoped independently.
func ObsEvalCompare(ctx context.Context, db *sql.DB, baseRef, compareRef string, scope Scope, selfUserID string) (ObsEvalCompareResult, bool, error) {
	res := ObsEvalCompareResult{Scorers: []ObsEvalScorerDelta{}}
	baseUser, baseRun, ok1 := decodeRunRef(baseRef)
	compUser, compRun, ok2 := decodeRunRef(compareRef)
	if !ok1 || !ok2 {
		return res, false, nil
	}
	uScope, uArgs := peopleScopeSQL("pushed_by_user_id", scope, selfUserID)
	if uScope == falseScope {
		return res, false, nil
	}
	baseSummary, foundB, err := evalRunSummary(ctx, db, baseUser, baseRun, baseRef, uScope, uArgs)
	if err != nil {
		return ObsEvalCompareResult{}, false, fmt.Errorf("rollup.ObsEvalCompare: base summary: %w", err)
	}
	compSummary, foundC, err := evalRunSummary(ctx, db, compUser, compRun, compareRef, uScope, uArgs)
	if err != nil {
		return ObsEvalCompareResult{}, false, fmt.Errorf("rollup.ObsEvalCompare: compare summary: %w", err)
	}
	if !foundB || !foundC {
		return res, false, nil
	}
	res.Base, res.Compare = baseSummary, compSummary

	baseByScorer, err := evalScorerAggregates(ctx, db, baseUser, baseRun, uScope, uArgs)
	if err != nil {
		return ObsEvalCompareResult{}, false, fmt.Errorf("rollup.ObsEvalCompare: base scorers: %w", err)
	}
	compByScorer, err := evalScorerAggregates(ctx, db, compUser, compRun, uScope, uArgs)
	if err != nil {
		return ObsEvalCompareResult{}, false, fmt.Errorf("rollup.ObsEvalCompare: compare scorers: %w", err)
	}

	// Union of scorer names, ordered, so a scorer added/removed between runs
	// still shows (with a zeroed side).
	seen := map[string]bool{}
	var scorers []string
	for name := range baseByScorer {
		if !seen[name] {
			seen[name] = true
			scorers = append(scorers, name)
		}
	}
	for name := range compByScorer {
		if !seen[name] {
			seen[name] = true
			scorers = append(scorers, name)
		}
	}
	sortStrings(scorers)
	for _, name := range scorers {
		b := baseByScorer[name]
		c := compByScorer[name]
		res.Scorers = append(res.Scorers, ObsEvalScorerDelta{
			Scorer:          name,
			BaseCount:       b.count,
			CompareCount:    c.count,
			BaseMean:        b.mean,
			CompareMean:     c.mean,
			MeanDelta:       c.mean - b.mean,
			BasePassRate:    b.passRate(),
			ComparePassRate: c.passRate(),
			PassRateDelta:   c.passRate() - b.passRate(),
		})
	}
	return res, true, nil
}

// ObsEvalItemContent returns one run's item content excerpts — a DEEPER,
// audited disclosure than the content-free scores. Rows with no excerpt (a
// hash-only node) are omitted. The handler MUST write a view_eval_item_content
// audit row BEFORE calling this. found=false on a malformed / out-of-scope ref.
func ObsEvalItemContent(ctx context.Context, db *sql.DB, ref string, scope Scope, selfUserID string) (ObsEvalItemContentResult, bool, error) {
	res := ObsEvalItemContentResult{Items: []ObsEvalItemContentRow{}}
	userID, runID, ok := decodeRunRef(ref)
	if !ok {
		return res, false, nil
	}
	uScope, uArgs := peopleScopeSQL("pushed_by_user_id", scope, selfUserID)
	if uScope == falseScope {
		return res, false, nil
	}
	//nolint:gosec // G201: uScope is a parameterized scope fragment; values bind via ?.
	q := `
SELECT item_id, span_id, scorer, ts, content_hash,
       COALESCE(input_excerpt,''), COALESCE(expected_excerpt,''),
       COALESCE(output_excerpt,''), COALESCE(rationale,'')
  FROM obs_eval_items
 WHERE pushed_by_user_id = ? AND run_id = ? AND ` + uScope + `
   AND (input_excerpt IS NOT NULL OR expected_excerpt IS NOT NULL
        OR output_excerpt IS NOT NULL OR rationale IS NOT NULL)
 ORDER BY item_id, scorer`
	args := append([]any{userID, runID}, uArgs...)
	if err := eachRow(ctx, db, q, args, func(rows *sql.Rows) error {
		var r ObsEvalItemContentRow
		if err := rows.Scan(&r.ItemID, &r.SpanID, &r.Scorer, &r.TS, &r.ContentHash,
			&r.InputExcerpt, &r.ExpectedExcerpt, &r.OutputExcerpt, &r.Rationale); err != nil {
			return err
		}
		res.Items = append(res.Items, r)
		return nil
	}); err != nil {
		return ObsEvalItemContentResult{}, false, fmt.Errorf("rollup.ObsEvalItemContent: %w", err)
	}
	return res, true, nil
}

// evalRunSummary aggregates one run's per-item scores into a summary row.
// found=false when the (user, run) pair has no in-scope rows.
func evalRunSummary(ctx context.Context, db *sql.DB, userID string, runID int64, ref, uScope string, uArgs []any) (ObsEvalRunListRow, bool, error) {
	//nolint:gosec // G201: uScope is a parameterized scope fragment; values bind via ?.
	q := `
SELECT MAX(run_name), MAX(dataset_name), MAX(user_email),
       COUNT(DISTINCT item_id), COUNT(*), COALESCE(SUM(passed),0), COALESCE(AVG(score),0),
       COALESCE(MIN(ts),''), COALESCE(MAX(ts),''), COALESCE(GROUP_CONCAT(DISTINCT scorer),''),
       COUNT(*)
  FROM obs_eval_items
 WHERE pushed_by_user_id = ? AND run_id = ? AND ` + uScope
	args := append([]any{userID, runID}, uArgs...)
	var (
		row   ObsEvalRunListRow
		total int64
	)
	var runName, datasetName, userEmail, startedAt, endedAt, scorers sql.NullString
	err := db.QueryRowContext(ctx, q, args...).Scan(
		&runName, &datasetName, &userEmail,
		&row.Items, &row.Scores, &row.Passed, &row.MeanScore,
		&startedAt, &endedAt, &scorers, &total,
	)
	if err != nil {
		return ObsEvalRunListRow{}, false, err
	}
	if total == 0 {
		return ObsEvalRunListRow{}, false, nil
	}
	row.Ref = ref
	row.RunName = runName.String
	row.DatasetName = datasetName.String
	row.UserEmail = userEmail.String
	row.StartedAt = startedAt.String
	row.EndedAt = endedAt.String
	row.Scorers = splitScorers(scorers.String)
	if row.Scores > 0 {
		row.PassRate = float64(row.Passed) / float64(row.Scores)
	}
	return row, true, nil
}

// evalScorerAgg is one scorer's aggregate within a run.
type evalScorerAgg struct {
	count  int64
	passed int64
	mean   float64
}

func (a evalScorerAgg) passRate() float64 {
	if a.count == 0 {
		return 0
	}
	return float64(a.passed) / float64(a.count)
}

// evalScorerAggregates returns per-scorer aggregates for one run.
func evalScorerAggregates(ctx context.Context, db *sql.DB, userID string, runID int64, uScope string, uArgs []any) (map[string]evalScorerAgg, error) {
	out := map[string]evalScorerAgg{}
	//nolint:gosec // G201: uScope is a parameterized scope fragment; values bind via ?.
	q := `
SELECT scorer, COUNT(*), COALESCE(SUM(passed),0), COALESCE(AVG(score),0)
  FROM obs_eval_items
 WHERE pushed_by_user_id = ? AND run_id = ? AND ` + uScope + `
 GROUP BY scorer`
	args := append([]any{userID, runID}, uArgs...)
	if err := eachRow(ctx, db, q, args, func(rows *sql.Rows) error {
		var name string
		var a evalScorerAgg
		if err := rows.Scan(&name, &a.count, &a.passed, &a.mean); err != nil {
			return err
		}
		out[name] = a
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// scanEvalRunSummary maps a runs-list aggregate row (with the leading
// pushed_by_user_id + run_id used to build the ref) to an ObsEvalRunListRow.
func scanEvalRunSummary(rows *sql.Rows) (ObsEvalRunListRow, error) {
	var (
		row       ObsEvalRunListRow
		userID    string
		runID     int64
		runName   sql.NullString
		datasetNm sql.NullString
		userEmail sql.NullString
		startedAt sql.NullString
		endedAt   sql.NullString
		scorers   sql.NullString
	)
	if err := rows.Scan(&userID, &runID, &runName, &datasetNm, &userEmail,
		&row.Items, &row.Scores, &row.Passed, &row.MeanScore,
		&startedAt, &endedAt, &scorers); err != nil {
		return ObsEvalRunListRow{}, err
	}
	row.Ref = encodeRunRef(userID, runID)
	row.RunName = runName.String
	row.DatasetName = datasetNm.String
	row.UserEmail = userEmail.String
	row.StartedAt = startedAt.String
	row.EndedAt = endedAt.String
	row.Scorers = splitScorers(scorers.String)
	if row.Scores > 0 {
		row.PassRate = float64(row.Passed) / float64(row.Scores)
	}
	return row, nil
}

// splitScorers splits a GROUP_CONCAT(DISTINCT scorer) comma list into a sorted
// slice, dropping empties. GROUP_CONCAT order is unspecified, so it is sorted
// for a deterministic surface. Returns a non-nil empty slice for a blank input.
func splitScorers(s string) []string {
	if s == "" {
		return []string{}
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	sortStrings(out)
	return out
}

// sortStrings sorts a string slice in place (ascending). Small helper to avoid
// pulling sort into the hot union path above with a closure.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
