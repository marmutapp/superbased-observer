package advisor

import (
	"testing"
	"time"
)

// spikeFacts builds Facts with one session per (day-offset, inputTokens)
// pair. At flatPrice $5/M input, 1M input tokens = $5.
func spikeFacts(now time.Time, days map[int][]int64) *Facts {
	f := &Facts{Now: now, WindowDays: 14, Price: flatPrice}
	for offset, inputs := range days {
		for i, in := range inputs {
			ts := now.AddDate(0, 0, -offset).Add(time.Duration(i+1) * time.Minute)
			f.Sessions = append(f.Sessions, SessionFacts{
				ID:   time.Duration(offset).String() + "-" + ts.Format("150405"),
				Tool: "claude-code", Model: "m",
				Rows: []TurnFact{{TS: ts, Model: "m", Input: in}},
			})
		}
	}
	return f
}

// TestDetectSpendSpike pins the P6.12 rows: fires at the multiple with
// enough history, escalates severity at 2× the multiple, respects the
// $5 floor and the minimum-prior-days gate, ignores zero-activity
// days, and reports the excess (not the total) as the $ figure.
func TestDetectSpendSpike(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 11, 15, 0, 0, 0, time.UTC)
	th := DefaultThresholds()
	M := int64(1_000_000) // $5 at flat input pricing

	cases := []struct {
		name     string
		days     map[int][]int64 // day-offset → per-session input tokens
		wantFire bool
		wantSev  string
		wantUSD  float64 // expected SavingsUSD (excess)
	}{
		{
			name: "fires at 5x with 4 prior active days",
			// prior: $5 each on offsets 1..4; today $25 → 5×, excess $20.
			days:     map[int][]int64{0: {5 * M}, 1: {M}, 2: {M}, 3: {M}, 4: {M}},
			wantFire: true, wantSev: SeverityInfo, wantUSD: 20,
		},
		{
			name:     "escalates to warning at 2x the multiple",
			days:     map[int][]int64{0: {7 * M}, 1: {M}, 2: {M}, 3: {M}},
			wantFire: true, wantSev: SeverityWarning, wantUSD: 30,
		},
		{
			name:     "below the multiple stays quiet",
			days:     map[int][]int64{0: {2 * M}, 1: {M}, 2: {M}, 3: {M}},
			wantFire: false,
		},
		{
			name:     "too few prior active days",
			days:     map[int][]int64{0: {5 * M}, 1: {M}, 2: {M}},
			wantFire: false,
		},
		{
			name: "zero-activity days do not dilute the baseline",
			// Only 3 active prior days spread across 10 calendar days.
			days:     map[int][]int64{0: {5 * M}, 2: {M}, 5: {M}, 10: {M}},
			wantFire: true, wantSev: SeverityInfo, wantUSD: 20,
		},
		{
			name: "dollar floor: a 8x day under $5 is noise",
			// prior $0.5/day, today $4 → 8× but under the floor.
			days:     map[int][]int64{0: {800_000}, 1: {100_000}, 2: {100_000}, 3: {100_000}},
			wantFire: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := detectSpendSpike(spikeFacts(now, tc.days), th)
			if !tc.wantFire {
				if len(got) != 0 {
					t.Fatalf("unexpected fire: %+v", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("want one suggestion, got %d", len(got))
			}
			s := got[0]
			if s.Severity != tc.wantSev {
				t.Errorf("severity: got %s want %s", s.Severity, tc.wantSev)
			}
			if s.SavingsUSD < tc.wantUSD-0.01 || s.SavingsUSD > tc.wantUSD+0.01 {
				t.Errorf("excess: got %f want %f", s.SavingsUSD, tc.wantUSD)
			}
			if s.DedupKey != "spend_spike|2026-06-11" {
				t.Errorf("dedup key: %q", s.DedupKey)
			}
			if s.Action == nil || s.Action.Target != "/live" {
				t.Errorf("action: %+v", s.Action)
			}
		})
	}
}
