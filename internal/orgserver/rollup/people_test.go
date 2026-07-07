package rollup

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func peopleMap(in []PersonRollup) map[string]PersonRollup {
	m := map[string]PersonRollup{}
	for _, p := range in {
		m[p.UserID] = p
	}
	return m
}

// TestPeople_AdminLeaderboard pins the org-wide per-developer leaderboard over
// the shared seed: cost, counts, tokens, top tool/model, last active, spark,
// and the cost-descending order.
func TestPeople_AdminLeaderboard(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	got, err := People(context.Background(), d, w30, adminScope, "", fixedNow)
	if err != nil {
		t.Fatalf("People: %v", err)
	}
	if len(got.People) != 3 {
		t.Fatalf("people = %d, want 3 (alice, bob, carol)", len(got.People))
	}
	// Cost-descending: bob 0.23, alice 0.15, carol 0.07.
	if got.People[0].UserID != "u-bob" || got.People[1].UserID != "u-alice" || got.People[2].UserID != "u-carol" {
		t.Errorf("order = %v, want bob, alice, carol", []string{got.People[0].UserID, got.People[1].UserID, got.People[2].UserID})
	}
	m := peopleMap(got.People)
	if a := m["u-alice"]; !near(a.CostUSD, 0.15) || a.Tokens != 300 || a.SessionCount != 1 || a.ActionCount != 1 ||
		a.TopTool != "claude-code" || a.TopModel != "claude" || a.Email != "alice@acme.example" || a.DisplayName != "Alice" {
		t.Errorf("alice = %+v, want cost 0.15 tok 300 sess 1 act 1 tool claude-code model claude", a)
	}
	if b := m["u-bob"]; !near(b.CostUSD, 0.23) || b.TopTool != "codex" || b.TopModel != "gpt" {
		t.Errorf("bob = %+v, want cost 0.23 tool codex model gpt", b)
	}
	if c := m["u-carol"]; !near(c.CostUSD, 0.07) || c.ActionCount != 2 {
		t.Errorf("carol = %+v, want cost 0.07 actions 2", c)
	}
	// Spark: alice spent 0.15 on 2026-05-20 (index 0 of the 7-day window
	// ending 2026-05-26); the rest are zero.
	a := m["u-alice"]
	if len(a.Spark) != sparkDays || !near(a.Spark[0], 0.15) {
		t.Errorf("alice spark = %v, want len %d with 0.15 at [0]", a.Spark, sparkDays)
	}
}

// TestPeople_LeadScoped confirms a lead sees only their teams' members.
func TestPeople_LeadScoped(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	got, err := People(context.Background(), d, w30, aliceScope, "", fixedNow)
	if err != nil {
		t.Fatalf("People: %v", err)
	}
	m := peopleMap(got.People)
	if len(got.People) != 2 || m["u-alice"].UserID == "" || m["u-carol"].UserID == "" {
		t.Fatalf("lead people = %d (%+v), want alice + carol only", len(got.People), got.People)
	}
	if _, leaked := m["u-bob"]; leaked {
		t.Errorf("lead leaderboard leaked bob (team-b)")
	}
}

// TestPeople_MemberSeesSelfOnly confirms a plain member (no led teams) sees
// only themselves, via the selfUserID self-scope.
func TestPeople_MemberSeesSelfOnly(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	memberScope := Scope{} // not admin, no led teams
	got, err := People(context.Background(), d, w30, memberScope, "u-carol", fixedNow)
	if err != nil {
		t.Fatalf("People: %v", err)
	}
	if len(got.People) != 1 || got.People[0].UserID != "u-carol" {
		t.Fatalf("member people = %+v, want only carol", got.People)
	}
	// With no self id, a teamless member sees nothing.
	empty, err := People(context.Background(), d, w30, memberScope, "", fixedNow)
	if err != nil {
		t.Fatalf("People: %v", err)
	}
	if len(empty.People) != 0 {
		t.Errorf("teamless member with no self id = %+v, want empty", empty.People)
	}
}

// TestPeople_NoSentinelColumns is the privacy guard: the marshaled leaderboard
// carries identity (SCIM email — which ships on the wire) but never a raw
// project path or other content column.
func TestPeople_NoSentinelColumns(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	got, err := People(context.Background(), d, w30, adminScope, "", fixedNow)
	if err != nil {
		t.Fatalf("People: %v", err)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "/repo/") {
		t.Errorf("People JSON leaked a raw project path:\n%s", b)
	}
}
