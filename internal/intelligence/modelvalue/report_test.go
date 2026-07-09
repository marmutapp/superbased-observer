package modelvalue

import (
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/routing"
)

var t0 = time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)

// mkTurn builds a proxy-observed turn n minutes after t0.
func mkTurn(session, model string, minute int, projectID int64) TurnRow {
	return TurnRow{
		SessionID: session, ProjectID: projectID, ProjectRoot: "/repo",
		Timestamp: t0.Add(time.Duration(minute) * time.Minute),
		Model:     model, Input: 1000, Output: 500,
		MessageCount: 10, ToolUseCount: 5,
		TTFTMs: 500, TotalMs: int64(1000 + minute), HasLatency: true,
		HTTPStatus: 200, HasStatus: true,
		StoredCostUSD: 0.01,
	}
}

// mkAction builds an action n minutes (+sec seconds) after t0.
func mkAction(session, actionType string, minute, sec int, success bool) ActionRow {
	return ActionRow{
		SessionID: session, Type: actionType, Success: success,
		Timestamp: t0.Add(time.Duration(minute)*time.Minute + time.Duration(sec)*time.Second),
	}
}

// flatPrice prices every model at $0.10/turn so cost assertions are easy.
func flatPrice(string, routing.PromptUsage) (float64, bool) { return 0.10, true }

// TestBuild_CellAggregation pins the core per-cell math: volume, cost
// via the injected PriceFn, latency percentiles over graded rows, error
// and tool-failure rates over their own graded denominators.
func TestBuild_CellAggregation(t *testing.T) {
	t.Parallel()
	f := &Facts{
		WindowDays:  30,
		GeneratedAt: t0.Add(24 * time.Hour),
		Price:       flatPrice,
		Turns: []TurnRow{
			mkTurn("s1", "claude-opus-4-8", 0, 1),
			mkTurn("s1", "claude-opus-4-8", 10, 1),
		},
		Actions: []ActionRow{
			// Window before turn 1: reads (classifies read_only).
			mkAction("s1", models.ActionReadFile, -1, 0, true),
			// Outcome window of turn 1 (between turn 1 and turn 2):
			// 3 tool actions, 1 failure. Also the recent window of turn 2
			// — read-only types keep turn 2 read_only as well.
			mkAction("s1", models.ActionReadFile, 5, 0, true),
			mkAction("s1", models.ActionReadFile, 6, 0, false),
			mkAction("s1", models.ActionSearchText, 7, 0, true),
		},
	}
	rep := Build(f, Options{})

	if rep.Caveat == "" {
		t.Error("report missing the attribution caveat")
	}
	if len(rep.Cells) != 1 {
		t.Fatalf("cells = %d, want 1 (%+v)", len(rep.Cells), rep.Cells)
	}
	c := rep.Cells[0]
	if c.Model != "claude-opus-4-8" || c.TurnKind != routing.TurnReadOnly || c.ProjectID != 1 {
		t.Errorf("cell identity = (%s, %s, %d)", c.Model, c.TurnKind, c.ProjectID)
	}
	if c.Tier != routing.TierOpusClass {
		t.Errorf("cell tier = %s, want opus-class", c.Tier)
	}
	if c.Turns != 2 {
		t.Errorf("turns = %d, want 2", c.Turns)
	}
	if c.CostUSD != 0.20 || c.CostPerTurn != 0.10 {
		t.Errorf("cost = %v / %v, want 0.20 / 0.10", c.CostUSD, c.CostPerTurn)
	}
	if c.ErrorGraded != 2 || c.ErrorCount != 0 || c.ErrorRate != 0 {
		t.Errorf("errors = %d/%d rate %v", c.ErrorCount, c.ErrorGraded, c.ErrorRate)
	}
	if c.ToolActions != 3 || c.ToolFailures != 1 {
		t.Errorf("tool outcome = %d/%d, want 1/3", c.ToolFailures, c.ToolActions)
	}
	if c.LatencyGraded != 2 || c.LatencyP50Ms != 1000 || c.LatencyP95Ms != 1010 {
		t.Errorf("latency = graded %d p50 %d p95 %d", c.LatencyGraded, c.LatencyP50Ms, c.LatencyP95Ms)
	}
	// Global roll-up mirrors the single project here.
	if len(rep.GlobalCells) != 1 || rep.GlobalCells[0].ProjectID != 0 || rep.GlobalCells[0].Turns != 2 {
		t.Errorf("global cells = %+v", rep.GlobalCells)
	}
}

// TestBuild_PriceFallback pins the fallback chain: PriceFn miss → the
// row's stored insert-time cost.
func TestBuild_PriceFallback(t *testing.T) {
	t.Parallel()
	miss := func(string, routing.PromptUsage) (float64, bool) { return 0, false }
	f := &Facts{
		GeneratedAt: t0,
		Price:       miss,
		Turns:       []TurnRow{mkTurn("s1", "claude-opus-4-8", 0, 1)},
	}
	rep := Build(f, Options{})
	if len(rep.Cells) != 1 || rep.Cells[0].CostUSD != 0.01 {
		t.Errorf("fallback cost = %+v, want stored 0.01", rep.Cells)
	}
	// Nil PriceFn degrades identically.
	f.Price = nil
	rep = Build(f, Options{})
	if rep.Cells[0].CostUSD != 0.01 {
		t.Errorf("nil-price cost = %v, want 0.01", rep.Cells[0].CostUSD)
	}
}

// TestBuild_PhaseHintClassifiesPlan pins the client-declared-phase wiring:
// a permission-mode "plan" marker before the turn classifies it plan.
func TestBuild_PhaseHintClassifiesPlan(t *testing.T) {
	t.Parallel()
	planMarker := ActionRow{
		SessionID: "s1", Type: models.ActionPermissionMode,
		Timestamp: t0.Add(-time.Minute), Success: true, PhaseHint: "plan",
	}
	f := &Facts{
		GeneratedAt: t0,
		Turns:       []TurnRow{mkTurn("s1", "claude-opus-4-8", 0, 1)},
		Actions:     []ActionRow{planMarker, mkAction("s1", models.ActionReadFile, -1, 30, true)},
	}
	rep := Build(f, Options{})
	if len(rep.Cells) != 1 || rep.Cells[0].TurnKind != routing.TurnPlan {
		t.Errorf("cells = %+v, want one plan cell", rep.Cells)
	}
	// A later non-plan marker clears the phase.
	f.Actions = append(f.Actions, ActionRow{
		SessionID: "s1", Type: models.ActionPermissionMode,
		Timestamp: t0.Add(-30 * time.Second), Success: true, PhaseHint: "default",
	})
	rep = Build(f, Options{})
	if rep.Cells[0].TurnKind == routing.TurnPlan {
		t.Error("phase not cleared by a later default marker")
	}
}

// TestBuild_SidechainAttribution pins the sub-agent approximation: a
// turn adjacent to sidechain actions classifies subagent.
func TestBuild_SidechainAttribution(t *testing.T) {
	t.Parallel()
	side := mkAction("s1", models.ActionReadFile, -1, 0, true)
	side.IsSidechain = true
	f := &Facts{
		GeneratedAt: t0,
		Turns:       []TurnRow{mkTurn("s1", "claude-opus-4-8", 0, 1)},
		Actions:     []ActionRow{side},
	}
	rep := Build(f, Options{})
	if len(rep.Cells) != 1 || rep.Cells[0].TurnKind != routing.TurnSubagent {
		t.Errorf("cells = %+v, want one subagent cell", rep.Cells)
	}
}

// TestBuild_Deltas pins baseline selection (highest tier, then volume)
// and the verdict floors: error-graded evidence on both sides grades on
// error rate; thin grading degrades to insufficient_evidence.
func TestBuild_Deltas(t *testing.T) {
	t.Parallel()
	f := &Facts{GeneratedAt: t0, Price: flatPrice}
	// 60 opus turns (baseline) and 60 haiku turns, same project + kind.
	for i := 0; i < 60; i++ {
		opus := mkTurn("s1", "claude-opus-4-8", i*10, 1)
		haiku := mkTurn("s2", "claude-haiku-4-5", i*10, 1)
		if i < 3 {
			haiku.HTTPStatus = 500 // 5% error rate on the candidate
		}
		f.Turns = append(f.Turns, opus, haiku)
		f.Actions = append(
			f.Actions,
			mkAction("s1", models.ActionReadFile, i*10-1, 0, true),
			mkAction("s2", models.ActionReadFile, i*10-1, 0, true),
		)
	}
	rep := Build(f, Options{MinSample: 50})
	// One global delta (project 0) + one per-project delta — sorted
	// global-first. The single-project corpus makes them identical.
	if len(rep.Deltas) != 2 {
		t.Fatalf("deltas = %d, want 2 (%+v)", len(rep.Deltas), rep.Deltas)
	}
	if rep.Deltas[0].ProjectID != 0 || rep.Deltas[1].ProjectID != 1 {
		t.Errorf("delta projects = %d/%d, want 0/1", rep.Deltas[0].ProjectID, rep.Deltas[1].ProjectID)
	}
	d := rep.Deltas[0]
	if d.BaselineModel != "claude-opus-4-8" || d.CandidateModel != "claude-haiku-4-5" {
		t.Errorf("baseline/candidate = %s/%s", d.BaselineModel, d.CandidateModel)
	}
	if d.NBaseline != 60 || d.NCandidate != 60 {
		t.Errorf("n = %d/%d, want 60/60", d.NBaseline, d.NCandidate)
	}
	if d.VerdictBasis != "error_rate" {
		t.Errorf("basis = %q, want error_rate", d.VerdictBasis)
	}
	if d.DeltaErrorRate <= 0 {
		t.Errorf("delta error rate = %v, want positive (candidate worse)", d.DeltaErrorRate)
	}
	// 5pp at n=60 vs 0: CI ≈ ±5.5pp → interval includes zero → parity.
	if d.Verdict != VerdictParity {
		t.Errorf("verdict = %q, want parity (CI includes zero)", d.Verdict)
	}
}

// TestBuild_DeltaInsufficientEvidence pins the floor: below MinSample on
// either side, no claim is made.
func TestBuild_DeltaInsufficientEvidence(t *testing.T) {
	t.Parallel()
	f := &Facts{GeneratedAt: t0, Price: flatPrice}
	for i := 0; i < 10; i++ {
		f.Turns = append(f.Turns,
			mkTurn("s1", "claude-opus-4-8", i*10, 1),
			mkTurn("s2", "claude-haiku-4-5", i*10, 1))
		f.Actions = append(f.Actions,
			mkAction("s1", models.ActionReadFile, i*10-1, 0, true),
			mkAction("s2", models.ActionReadFile, i*10-1, 0, true))
	}
	rep := Build(f, Options{MinSample: 50})
	if len(rep.Deltas) != 2 { // global + per-project pair
		t.Fatalf("deltas = %d, want 2", len(rep.Deltas))
	}
	for _, d := range rep.Deltas {
		if d.Verdict != VerdictInsufficientEvidence || d.VerdictBasis != "" {
			t.Errorf("verdict = (%q, %q), want (insufficient_evidence, \"\")", d.Verdict, d.VerdictBasis)
		}
	}
}

// TestBuild_Deterministic pins §R9.3 determinism end to end: same Facts,
// same Report — including slice ordering.
func TestBuild_Deterministic(t *testing.T) {
	t.Parallel()
	f := &Facts{GeneratedAt: t0, Price: flatPrice}
	for i := 0; i < 20; i++ {
		f.Turns = append(f.Turns,
			mkTurn("s1", "claude-opus-4-8", i*10, 1),
			mkTurn("s2", "claude-haiku-4-5", i*10, 2),
			mkTurn("s3", "gpt-5.4", i*10, 1))
		f.Actions = append(f.Actions,
			mkAction("s1", models.ActionEditFile, i*10-1, 0, true),
			mkAction("s2", models.ActionReadFile, i*10-1, 0, true),
			mkAction("s3", models.ActionRunCommand, i*10-1, 0, i%2 == 0))
	}
	first := Build(f, Options{})
	for run := 0; run < 5; run++ {
		got := Build(f, Options{})
		if len(got.Cells) != len(first.Cells) || len(got.Deltas) != len(first.Deltas) {
			t.Fatalf("run %d: shape diverged", run)
		}
		for i := range got.Cells {
			if got.Cells[i] != first.Cells[i] {
				t.Fatalf("run %d: cell %d diverged: %+v != %+v", run, i, got.Cells[i], first.Cells[i])
			}
		}
		for i := range got.Deltas {
			if got.Deltas[i] != first.Deltas[i] {
				t.Fatalf("run %d: delta %d diverged", run, i)
			}
		}
	}
}

// TestPercentile covers the nearest-rank math at the edges.
func TestPercentile(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		vals []int64
		p    float64
		want int64
	}{
		{"empty", nil, 0.5, 0},
		{"single", []int64{7}, 0.95, 7},
		{"p50_of_four", []int64{4, 1, 3, 2}, 0.50, 2},
		{"p95_of_ten", []int64{10, 9, 8, 7, 6, 5, 4, 3, 2, 1}, 0.95, 10},
		{"p50_odd", []int64{3, 1, 2}, 0.50, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := percentile(tc.vals, tc.p); got != tc.want {
				t.Errorf("percentile(%v, %v) = %d, want %d", tc.vals, tc.p, got, tc.want)
			}
		})
	}
}
