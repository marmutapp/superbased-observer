package clinecli

import (
	"database/sql"
	"encoding/json"
	"testing"
)

// TestBuildCacheObservations_ImplicitRoutingOverlay drives the
// §15.3 boundary at the cline-cli emitter: when a message's
// ModelInfo.Provider names an implicit-cache provider, the
// observation overlays ImplicitCache=true with cleared BlockHashes.
// Anthropic-routed sessions and unknown providers stay on the
// existing path.
//
// cline-cli supports per-message provider switching, so the
// resolution prefers ModelInfo.Provider when set; the test pins
// both the per-message override AND the session-level fallback.
func TestBuildCacheObservations_ImplicitRoutingOverlay(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name            string
		sessionProvider string
		sessionModel    string
		messageProvider string
		messageModel    string
		wantImplicit    bool
		wantHasBlocks   bool
	}{
		{"session anthropic / no override", "anthropic", "claude-opus-4-8", "", "", false, true},
		{"session openai / no override", "openai", "gpt-5", "", "", true, false},
		{"session anthropic / message override openai", "anthropic", "claude-opus-4-8", "openai", "gpt-5", true, false},
		{"session openai / message override anthropic", "openai", "gpt-5", "anthropic", "claude-opus-4-8", false, true},
		{"session deepseek / no override", "deepseek", "deepseek-chat", "", "", true, false},
		{"session openrouter / no override", "openrouter", "openai/gpt-5", "", "", true, false},
		{"unknown provider stays anthropic-shape", "vertex-ai", "claude-opus-4-8", "", "", false, true},
		{"empty everything stays anthropic-shape (conservative)", "", "claude-opus-4-8", "", "", false, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := sessionRow{
				ID:           "sess-impl-overlay",
				Provider:     tc.sessionProvider,
				Model:        tc.sessionModel,
				MessagesPath: sql.NullString{String: "/synthetic/path.messages.json", Valid: true},
			}
			msg := messageRecord{
				ID:   "msg-a-1",
				Role: "assistant",
				Ts:   1780702110629,
				Metrics: &messageUsageBlock{
					InputTokens: 4000, OutputTokens: 100,
					CacheReadTokens: 2048, CacheWriteTokens: 0,
				},
				Content: []messageBlock{
					{Type: "text", Text: "hello"},
				},
			}
			if tc.messageProvider != "" || tc.messageModel != "" {
				msg.ModelInfo = &messageModelInfo{
					ID:       tc.messageModel,
					Provider: tc.messageProvider,
				}
			}
			s.Messages.Messages = []messageRecord{msg}

			obs := buildCacheObservations([]sessionRow{s}, "/dev/null")
			if len(obs) != 1 {
				t.Fatalf("got %d observations, want 1", len(obs))
			}
			if obs[0].ImplicitCache != tc.wantImplicit {
				t.Errorf("ImplicitCache = %v, want %v (session %q/%q msg %q/%q)",
					obs[0].ImplicitCache, tc.wantImplicit,
					tc.sessionProvider, tc.sessionModel,
					tc.messageProvider, tc.messageModel)
			}
			hasBlocks := len(obs[0].BlockHashes) > 0
			if hasBlocks != tc.wantHasBlocks {
				t.Errorf("BlockHashes len > 0 = %v, want %v", hasBlocks, tc.wantHasBlocks)
			}
		})
	}
	// silence unused-import goroutine if added by linters
	_ = json.Marshal
}
