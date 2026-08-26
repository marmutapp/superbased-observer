package admission

// SchemaHint returns a one-line, allowed-keys descriptor for the
// admission.input body, for use as a PROMPT AID by a self-hosted
// policy-evolution reviewer so a small model does not emit stray keys that
// DecodeBody's DisallowUnknownFields would reject (a 3B leaked an evidence
// label — "flag_reason_labels" — into a criterion without it). It is NOT a
// validator — CompileBody / the lint gate remain the sole authority.
//
// This deliberately advertises the MINIMAL safe subset (mode + criteria, and
// only the four core criterion keys) rather than every optional field: the
// evolution reviewer edits an existing body and copies forward any other
// keys it already contains, and constraining the model to the core keys is
// what keeps its NEW keys schema-clean. Every advertised key is a real,
// accepted field (the drift guard TestSchemaHintDecodes decodes a body using
// exactly these keys). Co-located with the wire types (wire.go).
func SchemaHint() string {
	return `A JSON object with exactly two keys: "mode" (string, "observe" or "enforce") and "criteria" (array). ` +
		`Each element of "criteria" is an object with EXACTLY these keys and no others: ` +
		`"id" (string), "type" (one of "valid_use_case","jailbreak","denied_topics","custom"), ` +
		`"decision" (one of "deny","flag","ask","allow"), "definition" (string). ` +
		`Do not add any other key to a criterion (no counts, labels, or notes).`
}
