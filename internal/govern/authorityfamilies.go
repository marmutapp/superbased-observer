package govern

import "sort"

// The [org_client.policy] family vocabulary (internal/policyfam's closed v1
// enum), duplicated here as literal strings rather than imported. This
// mirrors the codebase's existing convention for a small closed enum owned
// by another boundary — internal/policystate/collector.go and
// internal/config.OrgClientPolicyConfig both keep their own literal copy of
// the same four strings rather than pulling in internal/policyfam, and
// govern must stay import-clean of internal/config/internal/store/etc
// (imports_test.go) regardless.
const (
	familyAdmissionInput   = "admission.input"
	familyEgressGuardrail  = "egress.routing_guardrail"
	familyGatewayProviders = "gateway.providers"
	familyNodeGovernance   = "node.governance"
)

// authorityFamilyRow is one row of the authority-to-family table (CLAUDE.md
// #5 — a data table walked in full, never an if/else-if ladder): which
// [org_client.policy] family (or families) a KnownAuthority token governs,
// i.e. which accept_families/preauthorize_enforce entries a node must
// consent to before that authority's directive class can actually take
// effect (W-5: docs/plans/admin-controlled-plane-b-spec-2026-08-15.md and
// the operator ruling that managed enrolment auto-writes this consent).
type authorityFamilyRow struct {
	authority string
	families  []string
}

// authorityFamilyTable is the SINGLE OWNER of the authority-to-family
// mapping (CLAUDE.md #4 — one owner of this piece of state). Every row is a
// KnownAuthority token; AuthorityCaptureRaise is the one deliberate
// exemption, tracked below in exemptFromFamilyMapping, not a silent gap.
//
//   - dashboard.visibility / settings.pin / capture.pin / feature.lock and
//     every extract.* token (including the extract.managed umbrella) are
//     all directive classes of the node.governance family
//     (internal/policyfam/nodegov compiles all of them; resolve.go's
//     "sections" / "pinned" / "share" / "features" rows are the four
//     directive classes that family carries).
//   - enforce.admission lifts the §R23 structural-ignore for the
//     admission.input family (internal/obs/admission).
//   - enforce.egress lifts it for the egress.routing_guardrail family
//     (internal/guard's egress policy).
//   - enforce.routing lifts it for the gateway.providers family (the
//     Phase 3 dashboard-managed proxy lane table). Model-routing
//     enforcement is delivered through the [routing] org policy fragment,
//     which rides the gateway.providers family compiler, not a routing-
//     specific one.
var authorityFamilyTable = []authorityFamilyRow{
	{AuthorityDashboardVisibility, []string{familyNodeGovernance}},
	{AuthoritySettingsPin, []string{familyNodeGovernance}},
	{AuthorityCapturePin, []string{familyNodeGovernance}},
	{AuthorityFeatureLock, []string{familyNodeGovernance}},
	{AuthorityExtractManaged, []string{familyNodeGovernance}},
	{AuthorityExtractCodeintel, []string{familyNodeGovernance}},
	{AuthorityExtractProcess, []string{familyNodeGovernance}},
	{AuthorityExtractTerminal, []string{familyNodeGovernance}},
	{AuthorityExtractToolBodies, []string{familyNodeGovernance}},
	{AuthorityExtractFolders, []string{familyNodeGovernance}},
	{AuthorityExtractTraces, []string{familyNodeGovernance}},
	{AuthorityExtractCache, []string{familyNodeGovernance}},
	{AuthorityExtractRouting, []string{familyNodeGovernance}},
	{AuthorityExtractPredictions, []string{familyNodeGovernance}},
	{AuthorityEnforceAdmission, []string{familyAdmissionInput}},
	{AuthorityEnforceEgress, []string{familyEgressGuardrail}},
	{AuthorityEnforceRouting, []string{familyGatewayProviders}},
}

// exemptFromFamilyMapping is the CONSCIOUS exemption list this package's own
// drift test (TestGovernedFamilies_EveryKnownAuthorityMapsOrIsExempt) checks
// against: a KnownAuthority token that deliberately maps to zero families,
// because RetiredAuthority already says it grants nothing at all.
//
// AuthorityCaptureRaise is RETIRED and PERMANENTLY INERT (types.go): it
// never governs anything, so it cannot govern a policy family either. A
// future token added here must be a similarly considered decision, not an
// oversight — that is the whole point of keeping the list separate from a
// silent "no match" fallthrough.
var exemptFromFamilyMapping = map[string]bool{
	AuthorityCaptureRaise: true,
}

// GovernedFamilies returns the sorted, deduplicated set of [org_client.policy]
// families the given authority tokens govern, per authorityFamilyTable. A
// token this build does not recognise, or one in exemptFromFamilyMapping,
// contributes nothing — GovernedFamilies never fabricates a family for a
// token it cannot honestly attribute one to (the same honesty rule
// KnownAuthority/RetiredAuthority already follow).
func GovernedFamilies(authority []string) []string {
	set := make(map[string]bool)
	for _, tok := range authority {
		for _, row := range authorityFamilyTable {
			if row.authority == tok {
				for _, f := range row.families {
					set[f] = true
				}
			}
		}
	}
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for f := range set {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}
