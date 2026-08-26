package egress

// ComposeOrgSpec composes an org-distributed (remote) compiled egress spec
// with the node's LOCAL layer — the egress.routing_guardrail counterpart of
// policyfam/admission.ComposeOrgSpec, and the same §R23 mirror
// (internal/routingconfig.ComposeOrgPolicy; finding B-B1 of
// docs/audits/cursor-arc-code-review-2026-08-13.md).
//
// Composition semantics:
//
//   - The org body's MODE is STRUCTURALLY IGNORED: this function never reads
//     org.Mode. The composed spec runs at the LOCAL layer's posture —
//     ModeOff when the node has no local layer, so a remote body can never
//     switch egress guardrails on, off, or up.
//   - Everything else — rules, targets, cohorts, the switch cooldown and the
//     content Hash — comes from the org body, preserving v1's "Org wins
//     outright over Local CONTENT, no merge" precedence (plan §6.1).
//
// An unset/unknown local mode normalizes to off (the safe default), so a
// hand-built local spec that never went through Compile cannot leave the
// composed spec in an unnormalized posture.
//
// orgEnforce is the Arc 4 P3 §R23 LIFT (managed tenancy + enforce.egress,
// carried in on OrgLayerMeta.ManagedEnforce): when true the org body's mode is
// HONORED as authored (the org may turn the guardrail on), with NO coercion —
// observe/off stays a real per-cohort opt-out. Always false on the individual
// plane, so the "never server-forced" posture is untouched there.
//
// local may be nil (no local layer installed).
func ComposeOrgSpec(local *PolicySpec, org PolicySpec, orgEnforce bool) PolicySpec {
	out := org
	if orgEnforce {
		return out // org.Mode honored as authored
	}
	out.Mode = ModeOff
	if local != nil {
		out.Mode = normalizeMode(local.Mode)
	}
	return out
}
