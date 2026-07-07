package rollup

import (
	"context"
	"testing"
)

// TestFilters_PeopleToolFilter proves the §6c-3 global tool filter actually
// constrains the substrate (not just that an empty filter is inert, which the
// unchanged golden tests already establish). The filter rides on Scope and is
// wired into the rollups whose sub-queries uniformly carry `tool` (People,
// Activity). In the shared seed() only session s-b1 (bob) has tool=codex, and
// its single proxy turn is 0.20 (bob's token_usage row is seeded
// tool=claude-code, so it is excluded).
func TestFilters_PeopleToolFilter(t *testing.T) {
	d := newDB(t)
	seed(t, d)

	// Baseline: admin People leaderboard has all three developers.
	base, err := People(context.Background(), d, w30, Scope{Admin: true}, "", fixedNow)
	if err != nil {
		t.Fatalf("People baseline: %v", err)
	}
	if len(base.People) != 3 {
		t.Fatalf("baseline people = %d, want 3", len(base.People))
	}

	// Tool filter → only the codex developer (bob), and only his codex spend
	// (0.20 proxy turn; his claude-code token_usage row is excluded).
	got, err := People(context.Background(), d, w30, Scope{Admin: true, Filters: Filters{Tool: "codex"}}, "", fixedNow)
	if err != nil {
		t.Fatalf("People tool filter: %v", err)
	}
	if len(got.People) != 1 {
		t.Fatalf("tool-filtered people = %d, want 1 (only the codex dev)", len(got.People))
	}
	p := got.People[0]
	if p.Email != "bob@acme.example" || !near(p.CostUSD, 0.20) {
		t.Errorf("tool-filtered person = %s/%v, want bob/0.20", p.Email, p.CostUSD)
	}
}
