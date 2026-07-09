package rollup

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// MoverRow is one dimension key's period-over-period spend movement.
type MoverRow struct {
	Key        string  `json:"key"`
	CurrentUSD float64 `json:"current_usd"`
	PriorUSD   float64 `json:"prior_usd"`
	DeltaUSD   float64 `json:"delta_usd"`
}

// MoversResult powers GET /api/org/movers — period-over-period spend movement
// by a chosen dimension (model | project | tool). Content-free: project is the
// hash-derived id, never a raw path.
type MoversResult struct {
	WindowDays  int        `json:"window_days"`
	Dimension   string     `json:"dimension"`
	Increases   []MoverRow `json:"increases"`
	Decreases   []MoverRow `json:"decreases"`
	NewEntrants []MoverRow `json:"new_entrants"`
}

// Movers compares spend in the current window [now-days, now] against the prior
// window of equal length [now-2*days, now-days), grouped by dimension. dim is
// "model" (default), "project", or "tool". Scoped (admin/lead). Reuses spendCTE.
func Movers(ctx context.Context, db *sql.DB, w Window, scope Scope, dim string, now time.Time) (MoversResult, error) {
	col, isProject := moverDimColumn(dim)
	res := MoversResult{
		WindowDays:  w.days(),
		Dimension:   moverDimName(dim),
		Increases:   []MoverRow{},
		Decreases:   []MoverRow{},
		NewEntrants: []MoverRow{},
	}
	uScope, uArgs := userScopeSQL("user_id", scope)

	curStart := since(w, now)
	priorStart := since(Window{Days: 2 * w.days()}, now)

	// One scan over [priorStart, now], bucketed into current vs prior by ts.
	//nolint:gosec // G202: spendCTE is a code constant; col is a code-constant column; uScope parameterized; values bind via ?.
	q := spendCTE + `
SELECT ` + col + `,
       COALESCE(SUM(CASE WHEN ts >= ? THEN cost ELSE 0 END),0) AS cur,
       COALESCE(SUM(CASE WHEN ts <  ? THEN cost ELSE 0 END),0) AS prior
  FROM spend
 WHERE ` + col + ` != '' AND ` + uScope + `
 GROUP BY ` + col
	args := append(append(spendArgs(priorStart), curStart, curStart), uArgs...)

	var rows []MoverRow
	if err := eachRow(ctx, db, q, args, func(r *sql.Rows) error {
		var key string
		var cur, prior float64
		if err := r.Scan(&key, &cur, &prior); err != nil {
			return err
		}
		if isProject {
			key = ProjectIDFromHash(key)
		}
		rows = append(rows, MoverRow{Key: key, CurrentUSD: cur, PriorUSD: prior, DeltaUSD: cur - prior})
		return nil
	}); err != nil {
		return MoversResult{}, fmt.Errorf("rollup.Movers: %w", err)
	}

	// Split into increases (Δ>0), decreases (Δ<0), and new entrants (prior==0 &&
	// cur>0). Increases/decreases ranked by |Δ|; entrants by current spend.
	for _, m := range rows {
		switch {
		case m.PriorUSD == 0 && m.CurrentUSD > 0:
			res.NewEntrants = append(res.NewEntrants, m)
		case m.DeltaUSD > 1e-9:
			res.Increases = append(res.Increases, m)
		case m.DeltaUSD < -1e-9:
			res.Decreases = append(res.Decreases, m)
		}
	}
	sort.SliceStable(res.Increases, func(i, j int) bool { return res.Increases[i].DeltaUSD > res.Increases[j].DeltaUSD })
	sort.SliceStable(res.Decreases, func(i, j int) bool { return res.Decreases[i].DeltaUSD < res.Decreases[j].DeltaUSD })
	sort.SliceStable(res.NewEntrants, func(i, j int) bool { return res.NewEntrants[i].CurrentUSD > res.NewEntrants[j].CurrentUSD })
	res.Increases = topMovers(res.Increases)
	res.Decreases = topMovers(res.Decreases)
	res.NewEntrants = topMovers(res.NewEntrants)
	return res, nil
}

func topMovers(m []MoverRow) []MoverRow {
	if len(m) > topN {
		return m[:topN]
	}
	return m
}

// moverDimColumn maps the dim param to the spend CTE column + whether it is the
// project dimension (so the key must be hash-derived for the response).
func moverDimColumn(dim string) (col string, isProject bool) {
	switch dim {
	case "project":
		return "project_root_hash", true
	case "tool":
		return "tool", false
	default:
		return "model", false
	}
}

func moverDimName(dim string) string {
	switch dim {
	case "project", "tool":
		return dim
	default:
		return "model"
	}
}
