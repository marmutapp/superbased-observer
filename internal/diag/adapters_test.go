package diag

import (
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/integration"
)

func TestAzureProviderConfigured(t *testing.T) {
	for _, k := range []string{"AZURE_RESOURCE_NAME", "AZURE_OPENAI_ENDPOINT", "AZURE_OPENAI_API_KEY", "AZURE_API_KEY", "OPENAI_API_TYPE"} {
		t.Setenv(k, "")
	}
	if azureProviderConfigured() {
		t.Fatal("expected false with all Azure signals cleared")
	}
	t.Setenv("AZURE_RESOURCE_NAME", "my-foundry")
	if !azureProviderConfigured() {
		t.Error("AZURE_RESOURCE_NAME should be detected")
	}
	t.Setenv("AZURE_RESOURCE_NAME", "")
	t.Setenv("OPENAI_API_TYPE", "Azure")
	if !azureProviderConfigured() {
		t.Error("OPENAI_API_TYPE=azure (any case) should be detected")
	}
}

func TestEnabledSet(t *testing.T) {
	if enabledSet(config.Config{}) != nil {
		t.Error("empty allow-list should be nil (all enabled)")
	}
	cfg := config.Config{}
	cfg.Observer.Watch.EnabledAdapters = []string{"claude-code", "codex"}
	got := enabledSet(cfg)
	if !got["claude-code"] || got["cursor"] {
		t.Errorf("enabledSet = %v", got)
	}
}

func TestReportFilter(t *testing.T) {
	r := Report{Checks: []Check{
		{Name: "db.schema"},
		{Name: "org enrolment"},
		{Name: "copilot-teams"},
	}}
	if got := r.Filter(""); len(got.Checks) != 3 {
		t.Errorf("empty filter dropped checks: %d", len(got.Checks))
	}
	if got := r.Filter("org"); len(got.Checks) != 1 {
		t.Errorf("org filter should match 'org enrolment': %+v", got.Checks)
	}
	if got := r.Filter("nonesuch"); len(got.Checks) != 0 {
		t.Errorf("nonmatching filter should be empty: %+v", got.Checks)
	}
}

func TestCheckAdapter(t *testing.T) {
	// Unknown tool → ok=false so the caller can fall back to Filter.
	if _, ok := CheckAdapter("definitely-not-an-adapter", config.Config{}); ok {
		t.Error("unknown tool should return ok=false")
	}

	// Known watcher-only adapter → ok=true, named, and notes the
	// watcher/hook capture path (no proxy launcher line).
	c, ok := CheckAdapter("cursor", config.Config{})
	if !ok {
		t.Fatal("cursor should be a known adapter")
	}
	if c.Name != "cursor" {
		t.Errorf("check name = %q", c.Name)
	}
	if !joinedHas(c.Details, "watcher/hooks") {
		t.Errorf("watcher-only adapter should note the watcher path: %v", c.Details)
	}

	// Routable adapter (opencode) → includes a proxy-routing line.
	oc, ok := CheckAdapter("opencode", config.Config{})
	if !ok {
		t.Fatal("opencode should be a known adapter")
	}
	if !joinedHas(oc.Details, "proxy") && !joinedHas(oc.Details, "OPENAI_BASE_URL") {
		t.Errorf("routable adapter should include routing pre-flight: %v", oc.Details)
	}
}

func joinedHas(details []string, sub string) bool {
	return strings.Contains(strings.Join(details, "\n"), sub)
}

// TestNativeRailsSummary pins the doctor's native-console ledger rendering
// (Phase 4): no rails → enrollment-only; rails render A/B/C with the note.
func TestNativeRailsSummary(t *testing.T) {
	tests := []struct {
		name string
		in   integration.NativeRails
		want string
	}{
		{"none", integration.NativeRails{}, "enrollment-only (no vendor telemetry rails)"},
		{"all", integration.NativeRails{A: true, B: true, C: true}, "rails A:node-telemetry B:managed-config C:org-analytics"},
		{"c-only-note", integration.NativeRails{C: true, Note: "config-gated"}, "rails C:org-analytics (config-gated)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := nativeRailsSummary(tc.in); got != tc.want {
				t.Errorf("nativeRailsSummary(%+v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestTokenTierSummary pins the doctor's token/cost coverage gauge (Phase 5):
// best tier always named; a known gap appended honestly; empty best → unknown.
func TestTokenTierSummary(t *testing.T) {
	tests := []struct {
		name string
		in   integration.TokenTier
		want string
	}{
		{"clean", integration.TokenTier{Best: "proxy"}, "best tier=proxy (no known gap)"},
		{"gapped", integration.TokenTier{Best: "sqlite", Gap: "model often blank"}, "best tier=sqlite — known gap: model often blank"},
		{"unknown", integration.TokenTier{}, "best tier=unknown (no known gap)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tokenTierSummary(tc.in); got != tc.want {
				t.Errorf("tokenTierSummary(%+v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
