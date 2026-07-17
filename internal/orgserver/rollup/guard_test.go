package rollup

import (
	"context"
	"database/sql"
	"testing"
)

// seedGuard layers guard_events onto the seed() org. Chain design:
//
//	alice (team-a): a0 (2026-04-01, OUT of the 30d window, genesis)
//	                ← a1 (05-20, deny R-001, enforced)
//	                ← a2 (05-21, flag R-001)
//	                ← a3 (05-25, allow, empty rule id → "other" bucket)
//	  → 1 head, 0 unlinked = 1 segment: CONTINUOUS. a1's predecessor is
//	    out-of-window, proving chain continuity is computed unwindowed.
//	bob (team-b):   b0 (05-22, flag R-180, genesis)
//	                b1 (05-23, deny R-001, enforced, prev MISSING)
//	  → 1 head + 1 unlinked = 2 segments: BROKEN.
//	carol (team-a): c1 (05-24, mask R-172, enforced, prev unpushed —
//	                the enrolled-mid-history shape)
//	  → 0 heads + 1 unlinked = 1 segment: CONTINUOUS by design.
func seedGuard(t *testing.T, d *sql.DB) {
	t.Helper()
	ctx := context.Background()
	ge := func(user, ts, rule, category, severity, decision string, enforced int, prev, hash string) {
		t.Helper()
		if _, err := d.ExecContext(ctx, `
INSERT INTO guard_events (chain_hash, user_id, session_id, timestamp, tool, event_kind,
                          rule_id, category, severity, decision, enforced, source,
                          target_hash, chain_prev, pushed_at, pushed_by_user_id)
VALUES (?, ?, 's-x', ?, 'claude-code', 'shell_exec', ?, ?, ?, ?, ?, 'hook', 'th', ?, '2026-05-26T11:00:00Z', ?)`,
			hash, user, ts, rule, category, severity, decision, enforced, prev, user); err != nil {
			t.Fatalf("seedGuard: %v", err)
		}
	}
	ge("u-alice", "2026-04-01T09:00:00Z", "R-101", "boundary", "medium", "flag", 0, "", "a0")
	ge("u-alice", "2026-05-20T09:00:00Z", "R-001", "destructive", "high", "deny", 1, "a0", "a1")
	ge("u-alice", "2026-05-21T09:00:00Z", "R-001", "destructive", "high", "flag", 0, "a1", "a2")
	ge("u-alice", "2026-05-25T09:00:00Z", "", "", "", "allow", 0, "a2", "a3")
	ge("u-bob", "2026-05-22T09:00:00Z", "R-180", "injection", "warn", "flag", 0, "", "b0")
	ge("u-bob", "2026-05-23T09:00:00Z", "R-001", "destructive", "high", "deny", 1, "missing-prev", "b1")
	ge("u-carol", "2026-05-24T09:00:00Z", "R-172", "exfil", "high", "mask", 1, "unpushed-prefix", "c1")
}

func TestGuardOverview(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	seedGuard(t, d)
	ctx := context.Background()

	cases := []struct {
		name  string
		scope Scope
		want  GuardOverviewResult
	}{
		{
			name:  "admin_org_wide",
			scope: adminScope,
			want: GuardOverviewResult{
				TotalEvents: 6, DenyCount: 2, AskCount: 0, FlagCount: 2, MaskCount: 1,
				EnforcedCount: 3, ActiveAgents: 3, RuleCount: 3, BrokenChainAgents: 1,
			},
		},
		{
			name:  "lead_scoped_to_team_a",
			scope: aliceScope,
			want: GuardOverviewResult{
				TotalEvents: 4, DenyCount: 1, AskCount: 0, FlagCount: 1, MaskCount: 1,
				EnforcedCount: 2, ActiveAgents: 2, RuleCount: 2, BrokenChainAgents: 0,
			},
		},
		{
			name:  "member_empty_scope_sees_nothing",
			scope: Scope{},
			want:  GuardOverviewResult{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := GuardOverview(ctx, d, w30, tc.scope, fixedNow)
			if err != nil {
				t.Fatalf("GuardOverview: %v", err)
			}
			if got.TotalEvents != tc.want.TotalEvents || got.DenyCount != tc.want.DenyCount ||
				got.AskCount != tc.want.AskCount || got.FlagCount != tc.want.FlagCount ||
				got.MaskCount != tc.want.MaskCount || got.EnforcedCount != tc.want.EnforcedCount ||
				got.ActiveAgents != tc.want.ActiveAgents || got.RuleCount != tc.want.RuleCount ||
				got.BrokenChainAgents != tc.want.BrokenChainAgents {
				t.Errorf("GuardOverview = %+v, want counts %+v", got, tc.want)
			}
		})
	}
}

func TestGuardOverview_TrendAndTopRules(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	seedGuard(t, d)

	got, err := GuardOverview(context.Background(), d, w30, adminScope, fixedNow)
	if err != nil {
		t.Fatalf("GuardOverview: %v", err)
	}

	// Six in-window events on six distinct days; the out-of-window a0 is
	// absent. Spot-check the deny day and the "other" (allow) day.
	if len(got.TrendByDay) != 6 {
		t.Fatalf("trend days = %d, want 6 (%+v)", len(got.TrendByDay), got.TrendByDay)
	}
	first := got.TrendByDay[0]
	if first.Date != "2026-05-20" || first.Deny != 1 || first.Total != 1 {
		t.Errorf("trend[0] = %+v, want 2026-05-20 deny=1 total=1", first)
	}
	last := got.TrendByDay[5]
	if last.Date != "2026-05-25" || last.Other != 1 || last.Total != 1 {
		t.Errorf("trend[5] = %+v, want 2026-05-25 other=1 total=1", last)
	}

	// Leaderboard: R-001 (3 hits, 2 agents, 2 denies) first; empty rule ids
	// are excluded.
	if len(got.TopRules) != 3 {
		t.Fatalf("top rules = %+v, want 3 rows", got.TopRules)
	}
	r := got.TopRules[0]
	if r.RuleID != "R-001" || r.Hits != 3 || r.Agents != 2 || r.DenyCount != 2 ||
		r.Category != "destructive" || r.Severity != "high" || r.LastSeen != "2026-05-23T09:00:00Z" {
		t.Errorf("top rule = %+v, want R-001 hits=3 agents=2 denies=2", r)
	}
	if got.TopRules[1].RuleID != "R-172" || got.TopRules[2].RuleID != "R-180" {
		t.Errorf("rule order = [%s %s %s], want count-desc then id-asc",
			got.TopRules[0].RuleID, got.TopRules[1].RuleID, got.TopRules[2].RuleID)
	}
}

func TestGuardRules_FullLeaderboardScoped(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	seedGuard(t, d)

	got, err := GuardRules(context.Background(), d, w30, aliceScope, fixedNow)
	if err != nil {
		t.Fatalf("GuardRules: %v", err)
	}
	// Team A saw R-001 (alice ×2) and R-172 (carol); bob's R-180 is out of scope.
	if len(got.Rules) != 2 || got.Rules[0].RuleID != "R-001" || got.Rules[0].Hits != 2 ||
		got.Rules[1].RuleID != "R-172" {
		t.Errorf("lead-scoped rules = %+v, want [R-001 hits=2, R-172]", got.Rules)
	}
}

func TestGuardTeams_PostureRows(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	seedGuard(t, d)

	got, err := GuardTeams(context.Background(), d, w30, adminScope, fixedNow)
	if err != nil {
		t.Fatalf("GuardTeams: %v", err)
	}
	if len(got.Teams) != 2 {
		t.Fatalf("teams = %d rows, want 2", len(got.Teams))
	}
	a, b := got.Teams[0], got.Teams[1] // ordered by display name: Team A, Team B
	if a.TeamID != "team-a" || a.MemberCount != 2 || a.ActiveAgents != 2 || a.Events != 4 ||
		a.DenyCount != 1 || a.FlagCount != 1 || a.MaskCount != 1 || !near(a.EnforcedShare, 0.5) ||
		a.BrokenChainAgents != 0 {
		t.Errorf("team-a posture = %+v", a)
	}
	if b.TeamID != "team-b" || b.MemberCount != 1 || b.ActiveAgents != 1 || b.Events != 2 ||
		b.DenyCount != 1 || b.FlagCount != 1 || !near(b.EnforcedShare, 0.5) ||
		b.BrokenChainAgents != 1 {
		t.Errorf("team-b posture = %+v", b)
	}

	// Lead scope sees only the led team.
	scoped, err := GuardTeams(context.Background(), d, w30, bobScope, fixedNow)
	if err != nil {
		t.Fatalf("GuardTeams scoped: %v", err)
	}
	if len(scoped.Teams) != 1 || scoped.Teams[0].TeamID != "team-b" {
		t.Errorf("bob-scoped teams = %+v, want [team-b]", scoped.Teams)
	}
}

func TestGuardAgents_ChainContinuity(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	seedGuard(t, d)

	got, err := GuardAgents(context.Background(), d, adminScope)
	if err != nil {
		t.Fatalf("GuardAgents: %v", err)
	}
	if len(got.Agents) != 3 {
		t.Fatalf("agents = %d rows, want 3", len(got.Agents))
	}

	// Broken sorts first: bob (1 head + 1 unlinked = 2 segments).
	bob := got.Agents[0]
	if bob.UserID != "u-bob" || !bob.Broken || bob.Heads != 1 || bob.Unlinked != 1 || bob.Segments != 2 || bob.Events != 2 {
		t.Errorf("bob chain = %+v, want broken 1+1=2 segments", bob)
	}
	// Alice: continuous across the out-of-window genesis (events counts the
	// FULL history, 4 rows, because chain continuity is unwindowed).
	alice := got.Agents[1]
	if alice.UserID != "u-alice" || alice.Broken || alice.Heads != 1 || alice.Unlinked != 0 ||
		alice.Segments != 1 || alice.Events != 4 {
		t.Errorf("alice chain = %+v, want continuous head=1 events=4", alice)
	}
	if alice.FirstSeen != "2026-04-01T09:00:00Z" || alice.LastSeen != "2026-05-25T09:00:00Z" {
		t.Errorf("alice first/last = %s/%s", alice.FirstSeen, alice.LastSeen)
	}
	// Carol: enrolled mid-history (0 heads + 1 unlinked = 1 segment) is
	// CONTINUOUS, not broken.
	carol := got.Agents[2]
	if carol.UserID != "u-carol" || carol.Broken || carol.Heads != 0 || carol.Unlinked != 1 || carol.Segments != 1 {
		t.Errorf("carol chain = %+v, want continuous 0+1=1 segment", carol)
	}

	// Lead scope excludes other teams' agents.
	scoped, err := GuardAgents(context.Background(), d, aliceScope)
	if err != nil {
		t.Fatalf("GuardAgents scoped: %v", err)
	}
	if len(scoped.Agents) != 2 {
		t.Errorf("alice-scoped agents = %+v, want alice+carol only", scoped.Agents)
	}
	for _, a := range scoped.Agents {
		if a.UserID == "u-bob" {
			t.Errorf("bob leaked into team-a scope")
		}
	}
}

func TestGuardRuleHitsForIDs_DryRun(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	seedGuard(t, d)

	got, err := GuardRuleHitsForIDs(context.Background(), d, w30, []string{"R-001", "R-999"}, fixedNow)
	if err != nil {
		t.Fatalf("GuardRuleHitsForIDs: %v", err)
	}
	if h := got["R-001"]; h.Hits != 3 || h.Agents != 2 {
		t.Errorf("R-001 dry-run = %+v, want hits=3 agents=2", h)
	}
	if _, present := got["R-999"]; present {
		t.Errorf("R-999 should be absent (no pushed hits); caller fills zeros")
	}

	// Empty id list short-circuits without touching the DB.
	empty, err := GuardRuleHitsForIDs(context.Background(), d, w30, nil, fixedNow)
	if err != nil || len(empty) != 0 {
		t.Errorf("empty ids = %v, %v; want empty map, nil error", empty, err)
	}
}
