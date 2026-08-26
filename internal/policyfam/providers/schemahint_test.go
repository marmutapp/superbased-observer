package providers

import "testing"

// TestSchemaHintDecodes is the drift guard for SchemaHint — see the egress
// package's equivalent for the rationale.
func TestSchemaHintDecodes(t *testing.T) {
	if SchemaHint() == "" {
		t.Fatal("providers.SchemaHint() is empty")
	}
	body := `{"upstreams": {"lane": {"base_url": "https://example"}}, "auto_default_lane": "lane"}`
	if _, err := DecodeBody([]byte(body), 1<<20); err != nil {
		t.Fatalf("hinted-keys body failed to decode (schema drifted from SchemaHint?): %v", err)
	}
}
