package config

import (
	"testing"

	"github.com/marmutapp/superbased-observer/internal/policyfam"
)

// TestPolicyResourceSupportedFamiliesMatchesPolicyfam pins the local
// policyResourceSupportedFamilies copy to policyfam.SupportedFamilies (the
// one owner of the closed family enum). The copy exists because
// internal/config must not import the policy layer at runtime for one map
// (see the var's doc comment), but a hand-copy drifts: gateway.providers
// shipped in policyfam on 2026-08-14 and was missing here until 2026-08-15,
// which made accept_families reject the family so every gateway resource
// landed delivered_unaccepted. A test-only import closes the drift class
// without coupling the packages.
func TestPolicyResourceSupportedFamiliesMatchesPolicyfam(t *testing.T) {
	if len(policyResourceSupportedFamilies) != len(policyfam.SupportedFamilies) {
		t.Fatalf("policyResourceSupportedFamilies has %d families, policyfam.SupportedFamilies has %d: add the missing family to the config copy (and its two error strings in validateOrgClientPolicy)",
			len(policyResourceSupportedFamilies), len(policyfam.SupportedFamilies))
	}
	for _, f := range policyfam.SupportedFamilies {
		if !policyResourceSupportedFamilies[f] {
			t.Fatalf("policyfam.SupportedFamilies contains %q but internal/config's policyResourceSupportedFamilies does not: accept_families would reject it and every resource of that family would land delivered_unaccepted", f)
		}
	}
}
