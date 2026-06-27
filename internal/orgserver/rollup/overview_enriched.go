package rollup

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// enrichOverview populates the Phase 1 additive fields of an OverviewResult:
// token buckets, cache efficiency, reliability/source split, error rate,
// proxy-only latency (degrading to nil), tool/model mix, activity grids, and
// prior-period deltas. It is content-free — every query aggregates the same
// non-content columns the v1 rollup already reads (token counts, costs,
// http_status, error_class, durations, success flags, tool/model labels).
//
// It is called at the tail of Overview with the totals already computed, so it
// can derive deltas by subtraction (spend-since-prior − spend-since-now)
// instead of a second windowed range scan.
func enrichOverview(ctx context.Context, db *sql.DB, w Window, scope Scope, now time.Time, res *OverviewResult) error {
	s := since(w, now)
	uScope, uArgs := userScopeSQL("user_id", scope)

	if err := enrichTokensCacheReliability(ctx, db, s, uScope, uArgs, res); err != nil {
		return fmt.Errorf("rollup.enrichOverview: tokens/cache/reliability: %w", err)
	}
	if err := enrichToolMix(ctx, db, s, uScope, uArgs, res); err != nil {
		return fmt.Errorf("rollup.enrichOverview: tool mix: %w", err)
	}
	if err := enrichModelMix(ctx, db, s, uScope, uArgs, res); err != nil {
		return fmt.Errorf("rollup.enrichOverview: model mix: %w", err)
	}
	if err := enrichActivity(ctx, db, s, uScope, uArgs, res); err != nil {
		return fmt.Errorf("rollup.enrichOverview: activity: %w", err)
	}
	if err := enrichErrors(ctx, db, s, uScope, uArgs, res); err != nil {
		return fmt.Errorf("rollup.enrichOverview: errors: %w", err)
	}
	if err := enrichLatency(ctx, db, s, uScope, uArgs, res); err != nil {
		return fmt.Errorf("rollup.enrichOverview: latency: %w", err)
	}
	if err := enrichDeltas(ctx, db, w, scope, now, uScope, uArgs, res); err != nil {
		return fmt.Errorf("rollup.enrichOverview: deltas: %w", err)
	}
	return nil
}

// enrichedCTE is the proxy-deduplicated UNION used by the enriched aggregates.
// Like spendCTE it drops token_usage rows whose source_event_id is already an
// api_turns.request_id in the window (the same turn seen twice). It carries the
// extra dimensions the v1 spendCTE omits: tier (proxy|estimated), tool, the
// four token buckets, and reasoning. api_turns has no tool column, so the tool
// dimension is resolved via the session LEFT JOIN; token_usage carries its own
// tool. Reasoning is 0 on the proxy branch (Anthropic folds it into output).
// Binds three `since` args (proxy_ids, api_turns, token_usage) — spendArgs(s).
const enrichedCTE = `
WITH proxy_ids AS (
    SELECT request_id FROM api_turns
     WHERE request_id IS NOT NULL AND request_id != '' AND timestamp >= ?
),
ev AS (
    SELECT t.user_id                              AS user_id,
           'proxy'                                AS tier,
           COALESCE(s.tool,'')                    AS tool,
           COALESCE(t.model,'')                   AS model,
           COALESCE(t.input_tokens,0)             AS input_tokens,
           COALESCE(t.output_tokens,0)            AS output_tokens,
           COALESCE(t.cache_read_tokens,0)        AS cache_read,
           COALESCE(t.cache_creation_tokens,0)    AS cache_write,
           0                                      AS reasoning,
           COALESCE(t.cost_usd,0)                 AS cost,
           t.timestamp                            AS ts
      FROM api_turns t
      LEFT JOIN sessions s ON s.id = t.session_id AND s.user_id = t.user_id
     WHERE t.timestamp >= ?
    UNION ALL
    SELECT tu.user_id                             AS user_id,
           'estimated'                            AS tier,
           COALESCE(tu.tool,'')                   AS tool,
           COALESCE(tu.model,'')                  AS model,
           COALESCE(tu.input_tokens,0)            AS input_tokens,
           COALESCE(tu.output_tokens,0)           AS output_tokens,
           COALESCE(tu.cache_read_tokens,0)       AS cache_read,
           COALESCE(tu.cache_creation_tokens,0)   AS cache_write,
           COALESCE(tu.reasoning_tokens,0)        AS reasoning,
           COALESCE(tu.estimated_cost_usd,0)      AS cost,
           tu.timestamp                           AS ts
      FROM token_usage tu
     WHERE tu.timestamp >= ?
       AND (tu.source_event_id IS NULL OR tu.source_event_id = ''
            OR tu.source_event_id NOT IN (SELECT request_id FROM proxy_ids))
)`

// enrichTokensCacheReliability computes the token buckets, cache efficiency,
// and the proxy-vs-estimated cost split in a single scan over the deduped CTE.
func enrichTokensCacheReliability(ctx context.Context, db *sql.DB, s, uScope string, uArgs []any, res *OverviewResult) error {
	//nolint:gosec // G202: enrichedCTE is a code constant and uScope is a parameterized scope fragment; all values bind via ? args.
	q := enrichedCTE + `
SELECT COALESCE(SUM(input_tokens),0),
       COALESCE(SUM(cache_read),0),
       COALESCE(SUM(cache_write),0),
       COALESCE(SUM(output_tokens),0),
       COALESCE(SUM(reasoning),0),
       COALESCE(SUM(CASE WHEN tier='proxy'     THEN cost ELSE 0 END),0),
       COALESCE(SUM(CASE WHEN tier='estimated' THEN cost ELSE 0 END),0)
  FROM ev WHERE ` + uScope
	var (
		input, read, write, output, reasoning int64
		proxyCost, estCost                    float64
	)
	if err := db.QueryRowContext(ctx, q, append(spendArgs(s), uArgs...)...).
		Scan(&input, &read, &write, &output, &reasoning, &proxyCost, &estCost); err != nil {
		return err
	}

	res.Tokens = &TokenBuckets{
		NetInput:   input,
		CacheRead:  read,
		CacheWrite: write,
		Output:     output,
		Reasoning:  reasoning,
	}
	res.Cache = &CacheStats{
		ReadTokens:     read,
		WriteTokens:    write,
		InputTokens:    input,
		HitRatio:       ratio(read, input+read),
		ReadWriteRatio: ratio(read, write),
	}
	res.Reliability = &ReliabilitySplit{
		ProxyCostUSD:     proxyCost,
		EstimatedCostUSD: estCost,
		ProxyShare:       fratio(proxyCost, proxyCost+estCost),
	}
	return nil
}

// enrichToolMix groups deduped spend by tool (top-N + folded "other").
func enrichToolMix(ctx context.Context, db *sql.DB, s, uScope string, uArgs []any, res *OverviewResult) error {
	//nolint:gosec // G202: code-constant CTE + parameterized scope; values bind via ? args.
	q := enrichedCTE + `
SELECT tool, COALESCE(SUM(cost),0), COALESCE(SUM(input_tokens+output_tokens),0)
  FROM ev WHERE tool != '' AND ` + uScope + `
 GROUP BY tool
 ORDER BY 2 DESC, tool`
	rows, err := db.QueryContext(ctx, q, append(spendArgs(s), uArgs...)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := []ToolSlice{}
	for rows.Next() {
		var t ToolSlice
		if err := rows.Scan(&t.Tool, &t.CostUSD, &t.Tokens); err != nil {
			return err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	res.ToolMix = foldToolOther(out)
	return nil
}

// enrichModelMix groups deduped spend by model (top-N + folded "other").
func enrichModelMix(ctx context.Context, db *sql.DB, s, uScope string, uArgs []any, res *OverviewResult) error {
	//nolint:gosec // G202: code-constant CTE + parameterized scope; values bind via ? args.
	q := enrichedCTE + `
SELECT model, COALESCE(SUM(cost),0), COALESCE(SUM(input_tokens+output_tokens),0)
  FROM ev WHERE model != '' AND ` + uScope + `
 GROUP BY model
 ORDER BY 2 DESC, model`
	rows, err := db.QueryContext(ctx, q, append(spendArgs(s), uArgs...)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := []ModelSlice{}
	for rows.Next() {
		var m ModelSlice
		if err := rows.Scan(&m.Model, &m.CostUSD, &m.Tokens); err != nil {
			return err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	res.ModelMix = foldModelOther(out)
	return nil
}

// enrichActivity fills the actions-by-day series and the hour-of-day histogram
// from the watcher-fed actions table (available regardless of proxy capture).
func enrichActivity(ctx context.Context, db *sql.DB, s, uScope string, uArgs []any, res *OverviewResult) error {
	//nolint:gosec // G201: actions/timestamp are code-constant identifiers; uScope is parameterized; window binds via ?.
	dayQ := `SELECT substr(timestamp,1,10) AS d, COUNT(*) FROM actions
	          WHERE timestamp >= ? AND ` + uScope + ` GROUP BY d ORDER BY d`
	dayRows, err := db.QueryContext(ctx, dayQ, append([]any{s}, uArgs...)...)
	if err != nil {
		return err
	}
	defer dayRows.Close()
	days := []DayCount{}
	for dayRows.Next() {
		var d DayCount
		if err := dayRows.Scan(&d.Date, &d.Count); err != nil {
			return err
		}
		days = append(days, d)
	}
	if err := dayRows.Err(); err != nil {
		return err
	}
	res.ActionsByDay = days

	//nolint:gosec // G201: code-constant identifiers; uScope parameterized; window binds via ?.
	hourQ := `SELECT CAST(substr(timestamp,12,2) AS INTEGER) AS h, COUNT(*) FROM actions
	           WHERE timestamp >= ? AND ` + uScope + ` GROUP BY h ORDER BY h`
	hourRows, err := db.QueryContext(ctx, hourQ, append([]any{s}, uArgs...)...)
	if err != nil {
		return err
	}
	defer hourRows.Close()
	hours := []HourCount{}
	for hourRows.Next() {
		var h HourCount
		if err := hourRows.Scan(&h.Hour, &h.Count); err != nil {
			return err
		}
		hours = append(hours, h)
	}
	if err := hourRows.Err(); err != nil {
		return err
	}
	res.HourOfDay = hours
	return nil
}

// enrichErrors fills the action error rate (always) plus the proxy-only HTTP
// error view (zero/empty without proxy capture). ErrorStats is always present.
func enrichErrors(ctx context.Context, db *sql.DB, s, uScope string, uArgs []any, res *OverviewResult) error {
	es := &ErrorStats{TotalActions: res.TotalActions, APITurns: res.TotalAPITurns}

	//nolint:gosec // G201: code-constant identifiers; uScope parameterized; window binds via ?.
	failQ := `SELECT COUNT(*) FROM actions WHERE timestamp >= ? AND success = 0 AND ` + uScope
	if err := db.QueryRowContext(ctx, failQ, append([]any{s}, uArgs...)...).Scan(&es.FailedActions); err != nil {
		return err
	}
	es.ActionErrorRate = fratio(float64(es.FailedActions), float64(es.TotalActions))

	//nolint:gosec // G201: code-constant identifiers; uScope parameterized; window binds via ?.
	httpQ := `SELECT COUNT(*) FROM api_turns WHERE timestamp >= ? AND http_status >= 400 AND ` + uScope
	if err := db.QueryRowContext(ctx, httpQ, append([]any{s}, uArgs...)...).Scan(&es.HTTPErrors); err != nil {
		return err
	}

	//nolint:gosec // G201: code-constant identifiers; uScope parameterized; window binds via ?.
	clsQ := `SELECT error_class, COUNT(*) FROM api_turns
	          WHERE timestamp >= ? AND error_class IS NOT NULL AND error_class != '' AND ` + uScope + `
	          GROUP BY error_class ORDER BY 2 DESC, error_class LIMIT ?`
	args := append([]any{s}, uArgs...)
	args = append(args, topN)
	rows, err := db.QueryContext(ctx, clsQ, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var kc KeyCount
		if err := rows.Scan(&kc.Key, &kc.Count); err != nil {
			return err
		}
		es.ByErrorClass = append(es.ByErrorClass, kc)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	res.Errors = es
	return nil
}

// enrichLatency fills the proxy-only latency view. The whole struct stays nil
// when no api_turns carry timing (an honest "needs proxy capture" empty state).
func enrichLatency(ctx context.Context, db *sql.DB, s, uScope string, uArgs []any, res *OverviewResult) error {
	medTTFT, ttftN, err := apiTurnPercentile(ctx, db, "time_to_first_token_ms", 0.5, s, uScope, uArgs)
	if err != nil {
		return err
	}
	medTotal, totalN, err := apiTurnPercentile(ctx, db, "total_response_ms", 0.5, s, uScope, uArgs)
	if err != nil {
		return err
	}
	p95Total, _, err := apiTurnPercentile(ctx, db, "total_response_ms", 0.95, s, uScope, uArgs)
	if err != nil {
		return err
	}
	if ttftN == 0 && totalN == 0 {
		res.Latency = nil // honest empty — no proxy timing captured.
		return nil
	}
	sample := totalN
	if sample == 0 {
		sample = ttftN
	}
	res.Latency = &LatencyStats{
		SampleSize:    sample,
		MedianTTFTMs:  medTTFT,
		MedianTotalMs: medTotal,
		P95TotalMs:    p95Total,
	}
	return nil
}

// apiTurnPercentile returns the value of a positive, non-null api_turns timing
// column at fractional rank f (0..1, nearest-rank) within the window+scope,
// plus the sample size. It counts first, then seeks via ORDER BY … OFFSET, so
// it never loads the full timing array into memory.
func apiTurnPercentile(ctx context.Context, db *sql.DB, col string, f float64, s, uScope string, uArgs []any) (val, n int64, err error) {
	//nolint:gosec // G201: col is one of two code-constant identifiers; uScope parameterized; window binds via ?.
	cq := fmt.Sprintf(`SELECT COUNT(*) FROM api_turns WHERE timestamp >= ? AND %s IS NOT NULL AND %s > 0 AND %s`, col, col, uScope)
	if err = db.QueryRowContext(ctx, cq, append([]any{s}, uArgs...)...).Scan(&n); err != nil {
		return 0, 0, err
	}
	if n == 0 {
		return 0, 0, nil
	}
	off := int64(f*float64(n-1) + 0.5)
	if off < 0 {
		off = 0
	}
	if off > n-1 {
		off = n - 1
	}
	//nolint:gosec // G201: col is a code-constant identifier; uScope parameterized; values bind via ?.
	vq := fmt.Sprintf(`SELECT %s FROM api_turns WHERE timestamp >= ? AND %s IS NOT NULL AND %s > 0 AND %s ORDER BY %s ASC LIMIT 1 OFFSET ?`, col, col, col, uScope, col)
	args := append([]any{s}, uArgs...)
	args = append(args, off)
	if err = db.QueryRowContext(ctx, vq, args...).Scan(&val); err != nil {
		return 0, 0, err
	}
	return val, n, nil
}

// enrichDeltas compares the current window to the immediately preceding one by
// subtraction: prior = spend-since-2w − spend-since-1w. Reuses the v1 totals
// already on res, so the only extra scans are the since-2w aggregates.
func enrichDeltas(ctx context.Context, db *sql.DB, w Window, scope Scope, now time.Time, uScope string, uArgs []any, res *OverviewResult) error {
	priorStart := since(Window{Days: 2 * w.days()}, now)

	// Spend since the prior window start (covers both windows).
	//nolint:gosec // G202: code-constant CTE + parameterized scope; values bind via ? args.
	spendQ := spendCTE + `SELECT COALESCE(SUM(cost),0) FROM spend WHERE ` + uScope
	var spendSincePrior float64
	if err := db.QueryRowContext(ctx, spendQ, append(spendArgs(priorStart), uArgs...)...).Scan(&spendSincePrior); err != nil {
		return err
	}
	sessSincePrior, err := scopedCount(ctx, db, "sessions", "started_at", priorStart, uScope, uArgs)
	if err != nil {
		return err
	}
	actSincePrior, err := scopedCount(ctx, db, "actions", "timestamp", priorStart, uScope, uArgs)
	if err != nil {
		return err
	}

	priorCost := spendSincePrior - res.TotalCostUSD
	priorSessions := sessSincePrior - res.TotalSessions
	priorActions := actSincePrior - res.TotalActions
	// Clamp tiny negative residue from float subtraction.
	if priorCost < 0 {
		priorCost = 0
	}
	if priorSessions < 0 {
		priorSessions = 0
	}
	if priorActions < 0 {
		priorActions = 0
	}

	hasPrior := priorCost > 0 || priorSessions > 0 || priorActions > 0
	res.Deltas = &PeriodDeltas{
		CostUSD:       signedDelta(res.TotalCostUSD, priorCost),
		Sessions:      signedDelta(float64(res.TotalSessions), float64(priorSessions)),
		Actions:       signedDelta(float64(res.TotalActions), float64(priorActions)),
		PriorCostUSD:  priorCost,
		PriorSessions: priorSessions,
		PriorActions:  priorActions,
		HasPrior:      hasPrior,
	}
	return nil
}

// --- small helpers ----------------------------------------------------------

// ratio is read÷denom as a float, 0 when denom is 0.
func ratio(num, denom int64) float64 {
	if denom <= 0 {
		return 0
	}
	return float64(num) / float64(denom)
}

// fratio is num÷denom for floats, 0 when denom is 0.
func fratio(num, denom float64) float64 {
	if denom <= 0 {
		return 0
	}
	return num / denom
}

// signedDelta is the signed fraction (cur-prior)/prior, 0 when prior is 0.
func signedDelta(cur, prior float64) float64 {
	if prior <= 0 {
		return 0
	}
	return (cur - prior) / prior
}

// foldToolOther keeps the top (topN-1) tool slices and folds the remainder
// into a single synthetic "other" row, preserving total cost + tokens.
func foldToolOther(in []ToolSlice) []ToolSlice {
	if len(in) <= topN {
		return in
	}
	sort.SliceStable(in, func(i, j int) bool { return in[i].Tokens > in[j].Tokens })
	head := in[:topN-1]
	other := ToolSlice{Tool: "other"}
	for _, t := range in[topN-1:] {
		other.CostUSD += t.CostUSD
		other.Tokens += t.Tokens
	}
	return append(append([]ToolSlice{}, head...), other)
}

// foldModelOther mirrors foldToolOther for model slices.
func foldModelOther(in []ModelSlice) []ModelSlice {
	if len(in) <= topN {
		return in
	}
	sort.SliceStable(in, func(i, j int) bool { return in[i].Tokens > in[j].Tokens })
	head := in[:topN-1]
	other := ModelSlice{Model: "other"}
	for _, m := range in[topN-1:] {
		other.CostUSD += m.CostUSD
		other.Tokens += m.Tokens
	}
	return append(append([]ModelSlice{}, head...), other)
}
