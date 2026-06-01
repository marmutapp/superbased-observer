package pidbridge

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "obs.db")
	d, err := db.Open(context.Background(), db.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return New(d)
}

func TestStore_WriteLookup(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	err := s.Write(ctx, Entry{
		PID:       12345,
		SessionID: "abc-session",
		Tool:      "claude-code",
		CWD:       "/home/me/repo",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	e, ok, err := s.Lookup(ctx, 12345)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !ok {
		t.Fatal("Lookup: not found")
	}
	if e.SessionID != "abc-session" || e.Tool != "claude-code" || e.CWD != "/home/me/repo" {
		t.Fatalf("Lookup: got %+v", e)
	}
	if e.CreatedAt.IsZero() || e.UpdatedAt.IsZero() {
		t.Fatalf("Lookup: timestamps zero: %+v", e)
	}
}

func TestStore_Lookup_Miss(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	_, ok, err := s.Lookup(ctx, 42)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if ok {
		t.Fatal("Lookup: unexpected hit")
	}
}

func TestStore_Write_UpsertOnPIDConflict(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if err := s.Write(ctx, Entry{PID: 99, SessionID: "old", Tool: "claude-code", CWD: "/a"}); err != nil {
		t.Fatalf("Write v1: %v", err)
	}
	if err := s.Write(ctx, Entry{PID: 99, SessionID: "new", Tool: "codex", CWD: ""}); err != nil {
		t.Fatalf("Write v2: %v", err)
	}
	e, ok, err := s.Lookup(ctx, 99)
	if err != nil || !ok {
		t.Fatalf("Lookup: ok=%v err=%v", ok, err)
	}
	if e.SessionID != "new" {
		t.Errorf("SessionID: got %q want %q", e.SessionID, "new")
	}
	if e.Tool != "codex" {
		t.Errorf("Tool: got %q want %q", e.Tool, "codex")
	}
	// Empty cwd on second write should NOT overwrite the earlier value.
	if e.CWD != "/a" {
		t.Errorf("CWD: got %q want %q (empty should preserve prior)", e.CWD, "/a")
	}
}

func TestStore_Write_Validation(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	cases := []struct {
		name  string
		entry Entry
	}{
		{"zero pid", Entry{SessionID: "s", Tool: "claude-code"}},
		{"negative pid", Entry{PID: -1, SessionID: "s", Tool: "claude-code"}},
		{"empty session", Entry{PID: 1, Tool: "claude-code"}},
		{"empty tool", Entry{PID: 1, SessionID: "s"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.Write(ctx, tc.entry); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestStore_Prune(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	base := time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)
	// Write an "old" entry at t=base.
	s.SetClock(func() time.Time { return base })
	if err := s.Write(ctx, Entry{PID: 100, SessionID: "old", Tool: "claude-code"}); err != nil {
		t.Fatalf("Write old: %v", err)
	}

	// Write a "fresh" entry at t=base+2h.
	s.SetClock(func() time.Time { return base.Add(2 * time.Hour) })
	if err := s.Write(ctx, Entry{PID: 200, SessionID: "new", Tool: "claude-code"}); err != nil {
		t.Fatalf("Write new: %v", err)
	}

	// Prune with the clock at base+3h; anything updated >1h ago goes.
	s.SetClock(func() time.Time { return base.Add(3 * time.Hour) })
	n, err := s.Prune(ctx, time.Hour)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 1 {
		t.Fatalf("Prune: got %d rows, want 1", n)
	}

	// Fresh entry survives.
	if _, ok, _ := s.Lookup(ctx, 200); !ok {
		t.Fatal("fresh entry pruned")
	}
	// Old entry gone.
	if _, ok, _ := s.Lookup(ctx, 100); ok {
		t.Fatal("old entry survived")
	}
}

func TestStore_Prune_NonPositive(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if err := s.Write(ctx, Entry{PID: 1, SessionID: "s", Tool: "claude-code"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	n, err := s.Prune(ctx, 0)
	if err != nil || n != 0 {
		t.Fatalf("Prune: n=%d err=%v; want 0, nil", n, err)
	}
}

func TestStore_List(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	base := time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return base })
	if err := s.Write(ctx, Entry{PID: 1, SessionID: "a", Tool: "claude-code"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	s.SetClock(func() time.Time { return base.Add(time.Minute) })
	if err := s.Write(ctx, Entry{PID: 2, SessionID: "b", Tool: "claude-code"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List: got %d rows, want 2", len(got))
	}
	// Newest first.
	if got[0].PID != 2 || got[1].PID != 1 {
		t.Fatalf("List: order mismatch: %+v", got)
	}
}

func TestNopResolver(t *testing.T) {
	var r Resolver = NopResolver{}
	sid, ok, err := r.Resolve(context.Background(), "127.0.0.1:1234")
	if sid != "" || ok || err != nil {
		t.Fatalf("NopResolver: sid=%q ok=%v err=%v", sid, ok, err)
	}
}
