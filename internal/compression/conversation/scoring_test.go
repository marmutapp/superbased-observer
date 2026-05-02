package conversation

import (
	"math"
	"testing"
)

func msg(role, text, toolID string, refs ...string) Message {
	var ids []string
	if toolID != "" {
		ids = []string{toolID}
	}
	return Message{
		Role:          role,
		Text:          text,
		ByteLen:       len(text),
		ToolUseIDs:    ids,
		ReferencedIDs: refs,
	}
}

func approxEqual(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestScoreEmptyReturnsNil(t *testing.T) {
	if got := Score(nil, ScoreOptions{}); got != nil {
		t.Fatalf("Score(nil) = %v, want nil", got)
	}
}

func TestScoreRecencyMonotonicOnUniformInput(t *testing.T) {
	msgs := make([]Message, 5)
	for i := range msgs {
		msgs[i] = msg(RoleAssistant, "alpha beta gamma", "", "")
		msgs[i].RawIndex = i
	}
	got := Score(msgs, ScoreOptions{})
	for i := 1; i < len(got); i++ {
		if got[i].Score < got[i-1].Score {
			t.Errorf("score[%d]=%v < score[%d]=%v — recency should be monotonic",
				i, got[i].Score, i-1, got[i-1].Score)
		}
	}
}

func TestScorePreserveLastNPinsToOne(t *testing.T) {
	msgs := make([]Message, 6)
	for i := range msgs {
		msgs[i] = msg(RoleAssistant, "x", "", "")
	}
	got := Score(msgs, ScoreOptions{PreserveLastN: 2})
	for i, m := range got {
		if i >= len(msgs)-2 {
			if !m.Preserved {
				t.Errorf("msg[%d]: expected Preserved=true", i)
			}
			if !approxEqual(m.Score, 1.0) {
				t.Errorf("msg[%d]: Preserved score = %v, want 1.0", i, m.Score)
			}
			continue
		}
		if m.Preserved {
			t.Errorf("msg[%d]: expected Preserved=false", i)
		}
		if m.Score >= 1.0 {
			t.Errorf("msg[%d]: non-preserved score = %v, want < 1.0", i, m.Score)
		}
	}
}

func TestScoreReferenceRaisesLiveToolThreads(t *testing.T) {
	// live thread: assistant → tool_use X, later user → tool_result X
	// dead thread: assistant → tool_use Y with no later reference
	msgs := []Message{
		msg(RoleAssistant, "call X", "X"),
		msg(RoleAssistant, "call Y", "Y"),
		msg(RoleUser, "result for X", "", "X"),
		msg(RoleUser, "follow up question", "", ""),
	}
	got := Score(msgs, ScoreOptions{})
	if got[0].Components.Reference != 1.0 {
		t.Errorf("live thread X: Reference = %v, want 1.0", got[0].Components.Reference)
	}
	if got[1].Components.Reference != 0.0 {
		t.Errorf("dead thread Y: Reference = %v, want 0.0", got[1].Components.Reference)
	}
	if got[0].Score <= got[1].Score {
		t.Errorf("live thread X (%v) should score higher than dead thread Y (%v)",
			got[0].Score, got[1].Score)
	}
}

// TestScoreToolUseLivePairIsPreserved pins the tool-pair-integrity
// contract: a tool_use message whose ID is referenced by a later
// tool_result MUST be marked Preserved so the budget-enforcer drop
// pass cannot remove it. Without this, dropping the tool_use leaves
// the surviving tool_result orphan and Anthropic returns 400
// "tool_result block must have a corresponding tool_use block in the
// previous message" (real bug observed in v1.3.0).
func TestScoreToolUseLivePairIsPreserved(t *testing.T) {
	// live thread: assistant emits tool_use X, later user attaches
	// tool_result for X — these two MUST stay together.
	// dead thread: assistant emits tool_use Y nobody references — Y is
	// safe to drop (no orphan possible).
	msgs := []Message{
		msg(RoleAssistant, "call X", "X"),      // 0: tool_use X — must be preserved
		msg(RoleAssistant, "call Y", "Y"),      // 1: tool_use Y — droppable (no result)
		msg(RoleUser, "result for X", "", "X"), // 2: tool_result for X
		msg(RoleUser, "follow up", "", ""),     // 3: regular user
	}
	got := Score(msgs, ScoreOptions{})
	if !got[0].Preserved {
		t.Errorf("live tool_use X must be Preserved (else drop pass orphans the tool_result), got Preserved=false score=%v", got[0].Score)
	}
	if got[1].Preserved {
		t.Errorf("dead tool_use Y has no later reference and should NOT be Preserved (no orphan risk), got Preserved=true")
	}
}

// TestScoreParallelToolUsesAllPreserved pins the parallel-tool-call
// contract: when an assistant message emits multiple tool_use blocks
// in one turn (Claude Code's "Read + 2× LS" pattern), the producer
// message must be Preserved as long as ANY of its IDs is referenced
// later. Pre-fix the producer side tracked only one id (last-block-
// wins), so multi-tool messages where the last id had no result
// could be dropped, orphaning the surviving tool_results.
func TestScoreParallelToolUsesAllPreserved(t *testing.T) {
	// Assistant emits parallel tool_uses X, Y, Z; user replies with
	// tool_results referencing all three in a single message.
	parallel := Message{
		Role:          RoleAssistant,
		Text:          "calls X, Y, Z",
		ByteLen:       16,
		ToolUseIDs:    []string{"X", "Y", "Z"},
		ReferencedIDs: nil,
	}
	results := Message{
		Role:          RoleTool,
		Text:          "results for X, Y, Z",
		ByteLen:       20,
		ToolUseIDs:    nil,
		ReferencedIDs: []string{"X", "Y", "Z"},
	}
	got := Score([]Message{parallel, results}, ScoreOptions{})
	if !got[0].Preserved {
		t.Errorf("parallel tool_use message must be Preserved (any of X/Y/Z referenced later), got Preserved=false")
	}
	if !got[1].Preserved {
		t.Errorf("tool_result message must be Preserved (its ReferencedIDs intersect the live producer set — dropping it leaves the surviving tool_use unanswered, which Anthropic 400s as 'tool use concurrency issues'), got Preserved=false")
	}
}

// TestScoreToolResultPreservedWhenProducerIsLive pins the consumer
// side: a tool_result message must be preserved when its producer
// counterpart is live. v1.3.1 protected the producer side (tool_use)
// but not the consumer side, allowing the drop pass to remove the
// tool_result and leave the surviving tool_use unanswered.
func TestScoreToolResultPreservedWhenProducerIsLive(t *testing.T) {
	msgs := []Message{
		msg(RoleAssistant, "call X", "X"),
		msg(RoleTool, "big result for X", "", "X"),
		msg(RoleAssistant, "follow up", ""),
	}
	got := Score(msgs, ScoreOptions{})
	if !got[0].Preserved {
		t.Errorf("tool_use X must be Preserved (referenced later)")
	}
	if !got[1].Preserved {
		t.Errorf("tool_result for X must be Preserved (producer X is live; dropping the result leaves X unanswered)")
	}
	if got[2].Preserved {
		t.Errorf("regular assistant follow-up should NOT be Preserved (no tool-pair anchor)")
	}
}

func TestScoreDensityPenalizesWhitespaceHeavyContent(t *testing.T) {
	dense := msg(RoleAssistant, "densepackedbytesnospaces", "")
	sparse := msg(RoleAssistant, "                                 "+string(make([]byte, 0))+"   ", "")
	got := Score([]Message{dense, sparse}, ScoreOptions{})
	// Index 1 (sparse) gets a recency boost but density is 0, so compare
	// components directly.
	if got[0].Components.Density <= got[1].Components.Density {
		t.Errorf("dense density (%v) should exceed sparse density (%v)",
			got[0].Components.Density, got[1].Components.Density)
	}
	if got[1].Components.Density != 0 {
		t.Errorf("whitespace-only density = %v, want 0", got[1].Components.Density)
	}
}

func TestScoreRoleOrderingSystemUserAssistantTool(t *testing.T) {
	weights := []struct {
		role string
		want float64
	}{
		{RoleSystem, 1.0},
		{RoleUser, 0.9},
		{RoleAssistant, 0.7},
		{RoleTool, 0.5},
		{"unknown", 0.7},
	}
	for _, tc := range weights {
		got := roleWeight(tc.role)
		if !approxEqual(got, tc.want) {
			t.Errorf("roleWeight(%q) = %v, want %v", tc.role, got, tc.want)
		}
	}
}

func TestScoreCustomWeightsOverrideDefaults(t *testing.T) {
	// All-recency weight: recency component is the only signal.
	msgs := []Message{
		msg(RoleAssistant, "first", ""),
		msg(RoleAssistant, "second", ""),
	}
	got := Score(msgs, ScoreOptions{
		Weights: ScoreWeights{Recency: 1.0},
	})
	if !approxEqual(got[0].Score, 0.5) {
		t.Errorf("msg[0].Score = %v, want 0.5", got[0].Score)
	}
	if !approxEqual(got[1].Score, 1.0) {
		t.Errorf("msg[1].Score = %v, want 1.0", got[1].Score)
	}
}
