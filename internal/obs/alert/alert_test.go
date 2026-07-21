package alert

import (
	"testing"
	"time"
)

func TestEvaluate(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name      string
		rule      Rule
		summary   Summary
		wantFired bool
		wantValue float64
	}{
		{
			name:      "error_rate crosses gt",
			rule:      Rule{Name: "err", Metric: MetricErrorRate, Comparator: ComparatorGT, Threshold: 0.1, WindowMinutes: 15},
			summary:   Summary{ErrorRate: 0.2},
			wantFired: true, wantValue: 0.2,
		},
		{
			name:      "error_rate below threshold does not fire",
			rule:      Rule{Name: "err", Metric: MetricErrorRate, Comparator: ComparatorGT, Threshold: 0.3, WindowMinutes: 15},
			summary:   Summary{ErrorRate: 0.2},
			wantFired: false,
		},
		{
			name:      "gte fires exactly at threshold",
			rule:      Rule{Name: "err", Metric: MetricErrorRate, Comparator: ComparatorGTE, Threshold: 0.2, WindowMinutes: 15},
			summary:   Summary{ErrorRate: 0.2},
			wantFired: true, wantValue: 0.2,
		},
		{
			name:      "gt does not fire exactly at threshold",
			rule:      Rule{Name: "err", Metric: MetricErrorRate, Comparator: ComparatorGT, Threshold: 0.2, WindowMinutes: 15},
			summary:   Summary{ErrorRate: 0.2},
			wantFired: false,
		},
		{
			name:      "empty comparator defaults to gt",
			rule:      Rule{Name: "err", Metric: MetricErrorRate, Threshold: 0.1, WindowMinutes: 15},
			summary:   Summary{ErrorRate: 0.2},
			wantFired: true, wantValue: 0.2,
		},
		{
			name:      "cost_usd crosses",
			rule:      Rule{Name: "cost", Metric: MetricCostUSD, Comparator: ComparatorGT, Threshold: 5, WindowMinutes: 60},
			summary:   Summary{CostUSD: 7.5},
			wantFired: true, wantValue: 7.5,
		},
		{
			name:      "latency_p95_ms crosses",
			rule:      Rule{Name: "lat", Metric: MetricLatencyP95Ms, Comparator: ComparatorGTE, Threshold: 2000, WindowMinutes: 30},
			summary:   Summary{LatencyP95Ms: 2500},
			wantFired: true, wantValue: 2500,
		},
		{
			name:      "unknown metric is skipped",
			rule:      Rule{Name: "bogus", Metric: "made_up", Comparator: ComparatorGT, Threshold: 0, WindowMinutes: 15},
			summary:   Summary{ErrorRate: 1},
			wantFired: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Evaluate([]Input{{Rule: tc.rule, Summary: tc.summary}}, now)
			if tc.wantFired {
				if len(got) != 1 {
					t.Fatalf("want 1 fired, got %d", len(got))
				}
				if got[0].Value != tc.wantValue {
					t.Errorf("value = %v, want %v", got[0].Value, tc.wantValue)
				}
				if !got[0].FiredAt.Equal(now) {
					t.Errorf("fired_at = %v, want %v", got[0].FiredAt, now)
				}
			} else if len(got) != 0 {
				t.Fatalf("want 0 fired, got %d", len(got))
			}
		})
	}
}

func TestEvaluateCooldownSuppression(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	base := Rule{Name: "err", Metric: MetricErrorRate, Comparator: ComparatorGT, Threshold: 0.1, WindowMinutes: 15, CooldownMinutes: 30}
	snap := Summary{ErrorRate: 0.5}

	// Fired 10m ago, cooldown 30m → suppressed.
	r := base
	r.LastFired = now.Add(-10 * time.Minute)
	if got := Evaluate([]Input{{Rule: r, Summary: snap}}, now); len(got) != 0 {
		t.Fatalf("expected suppression within cooldown, got %d fired", len(got))
	}

	// Fired 31m ago, cooldown 30m → fires again.
	r.LastFired = now.Add(-31 * time.Minute)
	if got := Evaluate([]Input{{Rule: r, Summary: snap}}, now); len(got) != 1 {
		t.Fatalf("expected re-fire past cooldown, got %d fired", len(got))
	}

	// Never fired → fires.
	r.LastFired = time.Time{}
	if got := Evaluate([]Input{{Rule: r, Summary: snap}}, now); len(got) != 1 {
		t.Fatalf("expected fire on never-fired rule, got %d fired", len(got))
	}
}

func TestEffectiveCooldownMinutes(t *testing.T) {
	cases := []struct {
		name string
		rule Rule
		want int
	}{
		{"explicit cooldown wins", Rule{CooldownMinutes: 45, WindowMinutes: 15}, 45},
		{"defaults to window", Rule{WindowMinutes: 20}, 20},
		{"falls back to 5", Rule{}, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EffectiveCooldownMinutes(tc.rule); got != tc.want {
				t.Errorf("cooldown = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestCrossed(t *testing.T) {
	if !Crossed(ComparatorGTE, 1, 1) {
		t.Error("gte 1>=1 should cross")
	}
	if Crossed(ComparatorGT, 1, 1) {
		t.Error("gt 1>1 should not cross")
	}
	if !Crossed("weird", 2, 1) {
		t.Error("unknown comparator should behave as gt")
	}
}
