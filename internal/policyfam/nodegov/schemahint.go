package nodegov

// SchemaHint returns a one-line, allowed-keys descriptor for the
// node.governance body (Body), for use as a PROMPT AID by a self-hosted
// policy-evolution reviewer so a small model does not emit stray keys that
// DecodeBody's DisallowUnknownFields would reject. It is NOT a validator —
// CompileBody / the lint gate remain the sole authority.
//
// It names the structural KEYS only, not the closed vocabularies of section
// ids / pinnable keys / share keys / feature ids (those are values the
// reviewer copies from the current body). Co-located with the wire types
// (wire.go); the drift guard is TestSchemaHintDecodes (schemahint_test.go).
func SchemaHint() string {
	return `A JSON object with keys (use ONLY these): ` +
		`"schema" (number, currently 2), "sections" (object), "pinned" (object mapping a dotted config key to a value), ` +
		`"share" (object), "features" (object mapping a feature id to a bool), "notice" (object). ` +
		`The "sections" object has keys: "hidden", "read_only", "settings_hidden", "settings_read_only" (each an array of section ids). ` +
		`The "notice" object has keys: "org_display_name", "contact", "policy_url". ` +
		`Do not introduce any other key; reuse only the section ids / config keys / feature ids already present in the current body.`
}
