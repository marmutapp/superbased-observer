package remoteauth

import (
	"errors"
	"testing"
	"time"
)

// seedRow persists one current-generation session row for `raw` with the given
// last-seen age, bypassing Create (which enforces the cap we are testing).
func seedRow(t *testing.T, p *fakePersister, raw string, created, lastSeen time.Time) {
	t.Helper()
	if err := p.Save(t.Context(), PersistedSession{
		IDHash: HashSessionID(raw), Gen: p.gen, CreatedAt: created, LastSeen: lastSeen,
	}); err != nil {
		t.Fatalf("seed %s: %v", raw, err)
	}
}

// TestRestoreHonoursMaxCap pins the restore()-bypasses-the-cap fix: a durable
// table holding MORE than Max live sessions must not put the in-memory store
// above the cap at boot (which made every subsequent pairing fail with
// ErrTooManySessions). The most-recently-seen Max survive; the surplus is
// pruned DURABLY so memory and the table agree.
func TestRestoreHonoursMaxCap(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	p := newFakePersister()
	// Four live sessions, oldest activity first. TTL 4h / Idle 2h keeps them
	// all non-expired at `now`, so ONLY the cap may remove any of them.
	seedRow(t, p, "dev-oldest", now.Add(-3*time.Hour), now.Add(-90*time.Minute))
	seedRow(t, p, "dev-older", now.Add(-3*time.Hour), now.Add(-60*time.Minute))
	seedRow(t, p, "dev-newer", now.Add(-3*time.Hour), now.Add(-30*time.Minute))
	seedRow(t, p, "dev-newest", now.Add(-3*time.Hour), now.Add(-5*time.Minute))

	s := NewSessionStore(SessionParams{
		TTL: 4 * time.Hour, Idle: 2 * time.Hour, Max: 2,
		Now:       func() time.Time { return now },
		Persister: p,
	})

	if got := s.Count(); got != 2 {
		t.Fatalf("restored %d sessions, want the Max of 2", got)
	}
	for _, raw := range []string{"dev-newest", "dev-newer"} {
		if err := s.Validate(raw); err != nil {
			t.Errorf("%s should have survived the cap (most recently seen): %v", raw, err)
		}
	}
	for _, raw := range []string{"dev-older", "dev-oldest"} {
		if err := s.Validate(raw); err == nil {
			t.Errorf("%s should have been pruned by the cap", raw)
		}
	}
	// Durable prune: the table must agree with memory, or the same surplus is
	// re-read on the next restart and the cap is breached again forever.
	if got := p.count(); got != 2 {
		t.Fatalf("durable rows after restore = %d, want 2 (surplus must be pruned durably)", got)
	}
	// A second "restart" over the pruned table is stable and still at the cap.
	s2 := NewSessionStore(SessionParams{
		TTL: 4 * time.Hour, Idle: 2 * time.Hour, Max: 2,
		Now:       func() time.Time { return now },
		Persister: p,
	})
	if got := s2.Count(); got != 2 {
		t.Fatalf("second restore restored %d, want 2", got)
	}
}

// TestRestoreCapLeavesRoomForPairingAfterRevoke proves the user-visible point:
// once the restored set respects the cap, revoking one device frees exactly one
// slot and pairing works again — the state the pre-fix store could never reach
// because it booted above Max.
func TestRestoreCapLeavesRoomForPairingAfterRevoke(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	p := newFakePersister()
	for _, raw := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		seedRow(t, p, raw, now.Add(-time.Hour), now.Add(-time.Minute))
	}
	s := NewSessionStore(SessionParams{
		TTL: 4 * time.Hour, Idle: 2 * time.Hour, Max: 3,
		Now:       func() time.Time { return now },
		Persister: p,
	})
	if got := s.Count(); got != 3 {
		t.Fatalf("restored %d, want 3", got)
	}
	// Still fail-CLOSED at the cap — pairing must never evict a live device.
	if _, err := s.Create(); !errors.Is(err, ErrTooManySessions) {
		t.Fatalf("Create at cap = %v, want ErrTooManySessions (fail closed)", err)
	}
	// Revoke one of the survivors, then pairing succeeds.
	var victim SessionInfo
	if list := s.List(); len(list) > 0 {
		victim = list[0]
	} else {
		t.Fatal("no sessions listed")
	}
	if err := s.RevokeByHash(victim.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := s.Create(); err != nil {
		t.Fatalf("Create after freeing a slot: %v", err)
	}
}

// TestRestoreUnderCapKeepsEverything guards the obvious regression: the cap
// logic must not drop sessions when the table is within Max.
func TestRestoreUnderCapKeepsEverything(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	p := newFakePersister()
	seedRow(t, p, "one", now.Add(-time.Hour), now.Add(-time.Minute))
	seedRow(t, p, "two", now.Add(-time.Hour), now.Add(-2*time.Minute))
	s := NewSessionStore(SessionParams{
		TTL: 4 * time.Hour, Idle: 2 * time.Hour, Max: 5,
		Now:       func() time.Time { return now },
		Persister: p,
	})
	if got := s.Count(); got != 2 {
		t.Fatalf("restored %d, want 2", got)
	}
	if got := p.count(); got != 2 {
		t.Fatalf("durable rows = %d, want 2 (nothing pruned under the cap)", got)
	}
}

// TestSessionStoreTTLReportsAppliedDefault pins the accessor the HTTP cookie
// layer derives Max-Age from: it must report the POST-default value, never a
// caller's zero.
func TestSessionStoreTTLReportsAppliedDefault(t *testing.T) {
	if got := NewSessionStore(SessionParams{}).TTL(); got != DefaultSessionTTL {
		t.Errorf("TTL() with zero params = %v, want the applied default %v", got, DefaultSessionTTL)
	}
	if got := NewSessionStore(SessionParams{TTL: 90 * time.Minute}).TTL(); got != 90*time.Minute {
		t.Errorf("TTL() = %v, want 90m", got)
	}
}
