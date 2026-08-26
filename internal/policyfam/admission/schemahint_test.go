package admission

import "testing"

// TestSchemaHintDecodes is the drift guard for SchemaHint — see the egress
// package's equivalent for the rationale. The admission hint advertises the
// minimal safe subset (mode + criteria with four core keys); every advertised
// key must still decode.
func TestSchemaHintDecodes(t *testing.T) {
	if SchemaHint() == "" {
		t.Fatal("admission.SchemaHint() is empty")
	}
	body := `{"mode": "observe", "criteria": [{"id": "c", "type": "jailbreak", "decision": "deny", "definition": "d"}]}`
	if _, err := DecodeBody([]byte(body), 1<<20); err != nil {
		t.Fatalf("hinted-keys body failed to decode (schema drifted from SchemaHint?): %v", err)
	}
}
