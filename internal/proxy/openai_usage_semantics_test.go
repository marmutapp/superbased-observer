package proxy

import (
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// TestApplyOpenAIUsage_MultiEventSemantics pins the accumulation semantics of
// applyOpenAIUsage across the multiple usage-bearing SSE events a single
// stream can carry. The function is LAST-NONZERO-WINS per field: a nonzero
// value overwrites, a zero is a no-op (never clobbers a prior nonzero). This
// is deliberate — OpenAI emits cumulative (monotonic) usage only on the
// terminal event, and empty/partial usage objects on intermediate events must
// not zero out a real count.
func TestApplyOpenAIUsage_MultiEventSemantics(t *testing.T) {
	t.Parallel()

	// One SSE usage event, in applyOpenAIUsage's positional argument order.
	type event struct {
		promptTokens, completionTokens int64
		inputTokens, outputTokens      int64
		promptCached, inputCached      int64
		promptWrite, inputWrite        int64
	}

	cases := []struct {
		name       string
		events     []event
		wantInput  int64
		wantOutput int64
		wantRead   int64
		wantWrite  int64
	}{
		{
			// (a) Cumulative usage events followed by a final: because
			// OpenAI usage is monotonic, last-nonzero-wins and last-wins
			// coincide — the terminal (largest) values survive.
			name: "cumulative_then_final_last_wins",
			events: []event{
				{promptTokens: 500, completionTokens: 50, promptCached: 100, promptWrite: 40},
				{promptTokens: 1000, completionTokens: 200, promptCached: 300, promptWrite: 150},
			},
			wantInput:  700, // 1000 gross - 300 cached
			wantOutput: 200,
			wantRead:   300,
			wantWrite:  150,
		},
		{
			// (b) A final usage event with write=0 (and read=0) AFTER a
			// mid-stream nonzero. Under last-nonzero-wins the zeros in the
			// trailing event do NOT clobber the earlier real counts — the
			// nonzero write/read survive. (This is NOT final-wins; it is
			// the documented existing behaviour. On the wire OpenAI never
			// emits a monotonic count that drops back to 0, so the guard
			// only ever fires against a spurious empty-usage event.)
			name: "mid_nonzero_then_trailing_zero_keeps_nonzero",
			events: []event{
				{promptTokens: 1000, completionTokens: 200, promptCached: 300, promptWrite: 150},
				{promptTokens: 0, completionTokens: 0, promptCached: 0, promptWrite: 0},
			},
			wantInput:  700, // unchanged from the mid-stream event
			wantOutput: 200,
			wantRead:   300,
			wantWrite:  150,
		},
		{
			// (d) A single event where the PREFERRED prompt_tokens_details
			// shape reports cached=0/write=0 while the FALLBACK
			// input_tokens_details carries the real nonzero counts. The
			// preference must prefer nonzero: a 0 in the preferred shape must
			// NOT shadow the nonzero fallback. Reads and writes both.
			name: "preferred_zero_does_not_shadow_nonzero_fallback",
			events: []event{
				{inputTokens: 1000, outputTokens: 200, promptCached: 0, inputCached: 300, promptWrite: 0, inputWrite: 150},
			},
			wantInput:  700, // 1000 gross - 300 cached (from the fallback shape)
			wantOutput: 200,
			wantRead:   300,
			wantWrite:  150,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var r streamResult
			for _, e := range tc.events {
				applyOpenAIUsage(
					&r,
					e.promptTokens, e.completionTokens,
					e.inputTokens, e.outputTokens,
					e.promptCached, e.inputCached,
					e.promptWrite, e.inputWrite,
				)
			}
			if r.InputTokens != tc.wantInput {
				t.Errorf("InputTokens = %d, want %d", r.InputTokens, tc.wantInput)
			}
			if r.OutputTokens != tc.wantOutput {
				t.Errorf("OutputTokens = %d, want %d", r.OutputTokens, tc.wantOutput)
			}
			if r.CacheReadTokens != tc.wantRead {
				t.Errorf("CacheReadTokens = %d, want %d", r.CacheReadTokens, tc.wantRead)
			}
			if r.CacheCreationTokens != tc.wantWrite {
				t.Errorf("CacheCreationTokens = %d, want %d", r.CacheCreationTokens, tc.wantWrite)
			}
			if r.CacheCreation1hTokens != 0 {
				t.Errorf("CacheCreation1hTokens = %d, want 0 (OpenAI writes untiered)", r.CacheCreation1hTokens)
			}
		})
	}
}

// TestParseSSEStream_OpenAI_BothTopLevelAndNested pins finding #2(c): one SSE
// event that carries BOTH a response.-nested usage object and a top-level
// usage object. The parse loop applies the nested usage first, then the
// top-level usage, so under last-nonzero-wins the top-level counts survive
// when both are nonzero. In practice only one shape is ever populated per
// event (Chat Completions => top-level; Responses API => nested), and the
// last-nonzero-wins guard is exactly what lets the empty shape's zeros not
// clobber the populated one — this test forces both nonzero to pin the order.
func TestParseSSEStream_OpenAI_BothTopLevelAndNested(t *testing.T) {
	t.Parallel()
	// Nested (response.usage) carries read=1000/write=500; top-level (usage)
	// carries read=300/write=150. Top-level is applied last => wins.
	sse := "data: {\"type\":\"response.completed\",\"model\":\"gpt-5.6-sol\"," +
		"\"response\":{\"id\":\"resp_both\",\"model\":\"gpt-5.6-sol\",\"status\":\"completed\"," +
		"\"usage\":{\"input_tokens\":5000,\"output_tokens\":40," +
		"\"input_tokens_details\":{\"cached_tokens\":1000,\"cache_write_tokens\":500}}}," +
		"\"usage\":{\"prompt_tokens\":1000,\"completion_tokens\":200," +
		"\"prompt_tokens_details\":{\"cached_tokens\":300,\"cache_write_tokens\":150}}}\n\n"

	got := parseSSEStream([]byte(sse), models.ProviderOpenAI)
	if got.CacheReadTokens != 300 {
		t.Errorf("CacheReadTokens = %d, want 300 (top-level applied last wins)", got.CacheReadTokens)
	}
	if got.CacheCreationTokens != 150 {
		t.Errorf("CacheCreationTokens = %d, want 150 (top-level applied last wins)", got.CacheCreationTokens)
	}
	if got.InputTokens != 700 { // 1000 gross - 300 cached
		t.Errorf("InputTokens = %d, want 700", got.InputTokens)
	}
	if got.OutputTokens != 200 {
		t.Errorf("OutputTokens = %d, want 200", got.OutputTokens)
	}
}

// TestParseOpenAIResponse_FixesNonStreamingResponsesCachedReads makes the
// 2026-07-16 behaviour change explicit (finding #3): before the
// input_tokens_details.cached_tokens fallback, a non-streaming /v1/responses
// body nested its cached-read count under input_tokens_details, which the
// parser never read. So this grounded gpt-5.6 sample previously produced
// Input=31841 / CacheRead=0 (cached reads over-billed at the full input rate).
// AFTER the fallback it correctly nets to Input=1377 / CacheRead=30464.
func TestParseOpenAIResponse_FixesNonStreamingResponsesCachedReads(t *testing.T) {
	t.Parallel()
	// Grounded gpt-5.6 Responses-API sample: cached read nested under
	// input_tokens_details, NO prompt_tokens_details present.
	body := `{"id":"resp_fix","model":"gpt-5.6-sol","usage":{"input_tokens":31841,"input_tokens_details":{"cached_tokens":30464,"cache_write_tokens":0},"output_tokens":199}}`

	got := parseOpenAIResponse([]byte(body))

	// Pre-change (documented): Input=31841, CacheRead=0.
	// Post-change (asserted):  Input=1377,  CacheRead=30464.
	if got.CacheReadTokens != 30464 {
		t.Errorf("CacheReadTokens = %d, want 30464 (was 0 pre-fallback)", got.CacheReadTokens)
	}
	if got.InputTokens != 1377 {
		t.Errorf("InputTokens = %d, want 1377 net (was 31841 gross pre-fallback)", got.InputTokens)
	}
}
