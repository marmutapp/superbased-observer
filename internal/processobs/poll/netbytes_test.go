package poll

import (
	"testing"

	"github.com/marmutapp/superbased-observer/internal/processobs"
)

// TestApplyNetworkBytes is the table-driven pin on the injected network
// sampler: the poll backend NEVER invents byte counts. It stamps them only
// when a sampler is injected AND reports accounting is live, and the
// HasNetworkMetrics flag is what downstream uses to tell a measured zero from
// an unmeasured one.
func TestApplyNetworkBytes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		fn          processobs.NetworkBytesFunc
		wantFlag    bool
		wantIn      int64
		wantOut     int64
		wantSampled bool // whether the sampler was consulted at all
	}{
		{
			name:     "no sampler injected leaves the event unmeasured",
			fn:       nil,
			wantFlag: false,
		},
		{
			name:        "accounting not live leaves the event unmeasured",
			fn:          func(int) (int64, int64, bool) { return 0, 0, false },
			wantFlag:    false,
			wantSampled: true,
		},
		{
			name:        "measured zero is flagged measured",
			fn:          func(int) (int64, int64, bool) { return 0, 0, true },
			wantFlag:    true,
			wantSampled: true,
		},
		{
			name:        "measured bytes are carried verbatim",
			fn:          func(int) (int64, int64, bool) { return 4096, 512, true },
			wantFlag:    true,
			wantIn:      4096,
			wantOut:     512,
			wantSampled: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			called := false
			fn := tt.fn
			if fn != nil {
				inner := fn
				fn = func(pid int) (int64, int64, bool) { called = true; return inner(pid) }
			}
			b := New(Options{Enumerate: func() ([]ProcInfo, error) { return nil, nil }, NetworkBytes: fn})
			p := pi(100, 1, 1000, "/usr/bin/claude")
			ev := b.metricsEvent(&p, tPoll)
			if ev.HasNetworkMetrics != tt.wantFlag {
				t.Fatalf("HasNetworkMetrics = %v, want %v", ev.HasNetworkMetrics, tt.wantFlag)
			}
			if ev.NetworkBytesIn != tt.wantIn || ev.NetworkBytesOut != tt.wantOut {
				t.Errorf("bytes = (%d,%d), want (%d,%d)", ev.NetworkBytesIn, ev.NetworkBytesOut, tt.wantIn, tt.wantOut)
			}
			if called != tt.wantSampled {
				t.Errorf("sampler called = %v, want %v", called, tt.wantSampled)
			}
		})
	}
}

// TestNetworkBytesStampedOnEveryMetricBearingEvent pins that the exec (first
// point of the chart) and exit (last point) events carry the series too, not
// just the mid-life refreshes.
func TestNetworkBytesStampedOnEveryMetricBearingEvent(t *testing.T) {
	t.Parallel()
	b := New(Options{
		Enumerate:    func() ([]ProcInfo, error) { return nil, nil },
		NetworkBytes: func(int) (int64, int64, bool) { return 7, 9, true },
	})
	p := pi(100, 1, 1000, "/usr/bin/claude")
	for name, ev := range map[string]processobs.RawEvent{
		"exec":    b.execEvent(&p, tPoll),
		"metrics": b.metricsEvent(&p, tPoll),
		"exit":    b.exitEvent(&p, tPoll),
	} {
		if !ev.HasNetworkMetrics || ev.NetworkBytesIn != 7 || ev.NetworkBytesOut != 9 {
			t.Errorf("%s event missing the network series: %+v", name, ev)
		}
	}
}
