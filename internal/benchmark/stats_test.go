package benchmark

import (
	"math"
	"testing"
)

func approx(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestWilson(t *testing.T) {
	t.Parallel()
	// Known reference: 8/10 successes, 95% Wilson ≈ [0.490, 0.943].
	iv := Wilson(8, 10, z95)
	if !approx(iv.Point, 0.8, 1e-9) {
		t.Errorf("point = %v", iv.Point)
	}
	if !approx(iv.Lo, 0.4902, 0.002) || !approx(iv.Hi, 0.9431, 0.002) {
		t.Errorf("Wilson 8/10 = [%.4f,%.4f], want ~[0.490,0.943]", iv.Lo, iv.Hi)
	}
	// n=0 → zero interval, no NaN.
	if z := Wilson(0, 0, z95); z.Lo != 0 || z.Hi != 0 || z.Point != 0 {
		t.Errorf("empty Wilson = %+v", z)
	}
	// Perfect success stays <= 1.
	if iv := Wilson(5, 5, z95); iv.Hi > 1 || iv.Lo < 0 {
		t.Errorf("5/5 out of range: %+v", iv)
	}
}

func TestNewcombeDiff(t *testing.T) {
	t.Parallel()
	// Equal proportions → diff point 0, CI straddles 0.
	iv := NewcombeDiff(5, 10, 5, 10, z95)
	if !approx(iv.Point, 0, 1e-9) {
		t.Errorf("diff point = %v", iv.Point)
	}
	if iv.Lo > 0 || iv.Hi < 0 {
		t.Errorf("equal props CI should straddle 0: %+v", iv)
	}
	// Candidate strictly better.
	up := NewcombeDiff(10, 10, 5, 10, z95)
	if up.Point <= 0 {
		t.Errorf("expected positive diff, got %+v", up)
	}
}

func TestClassifyVerdict(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   verdictInput
		want Verdict
	}{
		{"below floor", verdictInput{candN: 2, baseN: 10, minSample: 5, margin: 0.1, diffLo: 0, diffHi: 0}, VerdictInconclusive},
		{"below task floor", verdictInput{candN: 10, baseN: 10, minSample: 5, distinctTasks: 2, minDistinctTasks: 3, margin: 0.1, diffLo: -0.05, diffHi: 0.1, cheaper: true}, VerdictInsufficientDistinctTasks},
		{"task floor met", verdictInput{candN: 10, baseN: 10, minSample: 5, distinctTasks: 3, minDistinctTasks: 3, margin: 0.1, diffLo: -0.05, diffHi: 0.1, cheaper: true}, VerdictCheaperNonInferior},
		{"cheaper noninferior", verdictInput{candN: 10, baseN: 10, minSample: 5, margin: 0.1, diffLo: -0.05, diffHi: 0.1, cheaper: true}, VerdictCheaperNonInferior},
		{"noninferior not cheaper", verdictInput{candN: 10, baseN: 10, minSample: 5, margin: 0.1, diffLo: -0.05, diffHi: 0.1, cheaper: false}, VerdictNoDetectedDifference},
		{"worse beyond margin", verdictInput{candN: 10, baseN: 10, minSample: 5, margin: 0.1, diffLo: -0.5, diffHi: -0.2, cheaper: true}, VerdictWorse},
		{"margin inside CI", verdictInput{candN: 10, baseN: 10, minSample: 5, margin: 0.1, diffLo: -0.3, diffHi: 0.2, cheaper: true}, VerdictInconclusive},
		{"no margin, no diff", verdictInput{candN: 10, baseN: 10, minSample: 5, margin: 0, diffLo: -0.1, diffHi: 0.1, cheaper: true}, VerdictNoDetectedDifference},
		{"no margin, worse", verdictInput{candN: 10, baseN: 10, minSample: 5, margin: 0, diffLo: -0.4, diffHi: -0.1, cheaper: true}, VerdictWorse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyVerdict(tc.in); got != tc.want {
				t.Errorf("verdict = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCostPerSuccess(t *testing.T) {
	t.Parallel()
	if v, ok := CostPerSuccess(1.0, 4); !ok || !approx(v, 0.25, 1e-9) {
		t.Errorf("cps = %v ok=%v", v, ok)
	}
	if _, ok := CostPerSuccess(1.0, 0); ok {
		t.Error("no successes → undefined (censored)")
	}
}

func TestPairedDeltaBlocked(t *testing.T) {
	t.Parallel()
	// Candidate uniformly +0.5 over baseline on both tasks → mean delta 0.5.
	pairs := []TaskPair{
		{TaskID: "a", CandPasses: 3, CandN: 4, BasePasses: 1, BaseN: 4}, // .75 - .25 = .5
		{TaskID: "b", CandPasses: 4, CandN: 4, BasePasses: 2, BaseN: 4}, // 1 - .5 = .5
	}
	iv, tasks := PairedDelta(pairs, 2000)
	if tasks != 2 || !approx(iv.Point, 0.5, 1e-9) {
		t.Errorf("paired delta = %+v tasks=%d", iv, tasks)
	}
}

func TestMedianIQR(t *testing.T) {
	t.Parallel()
	m, q1, q3 := MedianIQR([]float64{1, 2, 3, 4, 5})
	if m != 3 || q1 != 2 || q3 != 4 {
		t.Errorf("median/IQR = %v/%v/%v", m, q1, q3)
	}
	if m, _, _ := MedianIQR(nil); m != 0 {
		t.Error("empty median should be 0")
	}
}
