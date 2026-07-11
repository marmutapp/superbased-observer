package rollup

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// seedOTel inserts one otel_content row. A nil content pointer stores SQL NULL
// (the hash-only / metadata-only case).
func seedOTel(t *testing.T, d *sql.DB, sessID, user, kind, reqID, toolUse, hash, ts string, content *string) {
	t.Helper()
	var c any
	if content != nil {
		c = *content
	}
	if _, err := d.ExecContext(context.Background(),
		`INSERT INTO otel_content (content_hash, user_id, org_id, user_email, request_id, session_id, tool_use_id, kind, content, timestamp, pushed_at, pushed_by_user_id)
		 VALUES (?, ?, 'org-1', ?, ?, ?, ?, ?, ?, ?, '2026-05-26T11:00:00Z', ?)`,
		hash, user, user+"@acme.example", reqID, sessID, toolUse, kind, c, ts, user); err != nil {
		t.Fatalf("seed otel_content: %v", err)
	}
}

func strptr(s string) *string { return &s }

func TestSessionMessages_AdminContentAndOrder(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	// Two bodies for s-a1 (alice): a prompt with content, then a hash-only
	// tool_output (content NULL — node shipped metadata + hash but not the body).
	seedOTel(t, d, "s-a1", "u-alice", "prompt", "req-a1", "", "h1", "2026-05-20T10:00:00Z", strptr("the user prompt body"))
	seedOTel(t, d, "s-a1", "u-alice", "tool_output", "req-a1", "tu1", "h2", "2026-05-20T10:00:05Z", nil)

	got, found, err := SessionMessages(context.Background(), d, "s-a1", Scope{Admin: true}, "", fixedNow)
	if err != nil || !found {
		t.Fatalf("SessionMessages s-a1: found=%v err=%v", found, err)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(got.Messages))
	}
	// Chronological order: prompt first, tool_output second.
	if got.Messages[0].Kind != "prompt" || got.Messages[1].Kind != "tool_output" {
		t.Errorf("order = %s,%s, want prompt,tool_output", got.Messages[0].Kind, got.Messages[1].Kind)
	}
	if got.Messages[0].Content != "the user prompt body" {
		t.Errorf("prompt content = %q, want the body", got.Messages[0].Content)
	}
	if !got.ContentAvailable {
		t.Errorf("ContentAvailable = false, want true (the prompt carries a body)")
	}
	// Hash-only row: no body, but the hash is still surfaced.
	if got.Messages[1].Content != "" || got.Messages[1].ContentHash != "h2" {
		t.Errorf("hash-only row = content %q hash %q, want empty body / h2", got.Messages[1].Content, got.Messages[1].ContentHash)
	}
	// Identity resolved; project id is the hash, never a path.
	if got.Email != "alice@acme.example" {
		t.Errorf("email = %q, want alice", got.Email)
	}
	if got.ProjectID == "" || strings.Contains(got.ProjectID, "/") {
		t.Errorf("project_id = %q, want a non-empty hash", got.ProjectID)
	}
}

func TestSessionMessages_InScopeButNoContent(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	// s-c1 (carol) exists and is in scope, but has no otel_content at all.
	got, found, err := SessionMessages(context.Background(), d, "s-c1", Scope{Admin: true}, "", fixedNow)
	if err != nil || !found {
		t.Fatalf("SessionMessages s-c1: found=%v err=%v", found, err)
	}
	if len(got.Messages) != 0 || got.ContentAvailable {
		t.Errorf("s-c1 = %d msgs / available=%v, want 0 / false (in scope, no capture)", len(got.Messages), got.ContentAvailable)
	}
}

func TestSessionMessages_HashOnlyNotAvailable(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	// Only a hash-only row → found, one message, but ContentAvailable false.
	seedOTel(t, d, "s-a1", "u-alice", "prompt", "req-a1", "", "h9", "2026-05-20T10:00:00Z", nil)
	got, found, err := SessionMessages(context.Background(), d, "s-a1", Scope{Admin: true}, "", fixedNow)
	if err != nil || !found {
		t.Fatalf("SessionMessages s-a1: found=%v err=%v", found, err)
	}
	if len(got.Messages) != 1 || got.ContentAvailable {
		t.Errorf("hash-only = %d msgs / available=%v, want 1 / false", len(got.Messages), got.ContentAvailable)
	}
}

func TestSessionMessages_OutOfScope404(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	seedOTel(t, d, "s-b1", "u-bob", "prompt", "req-b1", "", "hb", "2026-05-21T10:00:00Z", strptr("bob private prompt"))

	// Lead alice (team-a = {alice, carol}) cannot reach bob's s-b1 → 404.
	_, found, err := SessionMessages(context.Background(), d, "s-b1", Scope{TeamIDs: []string{"team-a"}}, "u-alice", fixedNow)
	if err != nil {
		t.Fatalf("SessionMessages s-b1: %v", err)
	}
	if found {
		t.Errorf("lead alice resolved out-of-scope s-b1 messages (must be 404)")
	}

	// Member-self: bob reaches his own s-b1.
	got, found, err := SessionMessages(context.Background(), d, "s-b1", Scope{}, "u-bob", fixedNow)
	if err != nil || !found {
		t.Fatalf("member-self s-b1: found=%v err=%v", found, err)
	}
	if len(got.Messages) != 1 || got.Messages[0].Content != "bob private prompt" {
		t.Errorf("member-self bob = %+v, want his own prompt body", got.Messages)
	}
}

// TestSessionMessages_NoRawPath is the privacy guard: identity (email) is
// audited-and-present, but the raw project_root path must never appear — project
// identity is the hash only.
func TestSessionMessages_NoRawPath(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	seedOTel(t, d, "s-a1", "u-alice", "prompt", "req-a1", "", "h1", "2026-05-20T10:00:00Z", strptr("body"))
	got, _, err := SessionMessages(context.Background(), d, "s-a1", Scope{Admin: true}, "", fixedNow)
	if err != nil {
		t.Fatalf("SessionMessages: %v", err)
	}
	s := string(mustJSON(t, got))
	if strings.Contains(s, "/repo/") {
		t.Errorf("messages JSON leaked a raw project_root path:\n%s", s)
	}
	if strings.Contains(s, "git_remote") || strings.Contains(s, "pushed_by") {
		t.Errorf("messages JSON leaked a forbidden field:\n%s", s)
	}
}
