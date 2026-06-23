package scrub

import (
	"strings"
	"testing"
)

// TestDetectSecrets_PerDetectorRow gives every typed detector row a
// hit + near-miss pair (§18: one case per table row minimum).
func TestDetectSecrets_PerDetectorRow(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		in       string
		wantType string // "" = expect NO findings
		certain  bool
	}{
		{
			name:     "github_pat hit",
			in:       "pushed with ghp_AbCdEfGhIjKlMnOpQrStUvWx1234",
			wantType: "github_pat", certain: true,
		},
		{
			name: "github_pat near-miss: too short",
			in:   "ghp_short",
		},
		{
			name:     "bearer_token hit",
			in:       `Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.payload.sig`,
			wantType: "bearer_token", certain: true,
		},
		{
			name: "bearer_token near-miss: prose",
			in:   "the Bearer of bad news",
		},
		{
			name:     "api_key_prefixed hit",
			in:       "use sk-proj_AbCd1234EfGh5678IjKl",
			wantType: "api_key_prefixed", certain: true,
		},
		{
			name: "api_key_prefixed near-miss: short tail",
			in:   "ask-me-anything sk-short",
		},
		{
			name:     "api_key_named hit",
			in:       "api_key_AbCdEfGh1234567890XyZabc",
			wantType: "api_key_named", certain: true,
		},
		{
			name:     "aws_access_key hit",
			in:       "creds AKIAIOSFODNN7EXAMPLE here",
			wantType: "aws_access_key", certain: true,
		},
		{
			name: "aws_access_key near-miss: lowercase",
			in:   "akiaiosfodnn7example",
		},
		{
			name:     "private_key_block hit",
			in:       "-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA\n-----END RSA PRIVATE KEY-----",
			wantType: "private_key_block", certain: true,
		},
		{
			name:     "private_key_block hit: truncated paste",
			in:       "-----BEGIN PRIVATE KEY-----\nMIIEpAIBAAKCAQEA7c8",
			wantType: "private_key_block", certain: true,
		},
		{
			name:     "json_secret_value hit",
			in:       `{"password": "hunter22"}`,
			wantType: "json_secret_value", certain: true,
		},
		{
			name: "json_secret_value near-miss: usage tokens field",
			in:   `{"input_tokens": 4096, "output_tokens": 200, "max_tokens": 8192}`,
		},
		{
			name:     "env_secret_value hit",
			in:       `{"OPENAI_API_KEY": "sk-svcacct-AbCd1234EfGh5678IjKl"}`,
			wantType: "env_secret_value", certain: true,
		},
		{
			name:     "secret_assignment hit",
			in:       "password=correcthorsebatterystaple",
			wantType: "secret_assignment", certain: true,
		},
		{
			name: "secret_assignment near-miss: short prose value",
			in:   "token: yes",
		},
		{
			name:     "api_key_assignment hit",
			in:       "api-key: 0123456789abcdef",
			wantType: "api_key_assignment", certain: true,
		},
		{
			name: "export_secret hit",
			// A *_KEY var name: covered by the export row but NOT by
			// secret_assignment (whose class set has no bare "key") —
			// pins the export row's own coverage.
			in:       "export DEPLOY_KEY=abc123def456",
			wantType: "export_secret", certain: true,
		},
		{
			name: "export_secret near-miss: unrelated var",
			in:   "export EDITOR=vim",
		},
		{
			name:     "connection_string_password hit",
			in:       "postgres://admin:s3cr3tpw@db.internal:5432/app",
			wantType: "connection_string_password", certain: true,
		},
		{
			name: "connection_string_password near-miss: no credentials",
			in:   "https://docs.example.com/path",
		},
		{
			name: "entropy hit: high-entropy token near secret context",
			// "access_key" opens an entropy window but triggers no
			// assignment row (no '=' / ':' between word and token).
			in:       "# access_key for deploy\naB3dE5fG7hI9jK1LmN3oP5qR7sT9uV1wX3yZ5aB7cD9eF1g",
			wantType: "entropy", certain: false,
		},
		{
			name: "entropy near-miss: same token without context word",
			in:   `value = "aB3dE5fG7hI9jK1LmN3oP5qR7sT9uV1wX3yZ5aB7cD9eF1g"`,
		},
		{
			name: "entropy near-miss: hex digest near context (no uppercase)",
			in:   "auth log sha 3b8f2c9d4e5a6b7c8d9e0f1a2b3c4d5e6f708192a3b4c5d6",
		},
		{
			name: "clean body",
			in:   `{"model":"claude-opus-4-8","messages":[{"role":"user","content":"hello"}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := DetectSecrets(tc.in)
			if tc.wantType == "" {
				if len(got) != 0 {
					t.Fatalf("DetectSecrets(%q) = %+v, want none", tc.in, got)
				}
				return
			}
			found := false
			for _, f := range got {
				if f.Type == tc.wantType {
					found = true
					if f.Certain != tc.certain {
						t.Errorf("finding %s Certain = %v, want %v", f.Type, f.Certain, tc.certain)
					}
					if f.Value == "" {
						t.Errorf("finding %s has empty Value", f.Type)
					}
				}
			}
			if !found {
				t.Fatalf("DetectSecrets(%q) = %+v, want a %q finding", tc.in, got, tc.wantType)
			}
		})
	}
}

// TestDetectSecrets_FernetShielded pins the encrypted_content shield:
// a Fernet blob that happens to contain a secret-shaped substring must
// not be flagged, and masking must leave it byte-identical.
func TestDetectSecrets_FernetShielded(t *testing.T) {
	t.Parallel()
	body := `{"encrypted_content":"gAAAAABghp_AbCdEfGhIjKlMnOpQrStUvWx1234zzz","note":"x"}`
	if got := DetectSecrets(body); len(got) != 0 {
		t.Fatalf("DetectSecrets flagged inside shielded encrypted_content: %+v", got)
	}
	masked, _ := MaskSecrets(body, func(TypedFinding) bool { return true })
	if masked != body {
		t.Fatalf("MaskSecrets mutated a shielded body:\n got %q\nwant %q", masked, body)
	}
}

// TestMaskSecrets_Selective pins selective masking: only findings the
// predicate accepts are rewritten; the rest of the body (and rejected
// findings) survive byte-identical, and the output stays valid in
// place ("[REDACTED:type]" markers).
func TestMaskSecrets_Selective(t *testing.T) {
	t.Parallel()
	body := `{"key":"ghp_AbCdEfGhIjKlMnOpQrStUvWx1234","msg":"access_key aB3dE5fG7hI9jK1LmN3oP5qR7sT9uV1wX3yZ5aB7cD9eF1g"}`
	masked, findings := MaskSecrets(body, func(f TypedFinding) bool { return f.Certain })
	if !strings.Contains(masked, "[REDACTED:github_pat]") {
		t.Fatalf("certain finding not masked: %q", masked)
	}
	if strings.Contains(masked, "ghp_AbCdEfGhIjKlMnOpQrStUvWx1234") {
		t.Fatalf("secret survived masking: %q", masked)
	}
	if !strings.Contains(masked, "aB3dE5fG7hI9jK1LmN3oP5qR7sT9uV1wX3yZ5aB7cD9eF1g") {
		t.Fatalf("entropy (non-certain) finding was masked despite predicate: %q", masked)
	}
	var types []string
	for _, f := range findings {
		types = append(types, f.Type)
	}
	if len(findings) < 2 {
		t.Fatalf("findings = %v, want both github_pat and an entropy hit", types)
	}
}

// TestMaskSecrets_NilPredicate pins pure-detection mode: nil predicate
// returns the input unchanged.
func TestMaskSecrets_NilPredicate(t *testing.T) {
	t.Parallel()
	body := "token ghp_AbCdEfGhIjKlMnOpQrStUvWx1234"
	out, findings := MaskSecrets(body, nil)
	if out != body {
		t.Fatalf("nil-predicate MaskSecrets mutated input: %q", out)
	}
	if len(findings) == 0 {
		t.Fatal("nil-predicate MaskSecrets returned no findings")
	}
}

// TestMaskSecrets_OverlapDedup pins overlap resolution: when the
// entropy candidate covers the same bytes as a certain detector, only
// the certain finding survives (table order wins).
func TestMaskSecrets_OverlapDedup(t *testing.T) {
	t.Parallel()
	// A GitHub PAT sitting next to the word "secret" is both a
	// github_pat hit and an entropy-window candidate.
	body := "secret: ghp_AbCd1234EfGh5678IjKl9012MnOp"
	_, findings := MaskSecrets(body, nil)
	var pat, ent int
	for _, f := range findings {
		switch f.Type {
		case "github_pat":
			pat++
		case "entropy":
			ent++
		}
	}
	if pat != 1 {
		t.Fatalf("github_pat findings = %d, want 1 (findings %+v)", pat, findings)
	}
	if ent != 0 {
		t.Fatalf("entropy findings = %d, want 0 — overlap with github_pat must dedup", ent)
	}
}

// TestCertainSecretTypes pins the R-172 injection surface: certain
// types only, deduplicated, first-seen order.
func TestCertainSecretTypes(t *testing.T) {
	t.Parallel()
	in := "curl -H 'Authorization: Bearer eyJabc12345.def.ghi' -d token=ghp_AbCdEfGhIjKlMnOpQrStUvWx1234 -d t2=ghp_ZyXwVuTsRqPoNmLkJiHg5678"
	got := CertainSecretTypes(in)
	want := map[string]bool{"bearer_token": true, "github_pat": true}
	if len(got) < 2 {
		t.Fatalf("CertainSecretTypes = %v, want at least bearer_token + github_pat", got)
	}
	seen := map[string]bool{}
	for _, ty := range got {
		if seen[ty] {
			t.Fatalf("CertainSecretTypes returned duplicate %q: %v", ty, got)
		}
		seen[ty] = true
	}
	for w := range want {
		if !seen[w] {
			t.Fatalf("CertainSecretTypes = %v, missing %q", got, w)
		}
	}
}
