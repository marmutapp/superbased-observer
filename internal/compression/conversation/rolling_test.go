package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestEstimateTokens_RoughChars4 pins the heuristic: total bytes / 4.
// Not byte-exact (real tokenisation differs) but stable enough that
// the threshold check is deterministic per input.
func TestEstimateTokens_RoughChars4(t *testing.T) {
	msgs := []Message{
		{Role: "user", Text: "hello world"},
		{Role: "assistant", Text: "hi there"},
	}
	got := EstimateTokens(msgs)
	want := (len("user") + len("hello world") + len("assistant") + len("hi there")) / 4
	if got != want {
		t.Errorf("estimate: got %d, want %d", got, want)
	}
}

// TestSummaryCache_RoundTrip pins the dedup property: the same input
// hashes to the same key, retrieving the cached summary.
func TestSummaryCache_RoundTrip(t *testing.T) {
	c := newSummaryCache(0)
	msgs := []Message{{Role: "user", Text: "a"}, {Role: "assistant", Text: "b"}}
	if _, ok := c.Get(msgs); ok {
		t.Errorf("cache hit on empty cache")
	}
	c.Put(msgs, "summary text")
	got, ok := c.Get(msgs)
	if !ok {
		t.Errorf("cache miss after put")
	}
	if got != "summary text" {
		t.Errorf("cache value: got %q, want %q", got, "summary text")
	}
}

// TestSummaryCache_DropsAtCap pins the eviction: at-cap triggers a
// drop-everything reset rather than serving stale entries forever.
func TestSummaryCache_DropsAtCap(t *testing.T) {
	c := newSummaryCache(2)
	c.Put([]Message{{Role: "u", Text: "a"}}, "A")
	c.Put([]Message{{Role: "u", Text: "b"}}, "B")
	// Third put hits the cap → cache resets, only the new entry remains.
	c.Put([]Message{{Role: "u", Text: "c"}}, "C")
	if got, ok := c.Get([]Message{{Role: "u", Text: "a"}}); ok {
		t.Errorf("evicted entry survived: %q", got)
	}
	if got, ok := c.Get([]Message{{Role: "u", Text: "c"}}); !ok || got != "C" {
		t.Errorf("newest entry lost: ok=%v got=%q", ok, got)
	}
}

// TestSummaryCache_Deterministic pins the load-bearing property: same
// input → same hash → same cache key. Without this, rolling-summ
// re-calls the upstream summariser on every turn even for an
// unchanged prefix.
func TestSummaryCache_Deterministic(t *testing.T) {
	c := newSummaryCache(0)
	msgs := []Message{{Role: "u", Text: "x"}, {Role: "a", Text: "y"}}
	first := c.hash(msgs)
	for i := 0; i < 50; i++ {
		if got := c.hash(msgs); got != first {
			t.Fatalf("non-deterministic on iteration %d: %q vs %q", i, got, first)
		}
	}
}

// mockSummarizer is a deterministic test double for the Summarizer
// interface: returns a fixed string from the constructor or an error.
type mockSummarizer struct {
	summary string
	err     error
	calls   int
}

func (m *mockSummarizer) Summarize(_ context.Context, _ []Message) (string, error) {
	m.calls++
	return m.summary, m.err
}

type mockFactory struct {
	s    Summarizer
	want string // sessionID gate; "" matches any
}

func (f *mockFactory) For(sessionID string) Summarizer {
	if f.want != "" && f.want != sessionID {
		return nil
	}
	return f.s
}

// TestSummarizeIfThreshold_FiresAboveThreshold pins the happy path:
// when the message list crosses the token threshold and the factory
// returns a summariser, the older messages are replaced with the
// canonical marker form.
func TestSummarizeIfThreshold_FiresAboveThreshold(t *testing.T) {
	long := strings.Repeat("filler text ", 1000) // ~12000 bytes ≈ 3000 tokens
	msgs := []Message{}
	for i := 0; i < 10; i++ {
		msgs = append(msgs, Message{Role: "user", Text: long, ByteLen: len(long)})
		msgs = append(msgs, Message{Role: "assistant", Text: long, ByteLen: len(long)})
	}
	// Add a tail.
	msgs = append(msgs, Message{Role: "user", Text: "what now", ByteLen: 8})

	mock := &mockSummarizer{summary: "Recap of earlier work."}
	p := &Pipeline{
		cfg:                    PipelineConfig{PreserveLastN: 6},
		summarizerFactory:      &mockFactory{s: mock},
		rollingThresholdTokens: 100,
	}
	out, events := p.summarizeIfThreshold(context.Background(), msgs, "session-x", "anthropic")
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d (%+v)", len(events), events)
	}
	if events[0].Mechanism != "rolling_summary" {
		t.Errorf("mechanism: %q", events[0].Mechanism)
	}
	if mock.calls != 1 {
		t.Errorf("expected 1 summariser call, got %d", mock.calls)
	}
	if !strings.Contains(out[0].Text, "Recap of earlier work.") {
		t.Errorf("marker missing summary text: %q", out[0].Text)
	}
	// First message is the marker, rest are tail (preserve=6).
	if len(out) != 1+6 {
		t.Errorf("expected 7 messages out (marker + 6 preserved), got %d", len(out))
	}
}

// TestSummarizeIfThreshold_BelowThresholdNoOp pins the gate: under-
// threshold conversations are returned unchanged with zero events.
func TestSummarizeIfThreshold_BelowThresholdNoOp(t *testing.T) {
	msgs := []Message{
		{Role: "user", Text: "hi", ByteLen: 2},
		{Role: "assistant", Text: "hi", ByteLen: 2},
	}
	mock := &mockSummarizer{summary: "should not be called"}
	p := &Pipeline{
		cfg:                    PipelineConfig{PreserveLastN: 6},
		summarizerFactory:      &mockFactory{s: mock},
		rollingThresholdTokens: 100,
	}
	out, events := p.summarizeIfThreshold(context.Background(), msgs, "session-x", "anthropic")
	if len(events) != 0 {
		t.Errorf("expected 0 events under threshold, got %+v", events)
	}
	if mock.calls != 0 {
		t.Errorf("summariser called below threshold: %d", mock.calls)
	}
	if len(out) != len(msgs) {
		t.Errorf("messages mutated below threshold: in=%d out=%d", len(msgs), len(out))
	}
}

// TestSummarizeIfThreshold_NoFactoryNoOp pins that the feature is
// opt-in: without a factory, the pipeline never tries to summarise.
func TestSummarizeIfThreshold_NoFactoryNoOp(t *testing.T) {
	long := strings.Repeat("x", 100_000)
	msgs := []Message{{Role: "user", Text: long, ByteLen: len(long)}}
	for i := 0; i < 10; i++ {
		msgs = append(msgs, Message{Role: "assistant", Text: long, ByteLen: len(long)})
	}
	p := &Pipeline{cfg: PipelineConfig{PreserveLastN: 2}, rollingThresholdTokens: 100}
	out, events := p.summarizeIfThreshold(context.Background(), msgs, "session-x", "anthropic")
	if len(events) != 0 || len(out) != len(msgs) {
		t.Errorf("expected no-op without factory, got events=%d outLen=%d", len(events), len(out))
	}
}

// TestSummarizeIfThreshold_NoSessionNoOp pins that empty session_id
// disables the feature so legacy [Pipeline.Run] callers don't
// accidentally trigger summarisation.
func TestSummarizeIfThreshold_NoSessionNoOp(t *testing.T) {
	long := strings.Repeat("x", 100_000)
	msgs := []Message{{Role: "user", Text: long, ByteLen: len(long)}}
	for i := 0; i < 10; i++ {
		msgs = append(msgs, Message{Role: "assistant", Text: long, ByteLen: len(long)})
	}
	mock := &mockSummarizer{summary: "should not be called"}
	p := &Pipeline{
		cfg:                    PipelineConfig{PreserveLastN: 2},
		summarizerFactory:      &mockFactory{s: mock},
		rollingThresholdTokens: 100,
	}
	out, events := p.summarizeIfThreshold(context.Background(), msgs, "", "anthropic")
	if len(events) != 0 || len(out) != len(msgs) {
		t.Errorf("expected no-op without session, got events=%d outLen=%d", len(events), len(out))
	}
	if mock.calls != 0 {
		t.Errorf("summariser called without session: %d", mock.calls)
	}
}

// TestSummarizeIfThreshold_SummarizerErrorNoOp pins the safety
// property: when Summarize returns an error, the pipeline returns
// the original messages and zero events instead of producing a
// degraded request.
func TestSummarizeIfThreshold_SummarizerErrorNoOp(t *testing.T) {
	long := strings.Repeat("filler ", 500)
	msgs := []Message{}
	for i := 0; i < 20; i++ {
		msgs = append(msgs, Message{Role: "user", Text: long, ByteLen: len(long)})
	}
	mock := &mockSummarizer{err: errors.New("upstream rate limit")}
	p := &Pipeline{
		cfg:                    PipelineConfig{PreserveLastN: 6},
		summarizerFactory:      &mockFactory{s: mock},
		rollingThresholdTokens: 100,
	}
	out, events := p.summarizeIfThreshold(context.Background(), msgs, "s", "anthropic")
	if len(events) != 0 {
		t.Errorf("expected zero events on summariser error, got %+v", events)
	}
	if len(out) != len(msgs) {
		t.Errorf("messages mutated despite summariser error")
	}
}

// TestSummarizableBoundary_PreservesToolPair pins the load-bearing
// behavioural fix: when initialK splits a tool_use/tool_result pair,
// the boundary expands backward to keep the producer in the tail.
// This is the regression test for the v1.4.43+ "unexpected
// tool_use_id" bug we hit live: D20 was dropping msgs[:K] wholesale
// without checking whether referenced tool_use producers lived in
// the dropped range.
func TestSummarizableBoundary_PreservesToolPair(t *testing.T) {
	// 20 messages. msg[4] is the assistant producing toolu_X; msg[5]
	// is the user with the tool_result for toolu_X. With initialK=14
	// (PreserveLastN=6 on 20 msgs), naive boundary would drop msg[4]
	// AND msg[5]. That's actually fine — both sides of the pair land
	// in the dropped range, no orphan. So this case is a no-op for
	// the safety check.
	//
	// The interesting case: msg[4] produces toolu_X, but the
	// tool_result for toolu_X is in the TAIL (msg[14]). Naive K=14
	// drops the producer (msg[4]) and keeps the consumer (msg[14]) →
	// API rejection. Post-fix, K expands to 4 to keep the producer.
	msgs := make([]Message, 20)
	for i := range msgs {
		msgs[i] = Message{Role: "user", Text: "filler", ByteLen: 6}
	}
	msgs[4] = Message{
		Role: "assistant", Text: "running tool", ByteLen: 12,
		Raw:        json.RawMessage(`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_X","name":"Bash","input":{}}]}`),
		ToolUseIDs: []string{"toolu_X"},
	}
	msgs[14] = Message{
		Role: "user", Text: "tool result", ByteLen: 11,
		Raw:           json.RawMessage(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_X","content":"ok"}]}`),
		ReferencedIDs: []string{"toolu_X"},
	}

	got := summarizableBoundary(msgs, 14)
	if got != 4 {
		t.Errorf("boundary should expand to 4 to preserve tool_use producer; got %d", got)
	}
}

// TestSummarizableBoundary_NoToolPairsIsNoOp pins that the boundary
// stays at initialK when the conversation has no tool_use/tool_result
// blocks at all (the common case for non-tool-using sessions).
func TestSummarizableBoundary_NoToolPairsIsNoOp(t *testing.T) {
	msgs := make([]Message, 20)
	for i := range msgs {
		msgs[i] = Message{Role: "user", Text: "filler", ByteLen: 6}
	}
	got := summarizableBoundary(msgs, 14)
	if got != 14 {
		t.Errorf("no-tool-pairs boundary should stay at initialK=14, got %d", got)
	}
}

// TestSummarizableBoundary_PairFullyInDroppedRangeIsNoOp pins that
// when both sides of a tool_use/tool_result pair are inside the
// dropped range (msgs[:initialK]), no expansion is needed.
func TestSummarizableBoundary_PairFullyInDroppedRangeIsNoOp(t *testing.T) {
	msgs := make([]Message, 20)
	for i := range msgs {
		msgs[i] = Message{Role: "user", Text: "filler", ByteLen: 6}
	}
	msgs[4] = Message{
		Role:       "assistant",
		Raw:        json.RawMessage(`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_X","name":"Bash"}]}`),
		ToolUseIDs: []string{"toolu_X"},
	}
	msgs[5] = Message{
		Role:          "user",
		Raw:           json.RawMessage(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_X","content":"ok"}]}`),
		ReferencedIDs: []string{"toolu_X"},
	}
	got := summarizableBoundary(msgs, 14)
	if got != 14 {
		t.Errorf("complete-pair-in-dropped-range should not expand boundary; got %d (want 14)", got)
	}
}

// TestSummarizeIfThreshold_PreservesToolPairsAcrossBoundary is the
// integration-level pin: the bug we hit in production was D20
// emitting a request body where msg[14]'s tool_result.tool_use_id
// had no corresponding tool_use because msg[4] was summarised away.
// Post-fix, summarizeIfThreshold returns an output where the
// tool_use producer is preserved in the tail.
func TestSummarizeIfThreshold_PreservesToolPairsAcrossBoundary(t *testing.T) {
	long := strings.Repeat("filler ", 500)
	msgs := make([]Message, 20)
	for i := range msgs {
		msgs[i] = Message{Role: "user", Text: long, ByteLen: len(long)}
	}
	msgs[4] = Message{
		Role: "assistant", Text: long, ByteLen: len(long),
		Raw:        json.RawMessage(`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_X","name":"Bash","input":{}}]}`),
		ToolUseIDs: []string{"toolu_X"},
	}
	msgs[14] = Message{
		Role: "user", Text: long, ByteLen: len(long),
		Raw:           json.RawMessage(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_X","content":"ok"}]}`),
		ReferencedIDs: []string{"toolu_X"},
	}

	mock := &mockSummarizer{summary: "Recap."}
	p := &Pipeline{
		cfg:                    PipelineConfig{PreserveLastN: 6},
		summarizerFactory:      &mockFactory{s: mock},
		rollingThresholdTokens: 100,
	}
	out, events := p.summarizeIfThreshold(context.Background(), msgs, "tp", "anthropic")
	if len(events) != 1 {
		t.Fatalf("expected 1 rolling_summary event, got %d", len(events))
	}
	// Boundary expanded from naive 14 → 4. Output = 1 marker + 16
	// preserved messages (indices 4..19).
	if len(out) != 1+16 {
		t.Errorf("expected 17 output messages (marker + 16 preserved from index 4), got %d", len(out))
	}
	// The first preserved message must be msg[4] — the tool_use
	// producer. Without the fix it would be msg[14] (the orphaned
	// consumer) and the output would fail Anthropic's validator.
	if string(out[1].Raw) != string(msgs[4].Raw) {
		t.Errorf("first preserved message should be the tool_use producer at index 4 (Raw=%s); got Raw=%s",
			msgs[4].Raw, out[1].Raw)
	}
}

// TestSummarizeIfThreshold_StickyBoundaryAcrossTurns pins the v1.4.43+
// cross-turn-invariance contract: once a session has been summarised
// at boundary K, subsequent turns with msgs ≤ K + 2*PreserveLastN
// reuse the same summary + boundary so the prefix bytes stay byte-
// identical (Anthropic prefix-cache hits hold). Only when the tail
// grows past the trigger does a new summary land.
func TestSummarizeIfThreshold_StickyBoundaryAcrossTurns(t *testing.T) {
	long := strings.Repeat("filler ", 500)
	mock := &mockSummarizer{summary: "Recap"}
	p := &Pipeline{
		cfg:                    PipelineConfig{PreserveLastN: 6},
		summarizerFactory:      &mockFactory{s: mock},
		rollingThresholdTokens: 100,
	}

	// Turn N: 21 msgs → boundary K = 21 - 6 = 15. One upstream call.
	turnN := []Message{}
	for i := 0; i < 21; i++ {
		turnN = append(turnN, Message{Role: "user", Text: long, ByteLen: len(long)})
	}
	outN, eventsN := p.summarizeIfThreshold(context.Background(), turnN, "stick", "anthropic")
	if len(eventsN) != 1 {
		t.Fatalf("turn N: expected 1 event, got %d", len(eventsN))
	}
	markerN := outN[0].Text

	// Turn N+1: append 4 more messages. 25 msgs total. Boundary still
	// 15 — tail = 10, ≤ 2*6 = 12. Reuse summary.
	turnN1 := append([]Message{}, turnN...)
	for i := 0; i < 4; i++ {
		turnN1 = append(turnN1, Message{Role: "assistant", Text: long, ByteLen: len(long)})
	}
	outN1, eventsN1 := p.summarizeIfThreshold(context.Background(), turnN1, "stick", "anthropic")
	if len(eventsN1) != 1 {
		t.Fatalf("turn N+1: expected 1 event (still firing), got %d", len(eventsN1))
	}
	if outN1[0].Text != markerN {
		t.Errorf("marker text changed across turns (cross-turn invariance broken):\n N:  %q\n N+1:%q", markerN, outN1[0].Text)
	}
	if mock.calls != 1 {
		t.Errorf("upstream re-called when boundary should have been sticky: %d calls", mock.calls)
	}

	// Turn N+2: append 9 more messages. 34 msgs total. Tail at K=15
	// would be 19 — past 2*6=12. Re-summarise.
	turnN2 := append([]Message{}, turnN1...)
	for i := 0; i < 9; i++ {
		turnN2 = append(turnN2, Message{Role: "user", Text: long, ByteLen: len(long)})
	}
	outN2, eventsN2 := p.summarizeIfThreshold(context.Background(), turnN2, "stick", "anthropic")
	if len(eventsN2) != 1 {
		t.Fatalf("turn N+2: expected 1 event, got %d", len(eventsN2))
	}
	if mock.calls != 2 {
		t.Errorf("upstream not re-called when tail outgrew threshold: %d calls", mock.calls)
	}
	// New boundary should be 34 - 6 = 28 → marker text reflects the
	// new count.
	if !strings.Contains(outN2[0].Text, "28 earlier messages") {
		t.Errorf("turn N+2 marker should reflect new boundary 28: %q", outN2[0].Text)
	}
}

// TestSummarizeIfThreshold_CacheHitsAvoidUpstream pins the
// idempotency property: a re-run with the same input messages reads
// from the cache instead of re-calling the summariser.
func TestSummarizeIfThreshold_CacheHitsAvoidUpstream(t *testing.T) {
	long := strings.Repeat("filler ", 500)
	msgs := []Message{}
	for i := 0; i < 20; i++ {
		msgs = append(msgs, Message{Role: "user", Text: long, ByteLen: len(long)})
	}
	mock := &mockSummarizer{summary: "Cached summary."}
	p := &Pipeline{
		cfg:                    PipelineConfig{PreserveLastN: 6},
		summarizerFactory:      &mockFactory{s: mock},
		rollingThresholdTokens: 100,
	}
	for i := 0; i < 5; i++ {
		_, _ = p.summarizeIfThreshold(context.Background(), msgs, "s", "anthropic")
	}
	if mock.calls != 1 {
		t.Errorf("expected 1 summariser call across 5 invocations (cache hits), got %d", mock.calls)
	}
}
