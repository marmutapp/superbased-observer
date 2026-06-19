package modelvalue

import (
	"testing"

	"github.com/marmutapp/superbased-observer/internal/routing"
)

// TestFilterSimTurnsByKind pins the R1.5 turn-kind filter: it runs the
// real §R8.2 classifier (sidechain → subagent; an empty window degrades
// to unknown), and an empty kind is a no-filter passthrough.
func TestFilterSimTurnsByKind(t *testing.T) {
	t.Parallel()
	turns := []routing.SimTurn{
		{SessionID: "sub", In: routing.DecisionInput{
			Session: routing.SessionState{IsSidechain: true},
		}},
		{SessionID: "unk", In: routing.DecisionInput{}},
	}

	sub := FilterSimTurnsByKind(turns, routing.TurnSubagent)
	if len(sub) != 1 || sub[0].SessionID != "sub" {
		t.Errorf("subagent filter = %+v, want the sidechain turn only", sub)
	}
	unk := FilterSimTurnsByKind(turns, routing.TurnUnknown)
	if len(unk) != 1 || unk[0].SessionID != "unk" {
		t.Errorf("unknown filter = %+v, want the empty-window turn only", unk)
	}
	if got := FilterSimTurnsByKind(turns, ""); len(got) != 2 {
		t.Errorf("empty kind must pass through all turns, got %d", len(got))
	}
	if got := FilterSimTurnsByKind(turns, routing.TurnPlan); len(got) != 0 {
		t.Errorf("non-matching kind must yield empty, got %d", len(got))
	}
}
