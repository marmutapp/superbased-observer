package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRecipes_InternalAndDocsAreIdentical pins the v1.7.6 contract
// that the embedded recipes (internal/config/recipes/) and the
// public-doc mirror (docs/recipes/) are byte-identical. Forces the
// 3-site carve-out sync in scripts/release.sh to keep them in lockstep.
// If this fails: rerun the sync command in
// docs/v1.7.6-codex-friendly-compression-plan-2026-05-30.md.
func TestRecipes_InternalAndDocsAreIdentical(t *testing.T) {
	t.Parallel()
	// Repo root from this test file: internal/config → up two.
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("repoRoot: %v", err)
	}
	for _, name := range RecipeNames() {
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			embedded, err := recipesFS.ReadFile("recipes/" + name + ".toml")
			if err != nil {
				t.Fatalf("read embedded %s: %v", name, err)
			}
			mirror, err := os.ReadFile(filepath.Join(repoRoot, "docs", "recipes", name+".toml"))
			if err != nil {
				t.Fatalf("read docs mirror %s: %v", name, err)
			}
			if string(embedded) != string(mirror) {
				t.Fatalf("recipe %s drifted: internal/config/recipes/%s.toml != docs/recipes/%s.toml\n"+
					"--- embedded (%d bytes) ---\n%s\n--- docs (%d bytes) ---\n%s",
					name, name, name, len(embedded), embedded, len(mirror), mirror)
			}
		})
	}
}

// TestRecipeNames_PinsTheThree pins the v1.7.6 recipe set:
// claude-code (Anthropic default formalised), codex-safe (gpt-mini
// family), codex-variant (`*-codex` family). Adding a recipe requires
// extending this test + the carve-out sync.
func TestRecipeNames_PinsTheThree(t *testing.T) {
	t.Parallel()
	got := RecipeNames()
	want := []string{"claude-code", "codex-safe", "codex-variant"}
	if len(got) != len(want) {
		t.Fatalf("RecipeNames: got %v want %v", got, want)
	}
	for i, name := range want {
		if got[i] != name {
			t.Errorf("RecipeNames[%d]: got %q want %q", i, got[i], name)
		}
	}
}

// TestLoadRecipe_UnknownReturnsError pins the error message format so
// the `--recipe` flag's error is operator-friendly.
func TestLoadRecipe_UnknownReturnsError(t *testing.T) {
	t.Parallel()
	_, err := LoadRecipe("definitely-not-a-recipe")
	if err == nil {
		t.Fatal("expected error for unknown recipe")
	}
	if !strings.Contains(err.Error(), "unknown recipe") {
		t.Errorf("expected 'unknown recipe' in error: %v", err)
	}
	if !strings.Contains(err.Error(), "claude-code") {
		t.Errorf("expected available recipes listed in error: %v", err)
	}
}

// TestLoadRecipe_EmptyReturnsDefault pins that RecipeName="" is a
// pass-through (used by callers who only conditionally apply a recipe).
func TestLoadRecipe_EmptyReturnsDefault(t *testing.T) {
	t.Parallel()
	cfg, err := LoadRecipe("")
	if err != nil {
		t.Fatalf("LoadRecipe(\"\"): %v", err)
	}
	if cfg.Compression.Conversation.Mode != Default().Compression.Conversation.Mode {
		t.Errorf("LoadRecipe(\"\") didn't return defaults")
	}
}

// TestLoadRecipe_ClaudeCode pins the claude-code recipe's headline
// values: enabled=true, mode=cache_aware,
// compress_types=["json","logs","code","tools"], stash disabled.
//
// V7-24 (v1.7.23, 2026-06-01) restored compress_types from V7-22's
// temporary [] back to ["json","logs","code"]. The V7-22 binary's
// pipeline (with V7-19 nil-trap + V7-21 tools-defs gate applied)
// no longer triggers the per-type re-marshal cascade — re-measured
// at n=8 on V7-22 binary, this recipe produces -6.9% total cost vs
// no-proxy (n=4 OFF baseline), zero tail outliers, CV 7.6%.
//
// A2 (2026-06-11) adopted "tools" as the fourth default: n=8/arm
// salted refactors on Opus 4.8 measured -12.5% on valid samples with
// ZERO tools_changed cache events (the trim is byte-stable; the
// V7-21 re-marshal tax is gone). Record:
// docs/plans/profile-content-refresh-ab-plan-2026-06-10.md §6.
//
// Operators MUST also set ENABLE_TOOL_SEARCH=true in the launching
// shell when using this recipe — without it Claude Code's SDK
// eager-inlines MCP schemas under ANTHROPIC_BASE_URL (~+21K tokens/turn).
//
// Stash is disabled by default: V7-24 n=1 measurement showed stash's
// content-replacement breaks Anthropic's prefix cache (+25% cost,
// cache_creation doubled vs Plan A). Operators can opt in but
// shouldn't expect savings on Anthropic traffic.
//
// See docs/v1.7.23-compression-savings-empirical-2026-06-01.md.
func TestLoadRecipe_ClaudeCode(t *testing.T) {
	t.Parallel()
	cfg, err := LoadRecipe("claude-code")
	if err != nil {
		t.Fatalf("LoadRecipe: %v", err)
	}
	conv := cfg.Compression.Conversation
	if !conv.Enabled {
		t.Errorf("claude-code: enabled must be true")
	}
	if conv.Mode != "cache_aware" {
		t.Errorf("claude-code: mode = %q, want cache_aware", conv.Mode)
	}
	wantTypes := []string{"json", "logs", "code", "tools"}
	if len(conv.CompressTypes) != len(wantTypes) {
		t.Errorf("claude-code: compress_types = %v, want %v (V7-24)", conv.CompressTypes, wantTypes)
	} else {
		for i, want := range wantTypes {
			if conv.CompressTypes[i] != want {
				t.Errorf("claude-code: compress_types[%d] = %q, want %q", i, conv.CompressTypes[i], want)
			}
		}
	}
	if conv.Stash.Enabled {
		t.Errorf("claude-code: stash must be disabled by default (V7-24: stash breaks Anthropic prefix cache)")
	}
}

// TestLoadRecipe_CodexSafe pins the codex-safe recipe's load-bearing
// values: compress_types=["logs","tools"] (tools A/B-gated in by run
// A6, 2026-06-11 — profile-content-refresh-ab-plan §8), token mode,
// preserve_last_n=15, max_excerpt_bytes=16384.
func TestLoadRecipe_CodexSafe(t *testing.T) {
	t.Parallel()
	cfg, err := LoadRecipe("codex-safe")
	if err != nil {
		t.Fatalf("LoadRecipe: %v", err)
	}
	conv := cfg.Compression.Conversation
	if !conv.Enabled {
		t.Errorf("codex-safe: enabled must be true")
	}
	if conv.Mode != "token" {
		t.Errorf("codex-safe: mode = %q, want token", conv.Mode)
	}
	if !equalSlice(conv.CompressTypes, []string{"logs", "tools"}) {
		t.Errorf("codex-safe: compress_types = %v, want [logs tools]", conv.CompressTypes)
	}
	if conv.PreserveLastN != 15 {
		t.Errorf("codex-safe: preserve_last_n = %d, want 15", conv.PreserveLastN)
	}
	if cfg.Compression.Indexing.MaxExcerptBytes != 16384 {
		t.Errorf("codex-safe: indexing.max_excerpt_bytes = %d, want 16384", cfg.Compression.Indexing.MaxExcerptBytes)
	}
}

// TestLoadRecipe_CodexVariant pins the codex-variant recipe's
// load-bearing values: compress_types=[] (the V7-11 mitigation),
// MaxLines=0 (LogsCompressor truncation disabled).
func TestLoadRecipe_CodexVariant(t *testing.T) {
	t.Parallel()
	cfg, err := LoadRecipe("codex-variant")
	if err != nil {
		t.Fatalf("LoadRecipe: %v", err)
	}
	conv := cfg.Compression.Conversation
	if len(conv.CompressTypes) != 0 {
		t.Errorf("codex-variant: compress_types = %v, want []", conv.CompressTypes)
	}
	if conv.Logs.MaxLines != 0 {
		t.Errorf("codex-variant: logs.max_lines = %d, want 0 (disable truncation)", conv.Logs.MaxLines)
	}
	if conv.PreserveLastN != 50 {
		t.Errorf("codex-variant: preserve_last_n = %d, want 50", conv.PreserveLastN)
	}
}

// NOTE (Track R, P2.2): the TestLoadAppliesRecipeAndValidates +
// TestLoadRecipeOverridenByUserFile pins that lived here covered the
// removed Load() recipe overlay. Recipes are now profiles resolved at
// the proxy boundary — see profiles_test.go for the resolution pins
// and cmd/observer for the deprecated `--recipe` alias pin.
