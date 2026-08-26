package cacheobs

import (
	"github.com/marmutapp/superbased-observer/internal/cachetrack"
	"github.com/marmutapp/superbased-observer/internal/models"
)

// SourceEventID returns the deterministic per-turn idempotency-key
// suffix every Tier-2 producer uses: "cachetrack:" + the adapter's
// own message id. Paired with SourceFile this is the (SourceFile,
// SourceEventID) dedup key store.CacheEventExistsForMessage checks,
// so a re-parse of the same transcript must reproduce this exact
// string for the same message.
func SourceEventID(messageID string) string {
	return "cachetrack:" + messageID
}

// ApplyImplicitCacheOverlay applies the §15.3 boundary overlay: when
// (provider, model) resolve to an implicit-cache provider shape
// (OpenAI / OpenAI-compatible gateways — see
// cachetrack.IsImplicitCacheProvider), it sets obs.ImplicitCache and
// clears obs.BlockHashes, since the implicit-cache attribution path
// never consumes the block chain. Anthropic-shape (provider, model)
// pairs are returned unchanged.
//
// Every existing Tier-2 producer applied this identical three-line
// overlay after Emit; this is that logic, factored once.
func ApplyImplicitCacheOverlay(obs models.CacheTurnObservation, provider, model string) models.CacheTurnObservation {
	if cachetrack.IsImplicitCacheProvider(provider, model) {
		obs.ImplicitCache = true
		obs.BlockHashes = nil
	}
	return obs
}

// IsZeroUsage reports whether a CacheUsage bundle carries no signal
// at all (every counter zero) — the "in-progress turn" carve-out
// every Tier-2 producer applies before emitting: a streaming row
// whose usage envelope hasn't landed yet must not emit an
// observation. Callers with an adapter-specific extra counter (e.g.
// a reasoning-token field that lives outside CacheUsage) should OR
// it into their own gate rather than pass it here.
func IsZeroUsage(u models.CacheUsage) bool {
	return u.NetInputTokens == 0 && u.OutputTokens == 0 &&
		u.CacheReadTokens == 0 && u.CacheCreationTokens == 0 &&
		u.CacheCreation1hTokens == 0
}
