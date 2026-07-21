package cachetrack

import (
	"testing"
	"time"
)

// TestTier_String pins the strings persisted to *.tier columns.
// Schema-stable; changing them is a schema break.
func TestTier_String(t *testing.T) {
	t.Parallel()
	tests := []struct {
		tier Tier
		want string
	}{
		{TierUnknown, "unknown"},
		{TierProxy, "proxy"},
		{TierTranscript, "transcript"},
		{TierCounts, "counts"},
		{Tier(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.tier.String(); got != tt.want {
			t.Errorf("Tier(%d).String() = %q, want %q", tt.tier, got, tt.want)
		}
	}
}

// TestCapabilitiesFor verifies the SINGLE tier → capability
// boundary (spec §24.3). Adding a new tier means adding a row
// here AND a case in CapabilitiesFor — nowhere else.
func TestCapabilitiesFor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		tier Tier
		want Capabilities
	}{
		{
			tier: TierProxy,
			want: Capabilities{
				ToolsVisible:        true,
				SystemVisible:       true,
				MarkersVisible:      true,
				UsageObserved:       true,
				BlocksAreCumulative: true,
				ImplicitCache:       false,
			},
		},
		{
			tier: TierTranscript,
			want: Capabilities{
				ToolsVisible:        false,
				SystemVisible:       false,
				MarkersVisible:      false,
				UsageObserved:       true,
				BlocksAreCumulative: false,
				ImplicitCache:       false,
			},
		},
		{
			tier: TierCounts,
			want: Capabilities{
				ToolsVisible:        false,
				SystemVisible:       false,
				MarkersVisible:      false,
				UsageObserved:       true,
				BlocksAreCumulative: false,
				ImplicitCache:       false,
			},
		},
		{
			tier: TierUnknown,
			want: Capabilities{},
		},
	}
	for _, tt := range tests {
		got := CapabilitiesFor(tt.tier)
		if got != tt.want {
			t.Errorf("CapabilitiesFor(%v) = %+v, want %+v", tt.tier, got, tt.want)
		}
	}
}

// TestCapabilitiesFor_BlocksAreCumulative_Pinning is the operator-
// requested §15.3 (c) phase-2 invariant. The BlocksAreCumulative
// flag is the load-bearing capability for predictKind's
// suffix-token slicing: a wrong value silently mislabels a Tier-1
// caller as delta (predictKind sees the cumulative conversation
// summed as the "new tail" → CacheableTokens always true →
// always predict Write → no-op fix). This test pins the per-tier
// values so a future tier addition can't accidentally inherit a
// zero-value default in this critical column.
//
// Pair invariant — Capabilities is only ever constructed via
// CapabilitiesFor outside this package: verified manually via
// `grep -rn 'cachetrack.Capabilities{' internal/`; the only
// constructions are inside CapabilitiesFor itself. The engine's
// boundary backfill at ObserveTurn (`if in.Tier != TierUnknown &&
// in.Caps == (Capabilities{})`) ensures a Tier-1 caller that
// passes only `Tier=TierProxy` gets the full Capabilities derived
// at the seam.
func TestCapabilitiesFor_BlocksAreCumulative_Pinning(t *testing.T) {
	t.Parallel()
	pinned := []struct {
		tier Tier
		want bool
	}{
		{TierProxy, true},       // Anthropic API request body is cumulative
		{TierTranscript, false}, // adapter accumulator emits per-turn delta
		{TierCounts, false},     // counts tier is delta-emit
		{TierUnknown, false},    // zero-value default — safe (treats as delta)
	}
	for _, p := range pinned {
		got := CapabilitiesFor(p.tier).BlocksAreCumulative
		if got != p.want {
			t.Errorf("CapabilitiesFor(%v).BlocksAreCumulative = %v, want %v — §15.3 (c) predictKind slice anchor depends on this; a wrong value re-opens the rate-blind growth-turn regression",
				p.tier, got, p.want)
		}
	}
}

// TestCapabilitiesFor_ImplicitCache_Pinning guards the §15.3
// invariant: ImplicitCache is FALSE by tier default for every tier.
// Provider identity (OpenAI vs Anthropic) is not carried by Tier
// alone, so the proxy/adapter boundary MUST overlay ImplicitCache=
// true when observed traffic is implicit-cache shape. A future tier
// addition that silently inherits ImplicitCache=true via a copy-
// paste would route Anthropic traffic into the reduced attribution
// path, breaking §10. This test surfaces that drift.
func TestCapabilitiesFor_ImplicitCache_Pinning(t *testing.T) {
	t.Parallel()
	pinned := []struct {
		tier Tier
		want bool
	}{
		{TierProxy, false},      // Default Anthropic at the proxy seam; OpenAI overlays at boundary.
		{TierTranscript, false}, // Default Anthropic transcripts; implicit-routed adapters overlay at boundary.
		{TierCounts, false},     // Counts tier is implicit-routed-by-default expectation, BUT the
		// resolved capability is still overlaid by the adapter
		// boundary so a future Tier-1-counts adapter (codex live
		// emitter) can opt in or out without re-wiring CapabilitiesFor.
		{TierUnknown, false}, // zero-value default — safe (no implicit-cache assumptions).
	}
	for _, p := range pinned {
		got := CapabilitiesFor(p.tier).ImplicitCache
		if got != p.want {
			t.Errorf("CapabilitiesFor(%v).ImplicitCache = %v, want %v — §15.3 boundary-overlay invariant: never set true by tier default; provider/adapter MUST overlay at the seam",
				p.tier, got, p.want)
		}
	}
}

// TestAssumedBreakpoints pins the R1(a)-resolved 2-entry model:
// one system-boundary breakpoint + one rolling last-message-block
// breakpoint, BOTH at 1h tier per the live opus-4-8 capture
// (2026-06-08). The system entry models the system[1]+system[2]
// pair collapsed (Tier-2 can't tell them apart). C8 Tier-1
// detection enumerates real markers and bypasses this function.
//
// A future SDK behavior change that drops or adds breakpoints
// fails this test first — the single adjustment site is
// assumedBreakpoints() itself.
func TestAssumedBreakpoints(t *testing.T) {
	t.Parallel()
	got := assumedBreakpoints()
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (system + rolling-message per R1(a))", len(got))
	}
	want := []AssumedBreakpoint{
		{Level: LevelSystem, Rolling: false, TTL: TTL1h},
		{Level: LevelMessage, Rolling: true, TTL: TTL1h},
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("breakpoint %d = %+v, want %+v", i, got[i], w)
		}
	}
}

// TestWithinLookback covers the §7 row 8 / §6 lookback predicate.
func TestWithinLookback(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                  string
		blocksSinceBreakpoint int
		want                  bool
	}{
		{"at breakpoint", 0, true},
		{"one back", 1, true},
		{"at window edge", LookbackWindow, true},
		{"one past window", LookbackWindow + 1, false},
		{"far past", 200, false},
		{"negative is out of range (defensive)", -1, false},
	}
	for _, tt := range tests {
		if got := WithinLookback(tt.blocksSinceBreakpoint); got != tt.want {
			t.Errorf("%s: WithinLookback(%d) = %v, want %v", tt.name, tt.blocksSinceBreakpoint, got, tt.want)
		}
	}
}

// TestTTLDuration covers the BlockTTL → time.Duration mapping.
// Unset / unknown defaults to 5m per §6.
func TestTTLDuration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		ttl  BlockTTL
		want time.Duration
	}{
		{TTL5m, TTL5mDuration},
		{TTL1h, TTL1hDuration},
		{TTLUnset, TTL5mDuration},
		{BlockTTL(99), TTL5mDuration},
	}
	for _, tt := range tests {
		if got := TTLDuration(tt.ttl); got != tt.want {
			t.Errorf("TTLDuration(%v) = %v, want %v", tt.ttl, got, tt.want)
		}
	}
}

// TestMinCacheableTokens pins the model-family → min-cacheable
// table per research doc §2.1. One row per family; any future
// provider update is a one-line table addition (§24.5
// discipline).
func TestMinCacheableTokens(t *testing.T) {
	t.Parallel()
	tests := []struct {
		model string
		want  int
	}{
		// Default 1024 — Opus 4.8, Sonnet 4.5/4.6, Opus 4/4.1.
		{"claude-opus-4-8", defaultMinCacheable},
		{"claude-sonnet-4-6", defaultMinCacheable},
		{"claude-opus-4", defaultMinCacheable},
		{"claude-opus-4-1", defaultMinCacheable},
		// 4096 — Opus 4.5–4.7 family.
		{"claude-opus-4-5", 4096},
		{"claude-opus-4-6-20250101", 4096},
		{"claude-opus-4-7", 4096},
		// 4096 — Haiku 4.5.
		{"claude-haiku-4-5", 4096},
		// 2048 — older Haiku 3.5.
		{"claude-haiku-3-5", 2048},
		// Unknown model → default.
		{"gpt-5", defaultMinCacheable},
		{"", defaultMinCacheable},
	}
	for _, tt := range tests {
		if got := MinCacheableTokens(tt.model); got != tt.want {
			t.Errorf("MinCacheableTokens(%q) = %d, want %d", tt.model, got, tt.want)
		}
	}
}

// TestMaxBreakpoints documents the constant — a regression here
// would mean a provider behavior change. Pinned at 4 per
// Anthropic docs.
func TestMaxBreakpoints(t *testing.T) {
	t.Parallel()
	if MaxBreakpoints != 4 {
		t.Errorf("MaxBreakpoints = %d, want 4 (Anthropic provider cap)", MaxBreakpoints)
	}
}
