package store

import (
	"context"
	"testing"
)

// Org-parity W2/W3 sentinel: the session-scoped enterprise wires (verbosity /
// cache / process) ship ONLY under shipsRawContent() (full_content or
// admin_managed). A teams-tier node — default metadata-only ShareOptions,
// even with every Arc-4 detail flag on — must never carry them.
func TestSelectUnpushedSince_SessionWiresGatedOnRawContent(t *testing.T) {
	s, db := newTestStore(t)
	seedPushData(t, s, db)
	ctx := context.Background()

	// Teams posture: metadata-only, all Arc-4 detail tiers enabled — the
	// session wires must still be absent.
	teams, err := s.SelectUnpushedSince(ctx, PushCursor{}, 1<<20, "o", "u",
		ShareOptions{CacheDetail: true, ProcessDetail: true, RoutingSummary: true}, ScopeOptions{})
	if err != nil {
		t.Fatalf("teams SelectUnpushedSince: %v", err)
	}
	if len(teams.SessionVerbositySummaries) != 0 || len(teams.SessionCacheSummaries) != 0 || len(teams.SessionProcesses) != 0 {
		t.Fatalf("session wires shipped under metadata-only posture: verbosity=%d cache=%d process=%d",
			len(teams.SessionVerbositySummaries), len(teams.SessionCacheSummaries), len(teams.SessionProcesses))
	}

	// Enterprise posture (admin_managed): the seeded run_command's
	// command-bytes verbosity row rides.
	ent, err := s.SelectUnpushedSince(ctx, PushCursor{}, 1<<20, "o", "u",
		ShareOptions{AdminManaged: true}, ScopeOptions{})
	if err != nil {
		t.Fatalf("enterprise SelectUnpushedSince: %v", err)
	}
	if len(ent.SessionVerbositySummaries) == 0 {
		t.Fatal("admin_managed batch carries no session verbosity row (expected the seeded command bytes)")
	}
	for _, r := range ent.SessionVerbositySummaries {
		if r.OrgID != "o" || r.UserEmail != "u" {
			t.Fatalf("session verbosity row not stamped: %+v", r)
		}
	}
}
