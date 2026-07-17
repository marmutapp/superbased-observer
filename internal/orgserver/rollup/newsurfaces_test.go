package rollup

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// These cover the 6d surfaces (Live / Movers / Report / Suggestions) against the
// shared seed() fixture: alice claude /repo/x; bob gpt|codex /repo/y; carol
// claude /repo/y. Deduped spend totals 0.45 (claude 0.22, gpt 0.23).

func TestMovers_ModelNewEntrants(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	// w30 → all seed activity falls in the current window, prior is empty, so
	// every model is a "new entrant" ranked by current spend.
	got, err := Movers(context.Background(), d, w30, Scope{Admin: true}, "model", fixedNow)
	if err != nil {
		t.Fatalf("Movers: %v", err)
	}
	if got.Dimension != "model" {
		t.Errorf("dimension = %q, want model", got.Dimension)
	}
	if len(got.NewEntrants) != 2 || got.NewEntrants[0].Key != "gpt" {
		t.Fatalf("new entrants = %+v, want [gpt, claude] (gpt first by current spend)", got.NewEntrants)
	}
	if !near(got.NewEntrants[0].CurrentUSD, 0.23) || got.NewEntrants[0].PriorUSD != 0 {
		t.Errorf("gpt entrant = cur %v prior %v, want 0.23/0", got.NewEntrants[0].CurrentUSD, got.NewEntrants[0].PriorUSD)
	}
	if len(got.Increases) != 0 || len(got.Decreases) != 0 {
		t.Errorf("expected no increases/decreases when prior window is empty")
	}
}

func TestMovers_ProjectKeysAreHashes(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	got, err := Movers(context.Background(), d, w30, Scope{Admin: true}, "project", fixedNow)
	if err != nil {
		t.Fatalf("Movers project: %v", err)
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "/repo/") {
		t.Errorf("Movers leaked a raw project path:\n%s", string(b))
	}
	for _, m := range got.NewEntrants {
		if strings.Contains(m.Key, "/") {
			t.Errorf("project mover key %q is not a hash", m.Key)
		}
	}
}

func TestReport_MonthlyTotalsAndPrivacy(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	got, err := Report(context.Background(), d, Scope{Admin: true}, "2026-05", fixedNow)
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if got.Month != "2026-05" {
		t.Errorf("month = %q, want 2026-05", got.Month)
	}
	if !near(got.TotalUSD, 0.45) {
		t.Errorf("total = %v, want 0.45", got.TotalUSD)
	}
	if len(got.ByModel) != 2 || got.ByModel[0].Key != "gpt" {
		t.Errorf("by_model = %+v, want gpt(0.23) then claude(0.22)", got.ByModel)
	}
	if len(got.TopSessions) == 0 {
		t.Errorf("expected top sessions in the month")
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "/repo/") {
		t.Errorf("Report leaked a raw project path:\n%s", string(b))
	}
}

func TestReport_EmptyMonth(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	// A month with no activity → zero totals, empty (non-nil) slices.
	got, err := Report(context.Background(), d, Scope{Admin: true}, "2026-01", fixedNow)
	if err != nil {
		t.Fatalf("Report empty: %v", err)
	}
	if got.TotalUSD != 0 || len(got.ByModel) != 0 || got.ByModel == nil {
		t.Errorf("empty month = total %v, byModel %+v; want 0 / empty non-nil", got.TotalUSD, got.ByModel)
	}
}

func TestLive_OldActivityIsInactive(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	// All seed activity is days old → nothing is "active" in the 15-min window.
	got, err := Live(context.Background(), d, Scope{Admin: true}, "", fixedNow)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if got.WindowMinutes != LiveWindowMinutes {
		t.Errorf("window = %d, want %d", got.WindowMinutes, LiveWindowMinutes)
	}
	if len(got.Sessions) != 0 || got.ActiveDevs != 0 {
		t.Errorf("live = %d sessions / %d devs, want 0/0 (activity is days old)", len(got.Sessions), got.ActiveDevs)
	}
}

func TestSuggestions_ContentFree(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	got, err := Suggestions(context.Background(), d, w30, Scope{Admin: true}, fixedNow)
	if err != nil {
		t.Fatalf("Suggestions: %v", err)
	}
	if got.Suggestions == nil {
		t.Errorf("suggestions slice must be non-nil")
	}
	b, _ := json.Marshal(got)
	if s := string(b); strings.Contains(s, "/repo/") || strings.Contains(s, "@") {
		t.Errorf("Suggestions leaked content/identity:\n%s", s)
	}
}
