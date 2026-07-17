package routing

import (
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
)

var simT0 = time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)

// simReadTurn builds an opus read_only SimTurn at minute m.
func simReadTurn(session string, m int) SimTurn {
	return SimTurn{
		SessionID: session,
		Timestamp: simT0.Add(time.Duration(m) * time.Minute),
		In: DecisionInput{
			Shape: TurnShape{Model: "claude-opus-4-8", MessageCount: 5, ToolUseCount: 3},
			Session: SessionState{
				RecentActions: []ActionSignal{{Type: models.ActionReadFile, Success: true}},
			},
			ObservedUsage: &PromptUsage{Input: 10_000, Output: 1_000},
		},
	}
}

// TestSimulate_CountsAndSavings pins the replay aggregates: every opus
// read turn beyond the stickiness window reroutes, savings accumulate,
// and the per-move table is populated.
func TestSimulate_CountsAndSavings(t *testing.T) {
	t.Parallel()
	p, _ := TemplateByName("value") // min-turns 5
	turns := make([]SimTurn, 0, 8)
	for i := 0; i < 8; i++ {
		turns = append(turns, simReadTurn("s1", i*10))
	}
	rep := Simulate(p, testSnapshot(), turns, nil)

	if rep.TurnsEvaluated != 8 {
		t.Errorf("evaluated = %d, want 8", rep.TurnsEvaluated)
	}
	// Turn 1 reroutes (never switched), turns 2–6 hold (stickiness
	// floor 5), turn 7 reroutes again, turn 8 holds.
	if rep.WouldReroute != 2 {
		t.Errorf("would reroute = %d, want 2 (stickiness floor in effect); report %+v", rep.WouldReroute, rep)
	}
	if rep.ByKind[TurnReadOnly] != 2 {
		t.Errorf("by-kind = %v", rep.ByKind)
	}
	if rep.ByReason[ReasonStickinessHold] != 6 {
		t.Errorf("stickiness holds = %d, want 6", rep.ByReason[ReasonStickinessHold])
	}
	if len(rep.ByMove) != 1 || rep.ByMove[0].From != "claude-opus-4-8" || rep.ByMove[0].To != "claude-haiku-4-5" || rep.ByMove[0].Count != 2 {
		t.Errorf("moves = %+v", rep.ByMove)
	}
	// Per-turn gross: (10k×5 + 1k×25 − 10k×1 − 1k×5)/1e6 = $0.06; ×2.
	if rep.EstSavingsUSD < 0.119 || rep.EstSavingsUSD > 0.121 {
		t.Errorf("savings = %v, want ≈0.12", rep.EstSavingsUSD)
	}
	if rep.PolicyName != "value" || rep.PolicyHash != p.Hash() || rep.EstimateVersion != EstimateVersion {
		t.Errorf("attribution = %+v", rep)
	}
}

// TestSimulate_QualityRiskFlags pins the evidence gate: a reroute whose
// (kind, target tier) lacks parity evidence is flagged; with parity
// evidence it is not.
func TestSimulate_QualityRiskFlags(t *testing.T) {
	t.Parallel()
	p, _ := TemplateByName("value")
	turns := []SimTurn{simReadTurn("s1", 0)}

	noEvidence := Simulate(p, testSnapshot(), turns, nil)
	if noEvidence.QualityRiskFlags != 1 || noEvidence.QualityRiskByKind[TurnReadOnly] != 1 {
		t.Errorf("no-evidence flags = %+v", noEvidence)
	}

	withParity := Simulate(p, testSnapshot(), turns, map[EvidenceKey]string{
		{Kind: TurnReadOnly, Tier: TierHaikuClass}: EvidenceParity,
	})
	if withParity.QualityRiskFlags != 0 {
		t.Errorf("parity-backed reroute still flagged: %+v", withParity)
	}

	withWorse := Simulate(p, testSnapshot(), turns, map[EvidenceKey]string{
		{Kind: TurnReadOnly, Tier: TierHaikuClass}: "candidate_worse",
	})
	if withWorse.QualityRiskFlags != 1 {
		t.Errorf("worse-graded reroute not flagged: %+v", withWorse)
	}
}

// TestSimulate_SessionsIsolated pins per-session coherence state: a
// switch in one session does not stick another.
func TestSimulate_SessionsIsolated(t *testing.T) {
	t.Parallel()
	p, _ := TemplateByName("value")
	turns := []SimTurn{
		simReadTurn("s1", 0),
		simReadTurn("s2", 1),
	}
	rep := Simulate(p, testSnapshot(), turns, nil)
	if rep.WouldReroute != 2 {
		t.Errorf("would reroute = %d, want 2 (sessions independent)", rep.WouldReroute)
	}
}

// TestSimulate_Deterministic pins §R18.1: same inputs, same report —
// including input-order independence (the walk sorts internally).
func TestSimulate_Deterministic(t *testing.T) {
	t.Parallel()
	p, _ := TemplateByName("frugal")
	turns := []SimTurn{
		simReadTurn("s2", 5),
		simReadTurn("s1", 0),
		simReadTurn("s1", 30),
		simReadTurn("s3", 12),
	}
	first := Simulate(p, testSnapshot(), turns, nil)
	reversed := []SimTurn{turns[3], turns[2], turns[1], turns[0]}
	second := Simulate(p, testSnapshot(), reversed, nil)
	if first.WouldReroute != second.WouldReroute ||
		first.EstSavingsUSD != second.EstSavingsUSD ||
		first.QualityRiskFlags != second.QualityRiskFlags ||
		len(first.ByMove) != len(second.ByMove) {
		t.Fatalf("order sensitivity: %+v != %+v", first, second)
	}
}

// TestSimulate_EmptyInput pins the zero state.
func TestSimulate_EmptyInput(t *testing.T) {
	t.Parallel()
	p, _ := TemplateByName("value")
	rep := Simulate(p, testSnapshot(), nil, nil)
	if rep.TurnsEvaluated != 0 || rep.WouldReroute != 0 || rep.EstSavingsUSD != 0 {
		t.Errorf("empty report = %+v", rep)
	}
}
