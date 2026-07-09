package hermes

import (
	"math"
	"testing"
	"time"
)

// TestUnixFloatToTime_RoundTrips pins the canonical Unix-epoch-float
// to time.Time conversion across representative inputs the live
// Hermes corpus emits. The §0.5 reality check confirmed Hermes writes
// REAL columns (Unix seconds with fractional milliseconds).
//
// `wantNS` is the time component we expect (UTC). Asserts within 1
// microsecond of the float to absorb float64 representation noise on
// the seconds → nanoseconds conversion.
func TestUnixFloatToTime_RoundTrips(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input float64
		want  time.Time
	}{
		{
			name:  "epoch_zero",
			input: 0,
			want:  time.Unix(0, 0).UTC(),
		},
		{
			name:  "integer_second",
			input: 1717500000,
			want:  time.Unix(1717500000, 0).UTC(),
		},
		{
			name:  "millisecond_fraction",
			input: 1717500000.123,
			want:  time.Unix(1717500000, 123_000_000).UTC(),
		},
		{
			name:  "microsecond_fraction",
			input: 1717500000.123456,
			want:  time.Unix(1717500000, 123_456_000).UTC(),
		},
		{
			name:  "live_fixture_timestamp",
			input: 1748700029.0, // approx of testdata/hermes/ session 20260605_154029_*
			want:  time.Unix(1748700029, 0).UTC(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := unixFloatToTime(tc.input)
			delta := got.Sub(tc.want)
			if delta < 0 {
				delta = -delta
			}
			if delta > time.Microsecond {
				t.Errorf("unixFloatToTime(%v) = %v, want %v (delta %v)", tc.input, got, tc.want, delta)
			}
		})
	}
}

// TestUnixFloatToTime_GuardsAgainstInvalidInputs pins the defensive
// branches in unixFloatToTime. A corrupt fixture should not panic;
// it should yield a zero time.Time that downstream code can detect
// via IsZero().
func TestUnixFloatToTime_GuardsAgainstInvalidInputs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input float64
	}{
		{"negative", -1},
		{"NaN", math.NaN()},
		{"PositiveInf", math.Inf(1)},
		{"NegativeInf", math.Inf(-1)},
		{"AbsurdlyFarFuture", 1e20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := unixFloatToTime(tc.input)
			if !got.IsZero() {
				t.Errorf("unixFloatToTime(%v) = %v, want zero time", tc.input, got)
			}
		})
	}
}
