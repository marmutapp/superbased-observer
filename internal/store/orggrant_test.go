package store

import (
	"context"
	"testing"
	"time"
)

func testGrantRow() EnrolmentGrant {
	return EnrolmentGrant{
		OrgKey:       "org-key-1",
		Generation:   3,
		OrgID:        "org-1",
		OrgName:      "Acme",
		OrgServerURL: "https://org.example.com",
		KeyPinSHA256: "abc123",
		Authority:    []string{"dashboard.visibility"},
		ConsentMode:  "interactive",
		ConsentActor: "dev@example.com",
		GrantedAt:    time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC),
		ExpiresAt:    time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC),
		Signature:    "sig",
		ReceiptHash:  "rh",
	}
}

func TestEnrolmentGrantRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newPolicyResourceTestStore(t)

	if _, ok, err := s.LoadEnrolmentGrant(ctx, "org-key-1"); err != nil || ok {
		t.Fatalf("fresh DB: ok=%v err=%v, want a grant-free node", ok, err)
	}

	want := testGrantRow()
	if err := s.WriteEnrolmentGrant(ctx, want); err != nil {
		t.Fatalf("WriteEnrolmentGrant: %v", err)
	}
	got, ok, err := s.LoadEnrolmentGrant(ctx, "org-key-1")
	if err != nil || !ok {
		t.Fatalf("LoadEnrolmentGrant: ok=%v err=%v", ok, err)
	}
	if got.Generation != want.Generation || got.OrgName != want.OrgName ||
		got.KeyPinSHA256 != want.KeyPinSHA256 || got.ConsentMode != want.ConsentMode ||
		got.ConsentActor != want.ConsentActor || got.Signature != want.Signature {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", got, want)
	}
	if len(got.Authority) != 1 || got.Authority[0] != "dashboard.visibility" {
		t.Fatalf("Authority = %v", got.Authority)
	}
	if !got.GrantedAt.Equal(want.GrantedAt) || !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Fatalf("times = %v / %v, want %v / %v", got.GrantedAt, got.ExpiresAt, want.GrantedAt, want.ExpiresAt)
	}
}

// TestEnrolmentGrantReplacedNotMerged pins the "no partial update" rule: a
// re-enrolment writes a WHOLE new grant, so a narrower second grant can never
// inherit the wider first one's authority.
func TestEnrolmentGrantReplacedNotMerged(t *testing.T) {
	ctx := context.Background()
	s := newPolicyResourceTestStore(t)
	wide := testGrantRow()
	// capture.raise is RETIRED in Phase 1b (it grants nothing), which is
	// exactly why it still serves here: this test is about the WIDTH of the
	// stored token list, not about what any token authorises.
	wide.Authority = []string{"dashboard.visibility", "capture.raise"}
	if err := s.WriteEnrolmentGrant(ctx, wide); err != nil {
		t.Fatalf("write wide: %v", err)
	}
	narrow := testGrantRow()
	narrow.Authority = []string{"dashboard.visibility"}
	narrow.Generation = 4
	if err := s.WriteEnrolmentGrant(ctx, narrow); err != nil {
		t.Fatalf("write narrow: %v", err)
	}
	got, ok, err := s.LoadEnrolmentGrant(ctx, "org-key-1")
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	if len(got.Authority) != 1 {
		t.Fatalf("Authority = %v, want the replacing grant's single token (authority must never accumulate)", got.Authority)
	}
	if got.Generation != 4 {
		t.Fatalf("Generation = %d, want 4", got.Generation)
	}
}

// TestDeleteEnrolmentGrant pins the revocation half: `observer unenroll`
// leaves nothing behind that could govern the machine, and running it twice
// is not an error.
func TestDeleteEnrolmentGrant(t *testing.T) {
	ctx := context.Background()
	s := newPolicyResourceTestStore(t)
	if err := s.WriteEnrolmentGrant(ctx, testGrantRow()); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := s.DeleteEnrolmentGrant(ctx, "org-key-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok, err := s.LoadEnrolmentGrant(ctx, "org-key-1"); err != nil || ok {
		t.Fatalf("after delete: ok=%v err=%v, want gone", ok, err)
	}
	if err := s.DeleteEnrolmentGrant(ctx, "org-key-1"); err != nil {
		t.Fatalf("second delete must be idempotent: %v", err)
	}

	// DeleteAll is the belt-and-braces path for an unenrol that can no
	// longer derive its org_key.
	if err := s.WriteEnrolmentGrant(ctx, testGrantRow()); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if err := s.DeleteAllEnrolmentGrants(ctx); err != nil {
		t.Fatalf("delete all: %v", err)
	}
	if _, ok, _ := s.LoadEnrolmentGrant(ctx, "org-key-1"); ok {
		t.Fatal("DeleteAllEnrolmentGrants left a grant behind")
	}
}

// TestWriteEnrolmentGrantRequiresOrgKey pins the one hard precondition: a
// grant with no identity could never be invalidated by the generation fence.
func TestWriteEnrolmentGrantRequiresOrgKey(t *testing.T) {
	ctx := context.Background()
	s := newPolicyResourceTestStore(t)
	g := testGrantRow()
	g.OrgKey = ""
	if err := s.WriteEnrolmentGrant(ctx, g); err == nil {
		t.Fatal("WriteEnrolmentGrant accepted a grant with no org_key")
	}
}
