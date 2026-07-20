package store

import (
	"context"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/aggregate"
)

func TestAggregateSubmissionLedger(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	// No state for an unattempted month.
	if row, err := s.LoadAggregateState(ctx, "2026-06"); err != nil || row != nil {
		t.Fatalf("LoadAggregateState empty = (%v, %v), want (nil, nil)", row, err)
	}

	// First attempt mints the id and creates a pending row.
	id, err := s.StartAggregateAttempt(ctx, "2026-06", "id-first", "hash1", `{"a":1}`, 1, now)
	if err != nil {
		t.Fatalf("StartAggregateAttempt: %v", err)
	}
	if id != "id-first" {
		t.Fatalf("first attempt id = %q, want id-first", id)
	}
	row, err := s.LoadAggregateState(ctx, "2026-06")
	if err != nil || row == nil {
		t.Fatalf("LoadAggregateState = (%v, %v)", row, err)
	}
	if row.State != AggregateStatePending || row.Attempts != 1 || row.PayloadHash != "hash1" {
		t.Fatalf("row after first attempt = %+v", row)
	}

	// Retry REUSES the persisted submission_id even if a new candidate is
	// offered — the double-count guard (finding #14).
	id2, err := s.StartAggregateAttempt(ctx, "2026-06", "id-different", "hash2", `{"a":2}`, 1, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if id2 != "id-first" {
		t.Fatalf("retry id = %q, want the persisted id-first (reuse-on-retry)", id2)
	}
	row, _ = s.LoadAggregateState(ctx, "2026-06")
	if row.Attempts != 2 || row.PayloadHash != "hash2" {
		t.Fatalf("row after retry = %+v (attempts should increment, payload refresh)", row)
	}

	// Mark submitted → terminal; a further attempt is a no-op returning the id.
	if err := s.MarkAggregateSubmitted(ctx, "2026-06", now.Add(2*time.Minute)); err != nil {
		t.Fatalf("MarkAggregateSubmitted: %v", err)
	}
	id3, err := s.StartAggregateAttempt(ctx, "2026-06", "id-new", "hash3", `{"a":3}`, 1, now.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("post-submit attempt: %v", err)
	}
	if id3 != "id-first" {
		t.Fatalf("post-submit id = %q, want id-first", id3)
	}
	row, _ = s.LoadAggregateState(ctx, "2026-06")
	if row.State != AggregateStateSubmitted || row.Attempts != 2 {
		t.Fatalf("submitted row mutated by a later attempt: %+v", row)
	}

	// Failure path leaves the row retryable with a bounded error.
	if _, err := s.StartAggregateAttempt(ctx, "2026-05", "id-may", "h", "{}", 1, now); err != nil {
		t.Fatalf("start may: %v", err)
	}
	if err := s.MarkAggregateFailed(ctx, "2026-05", "boom", now); err != nil {
		t.Fatalf("MarkAggregateFailed: %v", err)
	}
	rows, err := s.ListAggregateStates(ctx)
	if err != nil {
		t.Fatalf("ListAggregateStates: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("ListAggregateStates len = %d, want 2", len(rows))
	}
	// Newest month first.
	if rows[0].Month != "2026-06" {
		t.Fatalf("ordering wrong: %+v", rows)
	}
}

func TestAggregateConsentLifecycle(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()

	// Nothing recorded yet.
	if r, err := s.LoadConsentReceipt(ctx); err != nil || r != nil {
		t.Fatalf("LoadConsentReceipt empty = (%v, %v)", r, err)
	}

	rec := aggregate.Receipt{
		SchemaVersion:       aggregate.SchemaVersion,
		Endpoint:            "https://aggregate.superbased.app/v1/submit",
		PricingVersion:      aggregate.PricingVersion,
		CostMethodVersion:   aggregate.CostMethodVersion(),
		ToolRegistryVersion: 1,
		Actor:               aggregate.ActorInteractive,
		DisclosureHash:      "abc123",
		ScopeDBPath:         "/home/u/.observer/observer.db",
		ConsentedAt:         time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
	}
	if err := s.SaveConsentReceipt(ctx, rec); err != nil {
		t.Fatalf("SaveConsentReceipt: %v", err)
	}
	got, err := s.LoadConsentReceipt(ctx)
	if err != nil || got == nil {
		t.Fatalf("LoadConsentReceipt = (%v, %v)", got, err)
	}
	if got.SchemaVersion != rec.SchemaVersion || got.Actor != rec.Actor || got.DisclosureHash != "abc123" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	// Endpoint is normalized on save (trailing content-equal).
	if got.Endpoint != aggregate.NormalizeEndpoint(rec.Endpoint) {
		t.Fatalf("endpoint = %q, want normalized", got.Endpoint)
	}

	// Revoke → LoadConsentReceipt returns nil (treated as absent).
	if err := s.RevokeConsent(ctx, time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("RevokeConsent: %v", err)
	}
	if r, err := s.LoadConsentReceipt(ctx); err != nil || r != nil {
		t.Fatalf("post-revoke LoadConsentReceipt = (%v, %v), want (nil, nil)", r, err)
	}

	// Re-consent (SaveConsentReceipt again) clears the revocation.
	if err := s.SaveConsentReceipt(ctx, rec); err != nil {
		t.Fatalf("re-consent SaveConsentReceipt: %v", err)
	}
	if r, err := s.LoadConsentReceipt(ctx); err != nil || r == nil {
		t.Fatalf("post-re-consent LoadConsentReceipt = (%v, %v), want a receipt", r, err)
	}
}
