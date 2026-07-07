package cachetrack

import "testing"

// TestIsImplicitCacheProvider pins the §15.3 routing decision per
// adapter boundary. One row per documented (providerID, modelID)
// → shape mapping; a new provider lands as a new row in
// implicitProviderTable AND a matching row here.
func TestIsImplicitCacheProvider(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name              string
		provider          string
		model             string
		wantImplicit      bool
		wantInternalShape providerRoutingShape
	}{
		// Anthropic — the existing soak-validated path.
		{"anthropic / opus", "anthropic", "claude-opus-4-8", false, shapeAnthropic},
		{"anthropic / sonnet", "anthropic", "claude-sonnet-4-6", false, shapeAnthropic},
		{"anthropic-bedrock / opus", "anthropic-bedrock", "claude-opus-4-7", false, shapeAnthropic},
		{"anthropic-vertex / sonnet", "anthropic-vertex", "claude-sonnet-4-5", false, shapeAnthropic},
		{"case-insensitive anthropic", "Anthropic", "Claude-Opus-4-8", false, shapeAnthropic},

		// Kilo Gateway — Anthropic-backed for kilo-auto family.
		{"kilo / kilo-auto", "kilo", "kilo-auto/claude-sonnet-4-6", false, shapeAnthropic},
		{"kilo-code / kilo-auto/small", "kilo-code", "kilo-auto/small", false, shapeAnthropic},
		// Kilo Gateway stealth/* — same Anthropic-backed gateway (live 2026-06-26).
		{"kilo / stealth claude", "kilo", "stealth/claude-sonnet-4.6", false, shapeAnthropic},
		{"kilo-code / stealth claude", "kilo-code", "stealth/claude-sonnet-4.6", false, shapeAnthropic},

		// OpenAI / OpenAI-compatible — implicit.
		{"openai / gpt-5", "openai", "gpt-5", true, shapeImplicit},
		{"openai / gpt-5-codex", "openai", "gpt-5-codex", true, shapeImplicit},
		{"openai / gpt-4o", "openai", "gpt-4o", true, shapeImplicit},
		{"azure-openai", "azure-openai", "gpt-5", true, shapeImplicit},
		{"azure with gpt model", "azure", "gpt-4o-mini", true, shapeImplicit},
		{"deepseek / chat", "deepseek", "deepseek-chat", true, shapeImplicit},
		{"deepseek / flash", "deepseek", "deepseek-flash", true, shapeImplicit},
		{"groq", "groq", "llama-3.1-70b", true, shapeImplicit},
		{"fireworks", "fireworks", "llama-v3p1-405b", true, shapeImplicit},
		{"together", "together", "Meta-Llama-3.1-70B", true, shapeImplicit},
		{"mistral", "mistral", "mistral-large", true, shapeImplicit},
		{"cohere", "cohere", "command-r+", true, shapeImplicit},

		// OpenRouter — default implicit.
		{"openrouter / any", "openrouter", "anthropic/claude-opus-4-8", true, shapeImplicit},
		{"openrouter / openai", "openrouter", "openai/gpt-5", true, shapeImplicit},

		// Unknown providers default to Anthropic (conservative bias).
		{"unknown provider", "vertex-ai", "claude-opus-4-8", false, shapeUnknown},
		{"empty provider", "", "claude-opus-4-8", false, shapeUnknown},
		{"empty everything", "", "", false, shapeUnknown},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := IsImplicitCacheProvider(tt.provider, tt.model); got != tt.wantImplicit {
				t.Errorf("IsImplicitCacheProvider(%q, %q) = %v, want %v", tt.provider, tt.model, got, tt.wantImplicit)
			}
			// Pin the internal shape too so a future regression
			// where we change the table but forget to test for
			// "unknown defaults to Anthropic-shape" surfaces.
			if got := resolveProviderShape(tt.provider, tt.model); got != tt.wantInternalShape {
				t.Errorf("resolveProviderShape(%q, %q) = %d, want %d", tt.provider, tt.model, got, tt.wantInternalShape)
			}
		})
	}
}

// TestIsImplicitCacheProvider_ConservativeUnknownDefault pins the
// safety rule called out in the doc comment: an unknown
// (provider, model) pair NEVER returns true. Adding a new provider
// without a table row should fail-safe — a wrong "implicit"
// classification silently un-grades a turn from the §10 soak-
// validated gate, which is harder to catch than a wrong
// "anthropic" classification that just surfaces as a mispredict.
func TestIsImplicitCacheProvider_ConservativeUnknownDefault(t *testing.T) {
	t.Parallel()
	for _, p := range []string{"completely-new-provider", "vertex-ai", "watsonx", "claude-via-aws"} {
		if IsImplicitCacheProvider(p, "any-model") {
			t.Errorf("IsImplicitCacheProvider(%q, ...) = true, want false — unknown providers must default to Anthropic (conservative bias; see implicitProviderTable doc)", p)
		}
	}
}
