package dashboard

import (
	"net/http"
	"sort"
	"time"

	"github.com/marmutapp/superbased-observer/internal/intelligence/cost"
	"github.com/marmutapp/superbased-observer/internal/intelligence/discover"
)

// handleAnalysisHeadline serves /api/analysis/headline?days=N — six KPI
// values for the Analysis tab's headline band:
//
//  1. period.cost_usd / prior_cost_usd / delta_pct — current period vs
//     the prior period of equal length.
//  2. month.to_date_usd / projection_usd / budget_usd / budget_pct —
//     calendar-month-to-date spend, linearly extrapolated to month-end,
//     with optional progress vs the user-set MonthlyBudgetUSD.
//  3. effective.tokens / rate_per_million — period $ divided by
//     "effective" tokens (output + new cache writes — the user's true
//     bill rate per useful token).
//  4. cache.read_tokens / write_tokens / efficacy — cache_read share of
//     (cache_read + cache_creation). High = good; trending down = waste.
//  5. long_context.turns / surcharge_usd — how many turns crossed an LC
//     threshold and the extra $ that LC repricing added vs standard rates.
//  6. waste.usd / tokens — Discovery stale-read tokens valued at the
//     user's blended input rate.
//
// All numbers come from one per-turn-deduped scan of api_turns +
// token_usage over the [max(month_start, now-2*days), now] window.
// LC surcharge and per-row pricing are computed in Go because the cost
// engine's LongContext dispatch is per-turn — aggregating tokens at SQL
// then pricing once would false-trip the LC threshold whenever a
// session's summed prompt cleared it (mirroring the dashboard.go
// session-detail per-model fix).
func (s *Server) handleAnalysisHeadline(w http.ResponseWriter, r *http.Request) {
	days := intArg(r, "days", 30, 1, 36500)

	now := time.Now().UTC()
	periodStart := now.Add(-time.Duration(days) * 24 * time.Hour)
	priorStart := now.Add(-2 * time.Duration(days) * 24 * time.Hour)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	// Pull rows from whichever boundary is earliest so we can compute
	// MTD and the period+prior windows from a single scan.
	queryStart := priorStart
	if monthStart.Before(queryStart) {
		queryStart = monthStart
	}
	tsArg := queryStart.Format(time.RFC3339Nano)

	const q = `WITH proxy_turn_ids AS (
		SELECT request_id FROM api_turns
		 WHERE request_id IS NOT NULL AND request_id != ''
		   AND timestamp >= ?
	),
	combined AS (
		SELECT model, input_tokens, output_tokens, cache_read_tokens,
		       cache_creation_tokens, cache_creation_1h_tokens, timestamp,
		       cost_usd
		FROM api_turns WHERE timestamp >= ?
		UNION ALL
		SELECT model, input_tokens, output_tokens, cache_read_tokens,
		       cache_creation_tokens, cache_creation_1h_tokens, timestamp,
		       estimated_cost_usd
		FROM token_usage
		WHERE timestamp >= ?
		  AND (source_event_id IS NULL OR source_event_id = ''
		       OR source_event_id NOT IN (SELECT request_id FROM proxy_turn_ids))
	)
	SELECT COALESCE(model, ''),
	       COALESCE(input_tokens, 0), COALESCE(output_tokens, 0),
	       COALESCE(cache_read_tokens, 0), COALESCE(cache_creation_tokens, 0),
	       COALESCE(cache_creation_1h_tokens, 0),
	       timestamp,
	       COALESCE(cost_usd, 0)
	FROM combined`

	rows, err := s.opts.DB.QueryContext(r.Context(), q, tsArg, tsArg, tsArg)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()

	var (
		periodCost, priorCost, mtdCost                  float64
		periodOutput, periodCacheRead, periodCacheWrite int64
		lcTurns                                         int64
		lcSurcharge                                     float64
	)

	for rows.Next() {
		var (
			model    string
			bundle   cost.TokenBundle
			tsStr    string
			recorded float64
		)
		if err := rows.Scan(&model,
			&bundle.Input, &bundle.Output,
			&bundle.CacheRead, &bundle.CacheCreation, &bundle.CacheCreation1h,
			&tsStr, &recorded); err != nil {
			writeErr(w, err)
			return
		}
		ts, _ := time.Parse(time.RFC3339Nano, tsStr)

		// Per-turn cost (LC-aware) + standard-rate cost (LC stripped).
		// Difference = the LC tier surcharge attributable to this turn.
		var rowCost, rowStdCost float64
		if recorded > 0 {
			// Recorded cost from upstream client — trust as-is. We can't
			// decompose into LC-vs-standard, so it doesn't contribute to
			// the LC surcharge attribution.
			rowCost = recorded
			rowStdCost = recorded
		} else if p, ok := s.opts.CostEngine.Lookup(model); ok {
			rowCost = cost.Compute(p, bundle)
			rowStdCost = cost.Compute(stripLongContext(p), bundle)
		}

		if !ts.Before(periodStart) && !ts.After(now) {
			periodCost += rowCost
			periodOutput += bundle.Output
			periodCacheRead += bundle.CacheRead
			periodCacheWrite += bundle.CacheCreation
			if rowCost > rowStdCost {
				lcTurns++
				lcSurcharge += rowCost - rowStdCost
			}
		} else if !ts.Before(priorStart) && ts.Before(periodStart) {
			priorCost += rowCost
		}
		if !ts.Before(monthStart) && !ts.After(now) {
			mtdCost += rowCost
		}
	}
	if err := rows.Err(); err != nil {
		writeErr(w, err)
		return
	}

	// Linear projection to month-end: scale MTD by (days_in_month /
	// days_elapsed). days_elapsed is at least 1 to avoid div-by-zero
	// on the first calendar day of the month.
	daysElapsed := int(now.Sub(monthStart).Hours()/24) + 1
	if daysElapsed < 1 {
		daysElapsed = 1
	}
	daysInMonth := time.Date(now.Year(), now.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()
	projection := 0.0
	if daysElapsed > 0 {
		projection = mtdCost * float64(daysInMonth) / float64(daysElapsed)
	}
	var budgetPct float64
	if s.opts.MonthlyBudgetUSD > 0 {
		budgetPct = mtdCost / s.opts.MonthlyBudgetUSD * 100
	}

	// Cache efficacy = read / (read + write). High share of cache_read
	// vs cache_creation means the user is reusing cached prompts; low
	// share means they're paying to cache then not reusing.
	var cacheEfficacy float64
	if total := periodCacheRead + periodCacheWrite; total > 0 {
		cacheEfficacy = float64(periodCacheRead) / float64(total)
	}

	// Effective tokens = output + new cache writes. The "value" tokens
	// the user actually gets per dollar (input is the prompt overhead).
	effectiveTokens := periodOutput + periodCacheWrite
	var effRate float64
	if effectiveTokens > 0 {
		effRate = periodCost * 1_000_000 / float64(effectiveTokens)
	}

	// Period delta. When the prior period was zero (cold start) we
	// surface a sentinel rather than dividing — the JS formats it
	// distinctly so the user sees "first period" instead of "+∞%".
	var deltaPct float64
	priorIsZero := priorCost == 0
	if !priorIsZero {
		deltaPct = (periodCost - priorCost) / priorCost * 100
	}

	// Waste $: stale-read tokens (Discovery) × user's blended input
	// rate. Repeat-command waste isn't surfaced here yet — Discovery
	// only counts groups, not tokens.
	wasteTokens := int64(0)
	wasteUSD := 0.0
	blendedRate, brErr := s.opts.CostEngine.BlendedInputRate(r.Context(), s.opts.DB, 30)
	if brErr != nil {
		s.opts.Logger.Warn("analysis: blended input rate", "err", brErr)
		blendedRate = cost.DefaultBlendedInputRate
	}
	report, dErr := discover.New(s.opts.DB).Run(r.Context(), discover.Options{Days: days, Limit: 500})
	if dErr != nil {
		s.opts.Logger.Warn("analysis: discover", "err", dErr)
	} else {
		wasteTokens = report.Summary.EstWastedTokens
		wasteUSD = float64(wasteTokens) * blendedRate / 1_000_000
	}

	writeJSON(w, map[string]any{
		"days": days,
		"period": map[string]any{
			"cost_usd":       periodCost,
			"prior_cost_usd": priorCost,
			"prior_is_zero":  priorIsZero,
			"delta_pct":      deltaPct,
			"period_start":   periodStart.Format(time.RFC3339),
			"prior_start":    priorStart.Format(time.RFC3339),
		},
		"month": map[string]any{
			"to_date_usd":    mtdCost,
			"projection_usd": projection,
			"budget_usd":     s.opts.MonthlyBudgetUSD,
			"budget_pct":     budgetPct,
			"days_elapsed":   daysElapsed,
			"days_in_month":  daysInMonth,
		},
		"effective": map[string]any{
			"tokens":           effectiveTokens,
			"rate_per_million": effRate,
		},
		"cache": map[string]any{
			"read_tokens":  periodCacheRead,
			"write_tokens": periodCacheWrite,
			"efficacy":     cacheEfficacy,
		},
		"long_context": map[string]any{
			"turns":         lcTurns,
			"surcharge_usd": lcSurcharge,
		},
		"waste": map[string]any{
			"usd":                      wasteUSD,
			"tokens":                   wasteTokens,
			"blended_rate_per_million": blendedRate,
			"source_note":              "stale-read tokens × blended input rate",
		},
	})
}

// handleAnalysisTrend serves /api/analysis/trend?dim=model|project|tool
// &days=N — one point per (day, dim_value) cross-section so the
// Analysis tab can render a daily-stacked-bar drilldown across any of
// three dimensions. Same dedup + reliability as /api/cost (cost engine
// SourceAuto: proxy preferred, JSONL fallback).
//
// dim=model is functionally equivalent to /api/timeseries/tokens-by-model
// — duplicated under the analysis namespace so the JS only needs one
// fetch path regardless of dimension. dim=project and dim=tool are
// new groupings powered by GroupByDayProject / GroupByDayTool added
// to the cost engine alongside this endpoint.
func (s *Server) handleAnalysisTrend(w http.ResponseWriter, r *http.Request) {
	days := intArg(r, "days", 30, 1, 36500)
	projectFilter := r.URL.Query().Get("project")
	dim := r.URL.Query().Get("dim")
	if dim == "" {
		dim = "model"
	}
	var groupBy cost.GroupBy
	switch dim {
	case "model":
		groupBy = cost.GroupByDayModel
	case "project":
		groupBy = cost.GroupByDayProject
	case "tool":
		groupBy = cost.GroupByDayTool
	default:
		http.Error(w, "dim must be one of: model, project, tool", http.StatusBadRequest)
		return
	}

	summary, err := s.opts.CostEngine.Summary(r.Context(), s.opts.DB, cost.Options{
		Days:        days,
		GroupBy:     groupBy,
		Source:      cost.SourceAuto,
		ProjectRoot: projectFilter,
		// Generous limit: 365d × ~10 keys/day = 3650; round up to be
		// safe on accounts with many models/projects/tools.
		Limit: 5000,
	})
	if err != nil {
		writeErr(w, err)
		return
	}

	type point struct {
		Bucket      string  `json:"bucket"`
		Key         string  `json:"key"`
		TotalTokens int64   `json:"total_tokens"`
		CostUSD     float64 `json:"cost_usd"`
		TurnCount   int     `json:"turn_count"`
	}
	series := make([]point, 0, len(summary.Rows))
	for _, row := range summary.Rows {
		var day, key string
		switch groupBy {
		case cost.GroupByDayModel:
			day, key = cost.SplitDayModelKey(row.Key)
		case cost.GroupByDayProject:
			day, key = cost.SplitDayProjectKey(row.Key)
		case cost.GroupByDayTool:
			day, key = cost.SplitDayToolKey(row.Key)
		}
		series = append(series, point{
			Bucket:      day,
			Key:         key,
			TotalTokens: row.Tokens.Input + row.Tokens.Output + row.Tokens.CacheRead + row.Tokens.CacheCreation,
			CostUSD:     row.CostUSD,
			TurnCount:   row.TurnCount,
		})
	}
	// Cost engine returns rows sorted by cost DESC. Re-sort
	// chronologically (then by key for a stable stacking order within
	// a day) so the chart axis reads left-to-right.
	sort.SliceStable(series, func(i, j int) bool {
		if series[i].Bucket != series[j].Bucket {
			return series[i].Bucket < series[j].Bucket
		}
		return series[i].Key < series[j].Key
	})
	writeJSON(w, map[string]any{
		"metric": "trend",
		"dim":    dim,
		"bucket": "day",
		"days":   days,
		"series": series,
	})
}

// handleAnalysisMovers serves /api/analysis/movers?dim=model|project|tool
// &days=N — period-over-period diff for the chosen dimension. Returns
// top 5 cost increases, top 5 decreases, and "new entrants" (keys
// present in the current period but not the prior). Used by the
// Analysis tab's "What changed" band.
//
// Drives the same dim toggle as /api/analysis/trend so a user clicking
// "Project" on the trend chart automatically rerolls the movers table
// onto the same dimension.
func (s *Server) handleAnalysisMovers(w http.ResponseWriter, r *http.Request) {
	days := intArg(r, "days", 30, 1, 36500)
	projectFilter := r.URL.Query().Get("project")
	dim := r.URL.Query().Get("dim")
	if dim == "" {
		dim = "model"
	}
	var groupBy cost.GroupBy
	switch dim {
	case "model":
		groupBy = cost.GroupByModel
	case "project":
		groupBy = cost.GroupByProject
	case "tool":
		groupBy = cost.GroupByTool
	default:
		http.Error(w, "dim must be one of: model, project, tool", http.StatusBadRequest)
		return
	}

	now := time.Now().UTC()
	periodStart := now.Add(-time.Duration(days) * 24 * time.Hour)
	priorStart := now.Add(-2 * time.Duration(days) * 24 * time.Hour)

	current, err := s.opts.CostEngine.Summary(r.Context(), s.opts.DB, cost.Options{
		Since: periodStart, Until: now,
		GroupBy: groupBy, Source: cost.SourceAuto,
		ProjectRoot: projectFilter, Limit: 5000,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	prior, err := s.opts.CostEngine.Summary(r.Context(), s.opts.DB, cost.Options{
		Since: priorStart, Until: periodStart,
		GroupBy: groupBy, Source: cost.SourceAuto,
		ProjectRoot: projectFilter, Limit: 5000,
	})
	if err != nil {
		writeErr(w, err)
		return
	}

	priorByKey := map[string]float64{}
	for _, row := range prior.Rows {
		priorByKey[row.Key] = row.CostUSD
	}
	currentByKey := map[string]float64{}
	for _, row := range current.Rows {
		currentByKey[row.Key] = row.CostUSD
	}

	type mover struct {
		Key        string  `json:"key"`
		PriorUSD   float64 `json:"prior_usd"`
		CurrentUSD float64 `json:"current_usd"`
		DeltaUSD   float64 `json:"delta_usd"`
		DeltaPct   float64 `json:"delta_pct"`
	}
	type entrant struct {
		Key        string  `json:"key"`
		CurrentUSD float64 `json:"current_usd"`
	}

	var allMovers []mover
	var newEntrants []entrant
	// Keys present in current — emit either as a mover (had a prior)
	// or as a new entrant (no prior cost).
	for _, row := range current.Rows {
		priorCost, hadPrior := priorByKey[row.Key]
		if !hadPrior || priorCost == 0 {
			if row.CostUSD > 0 {
				newEntrants = append(newEntrants, entrant{Key: row.Key, CurrentUSD: row.CostUSD})
			}
			continue
		}
		delta := row.CostUSD - priorCost
		var deltaPct float64
		if priorCost > 0 {
			deltaPct = delta / priorCost * 100
		}
		allMovers = append(allMovers, mover{
			Key: row.Key, PriorUSD: priorCost, CurrentUSD: row.CostUSD,
			DeltaUSD: delta, DeltaPct: deltaPct,
		})
	}
	// Keys present only in prior — usage went to zero this period.
	// These count as decreases (negative delta) and surface in the
	// movers list so users see "we stopped using X."
	for key, priorCost := range priorByKey {
		if _, ok := currentByKey[key]; ok {
			continue
		}
		allMovers = append(allMovers, mover{
			Key: key, PriorUSD: priorCost, CurrentUSD: 0,
			DeltaUSD: -priorCost, DeltaPct: -100,
		})
	}

	// Top 5 increases (largest positive Δ$) and top 5 decreases
	// (largest negative Δ$). Sorted by signed delta — head of the list
	// is the biggest jump up, tail is the biggest drop.
	sort.SliceStable(allMovers, func(i, j int) bool {
		return allMovers[i].DeltaUSD > allMovers[j].DeltaUSD
	})
	const topN = 5
	var increases, decreases []mover
	for _, m := range allMovers {
		if m.DeltaUSD > 0 && len(increases) < topN {
			increases = append(increases, m)
		}
	}
	for i := len(allMovers) - 1; i >= 0; i-- {
		if allMovers[i].DeltaUSD < 0 && len(decreases) < topN {
			decreases = append(decreases, allMovers[i])
		}
	}
	// New entrants sorted by current cost DESC, capped at topN.
	sort.SliceStable(newEntrants, func(i, j int) bool {
		return newEntrants[i].CurrentUSD > newEntrants[j].CurrentUSD
	})
	if len(newEntrants) > topN {
		newEntrants = newEntrants[:topN]
	}

	writeJSON(w, map[string]any{
		"dim":          dim,
		"days":         days,
		"period_start": periodStart.Format(time.RFC3339),
		"prior_start":  priorStart.Format(time.RFC3339),
		"increases":    increases,
		"decreases":    decreases,
		"new_entrants": newEntrants,
	})
}

// handleAnalysisTopSessions serves /api/analysis/top-sessions?days=N&limit=10
// — the most expensive sessions in the window, with explanatory badges
// describing why each landed in the top.
//
// Per-turn-deduped scan (mirrors handleAnalysisHeadline) so LC dispatch
// runs on each turn before per-session aggregation. Badge logic:
//
//   - opus       → any turn used an Opus-class model (model name contains "opus")
//   - lc_tier    → at least one turn crossed an LC threshold (rowCost > rowStdCost)
//   - many_turns → > 30 turns (heuristic for verbose sessions)
//   - large_prompt → max single-turn prompt > 100K tokens (cache-heavy or LC-adjacent)
//
// Top-N enrichment: a second pass against the sessions table fills in
// tool + started_at + ended_at for the surviving session ids.
func (s *Server) handleAnalysisTopSessions(w http.ResponseWriter, r *http.Request) {
	days := intArg(r, "days", 30, 1, 36500)
	limit := intArg(r, "limit", 10, 1, 100)

	now := time.Now().UTC()
	since := now.Add(-time.Duration(days) * 24 * time.Hour)
	tsArg := since.Format(time.RFC3339Nano)

	const q = `WITH proxy_turn_ids AS (
		SELECT request_id FROM api_turns
		 WHERE request_id IS NOT NULL AND request_id != ''
		   AND timestamp >= ?
	),
	combined AS (
		SELECT session_id, model, input_tokens, output_tokens, cache_read_tokens,
		       cache_creation_tokens, cache_creation_1h_tokens, cost_usd
		FROM api_turns WHERE timestamp >= ?
		UNION ALL
		SELECT session_id, model, input_tokens, output_tokens, cache_read_tokens,
		       cache_creation_tokens, cache_creation_1h_tokens, estimated_cost_usd
		FROM token_usage
		WHERE timestamp >= ?
		  AND (source_event_id IS NULL OR source_event_id = ''
		       OR source_event_id NOT IN (SELECT request_id FROM proxy_turn_ids))
	)
	SELECT COALESCE(session_id, ''),
	       COALESCE(model, ''),
	       COALESCE(input_tokens, 0), COALESCE(output_tokens, 0),
	       COALESCE(cache_read_tokens, 0), COALESCE(cache_creation_tokens, 0),
	       COALESCE(cache_creation_1h_tokens, 0),
	       COALESCE(cost_usd, 0)
	FROM combined`

	rows, err := s.opts.DB.QueryContext(r.Context(), q, tsArg, tsArg, tsArg)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()

	type sessionAgg struct {
		ID          string
		Models      map[string]bool
		Turns       int
		CostUSD     float64
		MaxPrompt   int64
		LCTurnCount int
		HasOpus     bool
	}
	agg := map[string]*sessionAgg{}

	for rows.Next() {
		var (
			sid    string
			model  string
			bundle cost.TokenBundle
			rec    float64
		)
		if err := rows.Scan(&sid, &model,
			&bundle.Input, &bundle.Output,
			&bundle.CacheRead, &bundle.CacheCreation, &bundle.CacheCreation1h,
			&rec); err != nil {
			writeErr(w, err)
			return
		}
		if sid == "" {
			continue // unattributed row, can't bucket by session
		}

		var rowCost, rowStdCost float64
		if rec > 0 {
			rowCost = rec
			rowStdCost = rec
		} else if p, ok := s.opts.CostEngine.Lookup(model); ok {
			rowCost = cost.Compute(p, bundle)
			rowStdCost = cost.Compute(stripLongContext(p), bundle)
		}

		a, ok := agg[sid]
		if !ok {
			a = &sessionAgg{ID: sid, Models: map[string]bool{}}
			agg[sid] = a
		}
		if model != "" {
			a.Models[model] = true
			if containsCI(model, "opus") {
				a.HasOpus = true
			}
		}
		a.Turns++
		a.CostUSD += rowCost
		prompt := bundle.Input + bundle.CacheRead + bundle.CacheCreation
		if prompt > a.MaxPrompt {
			a.MaxPrompt = prompt
		}
		if rowCost > rowStdCost {
			a.LCTurnCount++
		}
	}
	if err := rows.Err(); err != nil {
		writeErr(w, err)
		return
	}

	// Sort by cost DESC and trim to limit.
	ranked := make([]*sessionAgg, 0, len(agg))
	for _, a := range agg {
		if a.CostUSD <= 0 {
			continue
		}
		ranked = append(ranked, a)
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		return ranked[i].CostUSD > ranked[j].CostUSD
	})
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}

	// Enrich with sessions metadata (tool / started_at / ended_at) for
	// the survivors. One IN-list query is fine for a list capped at 100.
	type sessionMeta struct {
		Tool      string
		StartedAt string
		EndedAt   string
	}
	meta := map[string]sessionMeta{}
	if len(ranked) > 0 {
		ids := make([]string, 0, len(ranked))
		placeholders := make([]string, 0, len(ranked))
		args := make([]any, 0, len(ranked))
		for _, a := range ranked {
			ids = append(ids, a.ID)
			placeholders = append(placeholders, "?")
			args = append(args, a.ID)
		}
		mq := `SELECT id, COALESCE(tool, ''), COALESCE(started_at, ''),
		              COALESCE(ended_at, '')
		       FROM sessions WHERE id IN (` + joinStrings(placeholders, ",") + `)`
		mrows, mErr := s.opts.DB.QueryContext(r.Context(), mq, args...)
		if mErr != nil {
			writeErr(w, mErr)
			return
		}
		for mrows.Next() {
			var id, tool, startedAt, endedAt string
			if err := mrows.Scan(&id, &tool, &startedAt, &endedAt); err != nil {
				mrows.Close()
				writeErr(w, err)
				return
			}
			meta[id] = sessionMeta{Tool: tool, StartedAt: startedAt, EndedAt: endedAt}
		}
		mrows.Close()
	}

	type session struct {
		ID        string   `json:"id"`
		Tool      string   `json:"tool"`
		StartedAt string   `json:"started_at"`
		EndedAt   string   `json:"ended_at,omitempty"`
		Models    []string `json:"models"`
		Turns     int      `json:"turns"`
		MaxPrompt int64    `json:"max_prompt_tokens"`
		LCTurns   int      `json:"lc_turn_count"`
		CostUSD   float64  `json:"cost_usd"`
		Badges    []string `json:"badges"`
	}
	out := make([]session, 0, len(ranked))
	for _, a := range ranked {
		modelList := make([]string, 0, len(a.Models))
		for m := range a.Models {
			modelList = append(modelList, m)
		}
		sort.Strings(modelList)

		var badges []string
		if a.HasOpus {
			badges = append(badges, "opus")
		}
		if a.LCTurnCount > 0 {
			badges = append(badges, "lc_tier")
		}
		if a.Turns > 30 {
			badges = append(badges, "many_turns")
		}
		if a.MaxPrompt > 100_000 {
			badges = append(badges, "large_prompt")
		}

		m := meta[a.ID]
		out = append(out, session{
			ID: a.ID, Tool: m.Tool,
			StartedAt: m.StartedAt, EndedAt: m.EndedAt,
			Models: modelList, Turns: a.Turns,
			MaxPrompt: a.MaxPrompt, LCTurns: a.LCTurnCount,
			CostUSD: a.CostUSD, Badges: badges,
		})
	}

	writeJSON(w, map[string]any{
		"days":     days,
		"limit":    limit,
		"sessions": out,
	})
}

// containsCI reports whether s contains substr, case-insensitive. Local
// helper because strings.Contains is case-sensitive and the model id
// case isn't normalized at ingest.
func containsCI(s, substr string) bool {
	if len(substr) == 0 {
		return true
	}
	for i := 0; i+len(substr) <= len(s); i++ {
		match := true
		for j := 0; j < len(substr); j++ {
			a := s[i+j]
			b := substr[j]
			if a >= 'A' && a <= 'Z' {
				a += 32
			}
			if b >= 'A' && b <= 'Z' {
				b += 32
			}
			if a != b {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// joinStrings is a 0-allocation strings.Join for the placeholder list.
// Avoids importing strings just for this single use.
func joinStrings(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	n := (len(parts) - 1) * len(sep)
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 0, n)
	out = append(out, parts[0]...)
	for _, p := range parts[1:] {
		out = append(out, sep...)
		out = append(out, p...)
	}
	return string(out)
}

// handleAnalysisRoutingSuggestions serves /api/analysis/routing-suggestions
// — the softened "you might have used a cheaper model" signal for the
// Analysis tab's Band 4. Heuristic-driven and intentionally
// conservative: only flag sessions whose work profile is unambiguously
// trivial (small prompt, low output, no LC-tier turns, single
// expensive-model usage), and only suggest a single sibling switch
// per family.
//
// Surfaced as an informational table. Framing is "could have saved $X"
// rather than "you wasted $X" — model choice may be deliberate (user
// wanted Opus's reasoning quality), so the panel is opt-in for the
// price-sensitive user, not a correction.
//
// Sibling map (v1, Anthropic only — cross-provider routing is more
// debatable):
//
//	claude-opus-* → claude-sonnet-4-6 (1/5 the standard rate)
//
// The cheaper sibling is intentionally the current Sonnet flagship
// (no LC tier, predictable pricing) so projected savings don't depend
// on the LC dispatch logic.
func (s *Server) handleAnalysisRoutingSuggestions(w http.ResponseWriter, r *http.Request) {
	days := intArg(r, "days", 30, 1, 36500)
	limit := intArg(r, "limit", 20, 1, 100)

	now := time.Now().UTC()
	since := now.Add(-time.Duration(days) * 24 * time.Hour)
	tsArg := since.Format(time.RFC3339Nano)

	// Conservative thresholds — only flag unambiguously trivial profiles.
	const (
		maxTrivialPromptTokens = 30_000
		maxTrivialOutputTokens = 5_000
		minSavingsUSD          = 0.05
	)

	const q = `WITH proxy_turn_ids AS (
		SELECT request_id FROM api_turns
		 WHERE request_id IS NOT NULL AND request_id != ''
		   AND timestamp >= ?
	),
	combined AS (
		SELECT session_id, model, input_tokens, output_tokens, cache_read_tokens,
		       cache_creation_tokens, cache_creation_1h_tokens, cost_usd
		FROM api_turns WHERE timestamp >= ?
		UNION ALL
		SELECT session_id, model, input_tokens, output_tokens, cache_read_tokens,
		       cache_creation_tokens, cache_creation_1h_tokens, estimated_cost_usd
		FROM token_usage
		WHERE timestamp >= ?
		  AND (source_event_id IS NULL OR source_event_id = ''
		       OR source_event_id NOT IN (SELECT request_id FROM proxy_turn_ids))
	)
	SELECT COALESCE(session_id, ''),
	       COALESCE(model, ''),
	       COALESCE(input_tokens, 0), COALESCE(output_tokens, 0),
	       COALESCE(cache_read_tokens, 0), COALESCE(cache_creation_tokens, 0),
	       COALESCE(cache_creation_1h_tokens, 0),
	       COALESCE(cost_usd, 0)
	FROM combined`

	rows, err := s.opts.DB.QueryContext(r.Context(), q, tsArg, tsArg, tsArg)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()

	type sessionAgg struct {
		ID       string
		Models   map[string]bool
		Bundle   cost.TokenBundle
		CostUSD  float64
		LCTurns  int
		HasOpus  bool
		AnyTurns int
	}
	agg := map[string]*sessionAgg{}

	for rows.Next() {
		var (
			sid    string
			model  string
			bundle cost.TokenBundle
			rec    float64
		)
		if err := rows.Scan(&sid, &model,
			&bundle.Input, &bundle.Output,
			&bundle.CacheRead, &bundle.CacheCreation, &bundle.CacheCreation1h,
			&rec); err != nil {
			writeErr(w, err)
			return
		}
		if sid == "" {
			continue
		}

		var rowCost, rowStdCost float64
		if rec > 0 {
			rowCost = rec
			rowStdCost = rec
		} else if p, ok := s.opts.CostEngine.Lookup(model); ok {
			rowCost = cost.Compute(p, bundle)
			rowStdCost = cost.Compute(stripLongContext(p), bundle)
		}

		a, ok := agg[sid]
		if !ok {
			a = &sessionAgg{ID: sid, Models: map[string]bool{}}
			agg[sid] = a
		}
		if model != "" {
			a.Models[model] = true
			if containsCI(model, "opus") {
				a.HasOpus = true
			}
		}
		a.Bundle.Add(bundle)
		a.CostUSD += rowCost
		a.AnyTurns++
		if rowCost > rowStdCost {
			a.LCTurns++
		}
	}
	if err := rows.Err(); err != nil {
		writeErr(w, err)
		return
	}

	type suggestion struct {
		SessionID        string   `json:"session_id"`
		CurrentModel     string   `json:"current_model"`
		SuggestedModel   string   `json:"suggested_model"`
		CurrentCostUSD   float64  `json:"current_cost_usd"`
		SuggestedCostUSD float64  `json:"suggested_cost_usd"`
		SavingsUSD       float64  `json:"savings_usd"`
		Reasons          []string `json:"reasons"`
	}
	var out []suggestion

	const cheaperSibling = "claude-sonnet-4-6"
	cheaperPricing, cheaperOK := s.opts.CostEngine.Lookup(cheaperSibling)

	for _, a := range agg {
		if !cheaperOK || a.AnyTurns == 0 {
			continue
		}
		// Filter to trivial Opus-only sessions. Mixed-model sessions
		// are intentional (often haiku+opus by design) so we leave
		// them alone.
		if !a.HasOpus || len(a.Models) != 1 {
			continue
		}
		if a.LCTurns > 0 {
			continue // any LC turn signals real work
		}
		promptTokens := a.Bundle.Input + a.Bundle.CacheRead + a.Bundle.CacheCreation
		if promptTokens >= maxTrivialPromptTokens {
			continue
		}
		if a.Bundle.Output >= maxTrivialOutputTokens {
			continue
		}
		projected := cost.Compute(cheaperPricing, a.Bundle)
		savings := a.CostUSD - projected
		if savings < minSavingsUSD {
			continue
		}

		// Resolve the current model to a single id (set has length 1).
		var currentModel string
		for m := range a.Models {
			currentModel = m
		}

		reasons := []string{}
		if promptTokens < maxTrivialPromptTokens {
			reasons = append(reasons, "small prompt")
		}
		if a.Bundle.Output < maxTrivialOutputTokens {
			reasons = append(reasons, "low output")
		}
		reasons = append(reasons, "no LC tier", "single-model session")

		out = append(out, suggestion{
			SessionID:        a.ID,
			CurrentModel:     currentModel,
			SuggestedModel:   cheaperSibling,
			CurrentCostUSD:   a.CostUSD,
			SuggestedCostUSD: projected,
			SavingsUSD:       savings,
			Reasons:          reasons,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].SavingsUSD > out[j].SavingsUSD
	})
	if len(out) > limit {
		out = out[:limit]
	}

	totalSavings := 0.0
	for _, s := range out {
		totalSavings += s.SavingsUSD
	}

	writeJSON(w, map[string]any{
		"days":              days,
		"suggestions":       out,
		"total_savings_usd": totalSavings,
		"sibling_map": map[string]string{
			"opus": cheaperSibling,
		},
		"thresholds": map[string]any{
			"max_trivial_prompt_tokens": maxTrivialPromptTokens,
			"max_trivial_output_tokens": maxTrivialOutputTokens,
			"min_savings_usd":           minSavingsUSD,
		},
		"framing_note": "informational only — model choice may be deliberate",
	})
}

// stripLongContext zeros every long-context field on a Pricing entry so
// Compute reverts to the standard tier. Used to compute "what this turn
// would have cost without LC repricing" so the headline can attribute
// the surcharge.
func stripLongContext(p cost.Pricing) cost.Pricing {
	p.LongContextThreshold = 0
	p.LongContextInput = 0
	p.LongContextOutput = 0
	p.LongContextCacheRead = 0
	p.LongContextCacheCreation = 0
	p.LongContextCacheCreation1h = 0
	return p
}
