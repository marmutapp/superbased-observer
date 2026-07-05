package rollup

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// Tools computes the org-wide per-tool usage breakdown for the window: cost,
// token buckets, sessions, active developers, action count + success rate, and
// the proxy-only average TTFT (0 when no timing was captured for the tool).
// Scoped by the caller's role via userScopeSQL. Content-free throughout.
func Tools(ctx context.Context, db *sql.DB, w Window, scope Scope, now time.Time) (ToolsResult, error) {
	s := since(w, now)
	res := ToolsResult{WindowDays: w.days(), Tools: []ToolRow{}}
	uScope, uArgs := userScopeSQL("user_id", scope)
	rows := map[string]*ToolRow{}
	get := func(tool string) *ToolRow {
		r, ok := rows[tool]
		if !ok {
			r = &ToolRow{Tool: tool}
			rows[tool] = r
		}
		return r
	}

	// Cost + token buckets + active devs per tool (deduped substrate).
	//nolint:gosec // G202: enrichedCTE code constant; uScope parameterized; values bind via ?.
	evQ := enrichedCTE + `
SELECT tool, COALESCE(SUM(cost),0),
       COALESCE(SUM(input_tokens),0), COALESCE(SUM(cache_read),0), COALESCE(SUM(cache_write),0),
       COALESCE(SUM(output_tokens),0), COALESCE(SUM(reasoning),0),
       COUNT(DISTINCT user_id)
  FROM ev WHERE tool != '' AND ` + uScope + ` GROUP BY tool`
	if err := eachRow(ctx, db, evQ, append(spendArgs(s), uArgs...), func(r *sql.Rows) error {
		var tool string
		var cost float64
		var b TokenBuckets
		var devs int64
		if err := r.Scan(&tool, &cost, &b.NetInput, &b.CacheRead, &b.CacheWrite, &b.Output, &b.Reasoning, &devs); err != nil {
			return err
		}
		t := get(tool)
		t.CostUSD, t.Buckets, t.ActiveDevs = cost, b, devs
		t.Tokens = b.NetInput + b.Output
		return nil
	}); err != nil {
		return ToolsResult{}, fmt.Errorf("rollup.Tools: ev: %w", err)
	}

	// Sessions per tool.
	if err := scopedGroupCount(ctx, db, "sessions", "started_at", "tool", s, uScope, uArgs, func(k string, n int64) {
		get(k).Sessions = n
	}); err != nil {
		return ToolsResult{}, fmt.Errorf("rollup.Tools: sessions: %w", err)
	}

	// Action count + success rate per tool.
	//nolint:gosec // G201: code-constant identifiers; uScope parameterized; window binds via ?.
	actQ := `SELECT tool, COUNT(*), COALESCE(SUM(success),0) FROM actions
	          WHERE timestamp >= ? AND tool != '' AND ` + uScope + ` GROUP BY tool`
	if err := eachRow(ctx, db, actQ, append([]any{s}, uArgs...), func(r *sql.Rows) error {
		var tool string
		var total, ok int64
		if err := r.Scan(&tool, &total, &ok); err != nil {
			return err
		}
		t := get(tool)
		t.ActionCount = total
		t.SuccessRate = fratio(float64(ok), float64(total))
		return nil
	}); err != nil {
		return ToolsResult{}, fmt.Errorf("rollup.Tools: actions: %w", err)
	}

	// Avg TTFT per tool (proxy-only; tool resolved via the session join). The
	// scope column is qualified to t.user_id — both joined tables carry
	// user_id, so the bare column would be ambiguous.
	tScope, _ := userScopeSQL("t.user_id", scope)
	//nolint:gosec // G201: code-constant identifiers; tScope parameterized; window binds via ?.
	ttftQ := `SELECT s.tool, AVG(t.time_to_first_token_ms)
	            FROM api_turns t JOIN sessions s ON s.id = t.session_id AND s.user_id = t.user_id
	           WHERE t.timestamp >= ? AND t.time_to_first_token_ms IS NOT NULL AND t.time_to_first_token_ms > 0
	             AND s.tool != '' AND ` + tScope + ` GROUP BY s.tool`
	if err := eachRow(ctx, db, ttftQ, append([]any{s}, uArgs...), func(r *sql.Rows) error {
		var tool string
		var avg sql.NullFloat64
		if err := r.Scan(&tool, &avg); err != nil {
			return err
		}
		if avg.Valid {
			get(tool).AvgTTFTMs = int64(avg.Float64 + 0.5)
		}
		return nil
	}); err != nil {
		return ToolsResult{}, fmt.Errorf("rollup.Tools: ttft: %w", err)
	}

	out := make([]ToolRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r)
	}
	sortByCostDesc(out, func(i int) (float64, string) { return out[i].CostUSD, out[i].Tool })
	res.Tools = out
	return res, nil
}

// Models computes the org-wide per-model usage breakdown. api_turns carries the
// model column directly, so AvgTTFTMs needs no session join.
func Models(ctx context.Context, db *sql.DB, w Window, scope Scope, now time.Time) (ModelsResult, error) {
	s := since(w, now)
	res := ModelsResult{WindowDays: w.days(), Models: []ModelRow{}}
	uScope, uArgs := userScopeSQL("user_id", scope)
	rows := map[string]*ModelRow{}
	get := func(model string) *ModelRow {
		r, ok := rows[model]
		if !ok {
			r = &ModelRow{Model: model}
			rows[model] = r
		}
		return r
	}

	//nolint:gosec // G202: enrichedCTE code constant; uScope parameterized; values bind via ?.
	evQ := enrichedCTE + `
SELECT model, COALESCE(SUM(cost),0),
       COALESCE(SUM(input_tokens),0), COALESCE(SUM(cache_read),0), COALESCE(SUM(cache_write),0),
       COALESCE(SUM(output_tokens),0), COALESCE(SUM(reasoning),0),
       COUNT(DISTINCT user_id)
  FROM ev WHERE model != '' AND ` + uScope + ` GROUP BY model`
	if err := eachRow(ctx, db, evQ, append(spendArgs(s), uArgs...), func(r *sql.Rows) error {
		var model string
		var cost float64
		var b TokenBuckets
		var devs int64
		if err := r.Scan(&model, &cost, &b.NetInput, &b.CacheRead, &b.CacheWrite, &b.Output, &b.Reasoning, &devs); err != nil {
			return err
		}
		m := get(model)
		m.CostUSD, m.Buckets, m.ActiveDevs = cost, b, devs
		m.Tokens = b.NetInput + b.Output
		return nil
	}); err != nil {
		return ModelsResult{}, fmt.Errorf("rollup.Models: ev: %w", err)
	}

	if err := scopedGroupCount(ctx, db, "sessions", "started_at", "model", s, uScope, uArgs, func(k string, n int64) {
		get(k).Sessions = n
	}); err != nil {
		return ModelsResult{}, fmt.Errorf("rollup.Models: sessions: %w", err)
	}

	//nolint:gosec // G201: code-constant identifiers; uScope parameterized; window binds via ?.
	ttftQ := `SELECT model, AVG(time_to_first_token_ms) FROM api_turns
	           WHERE timestamp >= ? AND time_to_first_token_ms IS NOT NULL AND time_to_first_token_ms > 0
	             AND model != '' AND ` + uScope + ` GROUP BY model`
	if err := eachRow(ctx, db, ttftQ, append([]any{s}, uArgs...), func(r *sql.Rows) error {
		var model string
		var avg sql.NullFloat64
		if err := r.Scan(&model, &avg); err != nil {
			return err
		}
		if avg.Valid {
			get(model).AvgTTFTMs = int64(avg.Float64 + 0.5)
		}
		return nil
	}); err != nil {
		return ModelsResult{}, fmt.Errorf("rollup.Models: ttft: %w", err)
	}

	out := make([]ModelRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r)
	}
	sortByCostDesc(out, func(i int) (float64, string) { return out[i].CostUSD, out[i].Model })
	res.Models = out
	return res, nil
}

// scopedGroupCount counts rows of <table> per <dimCol>, in window + scope.
func scopedGroupCount(ctx context.Context, db *sql.DB, table, tsCol, dimCol, s, uScope string, uArgs []any, set func(key string, n int64)) error {
	//nolint:gosec // G201: table/tsCol/dimCol are code-constant identifiers; uScope parameterized; window binds via ?.
	q := fmt.Sprintf(`SELECT %s, COUNT(*) FROM %s WHERE %s >= ? AND %s != '' AND %s GROUP BY %s`,
		dimCol, table, tsCol, dimCol, uScope, dimCol)
	return eachRow(ctx, db, q, append([]any{s}, uArgs...), func(r *sql.Rows) error {
		var key string
		var n int64
		if err := r.Scan(&key, &n); err != nil {
			return err
		}
		set(key, n)
		return nil
	})
}

// sortByCostDesc sorts a slice in place by (cost desc, label asc) using an
// index accessor — shared by Tools and Models.
func sortByCostDesc[T any](s []T, key func(i int) (float64, string)) {
	sort.SliceStable(s, func(i, j int) bool {
		ci, li := key(i)
		cj, lj := key(j)
		if ci != cj {
			return ci > cj
		}
		return li < lj
	})
}
