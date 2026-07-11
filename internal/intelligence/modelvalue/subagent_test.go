package modelvalue

import (
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/routing"
)

// sideAction builds a sidechain action n seconds after t0.
func sideAction(session, actionType string, sec int, success bool) ActionRow {
	return ActionRow{
		SessionID: session, Type: actionType, Success: success,
		IsSidechain: true,
		Timestamp:   t0.Add(time.Duration(sec) * time.Second),
	}
}

// subagentStart builds the persona marker.
func subagentStart(session, name string, sec int) ActionRow {
	return ActionRow{
		SessionID: session, Type: models.ActionSubagentStart, Success: true,
		IsSidechain: true, SubagentName: name,
		Timestamp: t0.Add(time.Duration(sec) * time.Second),
	}
}

// TestBuildSubagentEvidence_AttributionWalk pins the stream walk: the
// most recent subagent_start marker claims subsequent sidechain tool
// actions; a second marker re-attributes; non-sidechain and non-tool
// rows are ignored; the persona model is the session-dominant model.
func TestBuildSubagentEvidence_AttributionWalk(t *testing.T) {
	t.Parallel()
	turns := []TurnRow{
		mkTurn("s1", "claude-opus-4-8", 0, 1),
		mkTurn("s1", "claude-opus-4-8", 10, 1),
		mkTurn("s1", "claude-haiku-4-5", 20, 1), // minority model
	}
	actions := map[string][]ActionRow{
		"s1": {
			subagentStart("s1", "code-reviewer", 10),
			sideAction("s1", models.ActionReadFile, 20, true),
			sideAction("s1", models.ActionSearchText, 30, false), // failure
			{ // non-sidechain action between markers — ignored
				SessionID: "s1", Type: models.ActionReadFile, Success: true,
				Timestamp: t0.Add(35 * time.Second),
			},
			sideAction("s1", models.ActionUserPrompt, 38, true), // non-tool — ignored
			subagentStart("s1", "doc-writer", 40),
			sideAction("s1", models.ActionEditFile, 50, true),
			sideAction("s1", models.ActionRunCommand, 60, true),
		},
	}

	evidence := buildSubagentEvidence(turns, actions)
	if len(evidence) != 2 {
		t.Fatalf("personas = %d, want 2: %+v", len(evidence), evidence)
	}
	// Sorted by name: code-reviewer, doc-writer.
	cr, dw := evidence[0], evidence[1]
	if cr.Name != "code-reviewer" || dw.Name != "doc-writer" {
		t.Fatalf("names = %s/%s", cr.Name, dw.Name)
	}
	if cr.Actions != 2 || cr.Reads != 2 || cr.Failures != 1 || cr.Mutations != 0 {
		t.Errorf("code-reviewer profile = %+v", cr)
	}
	if dw.Actions != 2 || dw.Mutations != 1 || dw.Commands != 1 {
		t.Errorf("doc-writer profile = %+v", dw)
	}
	if cr.Model != "claude-opus-4-8" || dw.Model != "claude-opus-4-8" {
		t.Errorf("models = %s/%s, want session-dominant claude-opus-4-8", cr.Model, dw.Model)
	}
	if cr.Sessions != 1 {
		t.Errorf("sessions = %d, want 1", cr.Sessions)
	}
}

// TestBuild_SubagentRecommendationsSurface pins the report wiring end
// to end: a well-sampled read-only persona on an opus session surfaces
// an overpowered_subagent downshift in Report.Subagents.
func TestBuild_SubagentRecommendationsSurface(t *testing.T) {
	t.Parallel()
	f := &Facts{GeneratedAt: t0, Price: flatPrice}
	f.Turns = append(f.Turns, mkTurn("s1", "claude-opus-4-8", 0, 1))
	f.Actions = append(f.Actions, subagentStart("s1", "code-reviewer", 1))
	for i := 0; i < 60; i++ {
		f.Actions = append(f.Actions, sideAction("s1", models.ActionReadFile, 10+i, true))
	}

	rep := Build(f, Options{MinSample: 50})
	if len(rep.Subagents) != 1 {
		t.Fatalf("subagents = %d, want 1: %+v", len(rep.Subagents), rep.Subagents)
	}
	s := rep.Subagents[0]
	if s.Name != "code-reviewer" || s.Reason != routing.ReasonOverpoweredSubagent {
		t.Errorf("recommendation = %+v", s)
	}
	if s.SuggestedModel != "claude-haiku-4-5" || s.SuggestedTier != routing.TierHaikuClass {
		t.Errorf("suggestion = %s (%s)", s.SuggestedModel, s.SuggestedTier)
	}
	if s.Evidence.Actions != 60 || s.Evidence.Reads != 60 {
		t.Errorf("evidence = %+v", s.Evidence)
	}
}

// TestBuild_NoSidechainNoSubagents pins the quiet default.
func TestBuild_NoSidechainNoSubagents(t *testing.T) {
	t.Parallel()
	f := &Facts{
		GeneratedAt: t0,
		Turns:       []TurnRow{mkTurn("s1", "claude-opus-4-8", 0, 1)},
		Actions:     []ActionRow{mkAction("s1", models.ActionReadFile, -1, 0, true)},
	}
	rep := Build(f, Options{})
	if len(rep.Subagents) != 0 {
		t.Errorf("subagents = %+v, want none", rep.Subagents)
	}
}
