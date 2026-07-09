package routing

import "testing"

// TestShapeForModel_LocalPrefixes pins §R11.7: local-host id prefixes
// resolve to the OpenAI shape (every local inference server we route
// to speaks it), BEFORE the path-segment strip would discard them.
func TestShapeForModel_LocalPrefixes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		model string
		want  ProviderShape
	}{
		{"ollama/llama4:70b", ShapeOpenAI},
		{"lmstudio/qwen3-coder", ShapeOpenAI},
		{"vllm/deepseek-v4", ShapeOpenAI},
		{"local/my-finetune", ShapeOpenAI},
		{"anthropic/claude-sonnet-4-6", ShapeAnthropic}, // path strip still works
		{"mystery-model", ShapeUnknown},
	}
	for _, tc := range cases {
		if got := ShapeForModel(tc.model); got != tc.want {
			t.Errorf("ShapeForModel(%q) = %q, want %q", tc.model, got, tc.want)
		}
	}
}

// TestReloadWithLocalUpstreams pins the §R11.7 resolver surface: the
// declared model places in TierLocal and becomes the OpenAI-shape
// local representative; seed representatives survive the copy.
func TestReloadWithLocalUpstreams(t *testing.T) {
	t.Parallel()
	r := NewTierResolver()
	r.ReloadWithLocalUpstreams(
		map[string]Tier{"ollama/llama4": TierLocal},
		map[ProviderShape]string{ShapeOpenAI: "ollama/llama4"},
	)
	if tier, _ := r.Lookup("ollama/llama4"); tier != TierLocal {
		t.Errorf("local model tier = %s", tier)
	}
	rep, ok := r.Table().Representative(ShapeOpenAI, TierLocal)
	if !ok || rep != "ollama/llama4" {
		t.Errorf("local rep = %q ok=%v", rep, ok)
	}
	if rep, ok := r.Table().Representative(ShapeOpenAI, TierHaikuClass); !ok || rep != "gpt-5.4-mini" {
		t.Errorf("seed rep lost: %q ok=%v", rep, ok)
	}
	// The seed singleton must not have been mutated (copy-on-install).
	fresh := NewTierResolver()
	if _, ok := fresh.Table().Representative(ShapeOpenAI, TierLocal); ok {
		t.Error("seed representative map mutated by local install")
	}
}

// TestDecide_PrivacyLocalOnlyRoutesToLocalUpstream pins the §R11.7 ×
// §R16 end-to-end: an OpenAI-shape turn under a local_only privacy
// rule reroutes to the local representative.
func TestDecide_PrivacyLocalOnlyRoutesToLocalUpstream(t *testing.T) {
	t.Parallel()
	resolver := NewTierResolver()
	resolver.ReloadWithLocalUpstreams(
		map[string]Tier{"ollama/llama4": TierLocal},
		map[ProviderShape]string{ShapeOpenAI: "ollama/llama4"},
	)
	snap := &Snapshot{Tiers: resolver.Table()}
	p := Policy{
		Name: "custom", Bases: defaultBases(),
		// The floor must admit the local tier for this to be legal.
		Floors:       map[TurnKind]Tier{TurnReadOnly: TierLocal},
		PrivacyRules: []PrivacyRule{{Project: "internal-ml", LocalOnly: true}},
	}
	in := readOnlyInput()
	in.Shape.Model = "gpt-5.4" // OpenAI shape
	in.Project = "internal-ml"
	in.Entitlement = EntitlementAPIKey
	d := Decide(p, snap, in)
	if !d.Changed || d.SelectedModel != "ollama/llama4" {
		t.Fatalf("decision = %+v, want reroute to the local upstream", d)
	}
	if !hasReason(d.ReasonCodes, ReasonPrivacyHold) {
		t.Errorf("reasons = %v, want privacy_hold attribution", d.ReasonCodes)
	}
}
