package orgcontract

import (
	"strings"
	"testing"
)

// TestNormalizeSelectorsJSON is the canonicalization table: every accepted
// spelling collapses onto exactly ONE byte sequence (the one the signing
// message binds), and every grammar violation is an error rather than a
// silently-dropped field.
func TestNormalizeSelectorsJSON(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr string
	}{
		{name: "empty string is match-all", raw: "", want: "{}"},
		{name: "empty object is match-all", raw: "{}", want: "{}"},
		{name: "whitespace-only is match-all", raw: "   ", want: "{}"},
		{name: "single key", raw: `{"environment":"prod"}`, want: `{"environment":"prod"}`},
		{
			name: "keys sort alphabetically",
			raw:  `{"workspace":"acme","service":"api","environment":"prod"}`,
			want: `{"environment":"prod","service":"api","workspace":"acme"}`,
		},
		{
			name: "padded document and padded values are trimmed",
			raw:  "  { \"workspace\" : \" acme \" }  ",
			want: `{"workspace":"acme"}`,
		},
		{name: "explicitly empty values collapse to match-all", raw: `{"workspace":"","service":""}`, want: "{}"},
		{name: "unknown key rejected", raw: `{"team":"x"}`, wantErr: "unknown field"},
		{name: "unknown key alongside a known one rejected", raw: `{"workspace":"acme","team":"x"}`, wantErr: "unknown field"},
		{name: "non-string value rejected", raw: `{"workspace":123}`, wantErr: "cannot unmarshal"},
		{name: "array rejected", raw: `["workspace"]`, wantErr: "cannot unmarshal"},
		{name: "trailing content rejected", raw: `{"workspace":"a"}{"service":"b"}`, wantErr: "trailing content"},
		{name: "malformed json rejected", raw: `{"workspace":`, wantErr: "unexpected EOF"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeSelectorsJSON(tc.raw)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("NormalizeSelectorsJSON(%q) err = %v, want containing %q", tc.raw, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeSelectorsJSON(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("NormalizeSelectorsJSON(%q) = %q, want %q", tc.raw, got, tc.want)
			}
			// Idempotence: the canonical form must normalize to itself, or the
			// agent's byte-identity gate would reject the signer's own output.
			again, err := NormalizeSelectorsJSON(got)
			if err != nil || again != got {
				t.Fatalf("normalize not idempotent: %q -> %q (err=%v)", got, again, err)
			}
		})
	}
}

// TestValidateCanonicalSelectorsJSON pins the AGENT-side closed-envelope
// gate: only the exact canonical byte sequence passes. This is design §6
// case 9 — a semantically-equal but non-canonical spelling is a violation.
func TestValidateCanonicalSelectorsJSON(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    Selectors
		wantErr string
	}{
		{name: "canonical match-all", raw: "{}"},
		{name: "canonical single key", raw: `{"environment":"prod"}`, want: Selectors{Environment: "prod"}},
		{
			name: "canonical three keys",
			raw:  `{"environment":"prod","service":"api","workspace":"acme"}`,
			want: Selectors{Workspace: "acme", Environment: "prod", Service: "api"},
		},
		{name: "empty string is not canonical", raw: "", wantErr: "not canonical"},
		{name: "unsorted keys", raw: `{"workspace":"acme","environment":"prod"}`, wantErr: "not canonical"},
		{name: "padded document", raw: ` {"environment":"prod"} `, wantErr: "not canonical"},
		{name: "padded value", raw: `{"environment":" prod "}`, wantErr: "not canonical"},
		{name: "spaced separators", raw: `{"environment": "prod"}`, wantErr: "not canonical"},
		{name: "explicit empty value", raw: `{"environment":""}`, wantErr: "not canonical"},
		{name: "unknown key", raw: `{"team":"x"}`, wantErr: "unknown field"},
		{
			name:    "oversize",
			raw:     `{"workspace":"` + strings.Repeat("a", MaxPolicyResourceSelectorsBytes) + `"}`,
			wantErr: "over the 1024-byte maximum",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateCanonicalSelectorsJSON(tc.raw)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ValidateCanonicalSelectorsJSON(%q) err = %v, want containing %q", tc.raw, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateCanonicalSelectorsJSON(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("selectors = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestSelectorsIsEmpty pins the match-all predicate.
func TestSelectorsIsEmpty(t *testing.T) {
	if !(Selectors{}).IsEmpty() {
		t.Error("zero Selectors must be empty")
	}
	if (Selectors{Service: "api"}).IsEmpty() {
		t.Error("a set key must not report empty")
	}
}

// TestCorroborateSelectors is the three-valued targeting table (design §2
// step 2): configured-and-equal corroborates, configured-and-different
// mismatches, unconfigured is uncorroborated but never blocking.
func TestCorroborateSelectors(t *testing.T) {
	cases := []struct {
		name              string
		sel               Selectors
		attrs             Selectors
		wantMismatched    []string
		wantUncorroborate []string
	}{
		{
			name:  "match-all predicate corroborates trivially",
			sel:   Selectors{},
			attrs: Selectors{Workspace: "acme", Environment: "prod", Service: "api"},
		},
		{
			name:  "exact match on every set key",
			sel:   Selectors{Environment: "prod", Workspace: "acme"},
			attrs: Selectors{Workspace: "acme", Environment: "prod", Service: "api"},
		},
		{
			name:           "configured attribute contradicts",
			sel:            Selectors{Environment: "prod"},
			attrs:          Selectors{Environment: "dev"},
			wantMismatched: []string{"environment"},
		},
		{
			name:           "several contradictions come back sorted",
			sel:            Selectors{Workspace: "acme", Environment: "prod", Service: "api"},
			attrs:          Selectors{Workspace: "other", Environment: "dev", Service: "web"},
			wantMismatched: []string{"environment", "service", "workspace"},
		},
		{
			name:              "no attributes configured at all is uncorroborated, not a mismatch",
			sel:               Selectors{Environment: "prod", Service: "api"},
			attrs:             Selectors{},
			wantUncorroborate: []string{"environment", "service"},
		},
		{
			name:              "mixed: one contradiction, one unknown",
			sel:               Selectors{Environment: "prod", Service: "api"},
			attrs:             Selectors{Environment: "dev"},
			wantMismatched:    []string{"environment"},
			wantUncorroborate: []string{"service"},
		},
		{
			name:  "extra node attributes the predicate ignores are irrelevant",
			sel:   Selectors{Environment: "prod"},
			attrs: Selectors{Environment: "prod", Workspace: "anything", Service: "anything"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mismatched, uncorroborated := CorroborateSelectors(tc.sel, tc.attrs)
			if strings.Join(mismatched, ",") != strings.Join(tc.wantMismatched, ",") {
				t.Errorf("mismatched = %v, want %v", mismatched, tc.wantMismatched)
			}
			if strings.Join(uncorroborated, ",") != strings.Join(tc.wantUncorroborate, ",") {
				t.Errorf("uncorroborated = %v, want %v", uncorroborated, tc.wantUncorroborate)
			}
		})
	}
}

// TestReasonSelectorMismatchIsDistinct pins that the new closed-enum reason
// does not collide with (or get folded into) capability_mismatch — the
// design explicitly rejected reusing that code because it corrupts auto-halt
// diagnostics.
func TestReasonSelectorMismatchIsDistinct(t *testing.T) {
	if ReasonSelectorMismatch != "selector_mismatch" {
		t.Fatalf("ReasonSelectorMismatch = %q, want selector_mismatch", ReasonSelectorMismatch)
	}
	if ReasonSelectorMismatch == ReasonCapabilityMismatch {
		t.Fatal("selector_mismatch must be distinct from capability_mismatch")
	}
}
