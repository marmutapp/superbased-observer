package routing

import (
	"strings"
	"testing"
)

// readShapedEvidence is a well-sampled, clean, read-only persona on an
// opus-class model — the §R10.3 flagship case.
func readShapedEvidence() SubagentEvidence {
	return SubagentEvidence{
		Name: "code-reviewer", Model: "claude-opus-4-8", Sessions: 4,
		Actions: 312, Reads: 290, Mutations: 0, Commands: 22, Failures: 6,
	}
}

// TestRecommendSubagentModels_RuleRows exercises the emitter table one
// row at a time (§24.5).
func TestRecommendSubagentModels_RuleRows(t *testing.T) {
	t.Parallel()
	tiers := NewTierResolver().Table()
	cases := []struct {
		name          string
		ev            SubagentEvidence
		wantReason    ReasonCode
		wantSuggested string
	}{
		{
			name: "row_insufficient_evidence",
			ev: func() SubagentEvidence {
				ev := readShapedEvidence()
				ev.Actions = 12
				return ev
			}(),
			wantReason: ReasonInsufficientEvidence,
		},
		{
			name: "row_unplaceable_model",
			ev: func() SubagentEvidence {
				ev := readShapedEvidence()
				ev.Model = "mystery-model-9000"
				return ev
			}(),
			wantReason: ReasonUnclassifiedModel,
		},
		{
			name: "row_failure_heavy_hold",
			ev: func() SubagentEvidence {
				ev := readShapedEvidence()
				ev.Failures = 60 // ~19% — escalation evidence
				return ev
			}(),
			wantReason: ReasonEscalation,
		},
		{
			name:          "row_non_mutating_profile_downshift",
			ev:            readShapedEvidence(),
			wantReason:    ReasonOverpoweredSubagent,
			wantSuggested: "claude-haiku-4-5",
		},
		{
			name: "row_non_mutating_no_same_shape_candidate",
			ev: func() SubagentEvidence {
				ev := readShapedEvidence()
				ev.Model = "grok-4.3" // sonnet-class but ShapeUnknown
				return ev
			}(),
			wantReason: ReasonNoCandidate,
		},
		{
			name: "row_already_at_floor_keep",
			ev: func() SubagentEvidence {
				ev := readShapedEvidence()
				ev.Model = "claude-haiku-4-5"
				return ev
			}(),
			wantReason: ReasonNoRoute,
		},
		{
			name: "row_no_downshift_evidence_keep",
			ev: func() SubagentEvidence {
				ev := readShapedEvidence()
				ev.Mutations = 40 // write-shaped
				return ev
			}(),
			wantReason: ReasonNoRoute,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			recs := RecommendSubagentModels(tiers, []SubagentEvidence{tc.ev}, 50)
			if len(recs) != 1 {
				t.Fatalf("recs = %d, want 1", len(recs))
			}
			r := recs[0]
			if r.Reason != tc.wantReason {
				t.Errorf("reason = %s, want %s (rationale: %s)", r.Reason, tc.wantReason, r.Rationale)
			}
			if r.SuggestedModel != tc.wantSuggested {
				t.Errorf("suggested = %q, want %q", r.SuggestedModel, tc.wantSuggested)
			}
			if r.Rationale == "" {
				t.Error("empty rationale — recommendations must carry their evidence")
			}
			if r.Evidence != tc.ev {
				t.Errorf("evidence not echoed: %+v", r.Evidence)
			}
		})
	}
}

// TestRecommendSubagentModels_DownshiftRationaleCarriesEvidence pins the
// §R10.3 framing: the rationale names volume, profile, and sessions.
func TestRecommendSubagentModels_DownshiftRationaleCarriesEvidence(t *testing.T) {
	t.Parallel()
	tiers := NewTierResolver().Table()
	recs := RecommendSubagentModels(tiers, []SubagentEvidence{readShapedEvidence()}, 50)
	if len(recs) != 1 {
		t.Fatalf("recs = %d, want 1", len(recs))
	}
	for _, fragment := range []string{"312", "290", "4 session", "claude-opus-4-8"} {
		if !strings.Contains(recs[0].Rationale, fragment) {
			t.Errorf("rationale missing %q: %s", fragment, recs[0].Rationale)
		}
	}
}

// TestRecommendSubagentModels_DefaultFloor pins the minActions ≤ 0 →
// 50 default.
func TestRecommendSubagentModels_DefaultFloor(t *testing.T) {
	t.Parallel()
	tiers := NewTierResolver().Table()
	ev := readShapedEvidence()
	ev.Actions = 49
	recs := RecommendSubagentModels(tiers, []SubagentEvidence{ev}, 0)
	if len(recs) != 1 || recs[0].Reason != ReasonInsufficientEvidence {
		t.Errorf("recs = %+v, want insufficient_evidence at the default floor", recs)
	}
}

// TestSubagentRuleNames_Unique guards the emitter table.
func TestSubagentRuleNames_Unique(t *testing.T) {
	t.Parallel()
	names := SubagentRuleNames()
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			t.Errorf("duplicate rule name %q", n)
		}
		seen[n] = true
	}
	if names[len(names)-1] != "no_downshift_evidence_keep" {
		t.Errorf("last rule = %q, want the keep fallback", names[len(names)-1])
	}
}
