package benchmark

import (
	"os"
	"testing"
)

// TestWorkedCorpusParses pins that the checked-in worked spec stays valid — a
// broken corpus would make the documented operator command fail.
func TestWorkedCorpusParses(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../../testdata/benchmark/coding-corpus-v1.toml")
	if err != nil {
		t.Skipf("worked corpus not present: %v", err)
	}
	spec, err := ParseSpec(string(data))
	if err != nil {
		t.Fatalf("worked corpus failed to parse: %v", err)
	}
	if spec.PlannedCells() != 2*2*5 {
		t.Errorf("expected 20 cells, got %d", spec.PlannedCells())
	}
	if spec.Analysis.BaselineConfig != "codex-sol" || spec.Analysis.NonInferiorityMargin != 0.10 {
		t.Errorf("pre-registration not read: %+v", spec.Analysis)
	}
}
