package rollup

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// Activity computes the org-wide time-grid surface for the window: cost-by-day,
// actions-by-day (+ a stacked-by-tool series), tokens-by-day (billing buckets),
// the hour-of-day histogram, and the day-of-week × hour intensity grid. All
// from the watcher-fed actions table + the deduped token substrate — content-
// free, scoped by the caller's role.
func Activity(ctx context.Context, db *sql.DB, w Window, scope Scope, now time.Time) (ActivityResult, error) {
	s := since(w, now)
	res := ActivityResult{
		WindowDays:   w.days(),
		CostByDay:    []CostPoint{},
		ActionsByDay: []DayCount{},
		ToolByDay:    []ToolDayCount{},
		TokensByDay:  []DayBuckets{},
		HourOfDay:    []HourCount{},
		DowHour:      []DowHourCount{},
	}
	uScope, uArgs := userScopeSQL("user_id", scope)
	if ff, fa := spendFilterSQL(scope); ff != "" {
		uScope += ff
		uArgs = append(uArgs, fa...)
	}

	var err error
	if res.CostByDay, err = costByDay(ctx, db, s, uScope, uArgs); err != nil {
		return ActivityResult{}, fmt.Errorf("rollup.Activity: cost by day: %w", err)
	}

	// Actions by day + hour-of-day, from the actions table.
	//nolint:gosec // G201: code-constant identifiers; uScope parameterized; window binds via ?.
	dayQ := `SELECT substr(timestamp,1,10), COUNT(*) FROM actions WHERE timestamp >= ? AND ` + uScope + ` GROUP BY 1 ORDER BY 1`
	if err = eachRow(ctx, db, dayQ, append([]any{s}, uArgs...), func(r *sql.Rows) error {
		var d DayCount
		if err := r.Scan(&d.Date, &d.Count); err != nil {
			return err
		}
		res.ActionsByDay = append(res.ActionsByDay, d)
		return nil
	}); err != nil {
		return ActivityResult{}, fmt.Errorf("rollup.Activity: actions by day: %w", err)
	}

	//nolint:gosec // G201: code-constant identifiers; uScope parameterized; window binds via ?.
	toolQ := `SELECT substr(timestamp,1,10), tool, COUNT(*) FROM actions
	           WHERE timestamp >= ? AND tool != '' AND ` + uScope + ` GROUP BY 1, 2 ORDER BY 1, 2`
	if err = eachRow(ctx, db, toolQ, append([]any{s}, uArgs...), func(r *sql.Rows) error {
		var t ToolDayCount
		if err := r.Scan(&t.Date, &t.Tool, &t.Count); err != nil {
			return err
		}
		res.ToolByDay = append(res.ToolByDay, t)
		return nil
	}); err != nil {
		return ActivityResult{}, fmt.Errorf("rollup.Activity: tool by day: %w", err)
	}

	// Tokens by day (billing buckets) from the deduped substrate.
	//nolint:gosec // G202: enrichedCTE code constant; uScope parameterized; values bind via ?.
	tokQ := enrichedCTE + `
SELECT substr(ts,1,10) AS d,
       COALESCE(SUM(input_tokens),0), COALESCE(SUM(cache_read),0), COALESCE(SUM(cache_write),0),
       COALESCE(SUM(output_tokens),0), COALESCE(SUM(reasoning),0)
  FROM ev WHERE ` + uScope + ` GROUP BY d ORDER BY d`
	if err = eachRow(ctx, db, tokQ, append(spendArgs(s), uArgs...), func(r *sql.Rows) error {
		var b DayBuckets
		if err := r.Scan(&b.Date, &b.NetInput, &b.CacheRead, &b.CacheWrite, &b.Output, &b.Reasoning); err != nil {
			return err
		}
		res.TokensByDay = append(res.TokensByDay, b)
		return nil
	}); err != nil {
		return ActivityResult{}, fmt.Errorf("rollup.Activity: tokens by day: %w", err)
	}

	// Hour-of-day histogram + the dow×hour grid. One scan over (day, hour)
	// counts feeds both — the day-of-week is derived in Go (no strftime, which
	// is brittle on the trailing-Z RFC3339 timestamps).
	//nolint:gosec // G201: code-constant identifiers; uScope parameterized; window binds via ?.
	gridQ := `SELECT substr(timestamp,1,10) AS d, CAST(substr(timestamp,12,2) AS INTEGER) AS h, COUNT(*)
	            FROM actions WHERE timestamp >= ? AND ` + uScope + ` GROUP BY d, h`
	hourTot := [24]int64{}
	dowGrid := map[[2]int]int64{}
	if err = eachRow(ctx, db, gridQ, append([]any{s}, uArgs...), func(r *sql.Rows) error {
		var day string
		var hour int
		var n int64
		if err := r.Scan(&day, &hour, &n); err != nil {
			return err
		}
		if hour < 0 || hour > 23 {
			return nil
		}
		hourTot[hour] += n
		dow := dowOf(day)
		if dow >= 0 {
			dowGrid[[2]int{dow, hour}] += n
		}
		return nil
	}); err != nil {
		return ActivityResult{}, fmt.Errorf("rollup.Activity: grid: %w", err)
	}
	for h := 0; h < 24; h++ {
		if hourTot[h] > 0 {
			res.HourOfDay = append(res.HourOfDay, HourCount{Hour: h, Count: hourTot[h]})
		}
	}
	for k, n := range dowGrid {
		res.DowHour = append(res.DowHour, DowHourCount{Dow: k[0], Hour: k[1], Count: n})
	}
	sort.Slice(res.DowHour, func(i, j int) bool {
		if res.DowHour[i].Dow != res.DowHour[j].Dow {
			return res.DowHour[i].Dow < res.DowHour[j].Dow
		}
		return res.DowHour[i].Hour < res.DowHour[j].Hour
	})
	return res, nil
}

// dowOf returns the day-of-week (0=Sunday … 6=Saturday) for a YYYY-MM-DD string,
// or -1 if it cannot be parsed.
func dowOf(day string) int {
	t, err := time.Parse("2006-01-02", day)
	if err != nil {
		return -1
	}
	return int(t.Weekday())
}
