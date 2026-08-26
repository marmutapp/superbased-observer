package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestSelectGuardPinRows proves the org wire row is derived correctly from
// the node's guard_pins table (kind/name/client natural key, PinHash,
// FirstSeen/LastVerified, Status) and that PinKey is a stable, collision-
// free composite of the triple.
func TestSelectGuardPinRows(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()

	first := time.Now().UTC().Add(-48 * time.Hour)
	last := time.Now().UTC()
	if err := s.UpsertGuardPin(ctx, GuardPinRow{
		Kind: "mcp_server", Name: "observer", Client: "claude-code",
		PinHash: "deadbeef", FirstSeen: first, LastVerified: last, Status: "pinned",
	}); err != nil {
		t.Fatal(err)
	}
	// A second pin sharing (kind, name) but a DIFFERENT client must not
	// collide with the first — proves guardPinKey folds client into the key.
	if err := s.UpsertGuardPin(ctx, GuardPinRow{
		Kind: "mcp_server", Name: "observer", Client: "codex",
		PinHash: "cafef00d", FirstSeen: first, LastVerified: last, Status: "drifted",
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := s.SelectGuardPinRows(ctx)
	if err != nil {
		t.Fatalf("SelectGuardPinRows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(rows), rows)
	}

	byKey := map[string]int{}
	for _, r := range rows {
		byKey[r.PinKey]++
		if r.Kind != "mcp_server" || r.Name != "observer" {
			t.Errorf("unexpected kind/name: %+v", r)
		}
		if r.FirstSeen == "" || r.LastVerified == "" {
			t.Errorf("expected non-empty timestamps: %+v", r)
		}
	}
	if byKey["mcp_server:observer:claude-code"] != 1 {
		t.Errorf("missing claude-code pin key, got: %+v", rows)
	}
	if byKey["mcp_server:observer:codex"] != 1 {
		t.Errorf("missing codex pin key, got: %+v", rows)
	}

	// Re-upserting the same natural key (a re-sighting) must not duplicate
	// the row — pins are current-state, not events.
	if err := s.UpsertGuardPin(ctx, GuardPinRow{
		Kind: "mcp_server", Name: "observer", Client: "claude-code",
		PinHash: "deadbeef", FirstSeen: first, LastVerified: time.Now().UTC(), Status: "approved",
	}); err != nil {
		t.Fatal(err)
	}
	rows, err = s.SelectGuardPinRows(ctx)
	if err != nil {
		t.Fatalf("SelectGuardPinRows (after re-sighting): %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("re-sighting duplicated a row: got %d, want 2: %+v", len(rows), rows)
	}
	for _, r := range rows {
		if r.PinKey == "mcp_server:observer:claude-code" && r.Status != "approved" {
			t.Errorf("re-sighting should update status in place, got %q", r.Status)
		}
	}
}

// TestSelectGuardApprovalRows proves only currently-active approvals ship
// (an expired one is excluded, matching the node's own
// ActiveGuardApprovals), that ApprovalKey is stable per row, and that a
// non-expiring approval carries an empty ExpiresAt with Active=true.
func TestSelectGuardApprovalRows(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC()

	activeID, err := s.InsertGuardApproval(ctx, GuardApprovalRow{
		TS: now, RuleID: "dangerous-command", Scope: "session",
		SessionID: "sess-1", GrantedBy: "alice@example.com",
		ExpiresAt: now.Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	neverExpiresID, err := s.InsertGuardApproval(ctx, GuardApprovalRow{
		TS: now, RuleID: "project-write", Scope: "project",
		ProjectRootHash: "abc123hash", GrantedBy: "bob@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertGuardApproval(ctx, GuardApprovalRow{
		TS: now.Add(-72 * time.Hour), RuleID: "dangerous-command", Scope: "once",
		SessionID: "sess-old", GrantedBy: "carol@example.com",
		ExpiresAt: now.Add(-1 * time.Hour), // already expired
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := s.SelectGuardApprovalRows(ctx)
	if err != nil {
		t.Fatalf("SelectGuardApprovalRows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (expired one excluded): %+v", len(rows), rows)
	}

	byKey := map[string]struct{}{}
	for _, r := range rows {
		byKey[r.ApprovalKey] = struct{}{}
		if !r.Active {
			t.Errorf("expected every shipped row to be Active: %+v", r)
		}
		switch r.ApprovalKey {
		case fmt.Sprintf("%d", activeID):
			if r.SessionID != "sess-1" || r.ExpiresAt == "" {
				t.Errorf("active-with-expiry row wrong shape: %+v", r)
			}
		case fmt.Sprintf("%d", neverExpiresID):
			if r.ProjectRootHash != "abc123hash" || r.ExpiresAt != "" {
				t.Errorf("never-expires row wrong shape: %+v", r)
			}
		default:
			t.Errorf("unexpected approval key %q", r.ApprovalKey)
		}
	}
	if len(byKey) != 2 {
		t.Errorf("approval keys not unique: %+v", rows)
	}
}
