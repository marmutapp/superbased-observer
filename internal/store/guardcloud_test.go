package store

import (
	"context"
	"testing"
	"time"
)

// TestGuardEventsAfterAndCloudCursor pins the §15 cloud dispatcher's
// store seams: the id-ordered tail read and the schema_meta cursor.
func TestGuardEventsAfterAndCloudCursor(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	mk := func(rule string) GuardEventRow {
		return GuardEventRow{
			TS: time.Now().UTC(), SessionID: "s1", RuleID: rule,
			Severity: "high", Decision: "flag", Source: "watcher",
		}
	}
	if _, err := s.InsertGuardEvents(ctx, []GuardEventRow{mk("R-001"), mk("R-002"), mk("R-003")}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	all, err := s.GuardEventsAfter(ctx, 0, 0)
	if err != nil {
		t.Fatalf("GuardEventsAfter(0): %v", err)
	}
	if len(all) != 3 || all[0].RuleID != "R-001" || all[2].RuleID != "R-003" {
		t.Fatalf("tail = %+v, want 3 ascending rows", all)
	}
	for i := 1; i < len(all); i++ {
		if all[i].ID <= all[i-1].ID {
			t.Fatalf("ids not ascending: %d then %d", all[i-1].ID, all[i].ID)
		}
	}

	// Strictly-greater semantics + limit.
	tail, err := s.GuardEventsAfter(ctx, all[0].ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 1 || tail[0].RuleID != "R-002" {
		t.Errorf("after first, limit 1 = %+v, want [R-002]", tail)
	}

	// Cursor: unset reads 0; save/overwrite round-trips.
	if cur, err := s.LoadGuardCloudCursor(ctx); err != nil || cur != 0 {
		t.Errorf("unset cursor = %d, %v; want 0, nil", cur, err)
	}
	if err := s.SaveGuardCloudCursor(ctx, all[2].ID); err != nil {
		t.Fatal(err)
	}
	if cur, err := s.LoadGuardCloudCursor(ctx); err != nil || cur != all[2].ID {
		t.Errorf("cursor = %d, %v; want %d", cur, err, all[2].ID)
	}
	if err := s.SaveGuardCloudCursor(ctx, all[2].ID+10); err != nil {
		t.Fatal(err)
	}
	if cur, _ := s.LoadGuardCloudCursor(ctx); cur != all[2].ID+10 {
		t.Errorf("overwritten cursor = %d", cur)
	}

	// Nothing past the cursor → empty, not an error.
	empty, err := s.GuardEventsAfter(ctx, all[2].ID, 0)
	if err != nil || len(empty) != 0 {
		t.Errorf("past-tail read = %+v, %v; want empty", empty, err)
	}
}
