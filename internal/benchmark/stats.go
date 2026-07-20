package benchmark

import (
	"math"
	"math/rand"
	"sort"
)

// z95 is the two-sided 95% normal quantile — the default CI width.
const z95 = 1.959963984540054

// Interval is a point estimate with a lower/upper confidence bound.
type Interval struct {
	Point float64
	Lo    float64
	Hi    float64
}

// Wilson computes the Wilson score interval for a binomial proportion
// (passes/n) at confidence width z. Correct for the small-n regime
// (repeats=3–10) where the normal approximation degenerates. n=0 yields the
// zero interval.
func Wilson(passes, n int, z float64) Interval {
	if n <= 0 {
		return Interval{}
	}
	p := float64(passes) / float64(n)
	nf := float64(n)
	z2 := z * z
	denom := 1 + z2/nf
	center := (p + z2/(2*nf)) / denom
	margin := (z * math.Sqrt(p*(1-p)/nf+z2/(4*nf*nf))) / denom
	lo := center - margin
	hi := center + margin
	return Interval{Point: p, Lo: clampUnit(lo), Hi: clampUnit(hi)}
}

// NewcombeDiff computes the Newcombe (score-based) confidence interval for the
// DIFFERENCE of two independent proportions (candidate − baseline). It composes
// the two Wilson intervals — the correct small-sample method, avoiding the
// normal-approximation the modelvalue "parity" verdict used. Point =
// candP − baseP.
func NewcombeDiff(candPasses, candN, basePasses, baseN int, z float64) Interval {
	if candN <= 0 || baseN <= 0 {
		return Interval{}
	}
	c := Wilson(candPasses, candN, z)
	b := Wilson(basePasses, baseN, z)
	diff := c.Point - b.Point
	lo := diff - math.Sqrt(sq(c.Point-c.Lo)+sq(b.Hi-b.Point))
	hi := diff + math.Sqrt(sq(c.Hi-c.Point)+sq(b.Point-b.Lo))
	return Interval{Point: diff, Lo: clampDiff(lo), Hi: clampDiff(hi)}
}

// TaskPair holds one task's paired outcome between a candidate and a baseline
// config: each config's pass count and attempt count on that same task.
type TaskPair struct {
	TaskID     string
	CandPasses int
	CandN      int
	BasePasses int
	BaseN      int
}

// PairedDelta computes the task-BLOCKED mean of per-task (candidate −
// baseline) success-rate deltas plus a bootstrap CI over tasks (plan §3.5). The
// bootstrap is deterministic (fixed seed) so a report is reproducible. Treating
// task as a block avoids the Simpson's-paradox trap of pooling repeats across
// heterogeneous tasks. Tasks where either config has no attempts are skipped.
func PairedDelta(pairs []TaskPair, iters int) (Interval, int) {
	deltas := make([]float64, 0, len(pairs))
	for _, p := range pairs {
		if p.CandN == 0 || p.BaseN == 0 {
			continue
		}
		cr := float64(p.CandPasses) / float64(p.CandN)
		br := float64(p.BasePasses) / float64(p.BaseN)
		deltas = append(deltas, cr-br)
	}
	if len(deltas) == 0 {
		return Interval{}, 0
	}
	mean := meanF(deltas)
	if len(deltas) == 1 || iters <= 0 {
		return Interval{Point: mean, Lo: mean, Hi: mean}, len(deltas)
	}
	rng := rand.New(rand.NewSource(0x5EED)) //nolint:gosec // statistical bootstrap resampling — crypto randomness unnecessary, deterministic seeding desirable
	boot := make([]float64, iters)
	for i := 0; i < iters; i++ {
		var sum float64
		for j := 0; j < len(deltas); j++ {
			sum += deltas[rng.Intn(len(deltas))]
		}
		boot[i] = sum / float64(len(deltas))
	}
	sort.Float64s(boot)
	lo := percentile(boot, 2.5)
	hi := percentile(boot, 97.5)
	return Interval{Point: mean, Lo: lo, Hi: hi}, len(deltas)
}

// Verdict is the honest, cost-aware comparison label (plan §3.5). NEVER a bare
// "parity" — that over-claimed.
type Verdict string

const (
	// VerdictCheaperNonInferior — the candidate's success rate is non-inferior
	// to the baseline within the pre-declared margin AND it is cheaper per
	// successful completion.
	VerdictCheaperNonInferior Verdict = "candidate_cheaper_noninferior"
	// VerdictWorse — the candidate is inferior beyond the margin (the diff CI
	// upper bound is still below −margin).
	VerdictWorse Verdict = "candidate_worse"
	// VerdictNoDetectedDifference — non-inferior within the margin but NOT
	// cheaper (or the margin was not pre-declared): no compelling reason to
	// switch. The honest label when a directional difference isn't shown.
	VerdictNoDetectedDifference Verdict = "no_detected_difference"
	// VerdictInconclusive — below the pre-registered sample floor, or the
	// margin falls inside the diff CI (can conclude neither non-inferior nor
	// worse).
	VerdictInconclusive Verdict = "inconclusive"
	// VerdictInsufficientDistinctTasks — the comparison has too few DISTINCT
	// paired tasks (independent blocks) to support any directional verdict,
	// even if the raw attempt count clears the sample floor (audit P0.4). N
	// repeats over 2 tasks is 2 blocks, not N.
	VerdictInsufficientDistinctTasks Verdict = "insufficient_distinct_tasks"
)

// noninferiorityRule is one ordered decision row (CLAUDE.md #5). The engine
// walks these top-down; the first matching rule wins.
type noninferiorityRule struct {
	name  string
	match func(in verdictInput) bool
	label Verdict
}

// verdictInput is the resolved evidence a verdict rule tests.
type verdictInput struct {
	candN, baseN int
	minSample    int
	// distinctTasks is the number of paired tasks (independent blocks) the
	// comparison rests on; minDistinctTasks is the floor below which no
	// directional verdict is declared (0 = floor disabled).
	distinctTasks    int
	minDistinctTasks int
	margin           float64 // pre-declared; 0 = not declared
	diffLo           float64 // Newcombe lower bound (cand − base)
	diffHi           float64 // Newcombe upper bound
	cheaper          bool    // candidate cost-per-success < baseline (both defined)
}

// verdictRules is the ordered rule set. Below-floor and undeclared-margin cases
// short-circuit to honest non-directional labels before any margin logic runs.
var verdictRules = []noninferiorityRule{
	{
		name:  "below_sample_floor",
		match: func(in verdictInput) bool { return in.candN < in.minSample || in.baseN < in.minSample },
		label: VerdictInconclusive,
	},
	{
		// Too few independent task-blocks — the attempt count may clear the
		// sample floor, but the paired analysis rests on distinct tasks.
		name:  "below_task_floor",
		match: func(in verdictInput) bool { return in.minDistinctTasks > 0 && in.distinctTasks < in.minDistinctTasks },
		label: VerdictInsufficientDistinctTasks,
	},
	{
		// No pre-declared margin ⇒ we can only report a directional difference
		// or its absence, never a non-inferiority claim (plan §3.5 / finding 3).
		name:  "no_margin_worse",
		match: func(in verdictInput) bool { return in.margin <= 0 && in.diffHi < 0 },
		label: VerdictWorse,
	},
	{
		name:  "no_margin_nodiff",
		match: func(in verdictInput) bool { return in.margin <= 0 },
		label: VerdictNoDetectedDifference,
	},
	{
		// Definitively worse: even the best case (upper bound) is below the
		// margin.
		name:  "worse_beyond_margin",
		match: func(in verdictInput) bool { return in.diffHi < -in.margin },
		label: VerdictWorse,
	},
	{
		// Non-inferior established (lower bound above −margin) AND cheaper.
		name:  "cheaper_noninferior",
		match: func(in verdictInput) bool { return in.diffLo > -in.margin && in.cheaper },
		label: VerdictCheaperNonInferior,
	},
	{
		// Non-inferior established but not cheaper.
		name:  "noninferior_not_cheaper",
		match: func(in verdictInput) bool { return in.diffLo > -in.margin },
		label: VerdictNoDetectedDifference,
	},
}

// ClassifyVerdict resolves the comparison label by walking the ordered rule
// set. The margin falling inside the diff CI (neither worse nor non-inferior
// established) drops through to inconclusive.
func ClassifyVerdict(in verdictInput) Verdict {
	for _, r := range verdictRules {
		if r.match(in) {
			return r.label
		}
	}
	return VerdictInconclusive
}

// CostPerSuccess is the expected cost per successful completion (plan §3.5):
// totalSpend / passCount. Defined is false when no attempt succeeded (the
// figure is censored/undefined, never silently reported as 0).
func CostPerSuccess(totalSpendUSD float64, passes int) (value float64, defined bool) {
	if passes <= 0 {
		return 0, false
	}
	return totalSpendUSD / float64(passes), true
}

// MedianIQR returns the median and the [25th, 75th] percentile bounds of the
// samples (cost distribution is skewed — report median+IQR alongside mean±SD,
// plan §3.5). Empty input yields zeros.
func MedianIQR(samples []float64) (median, q1, q3 float64) {
	if len(samples) == 0 {
		return 0, 0, 0
	}
	s := append([]float64(nil), samples...)
	sort.Float64s(s)
	return percentile(s, 50), percentile(s, 25), percentile(s, 75)
}

// --- pure helpers ---

func sq(x float64) float64 { return x * x }

func clampUnit(x float64) float64 {
	switch {
	case x < 0:
		return 0
	case x > 1:
		return 1
	default:
		return x
	}
}

func clampDiff(x float64) float64 {
	switch {
	case x < -1:
		return -1
	case x > 1:
		return 1
	default:
		return x
	}
}

func meanF(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

// percentile returns the p-th percentile (0..100) of a SORTED slice using
// linear interpolation between the closest ranks.
func percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}
	rank := (p / 100) * float64(n-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	if lo == hi {
		return sorted[lo]
	}
	frac := rank - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}
