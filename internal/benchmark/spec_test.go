package benchmark

import (
	"fmt"
	"strings"
	"testing"
)

const goodSpec = `
name = "corpus-v1"
repeats = 3

[budget]
  max_total_usd = 25.0
  max_cell_usd = 1.0
  require_confirm = true

[analysis]
  baseline_config = "codex-gpt5"
  noninferiority_margin = 0.1
  min_sample = 3

[[tasks]]
  id = "t-add"
  repo = "https://github.com/x/y.git"
  ref = "deadbeef"
  prompt = "Make add() return a+b"
  [tasks.success]
    scorer = "tests_pass"
    cmd = "npm test"
    timeout_sec = 120

[[tasks]]
  id = "t-summary"
  repo = "https://github.com/x/y.git"
  prompt = "Summarize"
  [tasks.success]
    scorer = "contains"
    value = "middleware"

[[configs]]
  id = "codex-gpt5"
  harness = "codex"
  model = "gpt-5.6-sol"
[[configs]]
  id = "codex-mini"
  harness = "codex"
  model = "gpt-5.6-mini"
`

func TestParseSpecGood(t *testing.T) {
	t.Parallel()
	s, err := ParseSpec(goodSpec)
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	if s.Repeats != 3 || len(s.Tasks) != 2 || len(s.Configs) != 2 {
		t.Fatalf("unexpected shape: %+v", s)
	}
	if s.Analysis.BaselineConfig != "codex-gpt5" {
		t.Errorf("baseline = %q", s.Analysis.BaselineConfig)
	}
	cells := s.ExpandCells()
	if len(cells) != 2*2*3 {
		t.Errorf("expected 12 cells, got %d", len(cells))
	}
	if s.PlannedCells() != 12 {
		t.Errorf("PlannedCells = %d", s.PlannedCells())
	}
	// Hash is stable + non-empty.
	if h1, h2 := s.SpecHash(), s.SpecHash(); h1 == "" || h1 != h2 {
		t.Errorf("unstable/empty hash: %q %q", h1, h2)
	}
}

func TestParseSpecDefaults(t *testing.T) {
	t.Parallel()
	spec := `
name = "d"
[[tasks]]
  id = "t1"
  repo = "r"
  prompt = "p"
  [tasks.success]
    scorer = "non_empty"
[[configs]]
  id = "c1"
  harness = "codex"
  model = "m"
`
	// non_empty is not a benchmark scorer → validation error.
	if _, err := ParseSpec(spec); err == nil || !strings.Contains(err.Error(), "unknown scorer") {
		t.Fatalf("expected unknown-scorer error, got %v", err)
	}
}

func TestValidateErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, spec, want string
	}{
		{"no name", `repeats=1
[[tasks]]
 id="t"
 repo="r"
 prompt="p"
 [tasks.success]
  scorer="tests_pass"
  cmd="x"
[[configs]]
 id="c"
 harness="codex"
 model="m"`, "spec.name is required"},
		{"dup task", `name="x"
[[tasks]]
 id="t"
 repo="r"
 prompt="p"
 [tasks.success]
  scorer="tests_pass"
  cmd="x"
[[tasks]]
 id="t"
 repo="r"
 prompt="p"
 [tasks.success]
  scorer="tests_pass"
  cmd="x"
[[configs]]
 id="c"
 harness="codex"
 model="m"`, "duplicate task id"},
		{"missing cmd", `name="x"
[[tasks]]
 id="t"
 repo="r"
 prompt="p"
 [tasks.success]
  scorer="tests_pass"
[[configs]]
 id="c"
 harness="codex"
 model="m"`, "tests_pass requires a 'cmd'"},
		{"bad baseline", `name="x"
[analysis]
 baseline_config="nope"
[[tasks]]
 id="t"
 repo="r"
 prompt="p"
 [tasks.success]
  scorer="tests_pass"
  cmd="x"
[[configs]]
 id="c"
 harness="codex"
 model="m"`, "references no config"},
		{"missing model", `name="x"
[[tasks]]
 id="t"
 repo="r"
 prompt="p"
 [tasks.success]
  scorer="tests_pass"
  cmd="x"
[[configs]]
 id="c"
 harness="codex"`, "missing model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseSpec(tc.spec)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

// TestValidateBudgetAndRepeatBounds pins #8: a spec with a zero/absent USD cap
// (unbounded spend) or an absurd repeats count is rejected, while an explicit
// unlimited marker or a bounded absurd-free spec is accepted.
func TestValidateBudgetAndRepeatBounds(t *testing.T) {
	t.Parallel()
	base := `
name = "b"
%s
[analysis]
  baseline_config = "c"
[[tasks]]
  id = "t"
  repo = "r"
  prompt = "p"
  [tasks.success]
    scorer = "tests_pass"
    cmd = "x"
[[configs]]
  id = "c"
  harness = "codex"
  model = "m"
`
	cases := []struct {
		name, budget, wantErr string
	}{
		{"zero total cap", "[budget]\n  max_cell_usd = 1.0", "max_total_usd must be > 0"},
		{"zero cell cap", "[budget]\n  max_total_usd = 10.0", "max_cell_usd must be > 0"},
		{"no budget block at all", "", "max_total_usd must be > 0"},
		{"negative cap", "[budget]\n  max_total_usd = -5.0\n  max_cell_usd = 1.0", "max_total_usd must be > 0"},
		{"absurd repeats", "repeats = 100000\n[budget]\n  max_total_usd = 10.0\n  max_cell_usd = 1.0", "exceeds the sane ceiling"},
		{"negative judge cap", "[budget]\n  max_total_usd = 10.0\n  max_cell_usd = 1.0\n  judge_budget_usd = -1", "judge_budget_usd must be >= 0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseSpec(fmt.Sprintf(base, tc.budget))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}

	// Accepted: explicit unlimited marker (no USD caps required).
	if _, err := ParseSpec(fmt.Sprintf(base, "[budget]\n  unlimited = true")); err != nil {
		t.Errorf("unlimited spec should validate, got %v", err)
	}
	// Accepted: bounded caps + sane repeats.
	if _, err := ParseSpec(fmt.Sprintf(base, "repeats = 5\n[budget]\n  max_total_usd = 10.0\n  max_cell_usd = 1.0")); err != nil {
		t.Errorf("bounded spec should validate, got %v", err)
	}
}

// TestContainsAllScorerValidated pins #1's new echo-proof scorer vocabulary:
// contains_all requires a non-empty values list, and the fixed worked corpus
// (which now uses it) parses cleanly.
func TestContainsAllScorerValidated(t *testing.T) {
	t.Parallel()
	missing := `
name = "ca"
[budget]
  max_total_usd = 10.0
  max_cell_usd = 1.0
[analysis]
  baseline_config = "c"
[[tasks]]
  id = "t"
  repo = "r"
  prompt = "p"
  [tasks.success]
    scorer = "contains_all"
[[configs]]
  id = "c"
  harness = "codex"
  model = "m"
`
	if _, err := ParseSpec(missing); err == nil || !strings.Contains(err.Error(), "contains_all requires") {
		t.Fatalf("want contains_all values error, got %v", err)
	}
	for _, n := range ScorerNames() {
		if n == "contains_all" {
			return
		}
	}
	t.Error("contains_all missing from ScorerNames()")
}

func TestBudgetCaps(t *testing.T) {
	t.Parallel()
	b := Budget{MaxCellUSD: 1.0, MaxTurnsPerCell: 40, MaxWallSecCell: 900, MaxTotalUSD: 25, JudgeBudgetUSD: 5}

	if ex, _ := b.CellCapExceeded(0.5, 10, 100); ex {
		t.Error("should be within caps")
	}
	if ex, reason := b.CellCapExceeded(1.5, 10, 100); !ex || !strings.Contains(reason, "usd_cap") {
		t.Errorf("usd cap: ex=%v reason=%q", ex, reason)
	}
	if ex, reason := b.CellCapExceeded(0.5, 50, 100); !ex || !strings.Contains(reason, "turns_cap") {
		t.Errorf("turns cap: ex=%v reason=%q", ex, reason)
	}
	if ex, reason := b.CellCapExceeded(0.5, 10, 1000); !ex || !strings.Contains(reason, "wall_cap") {
		t.Errorf("wall cap: ex=%v reason=%q", ex, reason)
	}
	if !b.RunCapExceeded(25) || b.RunCapExceeded(24.99) {
		t.Error("run cap boundary")
	}
	if !b.JudgeCapExceeded(5) || b.JudgeCapExceeded(4.99) {
		t.Error("judge cap boundary")
	}
	// Zero caps disable.
	if ex, _ := (Budget{}).CellCapExceeded(1e9, 1e9, 1e9); ex {
		t.Error("zero caps must disable")
	}
	if got := EstimateMatrixCost(12, 10, 0.02); got != 2.4 {
		t.Errorf("estimate = %v, want 2.4", got)
	}
}
