package evalcore

import "testing"

// TestSmoke is a thin sanity check that the moved registry/Run/Summarize
// pipeline still works end to end after extraction. The full behavioral
// suite (every scorer, every edge case) stays in internal/obs/eval, which
// now exercises this package through its re-exported aliases — see
// docs/plans/org-eval-service-comprehensive-plan-2026-08-20.md §4 Wave 1.
func TestSmoke(t *testing.T) {
	sc, err := Build(Spec{Name: "exact_match"}, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	results := Run(t.Context(), []Sample{{ItemID: 1, Output: "hi", Reference: "hi"}}, []Scorer{sc})
	sum := Summarize(results)
	if sum.Total != 1 || sum.Passed != 1 || sum.PassRate() != 1 {
		t.Fatalf("summary = %+v, want total 1 passed 1 rate 1", sum)
	}
}
