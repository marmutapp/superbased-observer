package dashboard

import (
	"encoding/json"
	"testing"
)

// TestRedactWorkspaceLayoutForRemote pins the review-MED fix: a remote
// principal's layout GET is filtered to the handles its snapshot admits —
// docked entries and per-breakpoint items for hidden (attach/resume under
// view-off, setup, dead/tombstone) handles are stripped; a malformed blob
// fails CLOSED (ok=false → layout:null), never leaks verbatim.
func TestRedactWorkspaceLayoutForRemote(t *testing.T) {
	blob := `{"v":1,"docked":["visible1","hidden1","dead1"],"layouts":{` +
		`"lg":[{"i":"visible1","x":0,"y":0,"w":6,"h":10},{"i":"hidden1","x":6,"y":0,"w":6,"h":10}],` +
		`"xs":[{"i":"dead1","x":0,"y":0,"w":1,"h":10}]}}`
	visible := []LaunchInfo{{ID: "visible1"}, {ID: "otherlive"}}

	out, ok := redactWorkspaceLayoutForRemote(blob, visible)
	if !ok {
		t.Fatal("well-formed blob must redact, not fail")
	}
	var got struct {
		V       int                          `json:"v"`
		Docked  []string                     `json:"docked"`
		Layouts map[string][]json.RawMessage `json:"layouts"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("redacted blob not JSON: %v", err)
	}
	if len(got.Docked) != 1 || got.Docked[0] != "visible1" {
		t.Errorf("docked = %v, want only visible1", got.Docked)
	}
	if len(got.Layouts["lg"]) != 1 {
		t.Errorf("lg items = %d, want 1 (hidden1 stripped)", len(got.Layouts["lg"]))
	}
	if len(got.Layouts["xs"]) != 0 {
		t.Errorf("xs items = %d, want 0 (dead1 stripped)", len(got.Layouts["xs"]))
	}
	if got.V != 1 {
		t.Errorf("v = %d, want preserved 1", got.V)
	}

	if _, ok := redactWorkspaceLayoutForRemote(`not json at all`, visible); ok {
		t.Error("malformed blob must fail closed (ok=false)")
	}
}
