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
		// Env-var style env vars — should still redact via the env-var
		// pattern even though the key allowlist no longer matches bare
		// `*_KEY`. The `KEY` substring in the env var name triggers the
		// `(?i)(export\s+\w*(?:SECRET|KEY|...)\w*)` pattern.
		{"export_session_key", "export MY_SESSION_KEY=somesessionvalue123", []string{"somesessionvalue123"}},
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

// TestScrubPreservesNonSecretCacheLookupKeys pins the regression where
// codex's `prompt_cache_key` was being redacted because the broad
// `*_key` pattern matched it. Redacting this field would break the
// OpenAI prefix-cache hit on every codex turn — significantly worse
// than the hypothetical leak risk of an unredacted UUID.
//
// The same logic applies to other identifier-style `*_key` fields that
// ride alongside real secrets in the same JSON envelope (idempotency
// keys, partition keys, etc.).
func TestScrubPreservesNonSecretCacheLookupKeys(t *testing.T) {
	t.Parallel()
	s := New()
	cases := []struct {
		name string
		in   string
		want string // exact value that MUST survive in the scrubbed output.
	}{
		{
			"codex_prompt_cache_key_uuid",
			`{"prompt_cache_key":"019e05fc-dfe7-77a1-8db0-c7d13f8be248"}`,
			"019e05fc-dfe7-77a1-8db0-c7d13f8be248",
		},
		{
			"idempotency_key",
			`{"idempotency_key":"abc123-def456"}`,
			"abc123-def456",
		},
		{
			"partition_key",
			`{"partition_key":"customer-123"}`,
			"customer-123",
		},
		// Negative side: real secret-shaped keys still redact.
		{
			"private_key_redacted",
			`{"PRIVATE_KEY":"-----BEGIN RSA-----abcdef"}`,
			"", // blank means: must be redacted
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := s.String(tc.in)
			if tc.want == "" {
				if !strings.Contains(got, Redacted) {
					t.Fatalf("expected redaction for %s; got %q", tc.name, got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("expected non-secret value %q to survive scrubbing; got %q", tc.want, got)
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

// TestScrubForwardKeepsJSONValid is the regression for the conversation-
// compression 400 "unexpected character" incident: the raw String()
// scrubber, run over compact JSON, lets the generic `token:<value>`
// pattern's \S+ devour every structural byte to EOF. ScrubForward must
// keep the body valid JSON while still redacting.
func TestScrubForwardKeepsJSONValid(t *testing.T) {
	t.Parallel()
	s := New()
	cases := []struct {
		name string
		in   string
	}{
		{
			// compact body, "token:" inside a tool_result string followed by
			// a long no-whitespace run + more structural JSON after it.
			name: "compact token in tool_result",
			in:   `{"model":"claude-sonnet-4","messages":[{"role":"user","content":[{"type":"tool_result","content":"auth token:abc123def456ghi789"}]},{"role":"assistant","content":"ok"}]}`,
		},
		{
			name: "secret colon value mid-body",
			in:   `{"a":"see secret:hunter2longvalue","b":"trailing structural data here"}`,
		},
		{
			name: "api_key colon value",
			in:   `{"x":"api_key:sk_live_0123456789abcdef","y":1,"z":[true,false]}`,
		},
		{
			// "key":"value with an escaped quote" — the [^"]* sibling bug.
			name: "escaped quote in sensitive value",
			in:   `{"token":"abc\"def","next":"survives"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !json.Valid([]byte(tc.in)) {
				t.Fatalf("precondition: input not valid JSON")
			}
			out := s.ScrubForward([]byte(tc.in))
			if !json.Valid(out) {
				t.Fatalf("ScrubForward produced INVALID JSON:\n%s", out)
			}
			for _, leak := range []string{"abc123def456ghi789", "hunter2longvalue", "sk_live_0123456789abcdef"} {
				if strings.Contains(string(out), leak) {
					t.Errorf("secret %q leaked through ScrubForward:\n%s", leak, out)
				}
			}
		})
	}
}

// TestScrubForwardCleanBodyIsByteIdentical pins the cache-preserving fast
// path: a body with no secrets must come back unchanged (same bytes) so
// the proxy's prefix-cache byte stability holds.
func TestScrubForwardCleanBodyIsByteIdentical(t *testing.T) {
	t.Parallel()
	s := New()
	clean := []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"refactor the parser please"}]}`)
	out := s.ScrubForward(clean)
	if string(out) != string(clean) {
		t.Fatalf("clean body mutated:\n got %s\nwant %s", out, clean)
	}
}

// TestScrubForwardRedactsRealSecretField confirms ScrubForward still
// redacts a secret carried in a normal JSON field (no over-relaxation).
func TestScrubForwardRedactsRealSecretField(t *testing.T) {
	t.Parallel()
	s := New()
	in := []byte(`{"config":{"api_key":"sk-abcdef0123456789abcdef"},"ok":true}`)
	out := s.ScrubForward(in)
	if !json.Valid(out) {
		t.Fatalf("invalid JSON: %s", out)
	}
	if strings.Contains(string(out), "sk-abcdef0123456789abcdef") {
		t.Errorf("api_key not redacted: %s", out)
	}
	if !strings.Contains(string(out), Redacted) {
		t.Errorf("expected %s marker: %s", Redacted, out)
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
