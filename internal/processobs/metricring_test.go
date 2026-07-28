package processobs

import (
	"testing"
	"time"
)

// ringTestAttributor builds an Attributor with an already-exec'd, attributed
// run at pid 100 and the given ring policy, returning the attributor and the
// base timestamp.
func ringTestAttributor(t *testing.T, p MetricPolicy) (*Attributor, time.Time) {
	t.Helper()
	base := time.Unix(1_700_000_000, 0).UTC()
	a := NewAttributor(bridgeSeed(100, "s", "claude-code", 1), nil, nil)
	a.SetMetricPolicy(p)
	a.Observe(RawEvent{
		Type: EventExec, BootID: "b", PID: 100, PPID: 1, StartTimeTicks: 1000,
		HasStartTime: true, HasMetrics: true, Timestamp: base,
		ExePath: "/usr/bin/claude", WorkingSetBytes: 1,
	}, nil)
	return a, base
}

func ringMetricEvent(ts time.Time, ws int64) RawEvent {
	return RawEvent{
		Type: EventMetrics, BootID: "b", PID: 100, StartTimeTicks: 1000,
		HasStartTime: true, HasMetrics: true, Timestamp: ts, WorkingSetBytes: ws,
	}
}

// ringOf returns the live ring for the tracked pid-100 run (the Observe return
// value is nil whenever the persist throttle suppresses a write, so tests read
// the tracked run directly).
func ringOf(a *Attributor) []MetricSample {
	return a.runs[ProcessKey("b", 100, 1000)].MetricSamples
}

// TestMetricRingWindowEviction pins the bounded window: at a 1s sample rate
// with a 10s window the ring holds ~11 points forever, and the OLDEST point is
// always the one dropped — a high sample rate must never grow the buffer
// without bound.
func TestMetricRingWindowEviction(t *testing.T) {
	t.Parallel()
	a, base := ringTestAttributor(t, MetricPolicy{
		SampleInterval: time.Second,
		Window:         10 * time.Second,
		MaxSamples:     1000, // deliberately loose: the WINDOW must be the bound
	})

	for i := 1; i <= 300; i++ {
		a.Observe(ringMetricEvent(base.Add(time.Duration(i)*time.Second), int64(i)), nil)
	}
	ring := ringOf(a)
	if len(ring) > 11 {
		t.Fatalf("window eviction failed: %d points retained for a 10s/1s window", len(ring))
	}
	newest := ring[len(ring)-1]
	if newest.WorkingSet != 300 {
		t.Errorf("newest point must be the latest sample, got ws=%d", newest.WorkingSet)
	}
	oldest := ring[0]
	if oldest.T.Before(newest.T.Add(-10 * time.Second)) {
		t.Errorf("point older than the window survived: %s vs newest %s", oldest.T, newest.T)
	}
}

// TestMetricRingWindowBoundary pins eviction exactly at the boundary: a point
// exactly Window old is retained, one older is dropped.
func TestMetricRingWindowBoundary(t *testing.T) {
	t.Parallel()
	p := MetricPolicy{SampleInterval: time.Second, Window: 10 * time.Second, MaxSamples: 1000}
	newest := time.Unix(1_700_000_100, 0).UTC()
	in := []MetricSample{
		{T: newest.Add(-11 * time.Second), WorkingSet: 1}, // outside → dropped
		{T: newest.Add(-10 * time.Second), WorkingSet: 2}, // exactly at the edge → kept
		{T: newest, WorkingSet: 3},
	}
	got := evictMetricSamples(in, newest, p)
	if len(got) != 2 || got[0].WorkingSet != 2 || got[1].WorkingSet != 3 {
		t.Fatalf("boundary eviction wrong: %+v", got)
	}
}

// TestMetricRingHighFrequencyBounded pins the memory bound under a pathological
// configuration: a huge window with a tiny sample interval is still clamped by
// MaxSamples.
func TestMetricRingHighFrequencyBounded(t *testing.T) {
	t.Parallel()
	a, base := ringTestAttributor(t, MetricPolicy{
		SampleInterval: 100 * time.Millisecond,
		Window:         24 * time.Hour,
		MaxSamples:     50,
	})
	for i := 1; i <= 5000; i++ {
		a.Observe(ringMetricEvent(base.Add(time.Duration(i)*100*time.Millisecond), int64(i)), nil)
	}
	if got := len(ringOf(a)); got != 50 {
		t.Fatalf("MaxSamples not enforced: %d points", got)
	}
}

// TestMetricPersistDecoupledFromSampling is the core of decision 1: the ring
// must keep sampling at the fast cadence while the DB write (ChangeUpdated)
// only fires once per PersistInterval.
func TestMetricPersistDecoupledFromSampling(t *testing.T) {
	t.Parallel()
	a, base := ringTestAttributor(t, MetricPolicy{
		SampleInterval:  time.Second,
		Window:          5 * time.Minute,
		MaxSamples:      300,
		PersistInterval: 15 * time.Second,
	})

	persists := 0
	const seconds = 60
	for i := 1; i <= seconds; i++ {
		if _, ch := a.Observe(ringMetricEvent(base.Add(time.Duration(i)*time.Second), int64(i)), nil); ch == ChangeUpdated {
			persists++
		}
	}
	if got := len(ringOf(a)); got != seconds+1 { // +1 for the exec sample
		t.Fatalf("sampling should be unaffected by the persist throttle: %d points, want %d", got, seconds+1)
	}
	// 60s at one persist per 15s.
	if persists != 4 {
		t.Fatalf("persist cadence = %d writes in 60s, want 4 (one per 15s)", persists)
	}
}

// TestMetricPersistEveryRefreshWhenUnset pins the back-compat escape hatch:
// PersistInterval <= 0 restores "persist on every refresh".
func TestMetricPersistEveryRefreshWhenUnset(t *testing.T) {
	t.Parallel()
	a, base := ringTestAttributor(t, MetricPolicy{SampleInterval: time.Second, Window: time.Hour})
	for i := 1; i <= 5; i++ {
		if _, ch := a.Observe(ringMetricEvent(base.Add(time.Duration(i)*time.Second), int64(i)), nil); ch != ChangeUpdated {
			t.Fatalf("refresh %d: change = %v, want ChangeUpdated", i, ch)
		}
	}
}

// TestMetricSnapshotForPersistDownsamples pins the persisted view: it is capped,
// keeps the OLDEST and (load-bearing for a live chart) the NEWEST point exactly,
// and never aliases the live ring.
func TestMetricSnapshotForPersistDownsamples(t *testing.T) {
	t.Parallel()
	a, base := ringTestAttributor(t, MetricPolicy{
		SampleInterval:    time.Second,
		Window:            time.Hour,
		MaxSamples:        1000,
		PersistMaxSamples: 10,
	})
	for i := 1; i <= 200; i++ {
		a.Observe(ringMetricEvent(base.Add(time.Duration(i)*time.Second), int64(i)), nil)
	}
	run := a.runs[ProcessKey("b", 100, 1000)]
	snap := a.SnapshotForPersist(run)
	if len(snap.MetricSamples) != 10 {
		t.Fatalf("persisted ring = %d points, want 10", len(snap.MetricSamples))
	}
	live := ringOf(a)
	if snap.MetricSamples[9] != live[len(live)-1] {
		t.Errorf("newest point must survive downsampling exactly: %+v vs %+v",
			snap.MetricSamples[9], live[len(live)-1])
	}
	if snap.MetricSamples[0] != live[0] {
		t.Errorf("oldest point must survive downsampling exactly: %+v vs %+v",
			snap.MetricSamples[0], live[0])
	}
	// The snapshot must be a fresh slice: mutating it cannot disturb the ring.
	snap.MetricSamples[0].WorkingSet = -1
	if live[0].WorkingSet == -1 {
		t.Error("snapshot aliases the live ring")
	}
}

// TestDownsampleMetricSamples is the table-driven unit for the downsampler.
func TestDownsampleMetricSamples(t *testing.T) {
	t.Parallel()
	mk := func(n int) []MetricSample {
		out := make([]MetricSample, n)
		for i := range out {
			out[i] = MetricSample{WorkingSet: int64(i)}
		}
		return out
	}
	tests := []struct {
		name     string
		in       []MetricSample
		max      int
		wantLen  int
		wantLast int64
	}{
		{name: "empty", in: nil, max: 5, wantLen: 0},
		{name: "under cap", in: mk(3), max: 5, wantLen: 3, wantLast: 2},
		{name: "at cap", in: mk(5), max: 5, wantLen: 5, wantLast: 4},
		{name: "over cap", in: mk(100), max: 10, wantLen: 10, wantLast: 99},
		{name: "cap of one keeps newest", in: mk(100), max: 1, wantLen: 1, wantLast: 99},
		{name: "no cap copies all", in: mk(7), max: 0, wantLen: 7, wantLast: 6},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := downsampleMetricSamples(tt.in, tt.max)
			if len(got) != tt.wantLen {
				t.Fatalf("len = %d, want %d", len(got), tt.wantLen)
			}
			if tt.wantLen > 0 && got[len(got)-1].WorkingSet != tt.wantLast {
				t.Errorf("newest = %d, want %d", got[len(got)-1].WorkingSet, tt.wantLast)
			}
		})
	}
}

// TestMetricSampleNetworkMeasuredFlag pins the honest-degradation contract: a
// sample only carries network bytes when the backend measured them, and an
// unmeasured sample is distinguishable from a measured zero.
func TestMetricSampleNetworkMeasuredFlag(t *testing.T) {
	t.Parallel()
	a, base := ringTestAttributor(t, MetricPolicy{SampleInterval: time.Second, Window: time.Hour})

	// Unmeasured: HasNetworkMetrics false.
	a.Observe(ringMetricEvent(base.Add(time.Second), 10), nil)
	ring := ringOf(a)
	if last := ring[len(ring)-1]; last.NetMeasured || last.NetRxBytes != 0 || last.NetTxBytes != 0 {
		t.Fatalf("unmeasured sample must not claim bytes: %+v", last)
	}

	// Measured zero: the process is watched and simply moved nothing.
	ev := ringMetricEvent(base.Add(2*time.Second), 20)
	ev.HasNetworkMetrics = true
	a.Observe(ev, nil)
	ring = ringOf(a)
	if last := ring[len(ring)-1]; !last.NetMeasured || last.NetRxBytes != 0 {
		t.Fatalf("measured-zero sample must be flagged measured: %+v", last)
	}

	// Measured with traffic — cumulative counters carried straight through.
	ev = ringMetricEvent(base.Add(3*time.Second), 30)
	ev.HasNetworkMetrics = true
	ev.NetworkBytesIn, ev.NetworkBytesOut = 4096, 512
	a.Observe(ev, nil)
	ring = ringOf(a)
	if last := ring[len(ring)-1]; !last.NetMeasured || last.NetRxBytes != 4096 || last.NetTxBytes != 512 {
		t.Fatalf("measured bytes not carried: %+v", last)
	}
}

// TestNetworkAccountingStatus pins the status handle used to tell the UI
// "unmeasured" from "zero", including the nil-receiver default.
func TestNetworkAccountingStatus(t *testing.T) {
	t.Parallel()
	var nilStatus *NetworkAccounting
	if mode, _ := nilStatus.Status(); mode != NetworkAccountingOff {
		t.Errorf("nil status mode = %q, want %q", mode, NetworkAccountingOff)
	}
	nilStatus.Set(NetworkAccountingTCP, "") // must not panic

	n := &NetworkAccounting{}
	if mode, _ := n.Status(); mode != NetworkAccountingOff {
		t.Errorf("zero-value mode = %q, want %q", mode, NetworkAccountingOff)
	}
	n.Set(NetworkAccountingUnavailable, "missing CAP_BPF")
	mode, reason := n.Status()
	if mode != NetworkAccountingUnavailable || reason != "missing CAP_BPF" {
		t.Errorf("status = (%q, %q)", mode, reason)
	}
}

// TestMetricPolicyDefaults pins the shipped cadences and the derived ring cap —
// the numbers the write-amplification arithmetic in the docs depends on.
func TestMetricPolicyDefaults(t *testing.T) {
	t.Parallel()
	p := DefaultMetricPolicy()
	if p.SampleInterval != 2*time.Second || p.Window != 5*time.Minute {
		t.Fatalf("unexpected sampling defaults: %+v", p)
	}
	if p.PersistInterval != 15*time.Second || p.PersistMaxSamples != 60 {
		t.Fatalf("unexpected persist defaults: %+v", p)
	}
	// 5 min / 2s + 1 = 151 points, under the 300 hard cap.
	if got := p.ringCap(); got != 151 {
		t.Errorf("ringCap = %d, want 151", got)
	}
	// An Attributor that was never handed a policy uses these.
	a := NewAttributor(nil, nil, nil)
	if a.metricPolicy() != p {
		t.Errorf("unset policy = %+v, want the defaults", a.metricPolicy())
	}
}
