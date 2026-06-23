package routing

import "testing"

// TestTierTable_NormalizationOrder pins the lookup ladder reused from the
// cost engine: exact → :free guard → date-strip → family prefix →
// last-resort provider-prefix strip → miss. One row per ladder step.
func TestTierTable_NormalizationOrder(t *testing.T) {
	t.Parallel()
	r := NewTierResolver()
	cases := []struct {
		name       string
		model      string
		wantTier   Tier
		wantSource TierSource
	}{
		{"exact_match", "claude-opus-4-8", TierOpusClass, TierSourceExact},
		// :free guard fires BEFORE the family ladder: the claude-sonnet
		// family says sonnet-class, but the free SKU is free tier.
		{"free_guard_beats_family", "claude-sonnet-4-6:free", TierFree, TierSourceExact},
		{"free_guard_case_insensitive", "nemotron-3-super-120b-a12b:FREE", TierFree, TierSourceExact},
		// Date-strip lands on the exact "claude-opus-4-1" entry before the
		// shorter "claude-opus-4" family prefix could claim it.
		{"date_strip", "claude-opus-4-1-20250805", TierOpusClass, TierSourceDateStripped},
		{"family_prefix", "claude-sonnet-4-9", TierSonnetClass, TierSourceFamily},
		{"family_longest_wins", "gemini-3-flash-experimental", TierHaikuClass, TierSourceFamily},
		{"provider_prefix_strip", "anthropic/claude-sonnet-4-6", TierSonnetClass, TierSourceFamily},
		{"openrouter_path_strip", "openrouter/openai/gpt-oss-120b", TierHaikuClass, TierSourceFamily},
		{"capi_router_strip", "capi:claude-haiku-4-5", TierHaikuClass, TierSourceFamily},
		{"miss_unclassified", "totally-novel-model-x", TierUnclassified, TierSourceMiss},
		{"empty_model", "", TierUnclassified, TierSourceMiss},
		// Auto-router sentinels stay unplaced: we audit routers (§R17.4),
		// we don't pretend to know what they serve.
		{"auto_sentinel_unplaced", "auto", TierUnclassified, TierSourceMiss},
		{"cursor_default_unplaced", "default", TierUnclassified, TierSourceMiss},
		{"kilo_auto_family_unplaced", "kilo-auto/large", TierUnclassified, TierSourceMiss},
		{"kilo_auto_free_exact", "kilo-auto/free", TierFree, TierSourceExact},
		{"local_family", "ollama/gemma4:e4b", TierLocal, TierSourceFamily},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tier, src := r.Lookup(tc.model)
			if tier != tc.wantTier || src != tc.wantSource {
				t.Errorf("Lookup(%q) = (%s, %s), want (%s, %s)",
					tc.model, tier, src, tc.wantTier, tc.wantSource)
			}
		})
	}
}

// TestTierTable_OverridesWinOverSeed pins the override semantics: a user
// entry replaces the seed placement wholesale.
func TestTierTable_OverridesWinOverSeed(t *testing.T) {
	t.Parallel()
	r := NewTierResolver()
	r.Reload(map[string]Tier{
		"claude-opus-4-8": TierSonnetClass, // demote
		"my-finetune-v2":  TierSonnetClass, // novel model placed
	})
	if tier, src := r.Lookup("claude-opus-4-8"); tier != TierSonnetClass || src != TierSourceExact {
		t.Errorf("override lookup = (%s, %s), want (sonnet-class, exact)", tier, src)
	}
	if tier, _ := r.Lookup("my-finetune-v2"); tier != TierSonnetClass {
		t.Errorf("novel override = %s, want sonnet-class", tier)
	}
}

// TestTierResolver_HotReloadSnapshot pins the atomic.Pointer contract: a
// table snapshot taken before Reload keeps answering with the old data
// while fresh lookups see the new table — never a torn state.
func TestTierResolver_HotReloadSnapshot(t *testing.T) {
	t.Parallel()
	r := NewTierResolver()
	old := r.Table()
	r.Reload(map[string]Tier{"claude-opus-4-8": TierHaikuClass})
	if tier, _ := old.Lookup("claude-opus-4-8"); tier != TierOpusClass {
		t.Errorf("old snapshot mutated: got %s, want opus-class", tier)
	}
	if tier, _ := r.Lookup("claude-opus-4-8"); tier != TierHaikuClass {
		t.Errorf("fresh lookup = %s, want haiku-class", tier)
	}
}

// TestTierTable_SeedSelfConsistent sweeps the shipped seed: every
// placement is a known tier, and no seed key accidentally resolves to a
// different tier than its own entry (a longer family key shadowing an
// exact entry would be a seed-authoring bug).
func TestTierTable_SeedSelfConsistent(t *testing.T) {
	t.Parallel()
	r := NewTierResolver()
	for model, want := range seedTiers {
		if !want.Known() {
			t.Errorf("seed %q places into unknown tier %q", model, want)
		}
		if got, src := r.Lookup(model); got != want || src != TierSourceExact {
			t.Errorf("seed %q resolves (%s, %s), want (%s, exact)", model, got, src, want)
		}
	}
}

// TestTierTable_RepresentativesSelfConsistent sweeps the curated
// downshift targets: each representative resolves to its own cell's tier,
// carries the cell's shape, and is itself placeable (never unclassified).
func TestTierTable_RepresentativesSelfConsistent(t *testing.T) {
	t.Parallel()
	r := NewTierResolver()
	tbl := r.Table()
	for shape, byTier := range seedRepresentatives {
		for tier, model := range byTier {
			gotTier, _ := tbl.Lookup(model)
			if gotTier != tier {
				t.Errorf("representative %q for (%s,%s) resolves to tier %s", model, shape, tier, gotTier)
			}
			if gotShape := ShapeForModel(model); gotShape != shape {
				t.Errorf("representative %q for (%s,%s) resolves to shape %q", model, shape, tier, gotShape)
			}
		}
	}
}

// TestTierTable_Representative covers the lookup both ways.
func TestTierTable_Representative(t *testing.T) {
	t.Parallel()
	tbl := NewTierResolver().Table()
	cases := []struct {
		name      string
		shape     ProviderShape
		tier      Tier
		wantModel string
		wantOK    bool
	}{
		{"anthropic_haiku", ShapeAnthropic, TierHaikuClass, "claude-haiku-4-5", true},
		{"openai_sonnet", ShapeOpenAI, TierSonnetClass, "gpt-5.4", true},
		{"google_opus", ShapeGoogle, TierOpusClass, "gemini-3.1-pro-preview", true},
		{"no_unclassified_target", ShapeAnthropic, TierUnclassified, "", false},
		{"no_local_target", ShapeAnthropic, TierLocal, "", false},
		{"unknown_shape", ShapeUnknown, TierHaikuClass, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, ok := tbl.Representative(tc.shape, tc.tier)
			if m != tc.wantModel || ok != tc.wantOK {
				t.Errorf("Representative(%s,%s) = (%q,%v), want (%q,%v)",
					tc.shape, tc.tier, m, ok, tc.wantModel, tc.wantOK)
			}
		})
	}
}

// TestShapeForModel covers the prefix resolution table — one row per
// table entry plus the conservative-unknown cases.
func TestShapeForModel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		model string
		want  ProviderShape
	}{
		{"claude-sonnet-4-6", ShapeAnthropic},
		{"gpt-5.4-mini", ShapeOpenAI},
		{"o1-pro", ShapeOpenAI},
		{"o3-mini", ShapeOpenAI},
		{"o4-mini", ShapeOpenAI},
		{"davinci-002", ShapeOpenAI},
		{"babbage-002", ShapeOpenAI},
		{"gemini-3.5-flash", ShapeGoogle},
		{"anthropic/claude-haiku-4-5", ShapeAnthropic},
		{"openrouter/openai/gpt-5.4", ShapeOpenAI},
		{"CLAUDE-OPUS-4-8", ShapeAnthropic}, // case-insensitive
		{"grok-4.3", ShapeUnknown},          // OpenAI-compatible served, but we don't claim it
		// §R11.7 (P1): local-host prefixes resolve to the OpenAI shape
		// — every local inference server we route to speaks it.
		{"ollama/gemma4:e4b", ShapeOpenAI},
		{"", ShapeUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			t.Parallel()
			if got := ShapeForModel(tc.model); got != tc.want {
				t.Errorf("ShapeForModel(%q) = %q, want %q", tc.model, got, tc.want)
			}
		})
	}
}

// TestTierTable_Known smoke-checks the debug surface: sorted, non-empty,
// includes a seed key.
func TestTierTable_Known(t *testing.T) {
	t.Parallel()
	keys := NewTierResolver().Table().Known()
	if len(keys) == 0 {
		t.Fatal("Known() empty")
	}
	found := false
	for i, k := range keys {
		if k == "claude-opus-4-8" {
			found = true
		}
		if i > 0 && keys[i-1] > k {
			t.Fatalf("Known() not sorted at %d: %q > %q", i, keys[i-1], k)
		}
	}
	if !found {
		t.Error("Known() missing seed key claude-opus-4-8")
	}
}

// TestTierTable_NilSafety pins nil-receiver behavior — a nil table or
// resolver degrades to unclassified/empty, never panics (G7: routing must
// not add failure modes).
func TestTierTable_NilSafety(t *testing.T) {
	t.Parallel()
	var tbl *TierTable
	if tier, src := tbl.Lookup("claude-opus-4-8"); tier != TierUnclassified || src != TierSourceMiss {
		t.Errorf("nil table Lookup = (%s,%s), want (unclassified,miss)", tier, src)
	}
	if _, ok := tbl.Representative(ShapeAnthropic, TierHaikuClass); ok {
		t.Error("nil table Representative ok=true")
	}
	if got := tbl.Known(); got != nil {
		t.Errorf("nil table Known = %v, want nil", got)
	}
	var r *TierResolver
	if r.Table() != nil {
		t.Error("nil resolver Table != nil")
	}
}
