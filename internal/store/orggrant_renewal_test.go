package store

import (
	"context"
	"testing"
	"time"
)

// TestFreshGrantHasSignedWindow is review M1's first silent failure: without
// signed_expires_at in WriteEnrolmentGrant's own column list, migration 083's
// NOT NULL DEFAULT ” leaves it empty on every FRESH enrolment,
// parseGrantTime(”) yields the zero time, the derived TTL
// (signed_expires_at - granted_at) is a large NEGATIVE duration, and renewal
// silently never fires for any node enrolled after Phase 1b ships. The 083
// backfill only rescues rows that already existed.
func TestFreshGrantHasSignedWindow(t *testing.T) {
	ctx := context.Background()
	s := newPolicyResourceTestStore(t)
	row := testGrantRow()
	if err := s.WriteEnrolmentGrant(ctx, row); err != nil {
		t.Fatalf("WriteEnrolmentGrant: %v", err)
	}
	got, ok, err := s.LoadEnrolmentGrant(ctx, row.OrgKey)
	if err != nil || !ok {
		t.Fatalf("LoadEnrolmentGrant: ok=%v err=%v", ok, err)
	}
	if got.SignedExpiresAt.IsZero() {
		t.Fatal("a fresh grant has no signed window — the derived renewal TTL would be negative and renewal would never fire")
	}
	if !got.SignedExpiresAt.Equal(row.ExpiresAt) {
		t.Fatalf("SignedExpiresAt = %v, want the grant's own expiry %v", got.SignedExpiresAt, row.ExpiresAt)
	}
	if ttl := got.SignedExpiresAt.Sub(got.GrantedAt); ttl <= 0 {
		t.Fatalf("derived TTL = %v, want positive", ttl)
	}
}

// TestReEnrolmentResetsSignedWindow is review M1's second silent failure:
// ON CONFLICT DO UPDATE must list the new columns, or the PREVIOUS grant's
// signed window survives and the new grant's TTL is derived from the OLD
// organization authorization's window.
func TestReEnrolmentResetsSignedWindow(t *testing.T) {
	ctx := context.Background()
	s := newPolicyResourceTestStore(t)

	first := testGrantRow()
	if err := s.WriteEnrolmentGrant(ctx, first); err != nil {
		t.Fatalf("write first: %v", err)
	}
	second := testGrantRow()
	second.Generation = 4
	second.GrantedAt = first.GrantedAt.Add(48 * time.Hour)
	second.ExpiresAt = second.GrantedAt.Add(7 * 24 * time.Hour)
	if err := s.WriteEnrolmentGrant(ctx, second); err != nil {
		t.Fatalf("write second: %v", err)
	}
	got, _, err := s.LoadEnrolmentGrant(ctx, second.OrgKey)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !got.SignedExpiresAt.Equal(second.ExpiresAt) {
		t.Fatalf("SignedExpiresAt = %v, want the NEW grant's %v — the old authorization's window survived a re-enrolment",
			got.SignedExpiresAt, second.ExpiresAt)
	}
	if ttl := got.SignedExpiresAt.Sub(got.GrantedAt); ttl != 7*24*time.Hour {
		t.Fatalf("derived TTL = %v, want the new grant's 7 days", ttl)
	}
}

// TestRenewalMonotonic: the guarded UPDATE can only move the clock FORWARD,
// so a clock skew or a replayed renewal signal can never shorten a grant
// either.
func TestRenewalMonotonic(t *testing.T) {
	ctx := context.Background()
	s := newPolicyResourceTestStore(t)
	row := testGrantRow()
	if err := s.WriteEnrolmentGrant(ctx, row); err != nil {
		t.Fatalf("write: %v", err)
	}
	backwards := row.ExpiresAt.Add(-24 * time.Hour)
	if err := s.RenewEnrolmentGrant(ctx, row.OrgKey, row.Generation, backwards); err != nil {
		t.Fatalf("RenewEnrolmentGrant: %v", err)
	}
	got, _, err := s.LoadEnrolmentGrant(ctx, row.OrgKey)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !got.ExpiresAt.Equal(row.ExpiresAt) {
		t.Fatalf("a backward renewal shortened the grant: %v", got.ExpiresAt)
	}

	forward := row.ExpiresAt.Add(24 * time.Hour)
	if err := s.RenewEnrolmentGrant(ctx, row.OrgKey, row.Generation, forward); err != nil {
		t.Fatalf("RenewEnrolmentGrant: %v", err)
	}
	got, _, err = s.LoadEnrolmentGrant(ctx, row.OrgKey)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !got.ExpiresAt.Equal(forward) {
		t.Fatalf("ExpiresAt = %v, want %v", got.ExpiresAt, forward)
	}
	if got.LastRenewedAt.IsZero() {
		t.Fatal("a renewal did not stamp last_renewed_at")
	}
	// The evidence property (amendment A1): the SIGNED window is untouched,
	// so the stored signature still verifies against its own row.
	if !got.SignedExpiresAt.Equal(row.ExpiresAt) {
		t.Fatalf("renewal moved the SIGNED window to %v — the signature no longer describes the row", got.SignedExpiresAt)
	}
}

// TestRenewalIsGenerationScoped: a renewal signal that arrives after a
// re-enrol must not touch the new grant.
func TestRenewalIsGenerationScoped(t *testing.T) {
	ctx := context.Background()
	s := newPolicyResourceTestStore(t)
	row := testGrantRow()
	if err := s.WriteEnrolmentGrant(ctx, row); err != nil {
		t.Fatalf("write: %v", err)
	}
	stale := row.ExpiresAt.Add(365 * 24 * time.Hour)
	if err := s.RenewEnrolmentGrant(ctx, row.OrgKey, row.Generation+1, stale); err != nil {
		t.Fatalf("RenewEnrolmentGrant: %v", err)
	}
	got, _, err := s.LoadEnrolmentGrant(ctx, row.OrgKey)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !got.ExpiresAt.Equal(row.ExpiresAt) {
		t.Fatalf("a renewal for another generation moved this grant's clock to %v", got.ExpiresAt)
	}
}

// TestRenewalRefusesAZeroExpiry: a non-expiring grant is the one shape that
// approaches irrevocable-from-the-org's-perspective, so it can never be
// produced by a renewal.
func TestRenewalRefusesAZeroExpiry(t *testing.T) {
	ctx := context.Background()
	s := newPolicyResourceTestStore(t)
	if err := s.RenewEnrolmentGrant(ctx, "org-key-1", 3, time.Time{}); err == nil {
		t.Fatal("a zero expiry was accepted")
	}
	if err := s.RenewEnrolmentGrant(ctx, "", 3, time.Now()); err == nil {
		t.Fatal("an empty org_key was accepted")
	}
}
