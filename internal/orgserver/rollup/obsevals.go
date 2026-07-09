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

// obsevals.go is the OP4 org eval-health surface (obs-org-tier plan §5.3): team
// eval-run history + regression tracking over the content-free
// obs_eval_summaries aggregate member nodes push under
// [org_client.share].obs_eval_summary. One run groups its per-scorer rows;
// runs are ordered newest-first and a per-(dataset,scorer) pass-rate delta vs
// the previous run flags regressions. Admin-only (eval health is an
// org-governance surface, mirrors Routing).

// ObsEvalScorer is one scorer's result within a run.
type ObsEvalScorer struct {
	ScorerName string  `json:"scorer_name"`
	Total      int64   `json:"total"`
	Passed     int64   `json:"passed"`
	PassRate   float64 `json:"pass_rate"`
	MeanScore  float64 `json:"mean_score"`
	MinScore   float64 `json:"min_score"`
	// PassRateDelta is this scorer's pass-rate change vs the previous run of the
	// same dataset (0 when there is no prior run). Negative = regression.
	PassRateDelta float64 `json:"pass_rate_delta"`
}

// ObsEvalRunGroup is one eval run (a day × dataset × run-name × source).
type ObsEvalRunGroup struct {
	Day         string          `json:"day"`
	DatasetName string          `json:"dataset_name"`
	RunName     string          `json:"run_name"`
	Source      string          `json:"source"`
	Total       int64           `json:"total"`
	Passed      int64           `json:"passed"`
	PassRate    float64         `json:"pass_rate"`
	MeanScore   float64         `json:"mean_score"`
	Regressed   bool            `json:"regressed"` // any scorer's pass rate dropped vs the prior run
	Scorers     []ObsEvalScorer `json:"scorers"`
}

// ObsEvalsResult is the GET /api/org/obs/evals body.
type ObsEvalsResult struct {
	WindowDays int               `json:"window_days"`
	Configured bool              `json:"configured"`
	Runs       []ObsEvalRunGroup `json:"runs"`
}

// ObsEvals aggregates obs_eval_summaries into the eval-health surface.
func ObsEvals(ctx context.Context, db *sql.DB, w Window, now time.Time) (ObsEvalsResult, error) {
	sinceDay := now.UTC().AddDate(0, 0, -w.days()).Format("2006-01-02")
	res := ObsEvalsResult{WindowDays: w.days(), Runs: []ObsEvalRunGroup{}}

	type runKey struct{ day, dataset, run, source string }
	groups := map[runKey]*ObsEvalRunGroup{}
	var order []runKey

	q := `
SELECT day, dataset_name, run_name, source, scorer_name,
       COALESCE(SUM(total),0), COALESCE(SUM(passed),0),
       COALESCE(AVG(mean_score),0), COALESCE(MIN(min_score),0)
  FROM obs_eval_summaries
 WHERE day >= ?
 GROUP BY day, dataset_name, run_name, source, scorer_name
 ORDER BY day DESC, dataset_name, run_name`
	if err := eachRow(ctx, db, q, []any{sinceDay}, func(rows *sql.Rows) error {
		var k runKey
		var scorer string
		var total, passed int64
		var mean, min float64
		if err := rows.Scan(&k.day, &k.dataset, &k.run, &k.source, &scorer, &total, &passed, &mean, &min); err != nil {
			return err
		}
		g := groups[k]
		if g == nil {
			g = &ObsEvalRunGroup{Day: k.day, DatasetName: k.dataset, RunName: k.run, Source: k.source, Scorers: []ObsEvalScorer{}}
			groups[k] = g
			order = append(order, k)
		}
		sc := ObsEvalScorer{ScorerName: scorer, Total: total, Passed: passed, MeanScore: mean, MinScore: min}
		if total > 0 {
			sc.PassRate = float64(passed) / float64(total)
		}
		g.Scorers = append(g.Scorers, sc)
		g.Total += total
		g.Passed += passed
		return nil
	}); err != nil {
		return ObsEvalsResult{}, fmt.Errorf("rollup.ObsEvals: %w", err)
	}

	for _, k := range order {
		g := groups[k]
		if g.Total > 0 {
			g.PassRate = float64(g.Passed) / float64(g.Total)
		}
		var meanSum float64
		for _, sc := range g.Scorers {
			meanSum += sc.MeanScore
		}
		if len(g.Scorers) > 0 {
			g.MeanScore = meanSum / float64(len(g.Scorers))
		}
		res.Runs = append(res.Runs, *g)
	}
	// Newest-first overall.
	sort.SliceStable(res.Runs, func(i, j int) bool {
		if res.Runs[i].Day != res.Runs[j].Day {
			return res.Runs[i].Day > res.Runs[j].Day
		}
		return res.Runs[i].DatasetName < res.Runs[j].DatasetName
	})
	markRegressions(res.Runs)
	res.Configured = len(res.Runs) > 0
	return res, nil
}

// markRegressions fills each scorer's PassRateDelta vs the previous (older) run
// of the same dataset+scorer, and flags a run as Regressed if any scorer's pass
// rate dropped. Runs are processed newest-first, so the "previous" run is the
// next-older one we encounter for the same (dataset, scorer).
func markRegressions(runs []ObsEvalRunGroup) {
	// last seen pass rate per (dataset, scorer), walking oldest→newest.
	seen := map[string]float64{}
	for i := len(runs) - 1; i >= 0; i-- {
		for j := range runs[i].Scorers {
			sc := &runs[i].Scorers[j]
			key := runs[i].DatasetName + "\x00" + sc.ScorerName
			if prev, ok := seen[key]; ok {
				sc.PassRateDelta = sc.PassRate - prev
				if sc.PassRateDelta < 0 {
					runs[i].Regressed = true
				}
			}
			seen[key] = sc.PassRate
		}
	}
}
