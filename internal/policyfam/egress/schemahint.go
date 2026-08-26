package egress

// SchemaHint returns a one-line, allowed-keys descriptor for the
// egress.routing_guardrail body (BodyV1), for use as a PROMPT AID by a
// self-hosted policy-evolution reviewer (internal/orgserver/policyevolution)
// so a small model does not emit stray keys that DecodeBody's
// DisallowUnknownFields would reject. It is NOT a validator — CompileBody /
// the lint gate remain the sole authority; a stale hint costs at most a
// rejected proposal.
//
// It is co-located with the wire types (wire.go) so it moves when the schema
// moves. The drift guard is TestSchemaHintDecodes (schemahint_test.go): a
// body using exactly the keys named here must decode through DecodeBody, so a
// renamed/removed json tag fails the test until the hint is updated.
func SchemaHint() string {
	return `A JSON object. Top-level keys (use ONLY these): ` +
		`"mode" (string: "off", "advise", or "enforce"), "cooldown_seconds" (number), ` +
		`"targets" (array), "rules" (array), "cohorts" (object of string to string). ` +
		`Each "targets" element has keys: "id", "url", "shape". ` +
		`Each "rules" element has keys: "name", "when", "action", "on_unavailable", "reason", "reason_code". ` +
		`A rule's "when" object has keys: "verdict_at_least", "criterion", "severity_at_least", ` +
		`"content_class", "model_glob", "provider", "user", "user_cohort", "budget_band_at_least", "min_prompt_tokens". ` +
		`A rule's "action" object has keys: "route_to_upstream", "route_to_model", "set_effort", "deny", "no_route". ` +
		`Do not introduce any other key.`
}
