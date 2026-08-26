package nodegov

import "testing"

// TestSchemaHintDecodes is the drift guard for SchemaHint — see the egress
// package's equivalent for the rationale.
func TestSchemaHintDecodes(t *testing.T) {
	if SchemaHint() == "" {
		t.Fatal("nodegov.SchemaHint() is empty")
	}
	body := `{
		"schema": 2,
		"sections": {"hidden": [], "read_only": [], "settings_hidden": [], "settings_read_only": []},
		"pinned": {},
		"share": {},
		"features": {},
		"notice": {"org_display_name": "", "contact": "", "policy_url": ""}
	}`
	if _, err := DecodeBody([]byte(body), 1<<20); err != nil {
		t.Fatalf("hinted-keys body failed to decode (schema drifted from SchemaHint?): %v", err)
	}
}
