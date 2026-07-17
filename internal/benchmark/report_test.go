package benchmark

import "testing"

func mkFacts(config, task string, n, passes int, cost float64, status Status) []AttemptFact {
	out := make([]AttemptFact, 0, n)
	for i := 0; i < n; i++ {
		f := AttemptFact{
			TaskID: task, ConfigID: config, Harness: "codex", Model: "m",
			RepeatIdx: i, Status: status, WallMS: 30000, Turns: 3,
			Sessions: 1, CostUSD: cost, InputTokens: 1000, OutputTokens: 100,
			CacheReadTokens: 500, Scored: true,
		}
		if i < passes {
			f.Passed = true
		}
		out = append(out, f)
	}
	return out
}

func twoConfigSpec(t *testing.T, margin float64, minSample int) Spec {
	t.Helper()
	s := Spec{
		Name:    "r",
		Repeats: 5,
		// Unlimited budget so Validate passes (these are pure report tests, no
		// spend); MinTasks 0 leaves the distinct-task floor OFF so the 2-task
		// fixtures exercise the verdict machinery directly.
		Budget: Budget{Unlimited: true},
		Analysis: Analysis{
			BaselineConfig: "base", NonInferiorityMargin: margin, MinSample: minSample,
		},
		Tasks: []Task{
			{ID: "A", Repo: "r", Prompt: "p", Success: Success{Scorer: "tests_pass", Cmd: "x"}},
			{ID: "B", Repo: "r", Prompt: "p", Success: Success{Scorer: "tests_pass", Cmd: "x"}},
		},
		Configs: []Config{
			{ID: "base", Harness: "codex", Model: "m1"},
			{ID: "cand", Harness: "codex", Model: "m2"},
		},
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("spec invalid: %v", err)
	}
	return s
}

func TestComputeReportBasic(t *testing.T) {
	t.Parallel()
	spec := twoConfigSpec(t, 0.15, 3)
	var facts []AttemptFact
	// 20 attempts/task/config so the Newcombe diff CI is tight enough to
	// establish non-inferiority within a 0.15 margin. Equal success rate,
	// candidate half the cost.
	facts = append(facts, mkFacts("base", "A", 20, 20, 0.10, StatusOK)...)
	facts = append(facts, mkFacts("base", "B", 20, 20, 0.10, StatusOK)...)
	facts = append(facts, mkFacts("cand", "A", 20, 20, 0.05, StatusOK)...)
	facts = append(facts, mkFacts("cand", "B", 20, 20, 0.05, StatusOK)...)

	rep := ComputeReport(spec, "run-x", facts)
	if len(rep.Configs) != 2 {
		t.Fatalf("want 2 config reports, got %d", len(rep.Configs))
	}
	base := findConfig(rep, "base")
	if base.Executed != 40 || base.Passed != 40 || base.Tasks != 2 {
		t.Errorf("base census wrong: %+v", base)
	}
	if base.Planned != 2*5 {
		t.Errorf("base planned = %d, want 10", base.Planned)
	}
	if !approx(base.SuccessRate, 1.0, 1e-9) {
		t.Errorf("base rate = %v", base.SuccessRate)
	}
	if !base.CostPerSuccessDefined || !approx(base.CostPerSuccessUSD, 4.0/40, 1e-9) {
		t.Errorf("base cost/success = %v defined=%v", base.CostPerSuccessUSD, base.CostPerSuccessDefined)
	}
	// One comparison (cand vs base); cand cheaper + same rate → cheaper_noninferior.
	if len(rep.Comparisons) != 1 {
		t.Fatalf("want 1 comparison, got %d", len(rep.Comparisons))
	}
	c := rep.Comparisons[0]
	if c.Candidate != "cand" || !c.Cheaper {
		t.Errorf("comparison = %+v", c)
	}
	if c.Verdict != VerdictCheaperNonInferior {
		t.Errorf("verdict = %q, want cheaper_noninferior", c.Verdict)
	}
}

// TestSimpsonsParadoxGuard proves the paired/block analysis diverges from the
// naive pooled diff when task attempt counts are unbalanced — the pooled diff
// favors the baseline while the candidate wins EVERY task (plan §3.5).
func TestSimpsonsParadoxGuard(t *testing.T) {
	t.Parallel()
	spec := twoConfigSpec(t, 0.15, 3)
	var facts []AttemptFact
	// base pooled: A 9/10 + B 0/2 = 9/12 (.75)
	facts = append(facts, mkFacts("base", "A", 10, 9, 0.01, StatusOK)...)
	facts = append(facts, mkFacts("base", "B", 2, 0, 0.01, StatusOK)...)
	// cand pooled: A 2/2 + B 3/10 = 5/12 (.417) — but wins BOTH tasks per-task.
	facts = append(facts, mkFacts("cand", "A", 2, 2, 0.01, StatusOK)...)
	facts = append(facts, mkFacts("cand", "B", 10, 3, 0.01, StatusOK)...)

	rep := ComputeReport(spec, "run-simpson", facts)
	c := rep.Comparisons[0]
	// Pooled diff (Newcombe) is NEGATIVE (baseline looks better pooled).
	if c.DiffCI.Point >= 0 {
		t.Errorf("pooled diff should be negative, got %v", c.DiffCI.Point)
	}
	// Paired (task-blocked) delta is POSITIVE (candidate better on each task).
	if c.PairedDelta.Point <= 0 {
		t.Errorf("paired delta should be positive (candidate wins each task), got %v", c.PairedDelta.Point)
	}
	if c.PairedTasks != 2 {
		t.Errorf("paired tasks = %d, want 2", c.PairedTasks)
	}
}

// TestDistinctTaskFloorGates pins #2(b): a run with plenty of ATTEMPTS (10 per
// config: 5 repeats × 2 tasks) that clears min_sample=5 is still gated
// "insufficient_distinct_tasks" because it rests on only 2 distinct tasks and
// the floor (MinTasks=3) is not met. A warning explains it.
func TestDistinctTaskFloorGates(t *testing.T) {
	t.Parallel()
	spec := twoConfigSpec(t, 0.15, 5)
	spec.Analysis.MinTasks = 3 // ParseSpec defaults this to 3; set it directly here.
	var facts []AttemptFact
	// 5 repeats × 2 tasks per config = 10 attempts each (> min_sample 5).
	facts = append(facts, mkFacts("base", "A", 5, 5, 0.10, StatusOK)...)
	facts = append(facts, mkFacts("base", "B", 5, 5, 0.10, StatusOK)...)
	facts = append(facts, mkFacts("cand", "A", 5, 5, 0.05, StatusOK)...)
	facts = append(facts, mkFacts("cand", "B", 5, 5, 0.05, StatusOK)...)

	rep := ComputeReport(spec, "run-tf", facts)
	c := rep.Comparisons[0]
	if c.Verdict != VerdictInsufficientDistinctTasks {
		t.Errorf("verdict = %q, want insufficient_distinct_tasks (2 tasks < floor 3)", c.Verdict)
	}
	if c.PairedTasks != 2 {
		t.Errorf("paired tasks = %d, want 2", c.PairedTasks)
	}
	foundWarn := false
	for _, w := range rep.Warnings {
		if contains(w, "distinct task") && contains(w, "verdict suppressed") {
			foundWarn = true
		}
	}
	if !foundWarn {
		t.Errorf("expected a distinct-task-floor warning, got %v", rep.Warnings)
	}

	// With the floor lowered to 2, the same evidence yields a real verdict —
	// proving the gate is the task count, not the attempt count.
	spec.Analysis.MinTasks = 2
	rep2 := ComputeReport(spec, "run-tf2", facts)
	if v := rep2.Comparisons[0].Verdict; v == VerdictInsufficientDistinctTasks {
		t.Errorf("floor=2 with 2 tasks should NOT gate, got %q", v)
	}
}

func TestReportZeroAttemptConfigStillPresent(t *testing.T) {
	t.Parallel()
	spec := twoConfigSpec(t, 0.1, 3)
	// Only base produced facts; cand produced none.
	facts := mkFacts("base", "A", 5, 5, 0.02, StatusOK)
	rep := ComputeReport(spec, "run-z", facts)
	cand := findConfig(rep, "cand")
	if cand.Executed != 0 {
		t.Errorf("cand should have 0 executed, got %d", cand.Executed)
	}
	// Verdict inconclusive (below floor) and a warning names the empty config.
	if rep.Comparisons[0].Verdict != VerdictInconclusive {
		t.Errorf("verdict = %q, want inconclusive", rep.Comparisons[0].Verdict)
	}
	foundWarn := false
	for _, w := range rep.Warnings {
		if contains(w, "cand") && contains(w, "zero attempts") {
			foundWarn = true
		}
	}
	if !foundWarn {
		t.Errorf("expected a zero-attempts warning for cand, got %v", rep.Warnings)
	}
}

func TestReportStatusCensusCountsFailures(t *testing.T) {
	t.Parallel()
	spec := twoConfigSpec(t, 0.1, 3)
	var facts []AttemptFact
	facts = append(facts, mkFacts("base", "A", 3, 2, 0.02, StatusOK)...)
	// A setup_error attempt: not model-eligible, but counted in the denominator.
	se := mkFacts("base", "A", 1, 0, 0, StatusSetupError)
	se[0].Scored = false
	facts = append(facts, se...)
	rep := ComputeReport(spec, "run-c", facts)
	if rep.StatusCensus[StatusSetupError] != 1 {
		t.Errorf("census missing setup_error: %+v", rep.StatusCensus)
	}
	base := findConfig(rep, "base")
	if base.Executed != 4 {
		t.Errorf("executed (intention-to-test) = %d, want 4 (setup_error counted)", base.Executed)
	}
	if base.ModelEligibleN != 3 {
		t.Errorf("model-eligible N = %d, want 3 (setup_error excluded)", base.ModelEligibleN)
	}
}

func findConfig(r Report, id string) ConfigReport {
	for _, c := range r.Configs {
		if c.ConfigID == id {
			return c
		}
	}
	return ConfigReport{}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
