package scrub

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestScrubString(t *testing.T) {
	t.Parallel()
	s := New()
	cases := []struct {
		name string
		in   string
		want []string // substrings that MUST NOT appear in the scrubbed output.
	}{
		{"bearer", "curl -H 'Authorization: Bearer sk-ant-abc123def456ghi789'", []string{"sk-ant-abc123def456ghi789"}},
		{"aws", "export AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE", []string{"AKIAIOSFODNN7EXAMPLE"}},
		{"github_token", "export GITHUB_TOKEN=ghp_aaaabbbbccccddddeeeeffffgggghhhhiiii", []string{"ghp_aaaabbbbccccddddeeeeffffgggghhhhiiii"}},
		{"password_kv", "password=hunter2", []string{"hunter2"}},
		{"api_key_colon", "api_key: sk_DEMO_FAKEFIXTUREVALUE000111", []string{"sk_DEMO_FAKEFIXTUREVALUE000111"}},
		{"connection_string", "postgres://alice:superSecret123@db.internal:5432/app", []string{"superSecret123"}},
		{"secret_key_in_json", `{"SECRET_KEY":"topsecrethunter12345"}`, []string{"topsecrethunter12345"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := s.String(tc.in)
			for _, forbidden := range tc.want {
				if strings.Contains(got, forbidden) {
					t.Fatalf("secret leaked: %q still contains %q", got, forbidden)
				}
			}
			if !strings.Contains(got, Redacted) && !strings.Contains(got, "[REDACTED]") {
				t.Fatalf("expected redaction marker, got %q", got)
			}
		})
	}
}

func TestScrubFixtureFile(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile("../../testdata/scrub/sensitive-commands.txt")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	s := New()
	got := s.String(string(body))

	// None of the raw secret values should appear in the output.
	forbidden := []string{
		"sk-ant-abc123DEF456GHI789jkl012MNO345pqr",
		"AKIAIOSFODNN7EXAMPLE",
		"ghp_aaaabbbbccccddddeeeeffffgggghhhhiiii",
		"hunter2",
		"sk_DEMO_FAKEFIXTUREVALUE000111",
		"superSecret123",
		"pk_test_abcdefghij1234567890",
		"rootpass",
	}
	for _, f := range forbidden {
		if strings.Contains(got, f) {
			t.Errorf("fixture secret leaked: %q", f)
		}
	}
}

func TestScrubJSONDeepTraversal(t *testing.T) {
	t.Parallel()
	s := New()
	raw := `{
		"command": "curl -H 'Authorization: Bearer sk-abc123defXYZ0000111122223333'",
		"env": {
			"GITHUB_TOKEN": "ghp_secretlong0123456789abcdef000011112222",
			"nested": [
				"password=topsecret99",
				{"api_key": "pk_live_aaaaaaaaaaaa000011112222"}
			]
		},
		"benign": "hello world"
	}`
	out := s.RawJSON([]byte(raw))

	forbidden := []string{
		"sk-abc123defXYZ0000111122223333",
		"ghp_secretlong0123456789abcdef000011112222",
		"topsecret99",
		"pk_live_aaaaaaaaaaaa000011112222",
	}
	for _, f := range forbidden {
		if strings.Contains(out, f) {
			t.Errorf("secret leaked via JSON traversal: %q", f)
		}
	}
	if !strings.Contains(out, "hello world") {
		t.Error("benign content was dropped")
	}
	// Output must still be valid JSON.
	var check any
	if err := json.Unmarshal([]byte(out), &check); err != nil {
		t.Errorf("scrubbed JSON is invalid: %v\n%s", err, out)
	}
}

func TestScrubRawJSONFallsBackForInvalidJSON(t *testing.T) {
	t.Parallel()
	s := New()
	raw := []byte("not json but has Bearer sk-leaked-token-AAAAABBBBBCCCCCDDDDD inside")
	out := s.RawJSON(raw)
	if strings.Contains(out, "sk-leaked-token-AAAAABBBBBCCCCCDDDDD") {
		t.Errorf("invalid-JSON fallback didn't scrub: %q", out)
	}
}

func TestTruncate(t *testing.T) {
	t.Parallel()
	in := strings.Repeat("x", MaxRawInputBytes*2)
	got := Truncate(in)
	if len(got) > MaxRawInputBytes {
		t.Errorf("truncate exceeded max: len=%d", len(got))
	}
	if !strings.HasSuffix(got, "[truncated]") {
		t.Errorf("missing truncation marker: %q", got[len(got)-20:])
	}
	short := "abc"
	if Truncate(short) != short {
		t.Errorf("short string was modified")
	}
}

func TestExtraPatterns(t *testing.T) {
	t.Parallel()
	s := NewWithExtra([]string{`XYZ-\d{4}`})
	out := s.String("internal ref XYZ-1234 here")
	if strings.Contains(out, "XYZ-1234") {
		t.Errorf("extra pattern not applied: %q", out)
	}
}

func TestValidatePatternsCatchesBadRegex(t *testing.T) {
	t.Parallel()
	if err := ValidatePatterns([]string{"[invalid"}); err == nil {
		t.Fatal("expected error for invalid regex")
	}
	if err := ValidatePatterns([]string{`\d+`}); err != nil {
		t.Fatalf("unexpected error for valid regex: %v", err)
	}
}
