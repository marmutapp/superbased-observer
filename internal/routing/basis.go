package routing

import "sort"

// Basis names (§R6.2) — the closed vocabulary of user-selectable
// routing objectives. Lint validates Policy.Bases membership against
// this set; implementations register in basisRegistry (bases.go).
const (
	BasisQualityFloor    = "quality_floor"
	BasisPrivacy         = "privacy"
	BasisCapability      = "capability"
	BasisCostMinimize    = "cost_minimize"
	BasisLatency         = "latency"
	BasisQualityMax      = "quality_max"
	BasisBudget          = "budget"
	BasisAvailability    = "availability"
	BasisRateLimitWindow = "rate_limit_window"
	BasisPhase           = "phase"
)

// KnownBasisNames enumerates the shipped basis vocabulary in §R6.2
// table order.
func KnownBasisNames() []string {
	return []string{
		BasisQualityFloor, BasisPrivacy, BasisCapability,
		BasisCostMinimize, BasisLatency, BasisQualityMax,
		BasisBudget, BasisAvailability, BasisRateLimitWindow, BasisPhase,
	}
}

// BasisClass partitions bases by their role in the FIXED composition
// (§R6.2): filters intersect → rankers order → modifiers adjust →
// re-run once. No weighted blending.
type BasisClass string

// Basis classes.
const (
	ClassFilter   BasisClass = "filter"
	ClassRank     BasisClass = "rank"
	ClassModifier BasisClass = "modifier"
)

// BasisInput is the read-only context every basis evaluates against.
type BasisInput struct {
	In           DecisionInput
	Kind         TurnKind
	OriginalTier Tier
	Policy       *Policy
	Snap         *Snapshot
}

// Basis is one user-selectable routing objective (§R6.2). A basis
// implements exactly one of the class interfaces below.
type Basis interface {
	Name() string
	Class() BasisClass
}

// FilterBasis allows or denies individual candidates. Filters
// intersect: a candidate any filter denies is out.
type FilterBasis interface {
	Basis
	// Allow returns whether the candidate survives and, when denied,
	// the closed-enum reason.
	Allow(c ModelCandidate, bin BasisInput) (bool, ReasonCode)
}

// RankBasis orders candidates. The first ranker in the policy's basis
// list is the primary sort key; subsequent rankers tie-break in order.
type RankBasis interface {
	Basis
	// Key returns the candidate's sort key — ascending is better.
	Key(c ModelCandidate, bin BasisInput) float64
}

// ModifierBasis adjusts the decision after the first filter+rank pass:
// tier caps, exclusions, advise-only / hard-stop escalations. The
// pipeline applies the merged modifier and re-runs filters+rankers
// exactly once (§R6.2).
type ModifierBasis interface {
	Basis
	Modify(bin BasisInput) Modifier
}

// Modifier is a ModifierBasis's adjustment. Zero value = no change.
type Modifier struct {
	// TierCap demotes the maximum allowed tier ("" = none). Applies to
	// soft turn-kinds only unless TierCapAllKinds (§R14 plan/edit
	// floors hold).
	TierCap         Tier
	TierCapAllKinds bool
	// ExcludeModels removes specific candidates (availability §R12.3).
	ExcludeModels []string
	// Reasons annotate the decision with why the modifier acted.
	Reasons []ReasonCode
	// AdviseOnly forces log-without-acting even in enforce mode.
	AdviseOnly bool
	// HardStop marks the §R14 hard_stop exhaustion behavior; the
	// boundary decides what a stop means per channel (the engine
	// itself never breaks a turn — G7).
	HardStop bool
}

// merge folds m2 into m: the most restrictive of each field wins.
func (m Modifier) merge(m2 Modifier) Modifier {
	out := m
	if m2.TierCap != "" && (out.TierCap == "" || m2.TierCap.Rank() < out.TierCap.Rank()) {
		out.TierCap = m2.TierCap
	}
	out.TierCapAllKinds = out.TierCapAllKinds || m2.TierCapAllKinds
	out.ExcludeModels = append(out.ExcludeModels, m2.ExcludeModels...)
	out.Reasons = append(out.Reasons, m2.Reasons...)
	out.AdviseOnly = out.AdviseOnly || m2.AdviseOnly
	out.HardStop = out.HardStop || m2.HardStop
	return out
}

// CandidateState is one candidate's pipeline outcome.
type CandidateState struct {
	ModelCandidate
	// Allowed survives every filter (and modifier exclusion/cap).
	Allowed bool
	// DenyReasons collects the closed-enum codes that cut it.
	DenyReasons []ReasonCode
	// rankKeys are the rankers' sort keys in basis order.
	rankKeys []float64
}

// PipelineResult is RunPipeline's output: candidates in final rank
// order (allowed first, by rank keys; denied after, original order)
// plus the merged modifier.
type PipelineResult struct {
	Candidates []CandidateState
	Mod        Modifier
}

// Allowed returns the surviving candidates in rank order.
func (r PipelineResult) Allowed() []CandidateState {
	out := make([]CandidateState, 0, len(r.Candidates))
	for _, c := range r.Candidates {
		if c.Allowed {
			out = append(out, c)
		}
	}
	return out
}

// resolveBases maps the policy's basis names to implementations,
// always prepending the capability basis (§R6.2: always on) and
// silently skipping names with no registered implementation (the
// lint surface reports them; the engine fails open on vocabulary
// drift rather than erroring).
func resolveBases(names []string) []Basis {
	out := []Basis{capabilityBasisSingleton}
	for _, n := range names {
		if n == BasisCapability {
			continue // already first
		}
		if b, ok := basisRegistry[n]; ok {
			out = append(out, b)
		}
	}
	return out
}

// RunPipeline applies the FIXED basis composition (§R6.2) to a
// candidate set:
//
//  1. filters intersect (capability always first),
//  2. rankers order what survives (first ranker primary, rest
//     tie-break),
//  3. modifiers evaluate and merge,
//  4. the merged modifier's exclusions and tier cap are applied and
//     filters+rankers RE-RUN ONCE on the reduced set.
//
// No weighted blending — every basis's effect is individually
// inspectable (§R6.2). Pure and deterministic.
func RunPipeline(bases []Basis, cands []ModelCandidate, bin BasisInput) PipelineResult {
	states := make([]CandidateState, len(cands))
	for i, c := range cands {
		states[i] = CandidateState{ModelCandidate: c, Allowed: true}
	}

	filterAndRank(bases, states, bin)

	// Modifiers evaluate once, on the BasisInput (they read burn bands
	// and health, not candidates), then merge most-restrictive-wins.
	var mod Modifier
	for _, b := range bases {
		if m, ok := b.(ModifierBasis); ok {
			mod = mod.merge(m.Modify(bin))
		}
	}

	if modifierActs(mod, bin.Kind) {
		excluded := map[string]bool{}
		for _, m := range mod.ExcludeModels {
			excluded[m] = true
		}
		for i := range states {
			if !states[i].Allowed {
				continue
			}
			if excluded[states[i].Model] {
				states[i].Allowed = false
				states[i].DenyReasons = append(states[i].DenyReasons, ReasonAvailabilityFallback)
			}
			if mod.TierCap != "" && tierCapApplies(mod, bin.Kind) &&
				states[i].Tier.Rank() > mod.TierCap.Rank() {
				states[i].Allowed = false
				states[i].DenyReasons = append(states[i].DenyReasons, mod.Reasons...)
			}
		}
		// Re-run once (§R6.2): filters and rankers re-evaluate the
		// reduced set. Filters are deterministic so survivors keep
		// surviving; rank keys are recomputed against the same input.
		filterAndRank(bases, states, bin)
	}

	sortStates(states)
	return PipelineResult{Candidates: states, Mod: mod}
}

// modifierActs reports whether the merged modifier changes anything
// for this turn-kind.
func modifierActs(m Modifier, kind TurnKind) bool {
	if len(m.ExcludeModels) > 0 {
		return true
	}
	return m.TierCap != "" && tierCapApplies(m, kind)
}

// tierCapApplies honors §R14: caps demote soft kinds only, unless the
// exhaustion behavior escalated to degrade_all.
func tierCapApplies(m Modifier, kind TurnKind) bool {
	return m.TierCapAllKinds || kind.Soft()
}

// filterAndRank applies every filter (intersection) then recomputes
// every surviving candidate's rank keys in basis order.
func filterAndRank(bases []Basis, states []CandidateState, bin BasisInput) {
	for i := range states {
		if !states[i].Allowed {
			continue
		}
		for _, b := range bases {
			f, ok := b.(FilterBasis)
			if !ok {
				continue
			}
			allowed, reason := f.Allow(states[i].ModelCandidate, bin)
			if !allowed {
				states[i].Allowed = false
				states[i].DenyReasons = append(states[i].DenyReasons, reason)
				break
			}
		}
	}
	for i := range states {
		if !states[i].Allowed {
			continue
		}
		states[i].rankKeys = states[i].rankKeys[:0]
		for _, b := range bases {
			if r, ok := b.(RankBasis); ok {
				states[i].rankKeys = append(states[i].rankKeys, r.Key(states[i].ModelCandidate, bin))
			}
		}
	}
}

// sortStates orders allowed candidates by rank keys (lexicographic:
// primary ranker first, tie-breaks in basis order), with model id as
// the final deterministic tie-break. Denied candidates sort after, by
// model id.
func sortStates(states []CandidateState) {
	sort.SliceStable(states, func(i, j int) bool {
		a, b := states[i], states[j]
		if a.Allowed != b.Allowed {
			return a.Allowed
		}
		if a.Allowed {
			n := len(a.rankKeys)
			if len(b.rankKeys) < n {
				n = len(b.rankKeys)
			}
			for k := 0; k < n; k++ {
				if a.rankKeys[k] != b.rankKeys[k] {
					return a.rankKeys[k] < b.rankKeys[k]
				}
			}
		}
		return a.Model < b.Model
	})
}
