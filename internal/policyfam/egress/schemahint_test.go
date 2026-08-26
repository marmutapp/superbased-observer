package egress

import "testing"

// TestSchemaHintDecodes is the drift guard for SchemaHint: a body using
// exactly the keys the hint advertises must decode through DecodeBody
// (DisallowUnknownFields). If a wire json tag is renamed/removed without
// updating the hint (and this mirror body), decode fails here.
func TestSchemaHintDecodes(t *testing.T) {
	if SchemaHint() == "" {
		t.Fatal("egress.SchemaHint() is empty")
	}
	body := `{
		"mode": "enforce",
		"cooldown_seconds": 0,
		"targets": [{"id": "t", "url": "u", "shape": "s"}],
		"rules": [{
			"name": "r",
			"when": {"verdict_at_least": "", "criterion": "", "severity_at_least": "",
			         "content_class": "", "model_glob": "*", "provider": "", "user": "",
			         "user_cohort": "", "budget_band_at_least": 0, "min_prompt_tokens": 0},
			"action": {"route_to_upstream": "", "route_to_model": "", "set_effort": "", "deny": true, "no_route": false},
			"on_unavailable": "", "reason": "", "reason_code": ""
		}],
		"cohorts": {"a": "b"}
	}`
	if _, err := DecodeBody([]byte(body), 1<<20); err != nil {
		t.Fatalf("hinted-keys body failed to decode (schema drifted from SchemaHint?): %v", err)
	}
}
