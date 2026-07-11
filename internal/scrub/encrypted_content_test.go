package scrub

import (
	"strings"
	"testing"
)

// TestScrubPreservesEncryptedContentValues pins the encrypted_content
// shield: the OpenAI Responses API ZDR-mode reasoning blob (Fernet-
// encrypted base64, hundreds of chars) must round-trip through the
// scrubber byte-identically. Without this, codex's chatgpt-auth turns
// fail with `invalid_encrypted_content` on the second-and-later turn
// of any reasoning chain — Fernet body bytes look like base64, and a
// long base64 string contains substrings matching `ghp_<chars>`,
// `(?i)sk[_-]<chars>`, `AKIA<chars>` etc. roughly every fourth token.
// Surfaced 2026-05-08 dogfood; reproduced via inputs that contain the
// exact substring patterns.
func TestScrubPreservesEncryptedContentValues(t *testing.T) {
	tokens := []string{
		// Realistic shape from the user's error message.
		`gAAAAABoyrlqRtNPLrOXG72LPwWyGeIqBLJpvBfaYPwfBYAyZAtoBZ3ZBxSGgEXjcS-XdJtkLfMUpyiC0Ms7uKFKDJUxgEX9MuyHvQetSeIcGQOlSjsHaP9-zT5oZQA-WyhQePHZGpe9Mn3RZJBl_5KhzrwzIm2WP4yJZpV8Iw==`,
		// Synthetic shapes that hit each global secret pattern: sk_,
		// AKIA<16 caps>, ghp_, plus regular base64 chars. All MUST
		// pass through unchanged because they're inside encrypted_content.
		`gAAAAABoyrlSk_someRandomTextAKIA1234567890ABCDEFAK_endRw==`,
		`gAAAAABxxxsk-something-here_padding_padding_padding_endRw==`,
		`gAAAAABxxxghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaRw==`,
	}
	s := New()
	for i, tok := range tokens {
		body := `{"type":"reasoning","encrypted_content":"` + tok + `"}`
		out := s.String(body)
		if !strings.Contains(out, tok) {
			t.Errorf("token %d: scrubber redacted bytes inside encrypted_content\n  in:  %s\n  out: %s",
				i+1, body, out)
		}
	}
}

// TestScrubStillRedactsSecretsOutsideShield verifies that the shield
// only protects values of `encrypted_content` keys — secrets in
// surrounding text (or any other JSON field) are still redacted.
// Without this, the shield could mask a leak by accident.
func TestScrubStillRedactsSecretsOutsideShield(t *testing.T) {
	body := `{"type":"reasoning","encrypted_content":"gAAAghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaa==","authorization":"Bearer ghp_realtokeninlogs1234567890abcd"}`
	out := New().String(body)
	// Inside encrypted_content: untouched.
	if !strings.Contains(out, `"encrypted_content":"gAAAghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaa=="`) {
		t.Errorf("encrypted_content was modified: %s", out)
	}
	// Outside encrypted_content: Bearer token redacted.
	if strings.Contains(out, "ghp_realtokeninlogs1234567890abcd") {
		t.Errorf("real Bearer token NOT redacted; output may leak: %s", out)
	}
	if !strings.Contains(out, Redacted) {
		t.Errorf("expected at least one [REDACTED] in output, got: %s", out)
	}
}

// TestScrubHandlesMultipleEncryptedContentFields verifies the shield
// works when a body has several reasoning items, each with its own
// encrypted_content. Codex sessions accumulate one per agent loop turn,
// so multi-shield is the common case once a session has run for a
// while.
func TestScrubHandlesMultipleEncryptedContentFields(t *testing.T) {
	body := `{"input":[` +
		`{"type":"reasoning","encrypted_content":"gAAAghp_firstaaaaaaaaaaaaaaaaaaaa=="},` +
		`{"type":"reasoning","encrypted_content":"gAAAsk_secondbbbbbbbbbbbbbbbbbbbb=="},` +
		`{"type":"reasoning","encrypted_content":"gAAAAKIA1234567890ABCDEFcccccc=="}` +
		`]}`
	out := New().String(body)
	for _, want := range []string{
		`"gAAAghp_firstaaaaaaaaaaaaaaaaaaaa=="`,
		`"gAAAsk_secondbbbbbbbbbbbbbbbbbbbb=="`,
		`"gAAAAKIA1234567890ABCDEFcccccc=="`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("multi-shield failed for value %s\n  out: %s", want, out)
		}
	}
}
