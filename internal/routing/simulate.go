package routing

import (
	"sort"
	"time"
)

// SimTurn is one recorded turn prepared for counterfactual replay
// (§R18.1). Boundary loaders assemble these from the observed substrate;
// In.ObservedUsage must carry the realized token bundle for dollar math.
type SimTurn struct {
	SessionID string
	Timestamp time.Time
	In        DecisionInput
}

// EvidenceKey addresses calibration evidence for a proposed move: the
// turn-kind being rerouted and the tier it would land on.
type EvidenceKey struct {
	Kind TurnKind
	Tier Tier
}

// Evidence verdict vocabulary consumed by Simulate. Mirrors the Model
// Value Report's parity verdicts (kept as plain strings so routing does
// not import the intelligence tree).
const (
	EvidenceParity          = "parity"
	EvidenceCandidateBetter = "candidate_better"
)

// MoveStat aggregates one original→selected model move in a simulation.
type MoveStat struct {
	From          string  `json:"from"`
	To            string  `json:"to"`
	Count         int     `json:"count"`
	EstSavingsUSD float64 `json:"est_savings_usd"`
}

// SimReport is the deterministic output of one counterfactual replay
// (§R18.1): would-route counts, estimated dollars, cache-forfeit costs,
// and quality-risk flags for moves lacking parity evidence.
type SimReport struct {
	PolicyName      string `json:"policy_name"`
	PolicyHash      string `json:"policy_hash"`
	EstimateVersion string `json:"estimate_version"`

	TurnsEvaluated int `json:"turns_evaluated"`
	WouldReroute   int `json:"would_reroute"`

	ByKind   map[TurnKind]int   `json:"reroutes_by_turn_kind"`
	ByReason map[ReasonCode]int `json:"decisions_by_reason"`
	ByMove   []MoveStat         `json:"moves"`

	// EstSavingsUSD is the net counterfactual saving (gross re-pricing
	// minus cache forfeits) across all would-reroute turns.
	EstSavingsUSD float64 `json:"est_savings_usd"`
	// CacheForfeitUSD totals the warm-prefix re-creation costs the
	// reroutes would have paid (already netted out of EstSavingsUSD).
	CacheForfeitUSD float64 `json:"cache_forfeit_usd"`

	// QualityRiskFlags counts rerouted turns whose (turn-kind, target
	// tier) lacks parity evidence in the Model Value Report (§R18.1
	// quality-risk flags). QualityRiskByKind breaks them down.
	QualityRiskFlags  int              `json:"quality_risk_flags"`
	QualityRiskByKind map[TurnKind]int `json:"quality_risk_by_turn_kind"`
}

// Simulate replays recorded turns through the decision engine under a
// policy (§R18.1). Deterministic given (turns, policy, snapshot,
// evidence): turns are walked in (session, timestamp) order, and the
// engine itself is pure. Session coherence state (turns since the last
// simulated switch, the prior turn's warm-prefix size) is maintained
// here, overriding whatever the boundary put on those two fields.
//
// evidence maps (turn-kind, target tier) to a Model Value Report parity
// verdict; reroutes whose key is absent or graded anything other than
// parity / candidate_better are counted as quality risks. A nil map
// flags every reroute — no evidence, maximum caution.
func Simulate(p Policy, snap *Snapshot, turns []SimTurn, evidence map[EvidenceKey]string) SimReport {
	rep := SimReport{
		PolicyName:        p.Name,
		PolicyHash:        p.Hash(),
		EstimateVersion:   EstimateVersion,
		ByKind:            map[TurnKind]int{},
		ByReason:          map[ReasonCode]int{},
		QualityRiskByKind: map[TurnKind]int{},
	}

	ordered := make([]SimTurn, len(turns))
	copy(ordered, turns)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].SessionID != ordered[j].SessionID {
			return ordered[i].SessionID < ordered[j].SessionID
		}
		return ordered[i].Timestamp.Before(ordered[j].Timestamp)
	})

	type sessState struct {
		turnsSinceSwitch int // -1 = never switched
		priorCacheRead   int64
	}
	states := map[string]*sessState{}
	moves := map[[2]string]*MoveStat{}
	tiers := snap.Tiers // may be nil; Decide fails open per turn

	for _, st := range ordered {
		state, ok := states[st.SessionID]
		if !ok {
			state = &sessState{turnsSinceSwitch: -1}
			states[st.SessionID] = state
		}
		in := st.In
		in.Session.TurnsSinceSwitch = state.turnsSinceSwitch
		in.Session.PriorCacheReadTokens = state.priorCacheRead

		d := Decide(p, snap, in)
		rep.TurnsEvaluated++
		for _, rc := range d.ReasonCodes {
			rep.ByReason[rc]++
		}
		if d.Changed {
			rep.WouldReroute++
			rep.ByKind[d.TurnKind]++
			rep.EstSavingsUSD += d.EstSavingsUSD
			rep.CacheForfeitUSD += d.CacheForfeitUSD
			key := [2]string{d.OriginalModel, d.SelectedModel}
			m, ok := moves[key]
			if !ok {
				m = &MoveStat{From: d.OriginalModel, To: d.SelectedModel}
				moves[key] = m
			}
			m.Count++
			m.EstSavingsUSD += d.EstSavingsUSD

			targetTier := TierUnclassified
			if tiers != nil {
				targetTier, _ = tiers.Lookup(d.SelectedModel)
			}
			if v := evidence[EvidenceKey{Kind: d.TurnKind, Tier: targetTier}]; v != EvidenceParity && v != EvidenceCandidateBetter {
				rep.QualityRiskFlags++
				rep.QualityRiskByKind[d.TurnKind]++
			}

			state.turnsSinceSwitch = 0
		} else if state.turnsSinceSwitch >= 0 {
			state.turnsSinceSwitch++
		}
		if in.ObservedUsage != nil {
			state.priorCacheRead = in.ObservedUsage.CacheRead
		}
	}

	rep.ByMove = make([]MoveStat, 0, len(moves))
	for _, m := range moves {
		rep.ByMove = append(rep.ByMove, *m)
	}
	sort.Slice(rep.ByMove, func(i, j int) bool {
		if rep.ByMove[i].From != rep.ByMove[j].From {
			return rep.ByMove[i].From < rep.ByMove[j].From
		}
		return rep.ByMove[i].To < rep.ByMove[j].To
	})
	return rep
}
