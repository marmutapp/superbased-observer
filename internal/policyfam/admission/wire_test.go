package admission

import (
	"strings"
	"testing"
)

func TestDecodeBodyStrict(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		max     int64
		wantErr string
	}{
		{name: "valid", in: `{"mode":"enforce"}`, max: 1024},
		{name: "unknown field rejected", in: `{"mode":"enforce","bogus":1}`, max: 1024, wantErr: "unknown field"},
		{name: "trailing bytes rejected", in: `{"mode":"enforce"}{}`, max: 1024, wantErr: "trailing bytes"},
		{name: "over cap rejected", in: `{"mode":"enforce"}`, max: 4, wantErr: "exceeds"},
		{name: "non-positive cap rejected", in: `{"mode":"enforce"}`, max: 0, wantErr: "cap must be positive"},
		{name: "not json", in: `nope`, max: 1024, wantErr: "invalid character"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeBody([]byte(tc.in), tc.max)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("DecodeBody(%q) = %v, want nil", tc.in, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("DecodeBody(%q) error = %v, want substring %q", tc.in, err, tc.wantErr)
			}
		})
	}
}

func TestCanonicalJSONIsOrderAndWhitespaceInvariant(t *testing.T) {
	a, err := DecodeBody([]byte(`{"mode":"observe","criteria":[{"id":"x","type":"jailbreak","decision":"deny"}]}`), 4096)
	if err != nil {
		t.Fatalf("DecodeBody a: %v", err)
	}
	b, err := DecodeBody([]byte("  { \"criteria\":[{\"decision\":\"deny\",\"type\":\"jailbreak\",\"id\":\"x\"}] , \"mode\" : \"observe\" }  "), 4096)
	if err != nil {
		t.Fatalf("DecodeBody b: %v", err)
	}
	ca, err := CanonicalJSON(a)
	if err != nil {
		t.Fatalf("CanonicalJSON a: %v", err)
	}
	cb, err := CanonicalJSON(b)
	if err != nil {
		t.Fatalf("CanonicalJSON b: %v", err)
	}
	if string(ca) != string(cb) {
		t.Fatalf("canonical JSON must be reordering/whitespace invariant:\n%s\n!=\n%s", ca, cb)
	}
}

func TestCompileBodyRoundTrip(t *testing.T) {
	spec, canon, err := CompileBody([]byte(`{"mode":"enforce","criteria":[{"id":"x","type":"jailbreak","decision":"deny","severity":"high"}]}`), 4096)
	if err != nil {
		t.Fatalf("CompileBody: %v", err)
	}
	if spec.Mode != ModeEnforce {
		t.Errorf("mode = %v, want enforce", spec.Mode)
	}
	if len(spec.Criteria) != 1 || spec.Criteria[0].ID != "x" {
		t.Fatalf("criteria = %+v", spec.Criteria)
	}
	if len(canon) == 0 {
		t.Error("expected non-empty canonical body")
	}
	// Re-decoding the canonical body must compile to the identical spec hash
	// — CompileBody's contract is that Body==canon reproduces the same spec.
	spec2, _, err := CompileBody(canon, 4096)
	if err != nil {
		t.Fatalf("CompileBody(canon): %v", err)
	}
	if spec.Hash != spec2.Hash {
		t.Error("compiling the canonical body must reproduce the identical hash")
	}
}

func TestCompileBodyRejectsBadCriterion(t *testing.T) {
	if _, _, err := CompileBody([]byte(`{"criteria":[{"id":"x","type":"jailbreak","decision":"nope"}]}`), 4096); err == nil {
		t.Fatal("expected compile error for an unknown decision")
	}
}

func TestBodyV1RoundTripThroughPolicyInput(t *testing.T) {
	in := PolicyInput{
		Mode: "enforce",
		Criteria: []CriterionInput{
			{ID: "x", Type: "denied_topics", Decision: "flag", Severity: "warn", Topics: []string{"Acme"}},
		},
		Prefilter: PrefilterInput{Deny: []string{"secret"}},
	}
	body := BodyV1FromPolicyInput(in)
	out := body.ToPolicyInput()
	if out.Mode != in.Mode || len(out.Criteria) != 1 || out.Criteria[0].ID != "x" {
		t.Fatalf("round trip mismatch: %+v", out)
	}
	if len(out.Prefilter.Deny) != 1 || out.Prefilter.Deny[0] != "secret" {
		t.Fatalf("prefilter round trip mismatch: %+v", out.Prefilter)
	}
}
