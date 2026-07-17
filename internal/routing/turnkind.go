package routing

import "github.com/marmutapp/superbased-observer/internal/models"

// DefaultLongContextPromptTokens is the prompt-size band above which a
// turn classifies as long_context (§R8.2). 200K is the Anthropic/Gemini
// long-context repricing boundary — the conservative (lowest) provider
// threshold, so the engine never treats a repriced turn as routable small
// work. Per-provider thresholds become policy-tunable in P1.
const DefaultLongContextPromptTokens int64 = 200_000

// Classification is the classifier's output: the kind plus the rule row
// that produced it (transparency surface — decision rows and tests both
// name the rule).
type Classification struct {
	Kind TurnKind
	Rule string
}

// turnKindRule is one row of the classifier table. Rows are walked
// top-down; the first matching row wins (§24.5). Every row has exactly
// one test case in turnkind_test.go.
type turnKindRule struct {
	name string
	when func(DecisionInput) bool
	kind TurnKind
}

// Normalized action-type sets the window rules consume. The vocabulary is
// spec §5 (models.Action*); boundary loaders populate ActionSignal.Type
// from the actions.action_type column verbatim.
var (
	// readActions are provably non-mutating exploration actions.
	readActions = map[string]bool{
		models.ActionReadFile:    true,
		models.ActionSearchText:  true,
		models.ActionSearchFiles: true,
		models.ActionWebSearch:   true,
		models.ActionWebFetch:    true,
	}
	// mutationActions put file mutations in flight.
	mutationActions = map[string]bool{
		models.ActionWriteFile: true,
		models.ActionEditFile:  true,
	}
	// activeNonReadActions disqualify a window from read_only purity:
	// their effects are either mutating or unknowable from the action
	// type alone (a generic command, an arbitrary MCP tool, a browser).
	activeNonReadActions = map[string]bool{
		models.ActionWriteFile:     true,
		models.ActionEditFile:      true,
		models.ActionRunCommand:    true,
		models.ActionMCPCall:       true,
		models.ActionSpawnSubagent: true,
		models.ActionBrowserAction: true,
	}
	// housekeepingActions are administrative turn enders (Aider's
	// weak-model class lives on these plus vcs commands).
	housekeepingActions = map[string]bool{
		models.ActionTodoUpdate:   true,
		models.ActionTaskComplete: true,
	}
	// testCommandClasses are the run_command classes that mark a
	// build/test/lint loop.
	testCommandClasses = map[CommandClass]bool{
		CommandTest:  true,
		CommandBuild: true,
		CommandLint:  true,
	}
)

// turnKindRules is the v1 classifier table (§R8.3). Ordering is the
// precedence contract:
//
//  1. Structural facts that need no action window (sidechain, declared
//     phase, prompt-size band) fire first.
//  2. Degradation guards fire next: a lagged or empty action window
//     yields unknown — never a guess (§R8.3 freshness caveat).
//  3. Window rules read recency: the LAST action says what the turn is
//     doing right now (test loop, committing); any mutation in the
//     window keeps the turn an edit; a pure read window is exploration.
//  4. Anything else is unknown.
var turnKindRules = []turnKindRule{
	{
		name: "sidechain",
		when: func(in DecisionInput) bool { return in.Session.IsSidechain },
		kind: TurnSubagent,
	},
	{
		name: "client_declared_plan",
		when: func(in DecisionInput) bool { return in.Session.ClientPhase == "plan" },
		kind: TurnPlan,
	},
	{
		name: "long_context_band",
		when: func(in DecisionInput) bool {
			return in.Shape.PromptTokens >= DefaultLongContextPromptTokens
		},
		kind: TurnLongContext,
	},
	{
		name: "actions_lagged",
		when: func(in DecisionInput) bool { return in.Session.ActionsLagged },
		kind: TurnUnknown,
	},
	{
		name: "no_recent_actions",
		when: func(in DecisionInput) bool { return len(in.Session.RecentActions) == 0 },
		kind: TurnUnknown,
	},
	{
		name: "test_loop_last_command",
		when: func(in DecisionInput) bool {
			last := lastAction(in)
			return last.Type == models.ActionRunCommand && testCommandClasses[last.CommandClass]
		},
		kind: TurnTestRun,
	},
	{
		name: "housekeeping_last_action",
		when: func(in DecisionInput) bool {
			last := lastAction(in)
			if housekeepingActions[last.Type] {
				return true
			}
			return last.Type == models.ActionRunCommand && last.CommandClass == CommandVCS
		},
		kind: TurnHousekeeping,
	},
	{
		name: "edit_in_flight",
		when: func(in DecisionInput) bool {
			for _, a := range in.Session.RecentActions {
				if mutationActions[a.Type] {
					return true
				}
			}
			return false
		},
		kind: TurnEdit,
	},
	{
		name: "read_only_window",
		when: func(in DecisionInput) bool {
			reads := 0
			for _, a := range in.Session.RecentActions {
				if activeNonReadActions[a.Type] {
					return false
				}
				if readActions[a.Type] {
					reads++
				}
			}
			return reads > 0
		},
		kind: TurnReadOnly,
	},
	{
		name: "default_unknown",
		when: func(DecisionInput) bool { return true },
		kind: TurnUnknown,
	},
}

// lastAction returns the most recent action in the window (RecentActions
// is oldest-first). Zero value when the window is empty — callers above
// are guarded by the no_recent_actions row, but the zero value matches
// nothing anyway.
func lastAction(in DecisionInput) ActionSignal {
	n := len(in.Session.RecentActions)
	if n == 0 {
		return ActionSignal{}
	}
	return in.Session.RecentActions[n-1]
}

// ClassifyTurnKind classifies one request by walking the rule table
// top-down (§R8.3). Deterministic; no I/O; missing or lagged signals
// degrade to TurnUnknown — the engine never routes an unknown turn.
func ClassifyTurnKind(in DecisionInput) Classification {
	for _, r := range turnKindRules {
		if r.when(in) {
			return Classification{Kind: r.kind, Rule: r.name}
		}
	}
	// Unreachable: default_unknown always matches. Kept for safety so a
	// future table edit cannot make the classifier silently return a
	// zero value.
	return Classification{Kind: TurnUnknown, Rule: "default_unknown"}
}

// TurnKindRuleNames returns the classifier table's row names in walk
// order, for lint and status surfaces.
func TurnKindRuleNames() []string {
	out := make([]string, len(turnKindRules))
	for i, r := range turnKindRules {
		out[i] = r.name
	}
	return out
}
