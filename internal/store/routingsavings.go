package store

import (
	"math"
	"path/filepath"
	"sort"

	"github.com/marmutapp/superbased-observer/internal/routing"
)

// Router-savings aggregation (§R17.3) — shared by the CLI and the
// dashboard routing page. Honesty contract: "realized" is the
// decision-time estimate on APPLIED rewrites (the §R18.3 calibration
// loop grades the estimator against outcomes); "would-have" is the
// same estimate on unapplied advise decisions. Every group carries
// §R7.2 error bars (mean per decision ± 95% CI) — never a bare point
// claim.

// RouterSavingsGroup is one aggregation cell.
type RouterSavingsGroup struct {
	Key             string  `json:"key"`
	Decisions       int     `json:"decisions"`
	Reroutes        int     `json:"reroutes"`
	RealizedUSD     float64 `json:"realized_usd"`
	WouldHaveUSD    float64 `json:"would_have_usd"`
	MeanPerDecision float64 `json:"mean_per_decision_usd"`
	CI95PerDecision float64 `json:"ci95_per_decision_usd"`
	CacheForfeitUSD float64 `json:"cache_forfeit_usd"`
}

// RouterSavingsReport is the aggregated output shape.
type RouterSavingsReport struct {
	WindowDays   int                  `json:"window_days"`
	Decisions    int                  `json:"decisions"`
	RealizedUSD  float64              `json:"realized_usd"`
	WouldHaveUSD float64              `json:"would_have_usd"`
	ByProject    []RouterSavingsGroup `json:"by_project"`
	ByTool       []RouterSavingsGroup `json:"by_tool"`
	ByTier       []RouterSavingsGroup `json:"by_tier"`
	Note         string               `json:"note"`
}

// RouterSavingsNote is the structural honesty caveat on every report.
const RouterSavingsNote = "realized = decision-time estimates on applied rewrites (graded by the calibration loop, §R18.3); " +
	"would-have = the same estimates on unapplied advise decisions; error bars are 95% CIs over per-decision savings (§R7.2)."

// BuildRouterSavingsReport aggregates decision rows. Pure — testable
// without a DB.
func BuildRouterSavingsReport(rows []RouterDecisionDetail, windowDays int, tiers *routing.TierTable) RouterSavingsReport {
	rep := RouterSavingsReport{WindowDays: windowDays, Note: RouterSavingsNote}
	byProject := map[string][]RouterDecisionDetail{}
	byTool := map[string][]RouterDecisionDetail{}
	byTier := map[string][]RouterDecisionDetail{}
	for _, d := range rows {
		rep.Decisions++
		if d.Applied {
			rep.RealizedUSD += d.EstSavingsUSD
		} else if d.SelectedModel != d.OriginalModel {
			rep.WouldHaveUSD += d.EstSavingsUSD
		}
		project := filepath.Base(d.ProjectRoot)
		if d.ProjectRoot == "" {
			project = "(unattributed)"
		}
		tool := d.Tool
		if tool == "" {
			tool = "(unattributed)"
		}
		tier, _ := tiers.Lookup(d.OriginalModel)
		byProject[project] = append(byProject[project], d)
		byTool[tool] = append(byTool[tool], d)
		byTier[string(tier)] = append(byTier[string(tier)], d)
	}
	rep.ByProject = buildSavingsGroups(byProject)
	rep.ByTool = buildSavingsGroups(byTool)
	rep.ByTier = buildSavingsGroups(byTier)
	return rep
}

func buildSavingsGroups(m map[string][]RouterDecisionDetail) []RouterSavingsGroup {
	out := make([]RouterSavingsGroup, 0, len(m))
	for key, rows := range m {
		g := RouterSavingsGroup{Key: key, Decisions: len(rows)}
		var savings []float64
		for _, d := range rows {
			changed := d.SelectedModel != d.OriginalModel
			if changed {
				g.Reroutes++
			}
			if d.Applied {
				g.RealizedUSD += d.EstSavingsUSD
			} else if changed {
				g.WouldHaveUSD += d.EstSavingsUSD
			}
			g.CacheForfeitUSD += d.CacheForfeitUSD
			savings = append(savings, d.EstSavingsUSD)
		}
		g.MeanPerDecision, g.CI95PerDecision = meanCI95(savings)
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		si, sj := out[i].RealizedUSD+out[i].WouldHaveUSD, out[j].RealizedUSD+out[j].WouldHaveUSD
		if si != sj {
			return si > sj
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// meanCI95 returns the sample mean and its 95% confidence half-width
// (1.96·sd/√n; zero for n < 2 — no claim without spread).
func meanCI95(xs []float64) (mean, ci float64) {
	n := float64(len(xs))
	if n == 0 {
		return 0, 0
	}
	for _, x := range xs {
		mean += x
	}
	mean /= n
	if n < 2 {
		return mean, 0
	}
	var ss float64
	for _, x := range xs {
		ss += (x - mean) * (x - mean)
	}
	sd := math.Sqrt(ss / (n - 1))
	return mean, 1.96 * sd / math.Sqrt(n)
}
