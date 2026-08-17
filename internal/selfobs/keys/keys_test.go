package keys

import "testing"

// TestRetainedContainsEveryNamedConstOnce asserts every named scalar const
// appears exactly once in Retained (no drift between the const block and the
// registry).
func TestRetainedContainsEveryNamedConstOnce(t *testing.T) {
	t.Parallel()

	named := []string{ActorType, InitiatedBy, RunID, RunTraceID, Trigger, Component, Outcome, CostUSD, LatencyMS}

	count := map[string]int{}
	for _, k := range Retained {
		count[k.Key]++
	}
	if len(Retained) != len(named) {
		t.Fatalf("Retained has %d entries, want %d", len(Retained), len(named))
	}
	for _, k := range named {
		if count[k] != 1 {
			t.Errorf("Retained contains %q %d times, want exactly 1", k, count[k])
		}
	}
}

// TestExactRuleKeys asserts ExactRuleKeys returns exactly the 7 NeedsExactRule
// keys in the stable declaration order.
func TestExactRuleKeys(t *testing.T) {
	t.Parallel()

	want := []string{ActorType, InitiatedBy, RunID, RunTraceID, Trigger, Component, Outcome}
	got := ExactRuleKeys()

	if len(got) != len(want) {
		t.Fatalf("ExactRuleKeys() len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ExactRuleKeys()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestNonExactRuleKeys pins that CostUSD/LatencyMS are NOT in the exact-rule
// set (they are covered by generic substring rules).
func TestNonExactRuleKeys(t *testing.T) {
	t.Parallel()

	for _, k := range ExactRuleKeys() {
		if k == CostUSD || k == LatencyMS {
			t.Errorf("ExactRuleKeys() unexpectedly contains generic-rule key %q", k)
		}
	}
}
