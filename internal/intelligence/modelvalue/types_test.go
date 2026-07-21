package modelvalue

import "testing"

// TestGlobalDeltasFor pins the Channel A evidence filter: only
// global-group (project_id 0) rows for the asked candidate come back,
// in report order; no match is an empty slice, never nil-vs-invented.
func TestGlobalDeltasFor(t *testing.T) {
	t.Parallel()
	rep := &Report{Deltas: []Delta{
		{ProjectID: 0, TurnKind: "read_only", CandidateModel: "claude-haiku-4-5", BaselineModel: "claude-opus-4-8"},
		{ProjectID: 7, ProjectRoot: "/r", TurnKind: "read_only", CandidateModel: "claude-haiku-4-5"},
		{ProjectID: 0, TurnKind: "subagent", CandidateModel: "claude-haiku-4-5"},
		{ProjectID: 0, TurnKind: "read_only", CandidateModel: "claude-sonnet-4-6"},
	}}
	got := rep.GlobalDeltasFor("claude-haiku-4-5")
	if len(got) != 2 || got[0].TurnKind != "read_only" || got[1].TurnKind != "subagent" {
		t.Fatalf("GlobalDeltasFor = %+v", got)
	}
	if empty := rep.GlobalDeltasFor("nope"); empty == nil || len(empty) != 0 {
		t.Errorf("no-match = %#v, want empty non-nil", empty)
	}
}
