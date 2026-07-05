// Package cachewarm is the pure-logic core of the cache-expiry warning
// and smart keep-warm feature
// (docs/plans/cache-expiry-warning-and-keepwarm-plan-2026-06-25.md).
//
// It answers two questions over the cache state the cachetrack engine
// already models (the cache_entries table — expires_at, ttl_tier,
// token_count, state):
//
//   - Classify: "which live caches are about to expire, how soon, and
//     how much money is at risk if they go cold?" — the warning system
//     (Part A).
//   - Recommend: "is keeping this cache warm worth it, and if so what's
//     the cheapest lever?" — the keep-warm economics (Part B), reusing
//     the cost shape the cachetrack forecaster already computes.
//
// Module-boundary discipline (CLAUDE.md §1): this package is pure logic.
// It imports NO database/sql, net/http, or fsnotify — the cache rows are
// read at the store seam (internal/store/cachetrack.go::LoadLiveCacheWindows)
// and the per-token rates are resolved from cost.Table at the boundary,
// then handed in as plain values. Capability differences between
// providers (Anthropic authoritative expiry vs OpenAI estimated; the
// Anthropic 1h tier; Gemini explicit-cache TTL patch) are resolved into
// CacheWindow capability flags AT THE BOUNDARY, so the classifier branches
// on capabilities, never on tool/provider identity (discipline rule 3).
// imports_test.go pins this.
package cachewarm
