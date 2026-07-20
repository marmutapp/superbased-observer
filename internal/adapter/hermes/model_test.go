package hermes

import "testing"

// TestStripProviderPrefix pins the model-id-strip rule applied before
// cost-engine pricing lookup. The reality check in
// docs/hermes-adapter-plan.md §17.1 D documented the live shape:
// nvidia/nemotron-3-ultra:free, anthropic/claude-sonnet-4.6,
// openrouter/anthropic/claude-sonnet-4.6, plus bare ids (gpt-4o-mini)
// from non-provider-prefixed providers. The OpenRouter ":suffix" tail
// is preserved so the dashboard can distinguish :free vs paid tiers.
//
// Pricing aliases for hermes-specific / nous-portal / nvidia-tier
// models are NOT introduced in this commit — they're deferred until a
// real session lands data we can calibrate against. Unknown models
// surface as reliability=unknown rows in the meantime per the
// established observer convention.
func TestStripProviderPrefix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare_no_prefix", "gpt-4o-mini", "gpt-4o-mini"},
		{"single_provider_anthropic", "anthropic/claude-sonnet-4.6", "claude-sonnet-4.6"},
		{"single_provider_openai", "openai/gpt-4o", "gpt-4o"},
		{"openrouter_double_anthropic", "openrouter/anthropic/claude-sonnet-4.6", "claude-sonnet-4.6"},
		{"openrouter_free_tier_live", "nvidia/nemotron-3-ultra:free", "nemotron-3-ultra:free"},
		{"openrouter_paid_tier_synthetic", "anthropic/claude-sonnet-4.6:beta", "claude-sonnet-4.6:beta"},
		{"nous_portal_hermes_3", "nous-portal/hermes-3-405b", "hermes-3-405b"},
		{"empty", "", ""},
		{"trailing_slash_degenerate", "anthropic/", ""},
		{"deeply_nested_5_segments", "a/b/c/d/model-id", "model-id"},
		{"leading_slash_root_no_provider", "/claude-sonnet-4.6", "claude-sonnet-4.6"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := stripProviderPrefix(tc.in); got != tc.want {
				t.Errorf("stripProviderPrefix(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
