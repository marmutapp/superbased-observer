package routing

import (
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// sig is a test shorthand for building action windows.
func sig(actionType string, class CommandClass) ActionSignal {
	return ActionSignal{Type: actionType, CommandClass: class, Success: true}
}

// TestClassifyTurnKind_TableRows exercises the classifier table one row
// at a time (§24.5: one test case per rule row), plus the precedence
// pins between adjacent rows.
func TestClassifyTurnKind_TableRows(t *testing.T) {
	t.Parallel()
	reads := []ActionSignal{
		sig(models.ActionReadFile, CommandNone),
		sig(models.ActionSearchText, CommandNone),
	}
	cases := []struct {
		name     string
		in       DecisionInput
		wantKind TurnKind
		wantRule string
	}{
		{
			name:     "row_sidechain",
			in:       DecisionInput{Session: SessionState{IsSidechain: true, ClientPhase: "plan"}},
			wantKind: TurnSubagent,
			wantRule: "sidechain", // beats client_declared_plan
		},
		{
			name: "row_client_declared_plan",
			in: DecisionInput{
				Shape:   TurnShape{PromptTokens: DefaultLongContextPromptTokens + 1},
				Session: SessionState{ClientPhase: "plan"},
			},
			wantKind: TurnPlan,
			wantRule: "client_declared_plan", // beats long_context_band
		},
		{
			name: "row_long_context_band",
			in: DecisionInput{
				Shape:   TurnShape{PromptTokens: DefaultLongContextPromptTokens},
				Session: SessionState{RecentActions: reads},
			},
			wantKind: TurnLongContext,
			wantRule: "long_context_band", // beats every window rule
		},
		{
			name: "row_actions_lagged",
			in: DecisionInput{
				Session: SessionState{ActionsLagged: true, RecentActions: reads},
			},
			wantKind: TurnUnknown,
			wantRule: "actions_lagged", // lagged degrades even with a window present
		},
		{
			name:     "row_no_recent_actions",
			in:       DecisionInput{},
			wantKind: TurnUnknown,
			wantRule: "no_recent_actions",
		},
		{
			name: "row_test_loop_last_command",
			in: DecisionInput{Session: SessionState{RecentActions: []ActionSignal{
				sig(models.ActionEditFile, CommandNone), // edit earlier in window
				sig(models.ActionRunCommand, CommandTest),
			}}},
			wantKind: TurnTestRun,
			wantRule: "test_loop_last_command", // last action wins over edit_in_flight
		},
		{
			name: "row_housekeeping_last_action_vcs",
			in: DecisionInput{Session: SessionState{RecentActions: []ActionSignal{
				sig(models.ActionEditFile, CommandNone),
				sig(models.ActionRunCommand, CommandVCS), // committing after edits IS housekeeping
			}}},
			wantKind: TurnHousekeeping,
			wantRule: "housekeeping_last_action",
		},
		{
			name: "row_housekeeping_last_action_todo",
			in: DecisionInput{Session: SessionState{RecentActions: []ActionSignal{
				sig(models.ActionReadFile, CommandNone),
				sig(models.ActionTodoUpdate, CommandNone),
			}}},
			wantKind: TurnHousekeeping,
			wantRule: "housekeeping_last_action",
		},
		{
			name: "row_edit_in_flight",
			in: DecisionInput{Session: SessionState{RecentActions: []ActionSignal{
				sig(models.ActionWriteFile, CommandNone),
				sig(models.ActionReadFile, CommandNone), // read after write stays edit
			}}},
			wantKind: TurnEdit,
			wantRule: "edit_in_flight",
		},
		{
			name:     "row_read_only_window",
			in:       DecisionInput{Session: SessionState{RecentActions: reads}},
			wantKind: TurnReadOnly,
			wantRule: "read_only_window",
		},
		{
			name: "row_default_unknown",
			in: DecisionInput{Session: SessionState{RecentActions: []ActionSignal{
				sig(models.ActionUserPrompt, CommandNone), // neither read nor active
			}}},
			wantKind: TurnUnknown,
			wantRule: "default_unknown",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ClassifyTurnKind(tc.in)
			if got.Kind != tc.wantKind || got.Rule != tc.wantRule {
				t.Errorf("ClassifyTurnKind = (%s, %s), want (%s, %s)",
					got.Kind, got.Rule, tc.wantKind, tc.wantRule)
			}
		})
	}
}

// TestClassifyTurnKind_ReadOnlyPurity pins the conservative purity rule:
// any action whose effect is unknowable from its type (generic command,
// MCP call, subagent spawn, browser) disqualifies read_only — the turn
// degrades to unknown rather than guessing (§R8.3).
func TestClassifyTurnKind_ReadOnlyPurity(t *testing.T) {
	t.Parallel()
	disqualifiers := []string{
		models.ActionRunCommand,
		models.ActionMCPCall,
		models.ActionSpawnSubagent,
		models.ActionBrowserAction,
	}
	for _, d := range disqualifiers {
		t.Run(d, func(t *testing.T) {
			t.Parallel()
			in := DecisionInput{Session: SessionState{RecentActions: []ActionSignal{
				sig(models.ActionReadFile, CommandNone),
				sig(d, CommandOther),
				sig(models.ActionReadFile, CommandNone),
			}}}
			got := ClassifyTurnKind(in)
			if got.Kind == TurnReadOnly {
				t.Errorf("window with %s classified read_only; want degraded", d)
			}
		})
	}
}

// TestClassifyTurnKind_Deterministic pins §R9.3: equal inputs produce
// equal classifications.
func TestClassifyTurnKind_Deterministic(t *testing.T) {
	t.Parallel()
	in := DecisionInput{
		Shape: TurnShape{Model: "claude-opus-4-8", PromptTokens: 50_000},
		Session: SessionState{RecentActions: []ActionSignal{
			sig(models.ActionReadFile, CommandNone),
			sig(models.ActionRunCommand, CommandTest),
		}},
	}
	first := ClassifyTurnKind(in)
	for i := 0; i < 10; i++ {
		if got := ClassifyTurnKind(in); got != first {
			t.Fatalf("run %d: %+v != %+v", i, got, first)
		}
	}
}

// TestTurnKindRuleNames_UniqueAndOrdered guards the table itself: unique
// row names, default_unknown last (the safety net must stay the floor).
func TestTurnKindRuleNames_UniqueAndOrdered(t *testing.T) {
	t.Parallel()
	names := TurnKindRuleNames()
	if len(names) == 0 {
		t.Fatal("empty rule table")
	}
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			t.Errorf("duplicate rule name %q", n)
		}
		seen[n] = true
	}
	if names[len(names)-1] != "default_unknown" {
		t.Errorf("last rule = %q, want default_unknown", names[len(names)-1])
	}
}
