package modelvalue

import (
	"math"
	"sort"

	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/routing"
)

// toolActionTypes are the action types counted as "tool work" for the
// tool-failure rate denominator. Message-shaped and lifecycle rows
// (user_prompt, session markers, permission toggles, …) are excluded —
// they cannot fail in the tool sense.
var toolActionTypes = map[string]bool{
	models.ActionReadFile:      true,
	models.ActionWriteFile:     true,
	models.ActionEditFile:      true,
	models.ActionRunCommand:    true,
	models.ActionSearchText:    true,
	models.ActionSearchFiles:   true,
	models.ActionWebSearch:     true,
	models.ActionWebFetch:      true,
	models.ActionBrowserAction: true,
	models.ActionMCPCall:       true,
	models.ActionSpawnSubagent: true,
}

// Build computes the Model Value Report from loaded Facts. Pure and
// deterministic: equal Facts and Options produce an identical Report.
func Build(f *Facts, opts Options) Report {
	rep := Report{
		GeneratedAt: f.GeneratedAt,
		WindowDays:  f.WindowDays,
		Caveat:      Caveat,
	}
	tiers := f.Tiers
	if tiers == nil {
		tiers = routing.NewTierResolver().Table()
	}

	turns, actionsBySession := indexFacts(f)
	classified := classifyTurns(turns, actionsBySession, opts)

	type cellKey struct {
		model     string
		kind      routing.TurnKind
		projectID int64
	}
	perProject := map[cellKey]*Cell{}
	global := map[cellKey]*Cell{}
	latencies := map[cellKey][]int64{}

	accumulate := func(m map[cellKey]*Cell, key cellKey, ct classifiedTurn, root string) *Cell {
		c, ok := m[key]
		if !ok {
			tier, _ := tiers.Lookup(ct.row.Model)
			c = &Cell{
				Model: ct.row.Model, Tier: tier, TurnKind: ct.kind,
				ProjectID: key.projectID, ProjectRoot: root,
			}
			m[key] = c
		}
		c.Turns++
		c.CostUSD += turnCost(f.Price, ct.row)
		if ct.row.HasLatency {
			c.LatencyGraded++
		}
		if ct.row.HasStatus {
			c.ErrorGraded++
			if ct.row.HTTPStatus >= 400 || ct.row.ErrorClass != "" {
				c.ErrorCount++
			}
		}
		c.ToolActions += ct.toolActions
		c.ToolFailures += ct.toolFailures
		return c
	}

	for _, ct := range classified {
		pKey := cellKey{ct.row.Model, ct.kind, ct.row.ProjectID}
		gKey := cellKey{ct.row.Model, ct.kind, 0}
		accumulate(perProject, pKey, ct, ct.row.ProjectRoot)
		accumulate(global, gKey, ct, "")
		if ct.row.HasLatency {
			latencies[pKey] = append(latencies[pKey], ct.row.TotalMs)
			latencies[gKey] = append(latencies[gKey], ct.row.TotalMs)
		}
	}

	finalize := func(m map[cellKey]*Cell) []Cell {
		out := make([]Cell, 0, len(m))
		for key, c := range m {
			if ls := latencies[key]; len(ls) > 0 {
				c.LatencyP50Ms = percentile(ls, 0.50)
				c.LatencyP95Ms = percentile(ls, 0.95)
			}
			c.CostPerTurn = safeDiv(c.CostUSD, float64(c.Turns))
			c.ErrorRate = safeDiv(float64(c.ErrorCount), float64(c.ErrorGraded))
			c.ToolFailureRate = safeDiv(float64(c.ToolFailures), float64(c.ToolActions))
			out = append(out, *c)
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].ProjectID != out[j].ProjectID {
				return out[i].ProjectID < out[j].ProjectID
			}
			if out[i].TurnKind != out[j].TurnKind {
				return out[i].TurnKind < out[j].TurnKind
			}
			if out[i].Turns != out[j].Turns {
				return out[i].Turns > out[j].Turns
			}
			return out[i].Model < out[j].Model
		})
		return out
	}

	rep.Cells = finalize(perProject)
	rep.GlobalCells = finalize(global)
	// Deltas cover the global group (project_id 0 — the high-volume
	// evidence Simulate consumes) AND each per-project group.
	all := make([]Cell, 0, len(rep.GlobalCells)+len(rep.Cells))
	all = append(all, rep.GlobalCells...)
	all = append(all, rep.Cells...)
	rep.Deltas = buildDeltas(all, opts)
	// §R10.3 sub-agent recommendations from the sidechain stream.
	rep.Subagents = routing.RecommendSubagentModels(
		tiers, buildSubagentEvidence(turns, actionsBySession), opts.minSample(),
	)
	return rep
}

// classifiedTurn pairs a turn with its classification and the outcome
// window attributed to it (the actions between this turn and the
// session's next turn).
type classifiedTurn struct {
	row          TurnRow
	kind         routing.TurnKind
	toolActions  int64
	toolFailures int64
}

// indexFacts sorts the substrate: turns globally by (session, ts) and
// actions per session by ts. Build owns ordering — it is load-bearing
// for both classification and outcome attribution.
func indexFacts(f *Facts) ([]TurnRow, map[string][]ActionRow) {
	turns := make([]TurnRow, len(f.Turns))
	copy(turns, f.Turns)
	sort.SliceStable(turns, func(i, j int) bool {
		if turns[i].SessionID != turns[j].SessionID {
			return turns[i].SessionID < turns[j].SessionID
		}
		return turns[i].Timestamp.Before(turns[j].Timestamp)
	})
	actions := map[string][]ActionRow{}
	for _, a := range f.Actions {
		actions[a.SessionID] = append(actions[a.SessionID], a)
	}
	for sid := range actions {
		rows := actions[sid]
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].Timestamp.Before(rows[j].Timestamp) })
		actions[sid] = rows
	}
	return turns, actions
}

// assembleInput builds the pure classifier/engine input for one turn:
// the last-N action window, the client-declared phase from the most
// recent permission-mode marker, and the sidechain approximation (the
// action adjacent to the turn ran inside a sub-agent runtime, or the
// loader flagged the turn itself). Returns the input plus the action
// index cut (first action strictly after the turn) for outcome
// attribution.
func assembleInput(row TurnRow, acts []ActionRow, window int) (routing.DecisionInput, int) {
	cut := sort.Search(len(acts), func(k int) bool { return acts[k].Timestamp.After(row.Timestamp) })
	lo := cut - window
	if lo < 0 {
		lo = 0
	}
	recent := make([]routing.ActionSignal, 0, cut-lo)
	for _, a := range acts[lo:cut] {
		recent = append(recent, routing.ActionSignal{
			Type:         a.Type,
			CommandClass: a.CommandClass,
			Success:      a.Success,
			IsSidechain:  a.IsSidechain,
		})
	}
	phase := ""
	for k := cut - 1; k >= 0; k-- {
		if acts[k].Type == models.ActionPermissionMode && acts[k].PhaseHint != "" {
			if acts[k].PhaseHint == "plan" {
				phase = "plan"
			}
			break
		}
	}
	sidechain := row.IsSidechain
	if !sidechain && cut > 0 && acts[cut-1].IsSidechain {
		sidechain = true
	}
	return routing.DecisionInput{
		Shape: routing.TurnShape{
			Model:        row.Model,
			MessageCount: row.MessageCount,
			ToolUseCount: row.ToolUseCount,
			PromptTokens: row.Input + row.CacheRead + row.CacheCreation,
		},
		Session: routing.SessionState{
			RecentActions: recent,
			ClientPhase:   phase,
			IsSidechain:   sidechain,
		},
	}, cut
}

// classifyTurns walks each session's turn sequence, assembling the
// classifier input per turn and attributing the inter-turn action
// window as the turn's outcome.
func classifyTurns(turns []TurnRow, actionsBySession map[string][]ActionRow, opts Options) []classifiedTurn {
	out := make([]classifiedTurn, 0, len(turns))
	window := opts.recentWindow()

	for i, row := range turns {
		acts := actionsBySession[row.SessionID]
		in, cut := assembleInput(row, acts, window)
		cls := routing.ClassifyTurnKind(in)

		// Outcome window: actions strictly after this turn up to the
		// session's next turn (or end of window for the last turn).
		hi := len(acts)
		if i+1 < len(turns) && turns[i+1].SessionID == row.SessionID {
			next := turns[i+1].Timestamp
			hi = sort.Search(len(acts), func(k int) bool { return acts[k].Timestamp.After(next) })
		}
		var toolActs, toolFails int64
		for _, a := range acts[cut:hi] {
			if !toolActionTypes[a.Type] {
				continue
			}
			toolActs++
			if !a.Success {
				toolFails++
			}
		}

		out = append(out, classifiedTurn{row: row, kind: cls.Kind, toolActions: toolActs, toolFailures: toolFails})
	}
	return out
}

// turnCost prices one turn via the injected PriceFn, falling back to the
// row's stored insert-time cost when the model has no pricing entry (or
// no PriceFn was injected).
func turnCost(price routing.PriceFn, row TurnRow) float64 {
	if price != nil {
		usd, ok := price(row.Model, routing.PromptUsage{
			Input:             row.Input,
			Output:            row.Output,
			CacheRead:         row.CacheRead,
			CacheCreation:     row.CacheCreation,
			CacheCreation1h:   row.CacheCreation1h,
			Reasoning:         row.Reasoning,
			WebSearchRequests: row.WebSearchRequests,
			Fast:              row.Fast,
		})
		if ok {
			return usd
		}
	}
	return row.StoredCostUSD
}

// buildDeltas pairs every cell against the highest-tier baseline of its
// (project, turn-kind) group. The baseline is the highest-ranked tier's
// highest-volume cell; groups with fewer than two models yield nothing.
func buildDeltas(cells []Cell, opts Options) []Delta {
	type groupKey struct {
		projectID int64
		kind      routing.TurnKind
	}
	groups := map[groupKey][]Cell{}
	for _, c := range cells {
		k := groupKey{c.ProjectID, c.TurnKind}
		groups[k] = append(groups[k], c)
	}

	keys := make([]groupKey, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].projectID != keys[j].projectID {
			return keys[i].projectID < keys[j].projectID
		}
		return keys[i].kind < keys[j].kind
	})

	minN := opts.minSample()
	var out []Delta
	for _, k := range keys {
		group := groups[k]
		if len(group) < 2 {
			continue
		}
		base := group[0]
		for _, c := range group[1:] {
			if c.Tier.Rank() > base.Tier.Rank() ||
				(c.Tier.Rank() == base.Tier.Rank() && c.Turns > base.Turns) {
				base = c
			}
		}
		for _, c := range group {
			if c.Model == base.Model {
				continue
			}
			d := Delta{
				ProjectID: k.projectID, ProjectRoot: c.ProjectRoot, TurnKind: k.kind,
				BaselineModel: base.Model, BaselineTier: base.Tier,
				CandidateModel: c.Model, CandidateTier: c.Tier,
				NBaseline: base.Turns, NCandidate: c.Turns,
				ErrorGradedBaseline: base.ErrorGraded, ErrorGradedCandidate: c.ErrorGraded,
				ToolGradedBaseline: base.ToolActions, ToolGradedCandidate: c.ToolActions,
				DeltaErrorRate:       (c.ErrorRate - base.ErrorRate) * 100,
				ErrorCI95:            diffCI95(c.ErrorRate, c.ErrorGraded, base.ErrorRate, base.ErrorGraded) * 100,
				DeltaToolFailureRate: (c.ToolFailureRate - base.ToolFailureRate) * 100,
				ToolFailureCI95:      diffCI95(c.ToolFailureRate, c.ToolActions, base.ToolFailureRate, base.ToolActions) * 100,
				CostPerTurnBaseline:  base.CostPerTurn,
				CostPerTurnCandidate: c.CostPerTurn,
			}
			d.Verdict, d.VerdictBasis = verdictFor(d, minN)
			out = append(out, d)
		}
	}
	return out
}

// verdictFor grades a delta on whichever outcome dimension has enough
// graded observations on BOTH sides: error rate first (proxy-observed
// status rows), tool-failure rate as fallback (action attribution works
// on watcher-only substrates too). Below both floors the verdict is
// insufficient_evidence — no claim (§R7.2).
func verdictFor(d Delta, minN int64) (verdict, basis string) {
	if d.ErrorGradedBaseline >= minN && d.ErrorGradedCandidate >= minN {
		return ciVerdict(d.DeltaErrorRate, d.ErrorCI95), "error_rate"
	}
	if d.ToolGradedBaseline >= minN && d.ToolGradedCandidate >= minN {
		return ciVerdict(d.DeltaToolFailureRate, d.ToolFailureCI95), "tool_failure_rate"
	}
	return VerdictInsufficientEvidence, ""
}

// ciVerdict decides parity from a rate difference (pp) and its 95%
// half-width: the verdict is directional only when the interval excludes
// zero. A degenerate zero-width interval at delta 0 (e.g. zero failures
// on both well-sampled sides) is parity by construction.
func ciVerdict(delta, ci float64) string {
	switch {
	case delta-ci > 0:
		return VerdictCandidateWorse
	case delta+ci < 0:
		return VerdictCandidateBetter
	default:
		return VerdictParity
	}
}

// diffCI95 is the 95% normal-approximation half-width for a difference
// of two proportions. Returns 0 when either denominator is zero (no
// graded observations — the verdict then degrades).
func diffCI95(p1 float64, n1 int64, p2 float64, n2 int64) float64 {
	if n1 <= 0 || n2 <= 0 {
		return 0
	}
	return 1.96 * math.Sqrt(p1*(1-p1)/float64(n1)+p2*(1-p2)/float64(n2))
}

// percentile returns the pth percentile (nearest-rank) of the values.
func percentile(vals []int64, p float64) int64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := make([]int64, len(vals))
	copy(sorted, vals)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	rank := int(math.Ceil(p*float64(len(sorted)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}

func safeDiv(num, den float64) float64 {
	if den == 0 {
		return 0
	}
	return num / den
}
