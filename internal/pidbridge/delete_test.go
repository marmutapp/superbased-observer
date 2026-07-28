package pidbridge

import (
	"context"
	"testing"
)

// TestStore_Delete is the table-driven contract for the unseed half of the
// direct pid seed. The scoped form is the pid-reuse guard: a writer retracts
// exactly the row it wrote and can never delete a row a later writer has
// already claimed for the same (recycled) pid.
func TestStore_Delete(t *testing.T) {
	tests := []struct {
		name string
		// seed is the row present before the delete ("" session = no row).
		seedSession string
		delPID      int
		delSession  string
		wantDeleted bool
		wantRowGone bool
	}{
		{
			name:        "scoped delete removes the row it wrote",
			seedSession: "sess-a",
			delPID:      4242,
			delSession:  "sess-a",
			wantDeleted: true,
			wantRowGone: true,
		},
		{
			name:        "scoped delete leaves a row a later writer reclaimed",
			seedSession: "sess-b",
			delPID:      4242,
			delSession:  "sess-a",
			wantDeleted: false,
			wantRowGone: false,
		},
		{
			name:        "unscoped delete removes whatever is there",
			seedSession: "sess-b",
			delPID:      4242,
			delSession:  "",
			wantDeleted: true,
			wantRowGone: true,
		},
		{
			name:        "missing row is a clean miss",
			seedSession: "",
			delPID:      4242,
			delSession:  "sess-a",
			wantDeleted: false,
			wantRowGone: true,
		},
		{
			name:        "non-positive pid is a no-op",
			seedSession: "sess-a",
			delPID:      0,
			delSession:  "sess-a",
			wantDeleted: false,
			wantRowGone: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := newStore(t)
			if tc.seedSession != "" {
				if err := s.Write(ctx, Entry{PID: 4242, SessionID: tc.seedSession, Tool: "claude-code"}); err != nil {
					t.Fatalf("Write: %v", err)
				}
			}
			deleted, err := s.Delete(ctx, tc.delPID, tc.delSession)
			if err != nil {
				t.Fatalf("Delete: %v", err)
			}
			if deleted != tc.wantDeleted {
				t.Errorf("Delete deleted = %v, want %v", deleted, tc.wantDeleted)
			}
			_, ok, err := s.Lookup(ctx, 4242)
			if err != nil {
				t.Fatalf("Lookup: %v", err)
			}
			if gone := !ok; gone != tc.wantRowGone {
				t.Errorf("row gone = %v, want %v", gone, tc.wantRowGone)
			}
		})
	}
}

// TestStore_DeleteThenPidReuse pins the end-to-end pid-reuse property: after
// a bridged process exits and its seed is retracted, the same pid handed to a
// different session resolves to the NEW session — never the dead one.
func TestStore_DeleteThenPidReuse(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if err := s.Write(ctx, Entry{PID: 777, SessionID: "dead-session", Tool: "claude-code"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := s.Delete(ctx, 777, "dead-session"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok, err := s.Lookup(ctx, 777); err != nil || ok {
		t.Fatalf("Lookup after retract: ok=%v err=%v, want a clean miss", ok, err)
	}

	if err := s.Write(ctx, Entry{PID: 777, SessionID: "live-session", Tool: "codex"}); err != nil {
		t.Fatalf("Write (reused pid): %v", err)
	}
	e, ok, err := s.Lookup(ctx, 777)
	if err != nil || !ok {
		t.Fatalf("Lookup (reused pid): ok=%v err=%v", ok, err)
	}
	if e.SessionID != "live-session" {
		t.Errorf("reused pid resolved to %q, want %q", e.SessionID, "live-session")
	}

	// The dead terminal's late retract must NOT remove the new owner's row.
	deleted, err := s.Delete(ctx, 777, "dead-session")
	if err != nil {
		t.Fatalf("late Delete: %v", err)
	}
	if deleted {
		t.Error("late scoped Delete removed a row it did not write")
	}
	if _, ok, _ := s.Lookup(ctx, 777); !ok {
		t.Error("late scoped Delete destroyed the new owner's row")
	}
}
