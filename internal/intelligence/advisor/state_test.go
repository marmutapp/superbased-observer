package advisor

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
)

// TestStateMutingAndDigestRoundTrip pins the Phase-2 state layer: a
// dismissed key mutes for the cooldown, a snoozed key until its snooze
// elapses, an expired snooze unmutes; the digest snapshot round-trips.
func TestStateMutingAndDigestRoundTrip(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, db.Options{Path: filepath.Join(t.TempDir(), "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)

	if err := SetState(ctx, database, "k-dismissed", StatusDismissed, time.Time{}, now); err != nil {
		t.Fatal(err)
	}
	if err := SetState(ctx, database, "k-snoozed", StatusSnoozed, now.AddDate(0, 0, 3), now); err != nil {
		t.Fatal(err)
	}
	if err := SetState(ctx, database, "k-expired-snooze", StatusSnoozed, now.AddDate(0, 0, -1), now.AddDate(0, 0, -2)); err != nil {
		t.Fatal(err)
	}
	if err := SetState(ctx, database, "k-bad", "nonsense", time.Time{}, now); err == nil {
		t.Fatal("want error for unknown status")
	}

	muted, err := loadMuted(ctx, database, now, 7)
	if err != nil {
		t.Fatal(err)
	}
	if !muted["k-dismissed"] || !muted["k-snoozed"] {
		t.Errorf("dismissed + active-snooze must mute: %v", muted)
	}
	if muted["k-expired-snooze"] {
		t.Errorf("expired snooze must NOT mute")
	}
	// Past the cooldown the dismissed key resurfaces.
	muted, _ = loadMuted(ctx, database, now.AddDate(0, 0, 8), 7)
	if muted["k-dismissed"] {
		t.Errorf("dismissed key must unmute after the cooldown")
	}

	rep := Report{
		GeneratedAt: now.Format(time.RFC3339), WindowDays: 14,
		Suggestions: []Suggestion{{DedupKey: "a", SavingsUSD: 9}, {DedupKey: "b"}, {DedupKey: "c"}},
	}
	if err := SaveDigest(ctx, database, rep, 2); err != nil {
		t.Fatal(err)
	}
	got, ok, err := LoadDigest(ctx, database)
	if err != nil || !ok {
		t.Fatalf("LoadDigest: ok=%v err=%v", ok, err)
	}
	if len(got.Suggestions) != 2 || got.Suggestions[0].DedupKey != "a" {
		t.Errorf("digest round-trip: %+v", got.Suggestions)
	}
}
