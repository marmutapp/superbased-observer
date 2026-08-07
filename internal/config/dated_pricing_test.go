package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDatedPricingTomlRoundTrip pins the [intelligence.pricing.dated]
// block through the exact write→read cycle the dashboard's Settings
// page performs (config.WriteToml on the whole struct, then
// config.Load). The Settings PUT only replaces
// Intelligence.Pricing.Models and re-marshals everything else, so a
// dated timeline an operator hand-authored must survive an unrelated
// pricing save — otherwise saving a rate from the UI would silently
// delete the history that keeps old costs correct.
func TestDatedPricingTomlRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")

	cfg := Default()
	cfg.Intelligence.Pricing.Models = map[string]ModelPricing{
		"synth-dated-model": {Input: 4, Output: 16, CacheRead: 0.4},
	}
	cfg.Intelligence.Pricing.Dated = map[string][]DatedModelPricing{
		"synth-dated-model": {
			{EffectiveFrom: "", ModelPricing: ModelPricing{Input: 10, Output: 40, CacheRead: 1}},
			{EffectiveFrom: "2026-08-01", ModelPricing: ModelPricing{
				Input: 4, Output: 16, CacheRead: 0.4,
				LongContextThreshold: 272_000, LongContextInput: 8,
				FastMultiplier: 2,
			}},
		},
	}

	if err := WriteToml(path, cfg); err != nil {
		t.Fatalf("WriteToml: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Array-of-tables under a quoted map key is the documented shape.
	if !strings.Contains(string(raw), `[[intelligence.pricing.dated`) {
		t.Fatalf("dated block not emitted as an array of tables:\n%s", raw)
	}

	got, err := Load(LoadOptions{GlobalPath: path})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tl := got.Intelligence.Pricing.Dated["synth-dated-model"]
	if len(tl) != 2 {
		t.Fatalf("timeline round-tripped to %d entries, want 2: %+v", len(tl), tl)
	}
	if tl[0].EffectiveFrom != "" || tl[0].Input != 10 || tl[0].Output != 40 || tl[0].CacheRead != 1 {
		t.Fatalf("entry 0 = %+v", tl[0])
	}
	if tl[1].EffectiveFrom != "2026-08-01" || tl[1].Input != 4 ||
		tl[1].LongContextThreshold != 272_000 || tl[1].LongContextInput != 8 || tl[1].FastMultiplier != 2 {
		t.Fatalf("entry 1 = %+v — every ModelPricing field must survive, not just input/output", tl[1])
	}
	// The flat overrides are untouched by the dated block.
	if mp := got.Intelligence.Pricing.Models["synth-dated-model"]; mp.Input != 4 || mp.Output != 16 {
		t.Fatalf("flat override = %+v", mp)
	}
}

// TestDatedPricingAbsentByDefault pins the zero-config contract: a
// default config carries no dated timelines, so the cost engine stays on
// its pre-dated code path.
func TestDatedPricingAbsentByDefault(t *testing.T) {
	if d := Default().Intelligence.Pricing.Dated; len(d) != 0 {
		t.Fatalf("Default() ships dated pricing: %+v", d)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := WriteToml(path, Default()); err != nil {
		t.Fatal(err)
	}
	got, err := Load(LoadOptions{GlobalPath: path})
	if err != nil {
		t.Fatal(err)
	}
	if d := got.Intelligence.Pricing.Dated; len(d) != 0 {
		t.Fatalf("round-tripped default config grew dated pricing: %+v", d)
	}
}
