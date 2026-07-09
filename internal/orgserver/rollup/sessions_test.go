package rollup

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The shared seed() fixture (rollup_test.go) builds: sessions s-a1 (alice,
// claude-code, /repo/x, 05-20), s-b1 (bob, codex, /repo/y, 05-21), s-c1 (carol,
// claude-code, /repo/y, 05-22, token_usage-only → JSONL degradation). team-a =
// {alice lead, carol member}; team-b = {bob lead}.

func sessionMap(rows []SessionRow) map[string]SessionRow {
	m := map[string]SessionRow{}
	for _, r := range rows {
		m[r.SessionID] = r
	}
	return m
}

func TestSessions_AdminListAndSpend(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	got, err := Sessions(context.Background(), d, w30, Scope{Admin: true}, "", SessionFilters{}, 50, 0, fixedNow)
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if got.Total != 3 || len(got.Sessions) != 3 {
		t.Fatalf("total/len = %d/%d, want 3/3", got.Total, len(got.Sessions))
	}
	// Newest first.
	if got.Sessions[0].SessionID != "s-c1" || got.Sessions[2].SessionID != "s-a1" {
		t.Errorf("order = %s..%s, want s-c1..s-a1 (started_at DESC)", got.Sessions[0].SessionID, got.Sessions[2].SessionID)
	}
	m := sessionMap(got.Sessions)
	// s-a1: proxy 0.10 (150 tok, 1 turn) + token_usage t-a2 0.05 (150 tok); the
	// req-a1 dup is dropped. → 0.15 / 300 / 1 turn.
	a := m["s-a1"]
	if !near(a.CostUSD, 0.15) || a.Tokens != 300 || a.APITurnCount != 1 {
		t.Errorf("s-a1 = cost %v tokens %d turns %d, want 0.15/300/1", a.CostUSD, a.Tokens, a.APITurnCount)
	}
	if a.Email != "alice@acme.example" || a.DisplayName != "Alice" || a.Tool != "claude-code" {
		t.Errorf("s-a1 identity = %q/%q/%q, want alice/Alice/claude-code", a.Email, a.DisplayName, a.Tool)
	}
	if a.ProjectID == "" || strings.Contains(a.ProjectID, "/") {
		t.Errorf("s-a1 project_id = %q, want a non-empty hash (never a path)", a.ProjectID)
	}
	// s-c1: token_usage-only (no proxy) → 0 api turns.
	if c := m["s-c1"]; c.APITurnCount != 0 || !near(c.CostUSD, 0.07) {
		t.Errorf("s-c1 = turns %d cost %v, want 0/0.07 (JSONL-only)", c.APITurnCount, c.CostUSD)
	}
}

func TestSessions_LeadScope(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	// Alice leads team-a = {alice, carol}; must see s-a1 + s-c1, never bob's s-b1.
	got, err := Sessions(context.Background(), d, w30, Scope{TeamIDs: []string{"team-a"}}, "u-alice", SessionFilters{}, 50, 0, fixedNow)
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if got.Total != 2 {
		t.Fatalf("lead total = %d, want 2", got.Total)
	}
	m := sessionMap(got.Sessions)
	if _, ok := m["s-b1"]; ok {
		t.Errorf("lead alice leaked s-b1 (team-b/bob)")
	}
	if _, ok := m["s-a1"]; !ok {
		t.Errorf("lead alice missing s-a1")
	}
}

func TestSessions_MemberSelf(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	// A plain member (no led teams) sees only their own sessions.
	got, err := Sessions(context.Background(), d, w30, Scope{}, "u-bob", SessionFilters{}, 50, 0, fixedNow)
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if got.Total != 1 || len(got.Sessions) != 1 || got.Sessions[0].SessionID != "s-b1" {
		t.Fatalf("member-self = total %d rows %d, want only s-b1", got.Total, len(got.Sessions))
	}
}

func TestSessions_FilterAndPaginate(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	// Tool filter.
	got, err := Sessions(context.Background(), d, w30, Scope{Admin: true}, "", SessionFilters{Tool: "codex"}, 50, 0, fixedNow)
	if err != nil {
		t.Fatalf("Sessions filter: %v", err)
	}
	if got.Total != 1 || got.Sessions[0].SessionID != "s-b1" {
		t.Errorf("tool=codex = total %d, want only s-b1", got.Total)
	}
	// Pagination: limit 2 → page 1 has 2, total still 3; offset 2 → 1 row.
	p1, _ := Sessions(context.Background(), d, w30, Scope{Admin: true}, "", SessionFilters{}, 2, 0, fixedNow)
	if p1.Total != 3 || len(p1.Sessions) != 2 {
		t.Errorf("page1 = total %d rows %d, want 3/2", p1.Total, len(p1.Sessions))
	}
	p2, _ := Sessions(context.Background(), d, w30, Scope{Admin: true}, "", SessionFilters{}, 2, 2, fixedNow)
	if len(p2.Sessions) != 1 {
		t.Errorf("page2 rows = %d, want 1", len(p2.Sessions))
	}
}

func TestSessionDetail_FoundAndScope(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	// Admin → s-a1 detail with proxy buckets + action-type breakdown.
	got, found, err := SessionDetail(context.Background(), d, "s-a1", Scope{Admin: true}, "", fixedNow)
	if err != nil || !found {
		t.Fatalf("SessionDetail s-a1: found=%v err=%v", found, err)
	}
	if !near(got.CostUSD, 0.15) || got.Tokens != 300 || got.APITurnCount != 1 {
		t.Errorf("s-a1 detail = %v/%d/%d, want 0.15/300/1", got.CostUSD, got.Tokens, got.APITurnCount)
	}
	if got.Buckets.NetInput != 100 || got.Buckets.Output != 50 {
		t.Errorf("s-a1 buckets = %+v, want net_input 100 / output 50", got.Buckets)
	}
	if len(got.ActionTypes) != 1 || got.ActionTypes[0].ActionType != "read_file" || got.ActionTypes[0].Count != 1 {
		t.Errorf("s-a1 action types = %+v, want read_file x1", got.ActionTypes)
	}

	// Lead alice (team-a) requesting bob's s-b1 → not found (out-of-scope ≡ 404).
	_, found2, err := SessionDetail(context.Background(), d, "s-b1", Scope{TeamIDs: []string{"team-a"}}, "u-alice", fixedNow)
	if err != nil {
		t.Fatalf("SessionDetail s-b1: %v", err)
	}
	if found2 {
		t.Errorf("lead alice resolved out-of-scope s-b1 (must be 404)")
	}
}

func TestSessionDetail_JSONLDegradation(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	// s-c1 has no proxy turns → buckets degrade to JSONL net-input/output.
	got, found, err := SessionDetail(context.Background(), d, "s-c1", Scope{Admin: true}, "", fixedNow)
	if err != nil || !found {
		t.Fatalf("SessionDetail s-c1: found=%v err=%v", found, err)
	}
	if got.APITurnCount != 0 {
		t.Errorf("s-c1 api turns = %d, want 0", got.APITurnCount)
	}
	if got.Buckets.NetInput != 100 || got.Buckets.Output != 50 || got.Buckets.CacheRead != 0 {
		t.Errorf("s-c1 buckets = %+v, want JSONL net 100/out 50/cache 0", got.Buckets)
	}
}

// TestSessions_NoSentinelColumns is the privacy guard: identity (email) is
// intentionally present (audited), but the raw project_root path and git_remote
// must never appear — project identity is the hash only.
func TestSessions_NoSentinelColumns(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	list, err := Sessions(context.Background(), d, w30, Scope{Admin: true}, "", SessionFilters{}, 50, 0, fixedNow)
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	det, _, err := SessionDetail(context.Background(), d, "s-a1", Scope{Admin: true}, "", fixedNow)
	if err != nil {
		t.Fatalf("SessionDetail: %v", err)
	}
	for _, b := range [][]byte{mustJSON(t, list), mustJSON(t, det)} {
		s := string(b)
		if strings.Contains(s, "/repo/") {
			t.Errorf("session JSON leaked a raw project_root path:\n%s", s)
		}
		if strings.Contains(s, "git_remote") || strings.Contains(s, "git@") {
			t.Errorf("session JSON leaked a git remote:\n%s", s)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
