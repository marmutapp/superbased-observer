package nodegov

import (
	"encoding/json"
	"strings"
	"testing"
)

const schema2Max = 1 << 20

// TestCompileSchema2Rejected is the schema-2 grammar table (§6.3). Each row
// is a body an admin must not be able to publish and an agent must not be
// able to accept — one table, two enforcement points, because both call
// CompileBody.
func TestCompileSchema2Rejected(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"unknown pinnable key", `{"schema":2,"pinned":{"guard.turbo":true}}`, "not a settable key"},
		{"bootstrap-envelope key", `{"schema":2,"pinned":{"org_client.org_server_url":"https://evil.example"}}`, "not a settable key"},
		{"share key in pinned", `{"schema":2,"pinned":{"org_client.share.full_content":false}}`, "not a settable key"},
		{"type mismatch", `{"schema":2,"pinned":{"guard.enabled":"yes"}}`, "must be a boolean"},
		// The B4 blocker in one row: "strict" type-checks as a string and
		// would pass a Kind-only grammar, then be rejected by
		// config.Validate at every hook invocation on every node.
		{"enum violation", `{"schema":2,"pinned":{"guard.mode":"strict"}}`, "not one of"},
		{"restrictive-only violation", `{"schema":2,"pinned":{"remote.enabled":true}}`, "restrictive-only"},
		{"secrets scrubbing cannot be forced off", `{"schema":2,"pinned":{"observer.secrets.enable_scrubbing":false}}`, "restrictive-only"},
		{"unknown share key", `{"schema":2,"share":{"everything":true}}`, "not an organization-directable sharing key"},
		{"share type mismatch", `{"schema":2,"share":{"full_content":"no"}}`, "must be a boolean"},
		{"share list type mismatch", `{"schema":2,"share":{"target_action_allowlist":"read_file"}}`, "must be a list of strings"},
		{"unknown feature", `{"schema":2,"features":{"telepathy":true}}`, "not a known feature"},
		{"feature direction violation", `{"schema":2,"features":{"remote_access":true}}`, "restrictive-only"},
		{"feature contradicts a pin", `{"schema":2,"pinned":{"guard.enabled":false},"features":{"guard":true}}`, "one key, one value"},
		{"unhideable id still refused under schema 2", `{"schema":2,"sections":{"hidden":["privacy"]}}`, "can never be hidden or locked"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := CompileBody([]byte(tc.raw), schema2Max)
			if err == nil {
				t.Fatalf("CompileBody accepted %s, want a hard error", tc.raw)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), tc.want)
			}
		})
	}
}

// TestCompileSchema2Accepted covers the shapes that must compile, including
// the features→pinned expansion.
func TestCompileSchema2Accepted(t *testing.T) {
	spec, _, err := CompileBody([]byte(`{"schema":2,
		"pinned":{"guard.enabled":true,"guard.mode":"enforce"},
		"share":{"full_content":false,"target_action_allowlist":["run_command","read_file"]},
		"features":{"codeintel":false,"secrets":true}}`), schema2Max)
	if err != nil {
		t.Fatalf("CompileBody: %v", err)
	}
	if spec.Pinned["guard.enabled"] != true || spec.Pinned["guard.mode"] != "enforce" {
		t.Fatalf("pinned = %+v", spec.Pinned)
	}
	if spec.Share["full_content"] != false {
		t.Fatalf("share = %+v", spec.Share)
	}
	// string_list values are normalized: trimmed, deduped, SORTED, so two
	// semantically-equal bodies hash identically.
	got, _ := spec.Share["target_action_allowlist"].([]string)
	if len(got) != 2 || got[0] != "read_file" || got[1] != "run_command" {
		t.Fatalf("target_action_allowlist = %#v, want sorted [read_file run_command]", got)
	}
	if spec.FeaturePinned["codeintel.enabled"] != false ||
		spec.FeaturePinned["observer.secrets.enable_scrubbing"] != true {
		t.Fatalf("features did not expand into pins: %+v", spec.FeaturePinned)
	}
	if spec.Features["codeintel"] != false || spec.Features["secrets"] != true {
		t.Fatalf("features not retained for display: %+v", spec.Features)
	}
}

// TestSchema1BodyCompilesByteIdentically: schema 2 is additive, so a
// schema-1 body must produce exactly the schema-1 spec and canonical bytes
// it produced in Phase 1a.
func TestSchema1BodyCompilesByteIdentically(t *testing.T) {
	raw := []byte(`{"schema":1,"sections":{"hidden":["benchmarks"],"read_only":["policies"]},"notice":{"org_display_name":"Acme"}}`)
	spec, canon, err := CompileBody(raw, schema2Max)
	if err != nil {
		t.Fatalf("CompileBody: %v", err)
	}
	if len(spec.Pinned)+len(spec.Share)+len(spec.Features)+len(spec.FeaturePinned) != 0 {
		t.Fatalf("a schema-1 body produced Phase-1b directives: %+v", spec)
	}
	var decoded map[string]any
	if err := json.Unmarshal(canon, &decoded); err != nil {
		t.Fatalf("canonical body is not JSON: %v", err)
	}
	if decoded["schema"] != float64(1) {
		t.Fatalf("canonical schema = %v, want 1 (canonicalization must not silently upgrade a body)", decoded["schema"])
	}
	for _, key := range []string{"pinned", "share", "features"} {
		if _, present := decoded[key]; present {
			t.Fatalf("canonical schema-1 body grew a %q key: %s", key, canon)
		}
	}
}

// TestPinnedChangeChangesHash: the compiled hash must cover the new
// directive classes, so a body that differs only in its pinned map is a
// different resource.
func TestPinnedChangeChangesHash(t *testing.T) {
	a, _, err := CompileBody([]byte(`{"schema":2,"pinned":{"guard.enabled":true}}`), schema2Max)
	if err != nil {
		t.Fatalf("CompileBody: %v", err)
	}
	b, _, err := CompileBody([]byte(`{"schema":2,"pinned":{"guard.enabled":false}}`), schema2Max)
	if err != nil {
		t.Fatalf("CompileBody: %v", err)
	}
	if a.Hash == b.Hash {
		t.Fatal("two different pinned maps hash identically — a changed pin would look like convergence")
	}
}

// TestVocabularyIsComplete: the admin console's form is populated from these
// tables, so every table must be represented.
func TestVocabularyIsComplete(t *testing.T) {
	v := Vocabulary()
	if v.Schema != MaxSchema || v.Family != "node.governance" {
		t.Fatalf("vocab header = %+v", v)
	}
	if len(v.NavSections) != len(NavSectionIDs) || len(v.SettingsSections) != len(SettingsSectionIDs) {
		t.Fatalf("section counts drifted: %d/%d", len(v.NavSections), len(v.SettingsSections))
	}
	if len(v.PinnableKeys) != len(PinnableKeys) || len(v.ShareKeys) != len(ShareKeys) || len(v.Features) != len(Features) {
		t.Fatal("vocabulary dropped rows")
	}
	var unhideable int
	for _, s := range v.NavSections {
		if s.Unhideable {
			unhideable++
		}
	}
	if unhideable != len(UnhideableNavSectionIDs) {
		t.Fatalf("vocab marks %d unhideable nav sections, want %d", unhideable, len(UnhideableNavSectionIDs))
	}
	// capture.raise must be PRESENT and RETIRED, never omitted: an admin
	// looking at an older grant has to be able to see the token exists and
	// grants nothing.
	var sawRetired bool
	for _, a := range v.AuthorityTokens {
		if a.Token == authorityCaptureRaise {
			sawRetired = a.Retired && len(a.Directives) == 0
		}
	}
	if !sawRetired {
		t.Fatal("capture.raise is missing from the vocabulary or is not marked retired")
	}
}
