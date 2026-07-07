package cachewarm

import (
	"testing"
	"time"
)

func win(secs int, value float64, authoritative bool) CacheWindow {
	return CacheWindow{
		Model:               "claude-opus-4-8",
		PrefixTokens:        20000,
		TTLTier:             "5m",
		ExpiresAt:           time.Unix(1_000_000, 0).Add(time.Duration(secs) * time.Second),
		ExpiryAuthoritative: authoritative,
		ValueAtRiskUSD:      value,
	}
}

func TestClassify_SeverityBuckets(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	cases := []struct {
		name      string
		secsLeft  int
		want      Severity
		wantEstim bool
		authorit  bool
	}{
		{"comfortable headroom is ok", 600, SeverityOK, false, true},
		{"just inside warn window is soon", 80, SeveritySoon, false, true},
		{"at warn boundary is soon", 90, SeveritySoon, false, true},
		{"just inside critical window is critical", 20, SeverityCritical, false, true},
		{"at critical boundary is critical", 30, SeverityCritical, false, true},
		{"already expired is cold", -5, SeverityCold, false, true},
		{"estimated expiry flags Estimated", 80, SeveritySoon, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(ClassifyInput{
				Windows:     []CacheWindow{win(tc.secsLeft, 1.0, tc.authorit)},
				Now:         now,
				IncludeCold: true,
			})
			if len(got) != 1 {
				t.Fatalf("got %d warnings, want 1", len(got))
			}
			if got[0].Severity != tc.want {
				t.Errorf("severity = %q, want %q", got[0].Severity, tc.want)
			}
			if got[0].Estimated != tc.wantEstim {
				t.Errorf("estimated = %v, want %v", got[0].Estimated, tc.wantEstim)
			}
		})
	}
}

func TestClassify_DefaultThresholdsApplyWhenZero(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	// 80s left with zero thresholds → DefaultWarnAt(90s)/DefaultCriticalAt(30s)
	// → soon.
	got := Classify(ClassifyInput{
		Windows: []CacheWindow{win(80, 1.0, true)},
		Now:     now,
	})
	if len(got) != 1 || got[0].Severity != SeveritySoon {
		t.Fatalf("want one SeveritySoon with default thresholds, got %+v", got)
	}
}

func TestClassify_InvertedThresholdsAreClamped(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	// CriticalAt > WarnAt is nonsense; clamp critical down to warn so a
	// "soon" can't also read "critical". 50s left, warn=40s → ok? No: 50>40
	// → ok. Use 30s left: critical clamped to 40s → critical.
	got := Classify(ClassifyInput{
		Windows:    []CacheWindow{win(30, 1.0, true)},
		Now:        now,
		WarnAt:     40 * time.Second,
		CriticalAt: 120 * time.Second, // inverted; clamped to 40s
	})
	if len(got) != 1 || got[0].Severity != SeverityCritical {
		t.Fatalf("want SeverityCritical after clamp, got %+v", got)
	}
}

func TestClassify_MinValueSuppression(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	got := Classify(ClassifyInput{
		Windows: []CacheWindow{
			win(20, 0.01, true), // below floor → dropped
			win(20, 0.50, true), // above floor → kept
		},
		Now:         now,
		MinValueUSD: 0.05,
	})
	if len(got) != 1 {
		t.Fatalf("got %d, want 1 (low-value window dropped)", len(got))
	}
	if got[0].ValueAtRiskUSD != 0.50 {
		t.Errorf("kept the wrong window: value = %v", got[0].ValueAtRiskUSD)
	}
}

func TestClassify_ColdExcludedByDefault(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	got := Classify(ClassifyInput{
		Windows: []CacheWindow{win(-10, 1.0, true)},
		Now:     now,
		// IncludeCold false
	})
	if len(got) != 0 {
		t.Fatalf("cold window should be excluded by default, got %d", len(got))
	}
}

func TestClassify_OrderMostUrgentFirst(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	got := Classify(ClassifyInput{
		Windows: []CacheWindow{
			win(600, 1.0, true), // ok
			win(-5, 1.0, true),  // cold (most urgent)
			win(20, 1.0, true),  // critical
			win(80, 1.0, true),  // soon
		},
		Now:         now,
		IncludeCold: true,
	})
	wantOrder := []Severity{SeverityCold, SeverityCritical, SeveritySoon, SeverityOK}
	if len(got) != len(wantOrder) {
		t.Fatalf("got %d warnings, want %d", len(got), len(wantOrder))
	}
	for i, want := range wantOrder {
		if got[i].Severity != want {
			t.Errorf("position %d = %q, want %q", i, got[i].Severity, want)
		}
	}
}

func TestClassify_TieBreakSoonestThenValue(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	// Two criticals: soonest expiry wins; equal expiry → higher value wins.
	got := Classify(ClassifyInput{
		Windows: []CacheWindow{
			win(25, 0.10, true), // critical, later, low value
			win(15, 0.10, true), // critical, sooner → first
			win(25, 0.90, true), // critical, later, high value → before the 0.10@25
		},
		Now:         now,
		IncludeCold: true,
	})
	if len(got) != 3 {
		t.Fatalf("got %d, want 3", len(got))
	}
	if got[0].SecondsToExpiry != 15 {
		t.Errorf("first should be soonest (15s), got %ds", got[0].SecondsToExpiry)
	}
	if got[1].ValueAtRiskUSD != 0.90 || got[2].ValueAtRiskUSD != 0.10 {
		t.Errorf("equal-expiry tie should favor higher value: got %v then %v", got[1].ValueAtRiskUSD, got[2].ValueAtRiskUSD)
	}
}

func TestClassify_EmptyInputIsEmptyOutput(t *testing.T) {
	got := Classify(ClassifyInput{Now: time.Unix(1_000_000, 0)})
	if len(got) != 0 {
		t.Fatalf("empty input should give empty output, got %d", len(got))
	}
}
