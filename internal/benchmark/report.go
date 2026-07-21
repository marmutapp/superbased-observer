package benchmark

import (
	"math"
	"sort"
	"strconv"
)

// Report is the honest, all-cells-reported comparison of a benchmark run
// (plan §3.5). Every config is present; verdicts use the non-inferiority
// vocabulary and are suppressed below the pre-registered sample floor; N is
// shown at every level. Cost figures here are the RUN-TIME (snapshot) billed
// cost — the store/CLI boundary adds the "repriced at current rates" view
// (§3.11), which needs the cost engine and so lives outside this pure package.
type Report struct {
	SpecName  string
	RunID     string
	Baseline  string  // baseline config id (pre-registered)
	Margin    float64 // pre-declared non-inferiority margin (0 = none)
	MinSample int
	Configs   []ConfigReport
	// Comparisons is one row per non-baseline config vs the baseline.
	Comparisons []Comparison
	// StatusCensus is the run-wide count of every terminal attempt status —
	// nothing dropped from the denominator.
	StatusCensus map[Status]int
	Warnings     []string
}

// ConfigReport is one matrix row (a harness × model config) with N at every
// level and both success measures (intention-to-test + model-eligible).
type ConfigReport struct {
	ConfigID string
	Harness  string
	Model    string

	// N at every level (plan §3.5): planned / executed / scored / passed /
	// sessions / tasks.
	Planned  int
	Executed int
	Scored   int
	Passed   int
	Sessions int
	Tasks    int

	// Intention-to-test success (all executed attempts) with Wilson CI.
	SuccessRate float64
	SuccessCI   Interval

	// Model-eligible success (excluding infra failures, plan §3.6).
	ModelEligibleN      int
	ModelEligiblePassed int
	ModelEligibleRate   float64
	ModelEligibleCI     Interval

	// Cost — snapshot billed. Mean±SD AND median+IQR (cost is skewed).
	TotalSpendUSD         float64
	MeanCostPerAttempt    float64
	SDCostUSD             float64
	MedianCostUSD         float64
	IQRLoUSD              float64
	IQRHiUSD              float64
	CostPerSuccessUSD     float64
	CostPerSuccessDefined bool
	// CostSamples is the raw per-attempt cost — rendered as dots, never hidden.
	CostSamples []float64

	MeanWallMS float64

	// Tokens.
	InputTokens     int64
	OutputTokens    int64
	CacheReadTokens int64
	CacheReadPct    float64

	StatusCensus map[Status]int
}

// Comparison is one candidate-vs-baseline verdict with its full evidence.
type Comparison struct {
	Candidate string
	Baseline  string

	// DiffCI is the Newcombe CI for (candidate − baseline) success rate.
	DiffCI Interval
	// PairedDelta is the task-blocked mean paired delta + bootstrap CI.
	PairedDelta    Interval
	PairedTasks    int
	Cheaper        bool
	CostDeltaUSD   float64 // candidate cost-per-success − baseline (when both defined)
	CostComparable bool
	Verdict        Verdict
}

// ComputeReport aggregates attempt facts into the honest matrix + verdicts. It
// is pure: the store seam loads the facts (deriving cost from api_turns) and
// passes them in. Configs and their planned counts come from the spec so a
// config that produced zero attempts still appears (nothing hidden).
func ComputeReport(spec Spec, runID string, facts []AttemptFact) Report {
	rep := Report{
		SpecName:     spec.Name,
		RunID:        runID,
		Baseline:     spec.Analysis.BaselineConfig,
		Margin:       spec.Analysis.NonInferiorityMargin,
		MinSample:    spec.Analysis.MinSample,
		StatusCensus: map[Status]int{},
	}

	byConfig := map[string][]AttemptFact{}
	for _, f := range facts {
		byConfig[f.ConfigID] = append(byConfig[f.ConfigID], f)
		rep.StatusCensus[f.Status]++
	}

	plannedPerConfig := len(spec.Tasks) * spec.Repeats
	for _, c := range spec.Configs {
		rep.Configs = append(rep.Configs, buildConfigReport(c, plannedPerConfig, byConfig[c.ID]))
	}

	// Comparisons: every non-baseline config vs the baseline.
	base := byConfig[spec.Analysis.BaselineConfig]
	basePasses, baseN := passCount(base)
	baseCPS, baseDefined := configCostPerSuccess(base)
	for _, c := range spec.Configs {
		if c.ID == spec.Analysis.BaselineConfig {
			continue
		}
		cand := byConfig[c.ID]
		rep.Comparisons = append(rep.Comparisons,
			buildComparison(c.ID, spec.Analysis.BaselineConfig, spec.Analysis, cand, base,
				basePasses, baseN, baseCPS, baseDefined))
	}

	rep.Warnings = buildWarnings(spec, rep)
	return rep
}

func buildConfigReport(c Config, planned int, facts []AttemptFact) ConfigReport {
	cr := ConfigReport{
		ConfigID:     c.ID,
		Harness:      c.Harness,
		Model:        c.Model,
		Planned:      planned,
		Executed:     len(facts),
		StatusCensus: map[Status]int{},
	}
	taskSet := map[string]bool{}
	var wallSum float64
	var wallN int
	for _, f := range facts {
		cr.StatusCensus[f.Status]++
		taskSet[f.TaskID] = true
		cr.Sessions += f.Sessions
		if f.Scored {
			cr.Scored++
			if f.Passed {
				cr.Passed++
			}
		}
		if f.Status.ModelEligible() {
			cr.ModelEligibleN++
			if f.Scored && f.Passed {
				cr.ModelEligiblePassed++
			}
		}
		cr.TotalSpendUSD += f.CostUSD
		cr.CostSamples = append(cr.CostSamples, f.CostUSD)
		cr.InputTokens += f.InputTokens
		cr.OutputTokens += f.OutputTokens
		cr.CacheReadTokens += f.CacheReadTokens
		if f.WallMS > 0 {
			wallSum += float64(f.WallMS)
			wallN++
		}
	}
	cr.Tasks = len(taskSet)

	if cr.Executed > 0 {
		cr.SuccessRate = float64(cr.Passed) / float64(cr.Executed)
		cr.MeanCostPerAttempt = cr.TotalSpendUSD / float64(cr.Executed)
	}
	cr.SuccessCI = Wilson(cr.Passed, cr.Executed, z95)
	if cr.ModelEligibleN > 0 {
		cr.ModelEligibleRate = float64(cr.ModelEligiblePassed) / float64(cr.ModelEligibleN)
	}
	cr.ModelEligibleCI = Wilson(cr.ModelEligiblePassed, cr.ModelEligibleN, z95)

	cr.SDCostUSD = stddev(cr.CostSamples)
	cr.MedianCostUSD, cr.IQRLoUSD, cr.IQRHiUSD = MedianIQR(cr.CostSamples)
	cr.CostPerSuccessUSD, cr.CostPerSuccessDefined = CostPerSuccess(cr.TotalSpendUSD, cr.Passed)
	if wallN > 0 {
		cr.MeanWallMS = wallSum / float64(wallN)
	}
	if denom := cr.InputTokens + cr.CacheReadTokens; denom > 0 {
		cr.CacheReadPct = 100 * float64(cr.CacheReadTokens) / float64(denom)
	}
	return cr
}

func buildComparison(candID, baseID string, an Analysis, cand, base []AttemptFact,
	basePasses, baseN int, baseCPS float64, baseDefined bool,
) Comparison {
	candPasses, candN := passCount(cand)
	candCPS, candDefined := configCostPerSuccess(cand)

	cmp := Comparison{
		Candidate: candID,
		Baseline:  baseID,
		DiffCI:    NewcombeDiff(candPasses, candN, basePasses, baseN, z95),
	}
	cmp.PairedDelta, cmp.PairedTasks = PairedDelta(buildTaskPairs(cand, base), 2000)

	cmp.CostComparable = candDefined && baseDefined
	if cmp.CostComparable {
		cmp.CostDeltaUSD = candCPS - baseCPS
		cmp.Cheaper = candCPS < baseCPS
	}

	cmp.Verdict = ClassifyVerdict(verdictInput{
		candN:            candN,
		baseN:            baseN,
		minSample:        an.MinSample,
		distinctTasks:    cmp.PairedTasks,
		minDistinctTasks: an.MinTasks,
		margin:           an.NonInferiorityMargin,
		diffLo:           cmp.DiffCI.Lo,
		diffHi:           cmp.DiffCI.Hi,
		cheaper:          cmp.Cheaper,
	})
	return cmp
}

// buildTaskPairs groups two configs' facts by task into paired outcomes.
func buildTaskPairs(cand, base []AttemptFact) []TaskPair {
	type acc struct{ passes, n int }
	candBy := map[string]*acc{}
	baseBy := map[string]*acc{}
	tally := func(m map[string]*acc, f AttemptFact) {
		a := m[f.TaskID]
		if a == nil {
			a = &acc{}
			m[f.TaskID] = a
		}
		a.n++
		if f.Scored && f.Passed {
			a.passes++
		}
	}
	for _, f := range cand {
		tally(candBy, f)
	}
	for _, f := range base {
		tally(baseBy, f)
	}
	// Union of task ids, stable order.
	taskIDs := map[string]bool{}
	for id := range candBy {
		taskIDs[id] = true
	}
	for id := range baseBy {
		taskIDs[id] = true
	}
	ordered := make([]string, 0, len(taskIDs))
	for id := range taskIDs {
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)
	pairs := make([]TaskPair, 0, len(ordered))
	for _, id := range ordered {
		p := TaskPair{TaskID: id}
		if a := candBy[id]; a != nil {
			p.CandPasses, p.CandN = a.passes, a.n
		}
		if a := baseBy[id]; a != nil {
			p.BasePasses, p.BaseN = a.passes, a.n
		}
		pairs = append(pairs, p)
	}
	return pairs
}

func buildWarnings(spec Spec, rep Report) []string {
	var w []string
	for _, cr := range rep.Configs {
		if cr.Executed == 0 {
			w = append(w, "config "+cr.ConfigID+" produced zero attempts")
			continue
		}
		if cr.Executed < spec.Analysis.MinSample {
			w = append(w, "config "+cr.ConfigID+" below sample floor — ranking suppressed, verdict inconclusive")
		}
		// Wide CI from tiny N must not imply a stable ranking.
		if width := cr.SuccessCI.Hi - cr.SuccessCI.Lo; width > 0.5 {
			w = append(w, "config "+cr.ConfigID+" success CI is wide (>0.5) — treat any ranking cautiously")
		}
		if setupErr := cr.StatusCensus[StatusSetupError]; setupErr > 0 {
			w = append(w, "config "+cr.ConfigID+" had setup failures — a flaky corpus row can masquerade as a model difference")
		}
	}
	for _, cmp := range rep.Comparisons {
		if cmp.Verdict == VerdictInsufficientDistinctTasks {
			w = append(w, "comparison "+cmp.Candidate+" vs "+cmp.Baseline+" rests on only "+
				strconv.Itoa(cmp.PairedTasks)+" distinct task(s) (floor "+strconv.Itoa(spec.Analysis.MinTasks)+
				") — verdict suppressed; add more distinct tasks, repeats alone don't count")
		}
	}
	return w
}

// --- pure helpers ---

func passCount(facts []AttemptFact) (passes, n int) {
	for _, f := range facts {
		n++
		if f.Scored && f.Passed {
			passes++
		}
	}
	return passes, n
}

func configCostPerSuccess(facts []AttemptFact) (float64, bool) {
	var total float64
	var passes int
	for _, f := range facts {
		total += f.CostUSD
		if f.Scored && f.Passed {
			passes++
		}
	}
	return CostPerSuccess(total, passes)
}

func stddev(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	m := meanF(xs)
	var ss float64
	for _, x := range xs {
		ss += (x - m) * (x - m)
	}
	return math.Sqrt(ss / float64(len(xs)-1))
}
