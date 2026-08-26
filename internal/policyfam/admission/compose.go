package admission

// ComposeOrgSpec composes an org-distributed (remote) compiled admission
// spec with the node's LOCAL layer, mirroring
// internal/routingconfig.ComposeOrgPolicy's §R23 discipline for this family
// (docs/plane-a/unified-policy-resource.md; finding B-B1 of
// docs/audits/cursor-arc-code-review-2026-08-13.md).
//
// Composition semantics:
//
//   - The org body's MODE is STRUCTURALLY IGNORED: this function never reads
//     org.Mode. The composed spec runs at whatever posture the LOCAL node
//     config chose — ModeOff when the node has no local layer at all, so a
//     remote body can never switch the family on either.
//   - Everything else — criteria, prefilter, scope, strictness, the secret
//     gate, judge chunking, and the content Hash — comes from the org body,
//     preserving v1's "Org wins outright over Local CONTENT, no merge"
//     precedence (plan §6.1). Hash stays the org body's content address so
//     audit rows and the verdict cache still key on exactly the signed body
//     that was evaluated.
//
// The invariant this enforces is the ratified "never server-forced" posture:
// an org publisher can change WHAT a node evaluates, never WHETHER it
// evaluates or HOW HARD it enforces. A published {"mode":"off"} can no
// longer disable a locally-enforcing node, and a published
// {"mode":"enforce"} can no longer escalate a locally-observing one.
//
// orgEnforce is the Arc 4 P3 §R23 LIFT for Enterprise-Managed Tenancy: when
// the node enrolled managed AND holds the enforce.admission authority
// (govern.Effective.GrantsAdmissionEnforcement, resolved at the boundary and
// carried in on OrgLayerMeta.ManagedEnforce), the org body's mode is HONORED
// as authored — the org may turn enforcement on. There is NO coercion
// (operator decision): the composed mode is exactly org.Mode, so publishing
// observe/off stays a real per-cohort opt-out. On the individual plane
// orgEnforce is always false (the token is stripped there), so the "never
// server-forced" invariant above is untouched for every non-managed node.
//
// local may be nil (no local layer installed).
func ComposeOrgSpec(local *PolicySpec, org PolicySpec, orgEnforce bool) PolicySpec {
	out := org
	if orgEnforce {
		return out // org.Mode honored as authored
	}
	out.Mode = ModeOff
	if local != nil {
		out.Mode = local.Mode
	}
	return out
}
