package conversation

import (
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/compression/conversation/types"
)

func scoredMsg(role, text string, score float64, preserved bool) ScoredMessage {
	return ScoredMessage{
		Message:   Message{Role: role, Text: text, ByteLen: len(text)},
		Score:     score,
		Preserved: preserved,
	}
}

func TestEnforceEmptyReturnsZeroResult(t *testing.T) {
	got := Enforce(nil, BudgetOptions{TargetRatio: 0.5})
	if got.Messages != nil {
		t.Fatalf("Messages = %v, want nil", got.Messages)
	}
	// BudgetStats now contains a slice (Events); equality must compare
	// each scalar field individually.
	z := BudgetStats{}
	if got.Stats.OriginalBytes != z.OriginalBytes ||
		got.Stats.CompressedBytes != z.CompressedBytes ||
		got.Stats.CompressedCount != z.CompressedCount ||
		got.Stats.DroppedCount != z.DroppedCount ||
		got.Stats.MarkerCount != z.MarkerCount ||
		got.Stats.MessageCount != z.MessageCount ||
		len(got.Stats.Events) != 0 {
		t.Fatalf("Stats = %+v, want zero", got.Stats)
	}
}

func TestEnforceCompressIsPreferredOverDrop(t *testing.T) {
	// A big JSON tool_result plus a small user prompt: the per-type
	// compressor can shrink the JSON enough to hit target without any
	// drops — the enforcer should prefer compression.
	bigJSON := `{"records":[`
	for i := 0; i < 100; i++ {
		if i > 0 {
			bigJSON += ","
		}
		bigJSON += `{"id":` + itoa(i) + `,"payload":"`
		for j := 0; j < 30; j++ {
			bigJSON += "x"
		}
		bigJSON += `"}`
	}
	bigJSON += `]}`

	msgs := []ScoredMessage{
		scoredMsg(RoleUser, "short prompt", 0.9, false),
		scoredMsg(RoleTool, bigJSON, 0.2, false),
	}
	got := Enforce(msgs, BudgetOptions{
		TargetRatio:   0.5,
		Registry:      DefaultRegistry(),
		CompressTypes: map[types.ContentType]bool{types.JSON: true},
	})
	if got.Stats.CompressedCount == 0 {
		t.Errorf("expected JSON compression to fire: %+v", got.Stats)
	}
	if got.Stats.DroppedCount != 0 {
		t.Errorf("expected no drops (compression was enough): %+v", got.Stats)
	}
	if len(got.Messages) != 2 {
		t.Errorf("msg count = %d, want 2 (nothing dropped)", len(got.Messages))
	}
}

func TestEnforceCompressPassShrinksLargeJSON(t *testing.T) {
	bigJSON := `{"records":[`
	for i := 0; i < 50; i++ {
		if i > 0 {
			bigJSON += ","
		}
		bigJSON += `{"id":` + itoa(i) + `,"name":"user-`
		for j := 0; j < 20; j++ {
			bigJSON += "x"
		}
		bigJSON += `"}`
	}
	bigJSON += `]}`

	msgs := []ScoredMessage{
		scoredMsg(RoleUser, "brief prompt", 0.9, false),
		scoredMsg(RoleTool, bigJSON, 0.2, false),
		scoredMsg(RoleAssistant, "wrapping up", 0.8, false),
	}
	opts := BudgetOptions{
		TargetRatio:   0.9,
		Registry:      DefaultRegistry(),
		CompressTypes: map[types.ContentType]bool{types.JSON: true},
	}
	got := Enforce(msgs, opts)
	if got.Stats.CompressedCount == 0 {
		t.Fatalf("expected JSON message to be compressed; stats=%+v", got.Stats)
	}
	if got.Stats.CompressedBytes >= got.Stats.OriginalBytes {
		t.Errorf("CompressedBytes %d should be < OriginalBytes %d",
			got.Stats.CompressedBytes, got.Stats.OriginalBytes)
	}
}

func TestEnforceDropsLowestScoreNonPreserved(t *testing.T) {
	msgs := []ScoredMessage{
		scoredMsg(RoleUser, strings.Repeat("A", 100), 0.9, false),
		scoredMsg(RoleTool, strings.Repeat("B", 200), 0.1, false), // drop first
		scoredMsg(RoleTool, strings.Repeat("C", 200), 0.2, false), // drop second
		scoredMsg(RoleUser, strings.Repeat("D", 100), 0.8, true),
	}
	got := Enforce(msgs, BudgetOptions{TargetRatio: 0.5})
	if got.Stats.DroppedCount < 2 {
		t.Fatalf("expected ≥2 drops, got %+v", got.Stats)
	}
	// The two dropped messages were adjacent — expect one marker.
	markerFound := false
	for _, m := range got.Messages {
		if strings.Contains(m.Text, "messages compressed") {
			markerFound = true
			if !strings.Contains(m.Text, "search_past_outputs") {
				t.Errorf("marker missing MCP tool hint: %q", m.Text)
			}
		}
	}
	if !markerFound {
		t.Errorf("no marker message emitted: %v", got.Messages)
	}
}

func TestEnforcePreservedAreNeverDropped(t *testing.T) {
	msgs := []ScoredMessage{
		scoredMsg(RoleTool, strings.Repeat("A", 1000), 1.0, true),
		scoredMsg(RoleTool, strings.Repeat("B", 1000), 1.0, true),
	}
	got := Enforce(msgs, BudgetOptions{TargetRatio: 0.1})
	if got.Stats.DroppedCount != 0 {
		t.Errorf("preserved messages dropped: %+v", got.Stats)
	}
	if len(got.Messages) != 2 {
		t.Errorf("preserved count = %d, want 2", len(got.Messages))
	}
}

func TestEnforceCacheModeSkipsPrefixDrops(t *testing.T) {
	msgs := []ScoredMessage{
		scoredMsg(RoleTool, strings.Repeat("A", 500), 0.1, false), // prefix — protected
		scoredMsg(RoleTool, strings.Repeat("B", 500), 0.1, false), // prefix — protected
		scoredMsg(RoleTool, strings.Repeat("C", 500), 0.5, false), // tail — eligible
		scoredMsg(RoleTool, strings.Repeat("D", 500), 0.5, false), // tail — eligible
	}
	got := Enforce(msgs, BudgetOptions{TargetRatio: 0.5, Mode: ModeCache})
	// At least one drop happened and neither was in the prefix half.
	if got.Stats.DroppedCount == 0 {
		t.Fatalf("expected drops in cache mode: %+v", got.Stats)
	}
	// Check: first two messages (prefix) must survive.
	if got.Messages[0].Text != strings.Repeat("A", 500) {
		t.Errorf("prefix msg 0 dropped or replaced: %q", trimPreview(got.Messages[0].Text))
	}
	if got.Messages[1].Text != strings.Repeat("B", 500) {
		t.Errorf("prefix msg 1 dropped or replaced: %q", trimPreview(got.Messages[1].Text))
	}
}

func TestEnforceConsecutiveDropsProduceOneMarker(t *testing.T) {
	msgs := []ScoredMessage{
		scoredMsg(RoleUser, strings.Repeat("X", 50), 0.9, false),
		scoredMsg(RoleTool, strings.Repeat("A", 300), 0.1, false),
		scoredMsg(RoleTool, strings.Repeat("B", 300), 0.15, false),
		scoredMsg(RoleTool, strings.Repeat("C", 300), 0.2, false),
		scoredMsg(RoleUser, strings.Repeat("Y", 50), 0.95, false),
	}
	got := Enforce(msgs, BudgetOptions{TargetRatio: 0.3})
	if got.Stats.MarkerCount > 1 {
		t.Errorf("consecutive drops should collapse into 1 marker, got %d (stats=%+v)",
			got.Stats.MarkerCount, got.Stats)
	}
}

func TestMarkerMessagePointsAtSearchPastOutputs(t *testing.T) {
	m := MarkerMessage(7)
	if m.Role != RoleUser {
		t.Errorf("Role = %q, want %q", m.Role, RoleUser)
	}
	if !strings.Contains(m.Text, "7 messages") {
		t.Errorf("count not in text: %q", m.Text)
	}
	if !strings.Contains(m.Text, "search_past_outputs") {
		t.Errorf("tool hint missing: %q", m.Text)
	}
	if m.ByteLen != len(m.Text) {
		t.Errorf("ByteLen = %d, want %d", m.ByteLen, len(m.Text))
	}
}

func trimPreview(s string) string {
	if len(s) > 40 {
		return s[:40] + "…"
	}
	return s
}
