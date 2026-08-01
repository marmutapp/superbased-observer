package routing

import (
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/tooltax"
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
			// A turn that ends on an evidence-grounded terminus.
			name: "row_housekeeping_last_action_task_complete",
			in: DecisionInput{Session: SessionState{RecentActions: []ActionSignal{
				sig(models.ActionReadFile, CommandNone),
				sig(models.ActionTaskComplete, CommandNone),
			}}},
			wantKind: TurnHousekeeping,
			wantRule: "housekeeping_last_action",
		},
		{
			// The SAME turn after the WP-T6/B2 sweep re-typed the
			// assistant-text emit sites. This pair is the
			// behaviour-preservation pin: pre-sweep this window ended in
			// task_complete and classified TurnHousekeeping, so the
			// post-sweep window must classify identically. Delete the
			// assistant_message member of housekeepingActions and this
			// row fails with TurnUnknown.
			name: "row_housekeeping_last_action_assistant_message",
			in: DecisionInput{Session: SessionState{RecentActions: []ActionSignal{
				sig(models.ActionReadFile, CommandNone),
				sig(models.ActionAssistantMessage, CommandNone),
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

// TestHousekeepingActionsAreMetaCategory is the cross-package pin the
// housekeepingActions comment names. internal/routing deliberately does
// NOT derive the set from tooltax (see the comment on housekeepingActions:
// CategoryMeta holds ~25 action types, so category membership is far too
// wide to be this rule's predicate), so what gets pinned instead is the
// invariant the WP-T6/B2 sweep actually depends on:
//
//  1. every housekeeping action type is a canonical tooltax action type
//     in CategoryMeta — the set never drifts into a work category, and
//  2. task_complete and assistant_message resolve to the SAME tooltax
//     category, which is what makes re-typing the assistant-text emit
//     sites provably neutral for every category-driven surface
//     (patterns, scoring, /api/tools/breakdown, the Tools page).
//
// tooltax is importable here: it is the repo's zero-import package, so
// this creates no cycle and no boundary violation (imports_test.go scans
// non-test files only, and this is a test).
func TestHousekeepingActionsAreMetaCategory(t *testing.T) {
	t.Parallel()

	if len(housekeepingActions) == 0 {
		t.Fatal("housekeepingActions is empty")
	}
	for at := range housekeepingActions {
		meta, ok := tooltax.MetaForActionType(at)
		if !ok {
			t.Errorf("housekeeping action %q has no tooltax registry row", at)
			continue
		}
		if meta.Category != tooltax.CategoryMeta {
			t.Errorf("housekeeping action %q category = %q, want %q",
				at, meta.Category, tooltax.CategoryMeta)
		}
	}

	// The equality that makes the B2 relabel category-neutral.
	if !housekeepingActions[models.ActionTaskComplete] {
		t.Error("task_complete must stay in housekeepingActions (evidence-grounded turn terminus)")
	}
	if !housekeepingActions[models.ActionAssistantMessage] {
		t.Error("assistant_message must be in housekeepingActions to preserve pre-sweep classification")
	}
	if got, want := tooltax.CategoryForActionType(models.ActionAssistantMessage),
		tooltax.CategoryForActionType(models.ActionTaskComplete); got != want {
		t.Errorf("assistant_message category = %q, task_complete category = %q — the B2 relabel is no longer category-neutral",
			got, want)
	}
}
