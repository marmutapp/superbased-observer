package store

import (
	"context"
	"testing"
	"time"
)

// TestOrgRoutingPolicyCache_RoundTripsSignatureV2 pins the node-side half of
// the ROUTING-SIG-1 close-out (docs/security.md; agent migration 085). The
// cache exists so the node can RE-VERIFY the policy it is about to compose
// without a live server — so the version-bound v2 signature has to survive
// the round trip. If it were dropped here, that offline re-verification would
// silently fall back to the body-only rail, reintroducing the finding one
// layer down.
func TestOrgRoutingPolicyCache_RoundTripsSignatureV2(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	want := OrgRoutingPolicyRow{
		Version:      7,
		Body:         "[routing]\n",
		BodyHash:     "deadbeef",
		Signature:    "v1-signature-bytes",
		SignatureV2:  "v2-signature-bytes",
		ServerPubkey: "pinned-key",
		ReceivedAt:   time.Now().UTC().Truncate(time.Second),
	}
	if err := s.UpsertOrgRoutingPolicy(ctx, want); err != nil {
		t.Fatalf("UpsertOrgRoutingPolicy: %v", err)
	}
	got, ok, err := s.GetOrgRoutingPolicy(ctx)
	if err != nil || !ok {
		t.Fatalf("GetOrgRoutingPolicy: ok=%v err=%v", ok, err)
	}
	if got.SignatureV2 != want.SignatureV2 {
		t.Errorf("signature_v2 = %q, want %q", got.SignatureV2, want.SignatureV2)
	}
	if got.Signature != want.Signature || got.Version != want.Version || got.Body != want.Body {
		t.Errorf("row = %+v, want %+v", got, want)
	}

	// The single-row cache REPLACES: a later document must not leave the
	// previous document's v2 signature behind, which would make the cached
	// row unverifiable-but-plausible.
	next := want
	next.Version = 8
	next.Body = "[routing]\n# v8\n"
	next.SignatureV2 = "v2-signature-for-8"
	next.Signature = "v1-signature-for-8"
	if err := s.UpsertOrgRoutingPolicy(ctx, next); err != nil {
		t.Fatalf("UpsertOrgRoutingPolicy(replace): %v", err)
	}
	got, ok, err = s.GetOrgRoutingPolicy(ctx)
	if err != nil || !ok {
		t.Fatalf("GetOrgRoutingPolicy(after replace): ok=%v err=%v", ok, err)
	}
	if got.Version != 8 || got.SignatureV2 != next.SignatureV2 {
		t.Errorf("after replace row = %+v, want version 8 with its own v2 signature", got)
	}
}

// TestOrgRoutingPolicyCache_EmptySignatureV2 pins the pre-078-server case:
// a document served with no v2 signature caches an EMPTY one — never an
// invented value — and reads back as empty, so the fetch path can keep
// telling "no v2 offered" apart from "v2 failed".
func TestOrgRoutingPolicyCache_EmptySignatureV2(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	if err := s.UpsertOrgRoutingPolicy(ctx, OrgRoutingPolicyRow{
		Version: 1, Body: "[routing]\n", BodyHash: "abc",
		Signature: "legacy-only", ServerPubkey: "pinned-key",
		ReceivedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("UpsertOrgRoutingPolicy: %v", err)
	}
	got, ok, err := s.GetOrgRoutingPolicy(ctx)
	if err != nil || !ok {
		t.Fatalf("GetOrgRoutingPolicy: ok=%v err=%v", ok, err)
	}
	if got.SignatureV2 != "" {
		t.Errorf("signature_v2 = %q, want empty for a pre-078 document", got.SignatureV2)
	}
	if got.Signature != "legacy-only" {
		t.Errorf("signature = %q, want the legacy one preserved", got.Signature)
	}
}
