package providers

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
		{name: "valid", in: `{"upstreams":{"main":{"base_url":"https://api.example.com"}}}`, max: 1024},
		{name: "unknown top-level field rejected", in: `{"upstreams":{},"bogus":1}`, max: 1024, wantErr: "unknown field"},
		{name: "unknown nested field rejected", in: `{"upstreams":{"main":{"base_url":"https://x","bogus":1}}}`, max: 1024, wantErr: "unknown field"},
		{name: "trailing bytes rejected", in: `{"upstreams":{}}{}`, max: 1024, wantErr: "trailing bytes"},
		{name: "over cap rejected", in: `{"upstreams":{}}`, max: 4, wantErr: "exceeds"},
		{name: "non-positive cap rejected", in: `{"upstreams":{}}`, max: 0, wantErr: "cap must be positive"},
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
	a, err := DecodeBody([]byte(`{"upstreams":{"b":{"base_url":"https://b"},"a":{"base_url":"https://a"}},"auto_default_lane":"a"}`), 4096)
	if err != nil {
		t.Fatalf("DecodeBody a: %v", err)
	}
	b, err := DecodeBody([]byte("  { \"auto_default_lane\":\"a\" , \"upstreams\":{\"a\":{\"base_url\":\"https://a\"},\"b\":{\"base_url\":\"https://b\"}} }  "), 4096)
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
	spec, canon, err := CompileBody([]byte(`{"upstreams":{"openai":{"base_url":"https://api.openai.com"},"anthropic":{"base_url":"https://api.anthropic.com"}},"auto_default_lane":"anthropic"}`), 4096)
	if err != nil {
		t.Fatalf("CompileBody: %v", err)
	}
	if spec.AutoDefaultLane != "anthropic" {
		t.Errorf("auto_default_lane = %q, want anthropic", spec.AutoDefaultLane)
	}
	if len(spec.Upstreams) != 2 || spec.Upstreams["openai"] != "https://api.openai.com" {
		t.Fatalf("upstreams = %+v", spec.Upstreams)
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

func TestCompileBodyValidation(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr string
	}{
		{
			name:    "empty upstreams rejected",
			in:      `{"upstreams":{}}`,
			wantErr: "at least one upstream is required",
		},
		{
			name:    "reserved auto lane id rejected",
			in:      `{"upstreams":{"auto":{"base_url":"https://x"}}}`,
			wantErr: `"auto" is a reserved lane id`,
		},
		{
			name:    "non-absolute url rejected",
			in:      `{"upstreams":{"main":{"base_url":"not-a-url"}}}`,
			wantErr: "must be an absolute http or https URL",
		},
		{
			name:    "non-http scheme rejected",
			in:      `{"upstreams":{"main":{"base_url":"ftp://example.com"}}}`,
			wantErr: "must be an absolute http or https URL",
		},
		{
			name:    "dangling auto_default_lane rejected",
			in:      `{"upstreams":{"main":{"base_url":"https://example.com"}},"auto_default_lane":"missing"}`,
			wantErr: "does not name a configured upstream",
		},
		{
			name:    "auto_default_lane naming reserved auto rejected",
			in:      `{"upstreams":{"main":{"base_url":"https://example.com"}},"auto_default_lane":"auto"}`,
			wantErr: "does not name a configured upstream",
		},
		{
			name: "valid multi-lane body accepted",
			in:   `{"upstreams":{"main":{"base_url":"https://example.com"},"secondary":{"base_url":"http://internal.example.com:8080"}},"auto_default_lane":"main"}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := CompileBody([]byte(tc.in), 4096)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("CompileBody(%q) = %v, want nil", tc.in, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("CompileBody(%q) error = %v, want substring %q", tc.in, err, tc.wantErr)
			}
		})
	}
}

func TestBodyV1RoundTripThroughPolicyInput(t *testing.T) {
	in := PolicyInput{
		Upstreams:       map[string]string{"main": "https://example.com"},
		AutoDefaultLane: "main",
	}
	body := BodyV1FromPolicyInput(in)
	out := body.ToPolicyInput()
	if out.AutoDefaultLane != in.AutoDefaultLane || len(out.Upstreams) != 1 || out.Upstreams["main"] != "https://example.com" {
		t.Fatalf("round trip mismatch: %+v", out)
	}
}

func TestUpstreamsAsStringMapIsDefensiveCopy(t *testing.T) {
	spec, _, err := CompileBody([]byte(`{"upstreams":{"main":{"base_url":"https://example.com"}}}`), 4096)
	if err != nil {
		t.Fatalf("CompileBody: %v", err)
	}
	m := spec.UpstreamsAsStringMap()
	m["main"] = "mutated"
	if spec.Upstreams["main"] != "https://example.com" {
		t.Fatalf("mutating the returned map must not affect the spec, got %q", spec.Upstreams["main"])
	}
}
