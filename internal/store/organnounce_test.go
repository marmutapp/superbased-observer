package store

import (
	"context"
	"testing"
	"time"
)

// TestOrgAnnouncementCache pins the single-row cache seam (migration
// 076): absent is a clean ok=false (every solo install), an upsert
// round-trips, and a second upsert REPLACES rather than appends — the
// table holds exactly one document, the newest verified one.
func TestOrgAnnouncementCache(t *testing.T) {
	s, database := newTestStore(t)
	ctx := context.Background()

	if _, ok, err := s.GetOrgAnnouncement(ctx); err != nil || ok {
		t.Fatalf("GetOrgAnnouncement on a fresh DB = ok=%v err=%v, want ok=false nil", ok, err)
	}

	received := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	row := OrgAnnouncementRow{
		Version: 3, Body: `{"id":"x"}`, BodyHash: "abc", Signature: "sig",
		ServerPubkey: "pk", ReceivedAt: received,
	}
	if err := s.UpsertOrgAnnouncement(ctx, row); err != nil {
		t.Fatalf("UpsertOrgAnnouncement: %v", err)
	}
	got, ok, err := s.GetOrgAnnouncement(ctx)
	if err != nil || !ok {
		t.Fatalf("GetOrgAnnouncement: ok=%v err=%v", ok, err)
	}
	if got.Version != 3 || got.Body != row.Body || got.BodyHash != "abc" ||
		got.Signature != "sig" || got.ServerPubkey != "pk" {
		t.Errorf("round trip = %+v, want %+v", got, row)
	}
	if !got.ReceivedAt.Equal(received) {
		t.Errorf("ReceivedAt = %v, want %v", got.ReceivedAt, received)
	}

	// A retraction is a normal upsert with an empty body — and it must
	// REPLACE, so the previous announcement can never resurface.
	if err := s.UpsertOrgAnnouncement(ctx, OrgAnnouncementRow{
		Version: 4, Body: "", BodyHash: "def", Signature: "sig2",
		ServerPubkey: "pk", ReceivedAt: received.Add(time.Hour),
	}); err != nil {
		t.Fatalf("UpsertOrgAnnouncement (retraction): %v", err)
	}
	got, ok, err = s.GetOrgAnnouncement(ctx)
	if err != nil || !ok || got.Version != 4 || got.Body != "" {
		t.Fatalf("after retraction = %+v ok=%v err=%v", got, ok, err)
	}
	var n int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM org_announcements`).Scan(&n); err != nil || n != 1 {
		t.Errorf("row count = %d err=%v, want exactly 1 (single-row cache)", n, err)
	}
}
