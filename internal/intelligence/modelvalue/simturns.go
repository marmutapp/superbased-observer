package modelvalue

import (
	"github.com/marmutapp/superbased-observer/internal/routing"
)

// AssembleSimTurns converts loaded Facts into routing.SimTurn inputs for
// counterfactual replay (§R18.1), using the exact same per-turn input
// assembly the Model Value Report classifies with — one assembly path,
// two consumers. Each SimTurn carries the turn's realized token bundle
// as ObservedUsage so the engine's dollar math is replay-exact.
func AssembleSimTurns(f *Facts, opts Options) []routing.SimTurn {
	turns, actionsBySession := indexFacts(f)
	window := opts.recentWindow()
	out := make([]routing.SimTurn, 0, len(turns))
	for _, row := range turns {
		in, _ := assembleInput(row, actionsBySession[row.SessionID], window)
		in.ObservedUsage = &routing.PromptUsage{
			Input:             row.Input,
			Output:            row.Output,
			CacheRead:         row.CacheRead,
			CacheCreation:     row.CacheCreation,
			CacheCreation1h:   row.CacheCreation1h,
			Reasoning:         row.Reasoning,
			WebSearchRequests: row.WebSearchRequests,
			Fast:              row.Fast,
		}
		out = append(out, routing.SimTurn{
			SessionID: row.SessionID,
			Timestamp: row.Timestamp,
			In:        in,
		})
	}
	return out
}

// FilterSimTurnsByKind keeps the turns the §R8.2 classifier maps to
// kind — the simulate surfaces' turn-kind filter (R1.5). Empty kind =
// no filter. Classification runs the same table the replay itself
// uses, so the filtered set is exactly the subset the report would
// label with that kind.
func FilterSimTurnsByKind(turns []routing.SimTurn, kind routing.TurnKind) []routing.SimTurn {
	if kind == "" {
		return turns
	}
	out := make([]routing.SimTurn, 0, len(turns))
	for _, t := range turns {
		if routing.ClassifyTurnKind(t.In).Kind == kind {
			out = append(out, t)
		}
	}
	return out
}

// EvidenceByKindTier projects the report's global deltas into the
// evidence map Simulate consumes: for each (turn-kind, candidate tier)
// the parity verdict graded against the group's baseline. Per-project
// deltas are deliberately not folded in — the global cells carry the
// volume, and a per-project override layer is a P2 calibration concern.
func (r Report) EvidenceByKindTier() map[routing.EvidenceKey]string {
	out := map[routing.EvidenceKey]string{}
	for _, d := range r.Deltas {
		if d.ProjectID != 0 {
			continue // global group only (see doc comment)
		}
		key := routing.EvidenceKey{Kind: d.TurnKind, Tier: d.CandidateTier}
		// Keep the most cautious verdict when multiple candidate models
		// share a tier: a worse/insufficient grading must not be masked
		// by a parity sibling.
		if prev, ok := out[key]; ok && rankVerdict(prev) <= rankVerdict(d.Verdict) {
			continue
		}
		out[key] = d.Verdict
	}
	return out
}

// rankVerdict orders verdicts by caution: lower = more cautious. Used to
// keep the most cautious verdict per evidence key.
func rankVerdict(v string) int {
	switch v {
	case VerdictInsufficientEvidence:
		return 0
	case VerdictCandidateWorse:
		return 1
	case VerdictParity:
		return 2
	case VerdictCandidateBetter:
		return 3
	default:
		return 0
	}
}
