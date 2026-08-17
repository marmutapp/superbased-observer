package nodegov

import (
	"encoding/json"
	"strings"
	"testing"
)

const testMax = 256 << 10

func TestCompileBodyAccepted(t *testing.T) {
	raw := []byte(`{"schema":1,
	  "sections":{"hidden":["remote","benchmarks"],"read_only":["policies"],
	              "settings_hidden":["process"],"settings_read_only":["guard"]},
	  "notice":{"org_display_name":"Acme Platform Engineering","contact":"it-help@acme.example","policy_url":"https://intranet.acme.example/observer"}}`)
	spec, canon, err := CompileBody(raw, testMax)
	if err != nil {
		t.Fatalf("CompileBody: %v", err)
	}
	if got := strings.Join(spec.HiddenSections, ","); got != "benchmarks,remote" {
		t.Fatalf("HiddenSections = %q, want the sorted \"benchmarks,remote\"", got)
	}
	if spec.Notice.OrgDisplayName != "Acme Platform Engineering" {
		t.Fatalf("notice.org_display_name = %q", spec.Notice.OrgDisplayName)
	}
	if spec.Hash == "" || len(spec.Hash) != 64 {
		t.Fatalf("Hash = %q, want 64 hex chars", spec.Hash)
	}
	// Canonical bytes must round-trip and re-compile to the same hash: the
	// signed BodyHash is computed over these, so a body that does not
	// reproduce itself would break every downstream verification.
	spec2, canon2, err := CompileBody(canon, testMax)
	if err != nil {
		t.Fatalf("CompileBody(canonical): %v", err)
	}
	if string(canon2) != string(canon) {
		t.Fatalf("canonical form is not a fixed point:\n first: %s\nsecond: %s", canon, canon2)
	}
	if spec2.Hash != spec.Hash {
		t.Fatalf("hash changed across canonical round-trip: %s vs %s", spec.Hash, spec2.Hash)
	}
	// Order-insensitivity: the same resource submitted differently hashes
	// and canonicalizes identically.
	reordered := []byte(`{"schema":1,"sections":{"hidden":["benchmarks","remote"],"read_only":["policies"],"settings_hidden":["process"],"settings_read_only":["guard"]},"notice":{"org_display_name":"Acme Platform Engineering","contact":"it-help@acme.example","policy_url":"https://intranet.acme.example/observer"}}`)
	_, canon3, err := CompileBody(reordered, testMax)
	if err != nil {
		t.Fatalf("CompileBody(reordered): %v", err)
	}
	if string(canon3) != string(canon) {
		t.Fatalf("reordered body produced a different canonical form:\n%s\n%s", canon, canon3)
	}
}

func TestCompileBodyRejected(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"missing schema", `{"sections":{"hidden":["remote"]}}`, "schema is required"},
		{"future schema", `{"schema":3,"sections":{"hidden":["remote"]}}`, "understands up to 2"},
		{"unknown top-level key", `{"schema":2,"pins":{"guard.enabled":true}}`, "unknown field"},
		// A schema-1 body naming a Phase-1b directive class is a HARD error,
		// not a silent drop: the schema number must always describe what the
		// body actually contains.
		{"Phase-1b directive class under schema 1", `{"schema":1,"pinned":{"guard.enabled":true}}`, "requires schema 2"},
		{"unknown nav id", `{"schema":1,"sections":{"hidden":["dashboards"]}}`, "not a known nav section"},
		{"unknown settings id", `{"schema":1,"sections":{"settings_hidden":["nope"]}}`, "not a known settings section"},
		{"T8: settings nav cannot be hidden", `{"schema":1,"sections":{"hidden":["settings"]}}`, "can never be hidden or locked"},
		{"T8: privacy nav cannot be hidden", `{"schema":1,"sections":{"hidden":["privacy"]}}`, "can never be hidden or locked"},
		{"T8: settings nav cannot be locked read-only", `{"schema":1,"sections":{"read_only":["settings"]}}`, "can never be hidden or locked"},
		{"T8: enrolment settings section cannot be hidden", `{"schema":1,"sections":{"settings_hidden":["enrolment"]}}`, "can never be hidden or locked"},
		{"T8: org settings section cannot be locked", `{"schema":1,"sections":{"settings_read_only":["org"]}}`, "can never be hidden or locked"},
		{"duplicate id in one list", `{"schema":1,"sections":{"hidden":["remote","remote"]}}`, "twice"},
		{"same id hidden and read-only", `{"schema":1,"sections":{"hidden":["remote"],"read_only":["remote"]}}`, "never both"},
		{"empty id", `{"schema":1,"sections":{"hidden":[""]}}`, "empty id"},
		{"trailing bytes", `{"schema":1} {}`, "trailing bytes"},
		{"non-http policy url", `{"schema":1,"notice":{"policy_url":"javascript:alert(1)"}}`, "absolute http or https URL"},
		{"control character in notice", "{\"schema\":1,\"notice\":{\"contact\":\"a\\u0007b\"}}", "control character"},
		{"oversize notice field", `{"schema":1,"notice":{"contact":"` + strings.Repeat("x", 201) + `"}}`, "exceeds the 200-byte cap"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := CompileBody([]byte(tc.raw), testMax)
			if err == nil {
				t.Fatalf("CompileBody accepted %s, want a hard error", tc.raw)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), tc.want)
			}
		})
	}
}

// TestCompileBodyEmptyPolicyIsValid pins the "you are managed, with no
// restriction" resource: it is how an org lifts every directive without
// un-publishing, and how it delivers notice copy alone.
func TestCompileBodyEmptyPolicyIsValid(t *testing.T) {
	spec, canon, err := CompileBody([]byte(`{"schema":1}`), testMax)
	if err != nil {
		t.Fatalf("CompileBody: %v", err)
	}
	if len(spec.HiddenSections)+len(spec.ReadOnlySections)+len(spec.HiddenSettings)+len(spec.ReadOnlySettings) != 0 {
		t.Fatalf("empty body compiled to a non-empty spec: %+v", spec)
	}
	var back BodyV1
	if err := json.Unmarshal(canon, &back); err != nil {
		t.Fatalf("canonical body is not valid JSON: %v", err)
	}
	if back.Schema != 1 {
		t.Fatalf("canonical schema = %d, want 1", back.Schema)
	}
}

func TestDecodeBodyCap(t *testing.T) {
	if _, err := DecodeBody([]byte(`{"schema":1}`), 4); err == nil {
		t.Fatal("DecodeBody accepted a body over the cap")
	}
	if _, err := DecodeBody([]byte(`{"schema":1}`), 0); err == nil {
		t.Fatal("DecodeBody accepted a non-positive cap")
	}
}
