package rollup

import (
	"context"
	"testing"
)

// TestObsEvals_RunsAndRegression seeds two runs of one dataset on consecutive
// days and asserts the run grouping, pass rates, and the regression flag when a
// scorer's pass rate drops in the newer run.
func TestObsEvals_RunsAndRegression(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	ins := func(day, dataset, run, scorer string, total, passed int64, mean float64) {
		t.Helper()
		if _, err := d.ExecContext(ctx,
			`INSERT INTO obs_eval_summaries (org_id, user_email, day, dataset_name, run_name, scorer_name, source,
			   total, passed, mean_score, min_score, pushed_at, pushed_by_user_id)
			 VALUES ('org1','alice@x',?,?,?,?,'run',?,?,?,0,'2026-05-26T06:00:00Z','u-alice')`,
			day, dataset, run, scorer, total, passed, mean); err != nil {
			t.Fatalf("seed eval summary: %v", err)
		}
	}
	// Older run (2026-05-20): json_valid 100% (10/10).
	ins("2026-05-20", "ds1", "baseline", "json_valid", 10, 10, 1.0)
	// Newer run (2026-05-21): json_valid 60% (6/10) → regression.
	ins("2026-05-21", "ds1", "after-fix", "json_valid", 10, 6, 0.6)

	res, err := ObsEvals(ctx, d, w30, fixedNow)
	if err != nil {
		t.Fatalf("ObsEvals: %v", err)
	}
	if !res.Configured || len(res.Runs) != 2 {
		t.Fatalf("runs = %d, want 2 (configured=%v)", len(res.Runs), res.Configured)
	}
	// Newest first.
	newer := res.Runs[0]
	if newer.Day != "2026-05-21" || !near(newer.PassRate, 0.6) {
		t.Errorf("newer run = %+v, want 2026-05-21 @ 0.6", newer)
	}
	if !newer.Regressed {
		t.Error("newer run should be flagged regressed (json_valid 1.0 → 0.6)")
	}
	if len(newer.Scorers) != 1 || !near(newer.Scorers[0].PassRateDelta, -0.4) {
		t.Errorf("scorer delta = %+v, want -0.4", newer.Scorers)
	}
	// Older run has no prior → no regression, zero delta.
	older := res.Runs[1]
	if older.Regressed || older.Scorers[0].PassRateDelta != 0 {
		t.Errorf("older run should have no regression/delta = %+v", older)
	}
}
