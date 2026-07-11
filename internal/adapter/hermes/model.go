package hermes

import "strings"

// stripProviderPrefix removes leading provider path segments from a
// Hermes model string before the cost engine's pricing lookup. Hermes
// emits provider-qualified model ids (anthropic/claude-sonnet-4.6,
// nvidia/nemotron-3-ultra:free, openrouter/anthropic/claude-sonnet-4.6,
// nous-portal/hermes-3-405b, …); our pricing tables key off the bare
// model id. Strip every leading "<segment>/" pair until no slash
// remains.
//
// The OpenRouter-style ":<suffix>" tail (nemotron-3-ultra:free,
// claude-sonnet-4.5:beta, …) is preserved verbatim:
//   - The dashboard's per-model breakdown wants the suffix to
//     distinguish :free vs paid tiers in the same model family.
//   - The cost engine can fall back to the suffix-less family-prefix
//     match (internal/intelligence/cost/pricing.go::familyKeys) when
//     it doesn't carry an exact-pricing entry for the :free / :beta
//     variant.
//
// Examples (see model_test.go for the full table):
//
//	nvidia/nemotron-3-ultra:free            → nemotron-3-ultra:free
//	openrouter/anthropic/claude-sonnet-4.6  → claude-sonnet-4.6
//	anthropic/claude-sonnet-4.6             → claude-sonnet-4.6
//	gpt-4o-mini                             → gpt-4o-mini (no-op)
//	a/b/c/d/model-id                        → model-id
//	anthropic/                              → "" (degenerate)
//
// Pricing aliases for hermes-specific / nous-portal / nvidia-tier
// models are deferred until a real session lands data we can
// calibrate against — the plan section 17.1.G defers cost-engine
// additions until the first observed nemotron / hermes-3 / nous-portal
// session forces a number to ship. Unknown models surface as
// reliability=unknown rows in the meantime, per the established
// observer convention.
func stripProviderPrefix(model string) string {
	for {
		idx := strings.Index(model, "/")
		if idx < 0 {
			return model
		}
		model = model[idx+1:]
	}
}
