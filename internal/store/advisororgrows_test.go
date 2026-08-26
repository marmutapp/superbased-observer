package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/intelligence/advisor"
	"github.com/marmutapp/superbased-observer/internal/orgcontract"
)

// TestSelectAdvisorSuggestionRows proves the org wire rows are built from
// the node's own advisor digest via advisor.LoadDigest (not re-derived),
// including a raw path/command reference in Evidence and ScopeID, an
// attached Action, and a joined advisor_state status.
func TestSelectAdvisorSuggestionRows(t *testing.T) {
	t.Parallel()
	s, d := newTestStore(t)
	s.SetAdvisorOrgProvider(func(ctx context.Context) ([]orgcontract.AdvisorSuggestionRow, error) {
		return advisor.OrgSuggestionRows(ctx, d)
	})
	ctx := context.Background()

	rep := advisor.Report{
		Suggestions: []advisor.Suggestion{
			{
				DedupKey:   "context_balloon:sess-1",
				Detector:   "context_balloon",
				Category:   advisor.CategoryCost,
				Scope:      advisor.ScopeSession,
				ScopeID:    "sess-1",
				Severity:   advisor.SeverityAdvice,
				Title:      "Context ballooned past 500K tokens",
				Nudge:      "Consider trimming context or starting a fresh session.",
				SavingsUSD: 1.23,
				SavingsMin: 4,
				Confidence: 0.8,
				Evidence: advisor.Evidence{
					Numbers: map[string]float64{"balloon_tokens": 512000},
					Items: []advisor.EvidenceItem{
						{Label: "/repo/internal/big_module.go", Value: 128000, Unit: "tokens"},
					},
					Math: "512000 * 0.7 haircut",
				},
				ComputedAt: "2026-08-24T12:00:00Z",
				WindowDays: 7,
				Action: &advisor.Action{
					Kind: "settings_section", Target: "compression", Label: "Review compression settings",
				},
			},
			{
				DedupKey:   "trivial_routing:proj-x",
				Detector:   "trivial_routing",
				Category:   advisor.CategoryCost,
				Scope:      advisor.ScopeProject,
				ScopeID:    "/repo/proj-x",
				Severity:   advisor.SeverityInfo,
				Title:      "`go test ./...` never passed",
				Nudge:      "Route trivial turns to a cheaper model.",
				Confidence: 0.5,
				Evidence:   advisor.Evidence{Math: "no numbers"},
				ComputedAt: "2026-08-24T12:00:00Z",
				WindowDays: 7,
			},
		},
		TotalCount:      2,
		TotalSavingsUSD: 1.23,
		WindowDays:      7,
		GeneratedAt:     "2026-08-24T12:05:00Z",
		SessionsScanned: 3,
	}
	if err := advisor.SaveDigest(ctx, d, rep, 10); err != nil {
		t.Fatalf("SaveDigest: %v", err)
	}
	now := time.Date(2026, 8, 24, 12, 3, 0, 0, time.UTC)
	if err := advisor.SetState(ctx, d, "trivial_routing:proj-x", advisor.StatusActed, time.Time{}, now); err != nil {
		t.Fatalf("SetState: %v", err)
	}

	rows, err := s.SelectAdvisorSuggestionRows(ctx)
	if err != nil {
		t.Fatalf("SelectAdvisorSuggestionRows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(rows), rows)
	}

	var balloon, trivial *orgcontract.AdvisorSuggestionRow
	for i := range rows {
		switch rows[i].SuggestionKey {
		case "context_balloon:sess-1":
			balloon = &rows[i]
		case "trivial_routing:proj-x":
			trivial = &rows[i]
		}
	}
	if balloon == nil || trivial == nil {
		t.Fatalf("missing expected rows: %+v", rows)
	}

	if balloon.ScopeID != "sess-1" || balloon.Scope != advisor.ScopeSession {
		t.Errorf("balloon scope = %q/%q, want session/sess-1", balloon.Scope, balloon.ScopeID)
	}
	if balloon.SavingsUSD != 1.23 || balloon.Confidence != 0.8 {
		t.Errorf("balloon savings/confidence = %v/%v, want 1.23/0.8", balloon.SavingsUSD, balloon.Confidence)
	}
	if balloon.ActionKind != "settings_section" || balloon.ActionTarget != "compression" || balloon.ActionLabel != "Review compression settings" {
		t.Errorf("balloon action = %+v", balloon)
	}
	if balloon.GeneratedAt != "2026-08-24T12:05:00Z" {
		t.Errorf("GeneratedAt = %q, want digest generated_at", balloon.GeneratedAt)
	}
	if balloon.Status != "" {
		t.Errorf("balloon Status = %q, want empty (no advisor_state row)", balloon.Status)
	}

	var ev advisor.Evidence
	if err := json.Unmarshal([]byte(balloon.EvidenceJSON), &ev); err != nil {
		t.Fatalf("EvidenceJSON did not decode: %v", err)
	}
	if len(ev.Items) != 1 || ev.Items[0].Label != "/repo/internal/big_module.go" {
		t.Errorf("Evidence path not carried raw: %+v", ev)
	}
	if ev.Numbers["balloon_tokens"] != 512000 {
		t.Errorf("Evidence numbers not carried: %+v", ev)
	}

	// Command text in Title must survive verbatim (§0 enterprise-raw posture).
	if trivial.Title != "`go test ./...` never passed" {
		t.Errorf("trivial Title = %q, want raw command text preserved", trivial.Title)
	}
	if trivial.ScopeID != "/repo/proj-x" {
		t.Errorf("trivial ScopeID = %q, want raw project path preserved", trivial.ScopeID)
	}
	// This suggestion's status was set to Acted after the digest was saved —
	// SelectAdvisorSuggestionRows must join advisor_state live, not just
	// re-emit whatever the digest snapshot itself carried.
	if trivial.Status != advisor.StatusActed {
		t.Errorf("trivial Status = %q, want %q", trivial.Status, advisor.StatusActed)
	}
	if trivial.ActionKind != "" {
		t.Errorf("trivial ActionKind = %q, want empty (nil Action)", trivial.ActionKind)
	}
}

// TestSelectAdvisorSuggestionRows_NoDigestYet proves a fresh node with no
// advisor_digest row returns an empty slice, not an error.
func TestSelectAdvisorSuggestionRows_NoDigestYet(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()

	rows, err := s.SelectAdvisorSuggestionRows(ctx)
	if err != nil {
		t.Fatalf("SelectAdvisorSuggestionRows: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected no rows, got %+v", rows)
	}
}
