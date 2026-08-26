package providers

// SchemaHint returns a one-line, allowed-keys descriptor for the
// gateway.providers body (BodyV1), for use as a PROMPT AID by a self-hosted
// policy-evolution reviewer so a small model does not emit stray keys that
// DecodeBody's DisallowUnknownFields would reject. It is NOT a validator —
// CompileBody / the lint gate remain the sole authority.
//
// Co-located with the wire types (wire.go); the drift guard is
// TestSchemaHintDecodes (schemahint_test.go).
func SchemaHint() string {
	return `A JSON object with keys (use ONLY these): ` +
		`"upstreams" (object mapping each lane id to an object with a SINGLE key "base_url" whose value is a URL string), ` +
		`and optionally "auto_default_lane" (string naming one of the upstream lane ids). ` +
		`Do not introduce any other key, and do not give an upstream any key other than "base_url".`
}
