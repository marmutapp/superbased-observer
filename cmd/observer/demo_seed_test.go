package main

import (
	"context"
	"log/slog"
	"testing"
)

// TestDemoSeeder runs the real demo-mode seeder end to end: embedded
// fixtures → claude-code adapter → store.Ingest into a temp DB. Pins
// that the seeded database carries enough data to light up the
// dashboard (sessions, actions, token rows, FTS excerpts, at least
// one recorded failure) and that cleanup is error-free.
func TestDemoSeeder(t *testing.T) {
	seed := demoSeeder(slog.Default())
	database, cleanup, err := seed(context.Background())
	if err != nil {
		t.Fatalf("demoSeeder: %v", err)
	}
	defer func() {
		if err := cleanup(); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	}()

	counts := map[string]int{}
	for _, table := range []string{"projects", "sessions", "actions", "token_usage", "action_excerpts", "failure_context"} {
		var n int
		if err := database.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		counts[table] = n
	}
	if counts["sessions"] < 8 {
		t.Errorf("sessions = %d, want >= 8", counts["sessions"])
	}
	if counts["projects"] < 3 {
		t.Errorf("projects = %d, want >= 3", counts["projects"])
	}
	if counts["actions"] < 50 {
		t.Errorf("actions = %d, want >= 50", counts["actions"])
	}
	if counts["token_usage"] == 0 {
		t.Error("token_usage is empty")
	}
	if counts["action_excerpts"] == 0 {
		t.Error("action_excerpts is empty — demo search would return nothing")
	}
	if counts["failure_context"] == 0 {
		t.Error("failure_context is empty — the failures surface would be blank in demo mode")
	}
}
