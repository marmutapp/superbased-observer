package proxy

import (
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// TestParseOpenAIResponse_CacheWrite pins the GPT-5.6 cache-write capture on
// the non-streaming OpenAI lane against grounded raw-body shapes. It asserts
// three invariants at once:
//
//   - Input is NET of cached_tokens exactly as before — cache_write_tokens is
//     a SUBSET of the non-cached input and DISJOINT from cached_tokens, so it
//     is NEVER subtracted from input (no double-subtract regression).
//   - CacheReadTokens carries cached_tokens (prompt_* preferred, input_*
//     fallback — the Responses-API shape).
//   - CacheCreationTokens carries cache_write_tokens; CacheCreation1hTokens
//     stays 0 (OpenAI writes are untiered).
func TestParseOpenAIResponse_CacheWrite(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		body           string
		wantInput      int64
		wantOutput     int64
		wantRead       int64
		wantWrite      int64
		want1hCreation int64
	}{
		{
			// Chat Completions shape, zero write (ChatGPT-plan lane) —
			// must be byte-identical to pre-change accounting.
			name:       "chat_completions_zero_write",
			body:       `{"id":"chatcmpl-1","model":"gpt-5.6-sol","choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":200,"prompt_tokens_details":{"cached_tokens":300,"cache_write_tokens":0}}}`,
			wantInput:  700, // 1000 gross - 300 cached
			wantOutput: 200,
			wantRead:   300,
			wantWrite:  0,
		},
		{
			// Chat Completions shape, nonzero write.
			name:       "chat_completions_nonzero_write",
			body:       `{"id":"chatcmpl-2","model":"gpt-5.6-sol","choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":200,"prompt_tokens_details":{"cached_tokens":300,"cache_write_tokens":150}}}`,
			wantInput:  700, // write is NOT subtracted a second time
			wantOutput: 200,
			wantRead:   300,
			wantWrite:  150,
		},
		{
			// Responses API shape (grounded gpt-5.6 sample): read+write
			// nest under input_tokens_details.
			name:       "responses_api_nonzero_write",
			body:       `{"id":"resp_1","model":"gpt-5.6-sol","usage":{"input_tokens":31841,"input_tokens_details":{"cache_write_tokens":641,"cached_tokens":30464},"output_tokens":199,"output_tokens_details":{"reasoning_tokens":32}}}`,
			wantInput:  1377, // 31841 gross - 30464 cached (write 641 not re-subtracted)
			wantOutput: 199,
			wantRead:   30464,
			wantWrite:  641,
		},
		{
			// Grounded gpt-5.6 sample verbatim: cache_write_tokens=0.
			name:       "responses_api_grounded_zero_write",
			body:       `{"id":"resp_0","model":"gpt-5.6-sol","usage":{"input_tokens":31841,"input_tokens_details":{"cache_write_tokens":0,"cached_tokens":30464},"output_tokens":199,"output_tokens_details":{"reasoning_tokens":32}}}`,
			wantInput:  1377,
			wantOutput: 199,
			wantRead:   30464,
			wantWrite:  0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseOpenAIResponse([]byte(tc.body))
			if got.InputTokens != tc.wantInput {
				t.Errorf("InputTokens = %d, want %d (net of cached, write never re-subtracted)", got.InputTokens, tc.wantInput)
			}
			if got.OutputTokens != tc.wantOutput {
				t.Errorf("OutputTokens = %d, want %d", got.OutputTokens, tc.wantOutput)
			}
			if got.CacheReadTokens != tc.wantRead {
				t.Errorf("CacheReadTokens = %d, want %d", got.CacheReadTokens, tc.wantRead)
			}
			if got.CacheCreationTokens != tc.wantWrite {
				t.Errorf("CacheCreationTokens = %d, want %d (cache_write_tokens)", got.CacheCreationTokens, tc.wantWrite)
			}
			if got.CacheCreation1hTokens != tc.want1hCreation {
				t.Errorf("CacheCreation1hTokens = %d, want %d (OpenAI writes untiered)", got.CacheCreation1hTokens, tc.want1hCreation)
			}
		})
	}
}

// TestParseSSEStream_OpenAI_CacheWrite mirrors the non-streaming test on the
// streaming lane, covering both the top-level chat.completions usage and the
// response.completed Responses-API-nested usage.
func TestParseSSEStream_OpenAI_CacheWrite(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		sse        string
		wantInput  int64
		wantOutput int64
		wantRead   int64
		wantWrite  int64
	}{
		{
			name: "chat_completions_top_level",
			sse: "data: {\"id\":\"chatcmpl-s\",\"model\":\"gpt-5.6-sol\",\"choices\":[],\"usage\":{\"prompt_tokens\":1000,\"completion_tokens\":200," +
				"\"prompt_tokens_details\":{\"cached_tokens\":300,\"cache_write_tokens\":150}}}\n\n",
			wantInput:  700,
			wantOutput: 200,
			wantRead:   300,
			wantWrite:  150,
		},
		{
			name: "responses_api_completed",
			sse: "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_s\",\"model\":\"gpt-5.6-sol\",\"status\":\"completed\"," +
				"\"usage\":{\"input_tokens\":31841,\"output_tokens\":199,\"input_tokens_details\":{\"cached_tokens\":30464,\"cache_write_tokens\":641}}}}\n\n",
			wantInput:  1377,
			wantOutput: 199,
			wantRead:   30464,
			wantWrite:  641,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseSSEStream([]byte(tc.sse), models.ProviderOpenAI)
			if got.InputTokens != tc.wantInput {
				t.Errorf("InputTokens = %d, want %d", got.InputTokens, tc.wantInput)
			}
			if got.OutputTokens != tc.wantOutput {
				t.Errorf("OutputTokens = %d, want %d", got.OutputTokens, tc.wantOutput)
			}
			if got.CacheReadTokens != tc.wantRead {
				t.Errorf("CacheReadTokens = %d, want %d", got.CacheReadTokens, tc.wantRead)
			}
			if got.CacheCreationTokens != tc.wantWrite {
				t.Errorf("CacheCreationTokens = %d, want %d (cache_write_tokens)", got.CacheCreationTokens, tc.wantWrite)
			}
			if got.CacheCreation1hTokens != 0 {
				t.Errorf("CacheCreation1hTokens = %d, want 0 (OpenAI writes untiered)", got.CacheCreation1hTokens)
			}
		})
	}
}

// TestBuildCacheObserveInput_OpenAI_CacheWritePassThrough confirms a nonzero
// cache_write_tokens count reaches cachetrack via CacheUsageObserved so
// cache_events reflect real writes when a metered API-key turn lands.
func TestBuildCacheObserveInput_OpenAI_CacheWritePassThrough(t *testing.T) {
	t.Parallel()
	p := newTestProxyMinimal()
	turn := models.APITurn{
		SessionID:           "sess-openai-write",
		Model:               "gpt-5.6-sol",
		RequestID:           "req-openai-write",
		InputTokens:         1377,
		OutputTokens:        199,
		CacheReadTokens:     30464,
		CacheCreationTokens: 641,
	}
	in, ok := p.buildCacheObserveInput(turn, requestShape{Model: "gpt-5.6-sol"}, models.ProviderOpenAI, 9)
	if !ok {
		t.Fatalf("OpenAI path returned ok=false; want true")
	}
	if in.Usage.CacheCreationTokens != 641 {
		t.Errorf("CacheCreationTokens not threaded to cachetrack; got %d want 641", in.Usage.CacheCreationTokens)
	}
	if in.Usage.CacheCreation1hTokens != 0 {
		t.Errorf("CacheCreation1hTokens = %d, want 0 (OpenAI writes untiered)", in.Usage.CacheCreation1hTokens)
	}
	if in.Usage.CacheReadTokens != 30464 {
		t.Errorf("CacheReadTokens = %d, want 30464", in.Usage.CacheReadTokens)
	}
}
