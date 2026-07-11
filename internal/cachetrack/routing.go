package cachetrack

import "strings"

// §15.3 — routing helpers. Adapters that can run against MULTIPLE
// providers (cline-cli through Anthropic OR deepseek/OpenRouter;
// opencode/kilo through Anthropic OR OpenAI/etc.) need to resolve
// the implicit-cache capability AT THE ADAPTER BOUNDARY before
// emitting a CacheTurnObservation. This file is the single source
// of truth for that resolution — every adapter calls
// [IsImplicitCacheProvider] with its (providerID, modelID) and gets
// a boolean.
//
// Per CLAUDE.md §24.5 discipline: the routing is data, not an
// if/else ladder. Add a new implicit-cache provider by adding one
// row to [implicitProviderTable]; add a new test row to
// TestIsImplicitCacheProvider; everything else stays unchanged.
//
// THE GOLDEN RULE: a wrong answer in this function means an
// implicit-cache turn lands on the Anthropic §10 gate (false-
// positive mispredict pollution) OR an Anthropic turn lands on
// the reduced attribution path (silently un-graded by the soak-
// validated decision tree). Both directions are real regressions —
// adding a row here MUST land with a matching test row pinning the
// outcome.

// providerRoutingShape encodes the cache-shape one provider+model
// combination exposes. The closed vocabulary lives here, not in
// the table — adapters consume only [IsImplicitCacheProvider]'s
// bool, which collapses both shapes into the engine-facing flag.
type providerRoutingShape uint8

const (
	shapeUnknown providerRoutingShape = iota
	// shapeAnthropic — provider exposes Anthropic-shape cache
	// signals (cache_control markers + cache_creation_input_tokens
	// + cache_read_input_tokens + ephemeral_5m/1h breakdown).
	// Routes to the marker-aware §7 decision tree.
	shapeAnthropic
	// shapeImplicit — provider exposes only a scalar cached-input
	// count (OpenAI cached_tokens / cached_input_tokens). Routes
	// to the reduced [attributeImplicit] path.
	shapeImplicit
)

// implicitProviderRule matches a (providerID, modelID) pair into
// one [providerRoutingShape]. Walked top-down by
// [resolveProviderShape]; first match wins.
//
// Match strategy:
//   - providerID is matched verbatim (case-insensitive) — opencode
//     / kilo emit canonical strings like "anthropic", "openai",
//     "openrouter".
//   - modelID is matched as a substring (case-insensitive). Empty
//     ModelMatch matches any model under that provider — useful
//     for "openai" → always implicit. A non-empty modelMatch is
//     consulted only when providerID alone is ambiguous (e.g.
//     openrouter routes to any of dozens of upstreams; the model
//     string carries the routing identity).
type implicitProviderRule struct {
	ProviderMatch string // case-insensitive verbatim providerID
	ModelMatch    string // case-insensitive substring; "" = any
	Shape         providerRoutingShape
}

// implicitProviderTable is the §15.3 routing table. Walked
// top-down; first match wins. Adding a new provider:
//
//  1. Add a row here (more-specific BEFORE more-general — e.g. a
//     specific OpenRouter model goes ABOVE the bare "openrouter"
//     fallback).
//  2. Add a test row in TestIsImplicitCacheProvider pinning the
//     expected shape for representative model strings.
//
// Today's coverage:
//   - anthropic       → Anthropic (the existing soak-validated path)
//   - openai          → Implicit (every OpenAI/Azure-OpenAI model)
//   - openrouter      → Implicit by default (most OpenRouter
//     upstreams are OpenAI-shape; the few Anthropic-routed ones
//     need a more-specific row if a future capture proves the
//     pass-through preserves cache_control markers — TBD per the
//     §15.3 plan's Phase 0 "live soak end-deferred" item).
//   - deepseek        → Implicit (deepseek-flash via OpenRouter
//     was the cline-cli fixture in the spec §15.3(c) arc; the
//     audit doc confirmed implicit-cache shape).
//   - gateway aliases (kilo-auto family on kilo's Gateway) — Anthropic
//     by default (the gateway is Anthropic-backed for the auto-*
//     SKUs per the v1.8.2 kilo audit).
var implicitProviderTable = []implicitProviderRule{
	// Anthropic — the existing path.
	{ProviderMatch: "anthropic", ModelMatch: "", Shape: shapeAnthropic},
	{ProviderMatch: "anthropic-bedrock", ModelMatch: "", Shape: shapeAnthropic},
	{ProviderMatch: "anthropic-vertex", ModelMatch: "", Shape: shapeAnthropic},

	// Kilo Gateway — Anthropic-backed for the kilo-auto family;
	// the model string carries the family marker.
	{ProviderMatch: "kilo", ModelMatch: "kilo-auto", Shape: shapeAnthropic},
	{ProviderMatch: "kilo-code", ModelMatch: "kilo-auto", Shape: shapeAnthropic},
	// Kilo Gateway stealth/* anonymized routes are the SAME
	// Anthropic-backed gateway as kilo-auto; the underlying model
	// name follows the stealth/ prefix. stealth/claude-* (observed
	// live 2026-06-26: stealth/claude-sonnet-4.6, NET input +
	// cache.read = Anthropic-shape per docs/kilo-code-adapter.md) is
	// Anthropic. This makes the classification EXPLICIT — it already
	// resolved Anthropic via the conservative shapeUnknown default,
	// but an explicit pinned row removes the documented residual.
	// Non-claude stealth models (none observed live) keep falling
	// through to the conservative Anthropic default.
	{ProviderMatch: "kilo", ModelMatch: "stealth/claude", Shape: shapeAnthropic},
	{ProviderMatch: "kilo-code", ModelMatch: "stealth/claude", Shape: shapeAnthropic},

	// OpenAI / OpenAI-compatible — implicit cache.
	{ProviderMatch: "openai", ModelMatch: "", Shape: shapeImplicit},
	{ProviderMatch: "azure-openai", ModelMatch: "", Shape: shapeImplicit},
	{ProviderMatch: "azure", ModelMatch: "gpt-", Shape: shapeImplicit},
	{ProviderMatch: "deepseek", ModelMatch: "", Shape: shapeImplicit},
	{ProviderMatch: "groq", ModelMatch: "", Shape: shapeImplicit},
	{ProviderMatch: "fireworks", ModelMatch: "", Shape: shapeImplicit},
	{ProviderMatch: "together", ModelMatch: "", Shape: shapeImplicit},
	{ProviderMatch: "mistral", ModelMatch: "", Shape: shapeImplicit},
	{ProviderMatch: "cohere", ModelMatch: "", Shape: shapeImplicit},

	// OpenRouter — default Implicit. A specific anthropic/-prefixed
	// model would be Implicit too via OpenRouter (the pass-through
	// strips cache_control markers per the §15.3 plan's deferred
	// reality-check). Promote a row above this fallback if/when a
	// live capture proves a specific OpenRouter route preserves
	// markers.
	{ProviderMatch: "openrouter", ModelMatch: "", Shape: shapeImplicit},
}

// resolveProviderShape walks [implicitProviderTable] top-down and
// returns the matched shape. Returns shapeUnknown when no row
// matches — adapters MUST default to shapeAnthropic in that case
// (the conservative bias: a wrong "anthropic" classification
// surfaces as a §10 mispredict the operator can investigate,
// whereas a wrong "implicit" classification silently un-grades
// the turn from the soak-validated gate).
func resolveProviderShape(providerID, modelID string) providerRoutingShape {
	provider := strings.ToLower(providerID)
	model := strings.ToLower(modelID)
	for _, r := range implicitProviderTable {
		if !strings.EqualFold(r.ProviderMatch, provider) {
			continue
		}
		if r.ModelMatch == "" {
			return r.Shape
		}
		if strings.Contains(model, strings.ToLower(r.ModelMatch)) {
			return r.Shape
		}
	}
	return shapeUnknown
}

// IsImplicitCacheProvider reports whether a (providerID, modelID)
// pair routes to an implicit-cache provider. Adapters call this at
// the emit boundary to overlay [models.CacheTurnObservation.
// ImplicitCache] before appending; the engine boundary then
// overlays Caps.ImplicitCache=true in
// store.observationToObserveInput.
//
// Defaults to FALSE (Anthropic-shape) when no rule matches — the
// conservative bias. A new implicit-cache provider lands as a row
// in [implicitProviderTable]; adapters consume only this bool.
//
// Empty inputs return false (no signal to route on). Callers that
// can't observe providerID at all (early Tier-2 transcripts before
// schema evolution exposed it) MUST treat the turn as Anthropic
// shape — better to grade against an existing gate than silently
// exclude.
func IsImplicitCacheProvider(providerID, modelID string) bool {
	if providerID == "" {
		return false
	}
	return resolveProviderShape(providerID, modelID) == shapeImplicit
}
