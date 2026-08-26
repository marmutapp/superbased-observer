package store

import (
	"context"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/processobs"
)

func TestLaunchSeed_InsertPendingClaimRoundtrip(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()

	if err := s.InsertLaunchSeed(ctx, processobs.LaunchSeed{PID: 4242, Tool: "opencode", CWD: "/proj"}); err != nil {
		t.Fatalf("InsertLaunchSeed: %v", err)
	}
	pending, err := s.PendingLaunchSeeds(ctx, time.Hour)
	if err != nil {
		t.Fatalf("PendingLaunchSeeds: %v", err)
	}
	if len(pending) != 1 || pending[0].PID != 4242 || pending[0].Tool != "opencode" || pending[0].CWD != "/proj" {
		t.Fatalf("pending = %+v, want one opencode seed for pid 4242", pending)
	}

	claimed, err := s.ClaimLaunchSeed(ctx, 4242)
	if err != nil || !claimed {
		t.Fatalf("ClaimLaunchSeed = (%v, %v), want claimed", claimed, err)
	}
	again, err := s.ClaimLaunchSeed(ctx, 4242)
	if err != nil || again {
		t.Fatalf("second ClaimLaunchSeed = (%v, %v), want false (row gone)", again, err)
	}
	pending, err = s.PendingLaunchSeeds(ctx, time.Hour)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending after claim = (%+v, %v), want empty", pending, err)
	}
}

func TestLaunchSeed_UpsertReplacesRecycledPID(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()

	if err := s.InsertLaunchSeed(ctx, processobs.LaunchSeed{PID: 7, Tool: "pi", CWD: "/old"}); err != nil {
		t.Fatalf("InsertLaunchSeed: %v", err)
	}
	if err := s.InsertLaunchSeed(ctx, processobs.LaunchSeed{PID: 7, Tool: "opencode", CWD: "/new"}); err != nil {
		t.Fatalf("InsertLaunchSeed (recycled pid): %v", err)
	}
	pending, err := s.PendingLaunchSeeds(ctx, time.Hour)
	if err != nil {
		t.Fatalf("PendingLaunchSeeds: %v", err)
	}
	if len(pending) != 1 || pending[0].Tool != "opencode" || pending[0].CWD != "/new" {
		t.Fatalf("pending = %+v, want the recycled-pid upsert to replace in place", pending)
	}
}

func TestLaunchSeed_ClaimIsIdempotentDelete(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()

	if err := s.InsertLaunchSeed(ctx, processobs.LaunchSeed{PID: 9, Tool: "goose", CWD: "/proj"}); err != nil {
		t.Fatalf("InsertLaunchSeed: %v", err)
	}
	for i := 0; i < 2; i++ {
		claimed, err := s.ClaimLaunchSeed(ctx, 9)
		if err != nil {
			t.Fatalf("ClaimLaunchSeed pass %d: %v", i, err)
		}
		if (i == 0) != claimed {
			t.Fatalf("ClaimLaunchSeed pass %d claimed = %v, want %v", i, claimed, i == 0)
		}
	}
}

func TestLaunchSeed_ExpireStaleRemovesOnlyOldRows(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()

	if err := s.InsertLaunchSeed(ctx, processobs.LaunchSeed{PID: 11, Tool: "grok", CWD: "/proj"}); err != nil {
		t.Fatalf("InsertLaunchSeed: %v", err)
	}
	// Nothing is stale yet inside a generous window.
	n, err := s.ExpireStaleLaunchSeeds(ctx, time.Hour)
	if err != nil || n != 0 {
		t.Fatalf("ExpireStaleLaunchSeeds(fresh) = (%d, %v), want 0 removed", n, err)
	}
	// A TTL of zero expires everything.
	n, err = s.ExpireStaleLaunchSeeds(ctx, 0)
	if err != nil || n != 1 {
		t.Fatalf("ExpireStaleLaunchSeeds(0) = (%d, %v), want 1 removed", n, err)
	}
}

func TestRecentSessionRefsForLaunchMatch(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()

	projectID, err := s.UpsertProject(ctx, "/proj", "")
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, project_id, tool, started_at) VALUES (?, ?, ?, ?)`,
		"sess-launch-1", projectID, "opencode", timestamp(time.Now().UTC().Add(-time.Minute))); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	refs, err := s.RecentSessionRefsForLaunchMatch(ctx, 60)
	if err != nil {
		t.Fatalf("RecentSessionRefsForLaunchMatch: %v", err)
	}
	found := false
	for _, ref := range refs {
		if ref.SessionID == "sess-launch-1" {
			found = true
			if ref.Tool != "opencode" || ref.ProjectRoot != "/proj" {
				t.Fatalf("ref = %+v, want tool opencode + root /proj", ref)
			}
			if ref.StartedAt.IsZero() {
				t.Fatal("ref.StartedAt zero — parseStamp failed")
			}
		}
	}
	if !found {
		t.Fatalf("refs = %+v, want sess-launch-1 present", refs)
	}
}
