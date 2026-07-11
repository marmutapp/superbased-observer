package clinecli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"testing"
)

// TestBuildCacheObservations_Anthropic_Shape pins the §14.3
// cline-cli Tier-2 emission: the canonical messages.json fixture
// has three assistant turns with metrics; we expect three
// CacheTurnObservation rows with the correct usage bundle on each.
func TestBuildCacheObservations_Anthropic_Shape(t *testing.T) {
	t.Parallel()

	// One session whose messages.json carries three assistant
	// turns. Numbers match the operator-shaped fixture used for the
	// audit doc: deepseek-v4-flash with progressive cache reads
	// (0 / 5888 / 140416 — first turn cold, second turn warm, third
	// turn deep-cache).
	s := sessionRow{
		ID:           "sess-cachetrack",
		Model:        "deepseek/deepseek-v4-flash",
		StartedAt:    "2026-06-05T23:21:51.000Z",
		UpdatedAt:    "2026-06-05T23:21:55.000Z",
		MessagesPath: sql.NullString{String: "/synthetic/path/sess-cachetrack.messages.json", Valid: true},
	}
	s.Messages.Messages = []messageRecord{
		{
			ID:   "msg_u1",
			Role: "user",
			Ts:   1780701724628,
			Content: []messageBlock{
				{Type: "text", Text: "Hi"},
			},
		},
		{
			ID:   "msg_a1",
			Role: "assistant",
			Ts:   1780701727101,
			ModelInfo: &messageModelInfo{
				ID: "deepseek/deepseek-v4-flash",
			},
			Metrics: &messageUsageBlock{
				InputTokens: 5875, OutputTokens: 112,
				CacheReadTokens: 0, CacheWriteTokens: 0,
			},
			Content: []messageBlock{
				{Type: "thinking", Thinking: "Just a greeting."},
				{Type: "text", Text: "Hello!"},
				{Type: "tool_use", ID: "call_1", Name: "read_files", Input: json.RawMessage(`{"files":[{"path":"/r/PROGRESS.md"}]}`)},
			},
		},
		{
			ID:   "msg_u2",
			Role: "user",
			Ts:   1780701727116,
			Content: []messageBlock{
				{Type: "tool_result", ToolUseID: "call_1", Name: "read_files", Content: json.RawMessage(`[{"query":"/r/PROGRESS.md","result":"<redacted>","success":true}]`)},
			},
		},
		{
			ID:   "msg_a2",
			Role: "assistant",
			Ts:   1780701737766,
			ModelInfo: &messageModelInfo{
				ID: "deepseek/deepseek-v4-flash",
			},
			Metrics: &messageUsageBlock{
				InputTokens: 140169, OutputTokens: 355,
				CacheReadTokens: 5888, CacheWriteTokens: 0,
			},
			Content: []messageBlock{
				{Type: "thinking", Thinking: "Catch them up."},
				{Type: "text", Text: "Welcome back."},
			},
		},
		{
			ID:   "msg_u3",
			Role: "user",
			Ts:   1780702104870,
			Content: []messageBlock{
				{Type: "text", Text: "What kind of model are you?"},
			},
		},
		{
			ID:   "msg_a3",
			Role: "assistant",
			Ts:   1780702110629,
			ModelInfo: &messageModelInfo{
				ID: "deepseek/deepseek-v4-flash",
			},
			Metrics: &messageUsageBlock{
				InputTokens: 140535, OutputTokens: 244,
				CacheReadTokens: 140416, CacheWriteTokens: 0,
			},
			Content: []messageBlock{
				{Type: "thinking", Thinking: "Identify myself."},
				{Type: "text", Text: "I'm Claude."},
			},
		},
	}

	out := buildCacheObservations([]sessionRow{s}, "/x/sessions.db")
	if got := len(out); got != 3 {
		t.Fatalf("CacheObservations = %d, want 3 (one per assistant turn with metrics)", got)
	}
	wantUsages := []struct {
		net   int64
		out   int64
		read  int64
		write int64
	}{
		{5875, 112, 0, 0},
		{140169, 355, 5888, 0},
		{140535, 244, 140416, 0},
	}
	wantMsgIDs := []string{"msg_a1", "msg_a2", "msg_a3"}
	for i, obs := range out {
		if obs.MessageID != wantMsgIDs[i] {
			t.Errorf("obs[%d] MessageID = %q, want %q", i, obs.MessageID, wantMsgIDs[i])
		}
		if obs.SourceEventID != "cachetrack:"+wantMsgIDs[i] {
			t.Errorf("obs[%d] SourceEventID = %q", i, obs.SourceEventID)
		}
		if obs.SessionID != "sess-cachetrack" {
			t.Errorf("obs[%d] SessionID = %q", i, obs.SessionID)
		}
		if obs.Model != "deepseek/deepseek-v4-flash" {
			t.Errorf("obs[%d] Model = %q", i, obs.Model)
		}
		if obs.Usage.NetInputTokens != wantUsages[i].net {
			t.Errorf("obs[%d] NetInputTokens = %d, want %d", i, obs.Usage.NetInputTokens, wantUsages[i].net)
		}
		if obs.Usage.OutputTokens != wantUsages[i].out {
			t.Errorf("obs[%d] OutputTokens = %d, want %d", i, obs.Usage.OutputTokens, wantUsages[i].out)
		}
		if obs.Usage.CacheReadTokens != wantUsages[i].read {
			t.Errorf("obs[%d] CacheReadTokens = %d, want %d", i, obs.Usage.CacheReadTokens, wantUsages[i].read)
		}
		if obs.Usage.CacheCreationTokens != wantUsages[i].write {
			t.Errorf("obs[%d] CacheCreationTokens = %d, want %d", i, obs.Usage.CacheCreationTokens, wantUsages[i].write)
		}
	}

	// First observation must include the user prompt block + the
	// three assistant blocks = 4 blocks. (No cap reached.)
	if got := len(out[0].BlockHashes); got != 4 {
		t.Errorf("obs[0] BlockHashes = %d, want 4 (user text + thinking + text + tool_use)", got)
	}
	// Second observation: tool_result block from user + 2 assistant
	// blocks = 3 blocks.
	if got := len(out[1].BlockHashes); got != 3 {
		t.Errorf("obs[1] BlockHashes = %d, want 3 (tool_result + thinking + text)", got)
	}
	// Third observation: 1 user text + 2 assistant blocks = 3.
	if got := len(out[2].BlockHashes); got != 3 {
		t.Errorf("obs[2] BlockHashes = %d, want 3 (user text + thinking + text)", got)
	}
}

// TestBuildCacheObservations_Idempotent pins byte-stability across
// re-builds. The dedup gate (SourceFile, SourceEventID) AND the
// engine's chain hash both depend on this.
func TestBuildCacheObservations_Idempotent(t *testing.T) {
	t.Parallel()

	s := sessionRow{
		ID:           "sess-id",
		Model:        "claude-sonnet-4",
		MessagesPath: sql.NullString{String: "/synthetic/path/sess-id.messages.json", Valid: true},
	}
	s.Messages.Messages = []messageRecord{
		{
			ID:      "msg_u",
			Role:    "user",
			Ts:      1780701724628,
			Content: []messageBlock{{Type: "text", Text: "Hello"}},
		},
		{
			ID:      "msg_a",
			Role:    "assistant",
			Ts:      1780701727101,
			Metrics: &messageUsageBlock{InputTokens: 100, OutputTokens: 50, CacheReadTokens: 64},
			Content: []messageBlock{
				{Type: "thinking", Thinking: "Think hard."},
				{Type: "text", Text: "Hi!"},
				{Type: "tool_use", ID: "call_x", Name: "ls", Input: json.RawMessage(`{"path":"."}`)},
			},
		},
	}

	first := buildCacheObservations([]sessionRow{s}, "/x/sessions.db")
	second := buildCacheObservations([]sessionRow{s}, "/x/sessions.db")
	if len(first) != len(second) {
		t.Fatalf("count drift: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].SourceEventID != second[i].SourceEventID {
			t.Errorf("obs[%d] SourceEventID drift", i)
		}
		if len(first[i].BlockHashes) != len(second[i].BlockHashes) {
			t.Fatalf("obs[%d] block count drift: %d vs %d", i, len(first[i].BlockHashes), len(second[i].BlockHashes))
		}
		for j := range first[i].BlockHashes {
			a := first[i].BlockHashes[j].CanonicalBytes
			b := second[i].BlockHashes[j].CanonicalBytes
			if !bytes.Equal(a, b) {
				t.Errorf("obs[%d] block[%d] canonical bytes drift\n  first:  %s\n  second: %s", i, j, a, b)
			}
		}
	}
}

// TestBuildCacheObservations_DeterministicBlockOrder pins R3: the
// input-array order of content blocks MUST map to the output
// slice order. cline-cli's content array is itself an ordered
// slice (Go preserves order); this test guards against an
// accidental map-iteration regression in the canonical
// marshaller.
func TestBuildCacheObservations_DeterministicBlockOrder(t *testing.T) {
	t.Parallel()

	s := sessionRow{
		ID:           "sess-order",
		Model:        "claude-sonnet-4",
		MessagesPath: sql.NullString{String: "/synthetic/path/sess-order.messages.json", Valid: true},
	}
	s.Messages.Messages = []messageRecord{
		{
			ID:      "msg_a",
			Role:    "assistant",
			Ts:      1780701727101,
			Metrics: &messageUsageBlock{InputTokens: 1, OutputTokens: 1},
			// Deliberately mixed types in a known order. Output
			// must match this order block-for-block.
			Content: []messageBlock{
				{Type: "tool_use", ID: "u1", Name: "first"},
				{Type: "text", Text: "second"},
				{Type: "thinking", Thinking: "third"},
				{Type: "tool_use", ID: "u4", Name: "fourth"},
			},
		},
	}
	out := buildCacheObservations([]sessionRow{s}, "/x/sessions.db")
	if len(out) != 1 {
		t.Fatalf("observations = %d, want 1", len(out))
	}
	wantKinds := []string{"tool_use", "text", "thinking", "tool_use"}
	for i, b := range out[0].BlockHashes {
		if b.Kind != wantKinds[i] {
			t.Errorf("block[%d] Kind = %q, want %q", i, b.Kind, wantKinds[i])
		}
	}
}

// TestBuildCacheObservations_SkipsAssistantWithoutMetrics pins
// the carve-out: an assistant message whose metrics block is nil
// (early streaming row, or a slot that never completed) emits
// NO CacheTurnObservation. Matches the existing perMessageTokenEvent
// gate.
func TestBuildCacheObservations_SkipsAssistantWithoutMetrics(t *testing.T) {
	t.Parallel()

	s := sessionRow{
		ID:           "sess-nometrics",
		Model:        "x",
		MessagesPath: sql.NullString{String: "/synthetic/path/sess-nometrics.messages.json", Valid: true},
	}
	s.Messages.Messages = []messageRecord{
		{
			ID:      "msg_a",
			Role:    "assistant",
			Ts:      1780701727101,
			Content: []messageBlock{{Type: "text", Text: "in progress"}},
		},
	}
	out := buildCacheObservations([]sessionRow{s}, "/x/sessions.db")
	if got := len(out); got != 0 {
		t.Fatalf("CacheObservations = %d, want 0 (no metrics block — assistant turn in progress)", got)
	}
}
