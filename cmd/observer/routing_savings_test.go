package main

import (
	"math"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/intelligence/modelvalue"

	"github.com/marmutapp/superbased-observer/internal/routing"
	"github.com/marmutapp/superbased-observer/internal/store"
)

func savingsRows() []store.RouterDecisionDetail {
	return []store.RouterDecisionDetail{
		{
			ID: 1, OriginalModel: "claude-opus-4-8", SelectedModel: "claude-haiku-4-5",
			Applied: true, Mode: "enforce", EstSavingsUSD: 0.50, CacheForfeitUSD: 0.05,
			ProjectRoot: "/home/u/acme", Tool: "claude-code", TurnKind: "read_only",
		},
		{
			ID: 2, OriginalModel: "claude-opus-4-8", SelectedModel: "claude-haiku-4-5",
			Applied: true, Mode: "enforce", EstSavingsUSD: 0.30, CacheForfeitUSD: 0.02,
			ProjectRoot: "/home/u/acme", Tool: "claude-code", TurnKind: "read_only",
		},
		{
			ID: 3, OriginalModel: "claude-opus-4-8", SelectedModel: "claude-sonnet-4-6",
			Applied: false, Mode: "advise", EstSavingsUSD: 0.20,
			ProjectRoot: "/home/u/other", Tool: "codex", TurnKind: "edit",
		},
		{
			ID: 4, OriginalModel: "gpt-5.4", SelectedModel: "gpt-5.4",
			Applied: false, Mode: "advise", EstSavingsUSD: 0,
			ProjectRoot: "", Tool: "", TurnKind: "unknown",
		},
	}
}

// TestBuildSavingsReport pins the §R17.3 aggregation: realized vs
// would-have split, per-group sums, unattributed bucketing, tier
// grouping through the seed table, and the §R7.2 error bars.
func TestBuildSavingsReport(t *testing.T) {
	t.Parallel()
	rep := store.BuildRouterSavingsReport(savingsRows(), 30, routing.NewTierResolver().Table())
	if rep.Decisions != 4 {
		t.Fatalf("decisions = %d", rep.Decisions)
	}
	if math.Abs(rep.RealizedUSD-0.80) > 1e-9 {
		t.Errorf("realized = %v, want 0.80 (applied rows only)", rep.RealizedUSD)
	}
	if math.Abs(rep.WouldHaveUSD-0.20) > 1e-9 {
		t.Errorf("would-have = %v, want 0.20 (unapplied changed rows only)", rep.WouldHaveUSD)
	}
	if rep.ByProject[0].Key != "acme" || rep.ByProject[0].Reroutes != 2 {
		t.Errorf("top project group = %+v", rep.ByProject[0])
	}
	foundUnattr := false
	for _, g := range rep.ByProject {
		if g.Key == "(unattributed)" {
			foundUnattr = true
		}
	}
	if !foundUnattr {
		t.Error("unattributed bucket missing")
	}
	foundOpusTier := false
	for _, g := range rep.ByTier {
		if g.Key == "opus-class" && g.Decisions == 3 {
			foundOpusTier = true
		}
	}
	if !foundOpusTier {
		t.Errorf("opus-class tier group missing/wrong: %+v", rep.ByTier)
	}
	// Error bars: acme has n=2 savings {0.5, 0.3} → mean 0.4,
	// sd ≈ 0.1414, CI ≈ 1.96·0.1414/√2 ≈ 0.196.
	acme := rep.ByProject[0]
	if math.Abs(acme.MeanPerDecision-0.4) > 1e-9 {
		t.Errorf("acme mean = %v, want 0.4", acme.MeanPerDecision)
	}
	if math.Abs(acme.CI95PerDecision-0.196) > 0.01 {
		t.Errorf("acme CI95 = %v, want ≈0.196", acme.CI95PerDecision)
	}
}

func TestParseWindowDays(t *testing.T) {
	t.Parallel()
	if d, err := parseWindowDays("30d"); err != nil || d != 30 {
		t.Errorf("30d → %d, %v", d, err)
	}
	if d, err := parseWindowDays("7"); err != nil || d != 7 {
		t.Errorf("7 → %d, %v", d, err)
	}
	if _, err := parseWindowDays("month"); err == nil {
		t.Error("bad window accepted")
	}
}

// TestComputeRuleDemotions — one row per §R18.3 demotion behavior:
// evidence-gated regression demotes the matching rule; parity does
// not; per-project deltas never act; escalation pressure demotes;
// unbounded rules grade against every kind.
func TestComputeRuleDemotions(t *testing.T) {
	t.Parallel()
	policy, _ := routing.Compile(routing.PolicySpec{Policy: "value", RespectCache: true})

	regressDelta := func(projectID int64, kind routing.TurnKind, tier routing.Tier, verdict string) modelvalue.Delta {
		return modelvalue.Delta{ProjectID: projectID, TurnKind: kind, CandidateTier: tier, Verdict: verdict}
	}

	cases := []struct {
		name        string
		deltas      []modelvalue.Delta
		escalations map[string]int
		wantDemoted []string
	}{
		{
			name:        "regression_demotes_matching_rule",
			deltas:      []modelvalue.Delta{regressDelta(0, routing.TurnReadOnly, routing.TierHaikuClass, modelvalue.VerdictCandidateWorse)},
			wantDemoted: []string{"read_only_overpowered"},
		},
		{
			name:   "parity_never_demotes",
			deltas: []modelvalue.Delta{regressDelta(0, routing.TurnReadOnly, routing.TierHaikuClass, modelvalue.VerdictParity)},
		},
		{
			name:   "per_project_delta_never_acts_node_level",
			deltas: []modelvalue.Delta{regressDelta(7, routing.TurnReadOnly, routing.TierHaikuClass, modelvalue.VerdictCandidateWorse)},
		},
		{
			name:        "escalation_pressure_demotes",
			escalations: map[string]int{"housekeeping": 5},
			wantDemoted: []string{"housekeeping_overpowered"},
		},
		{
			name:        "escalations_below_threshold_hold",
			escalations: map[string]int{"housekeeping": 4},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rep := modelvalue.Report{Deltas: tc.deltas}
			got := computeRuleDemotions(policy, rep, tc.escalations)
			if len(got) != len(tc.wantDemoted) {
				t.Fatalf("demotions = %v, want %v", got, tc.wantDemoted)
			}
			for _, rule := range tc.wantDemoted {
				if _, ok := got[rule]; !ok {
					t.Errorf("rule %q not demoted: %v", rule, got)
				}
			}
		})
	}
}

// TestWriteDecisionCSV pins the §R19.5 node export shape: header +
// one decision per line, reason codes pipe-joined, nullable turn id.
func TestWriteDecisionCSV(t *testing.T) {
	t.Parallel()
	turnID := int64(42)
	rows := []store.RouterDecisionDetail{{
		ID: 7, APITurnID: &turnID, SessionID: "s1",
		Mode: "enforce", Channel: "B", TurnKind: "read_only",
		OriginalModel: "claude-opus-4-8", SelectedModel: "claude-haiku-4-5",
		Applied: true, ReasonCodes: []string{"overpowered_read", "availability_fallback"},
		PolicyName: "value", PolicyHash: "abc", EstSavingsUSD: 0.5,
		EstimateVersion: "p1-v1", ProjectRoot: "/x", Tool: "claude-code",
	}, {
		ID: 8, SessionID: "s2", Mode: "advise", Channel: "B", TurnKind: "edit",
		OriginalModel: "m", SelectedModel: "m", PolicyName: "value", PolicyHash: "abc",
		EstimateVersion: "p1-v1",
	}}
	var buf strings.Builder
	if err := writeDecisionCSV(&buf, rows); err != nil {
		t.Fatalf("writeDecisionCSV: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %d, want header + 2", len(lines))
	}
	if !strings.HasPrefix(lines[0], "id,timestamp,session_id,api_turn_id") {
		t.Errorf("header: %q", lines[0])
	}
	if !strings.Contains(lines[1], "overpowered_read|availability_fallback") ||
		!strings.Contains(lines[1], ",42,") {
		t.Errorf("row 1: %q", lines[1])
	}
	if !strings.Contains(lines[2], ",,advise,") {
		t.Errorf("row 2 (null turn id): %q", lines[2])
	}
}
