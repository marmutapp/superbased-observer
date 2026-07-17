//go:build !no_obs

package main

import "testing"

// TestCalibratePercentiles pins the pure nearest-rank helper behind
// `observer obs admission calibrate` (admission spec §14 Q2).
func TestCalibratePercentiles(t *testing.T) {
	cases := []struct {
		name             string
		in               []int
		p50, p95, wanted int
	}{
		{"empty", nil, 0, 0, 0},
		{"single", []int{42}, 42, 42, 42},
		{"eight-ascending", []int{100, 200, 300, 400, 500, 600, 700, 800}, 400, 800, 800},
		{"unsorted", []int{800, 100, 500, 300}, 300, 800, 800},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p50, p95, mx := calibratePercentiles(c.in)
			if p50 != c.p50 || p95 != c.p95 || mx != c.wanted {
				t.Errorf("calibratePercentiles(%v) = (%d,%d,%d), want (%d,%d,%d)",
					c.in, p50, p95, mx, c.p50, c.p95, c.wanted)
			}
		})
	}
}

// TestCalibratePercentilesDoesNotMutate guards that the helper sorts a copy —
// the caller's latency slice must survive untouched.
func TestCalibratePercentilesDoesNotMutate(t *testing.T) {
	in := []int{800, 100, 500, 300}
	_, _, _ = calibratePercentiles(in)
	if in[0] != 800 || in[3] != 300 {
		t.Errorf("input slice was mutated: %v", in)
	}
}
