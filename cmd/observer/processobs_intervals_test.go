package main

import (
	"testing"

	"github.com/marmutapp/superbased-observer/internal/config"
)

// TestResolveProcessPollIntervals pins the single-knob semantics the dashboard
// "process poll rate" setting relies on: PollIntervalMS drives both the Linux
// poll backend and the Windows bridge, a non-positive value falls back to the
// 2000ms default, and BridgePollIntervalMS only diverges when explicitly set.
func TestResolveProcessPollIntervals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name             string
		poll, bridge     int
		wantPoll, wantBr int
	}{
		{"defaults", 0, 0, 2000, 2000},
		{"poll set, bridge inherits", 5000, 0, 5000, 5000},
		{"bridge override diverges", 5000, 1000, 5000, 1000},
		{"negative poll falls back to default", -1, 0, 2000, 2000},
		{"bridge inherits the resolved (not raw) poll", -1, 0, 2000, 2000},
		{"bridge set while poll defaults", 0, 750, 2000, 750},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pc := config.ProcessConfig{PollIntervalMS: tc.poll, BridgePollIntervalMS: tc.bridge}
			gotPoll, gotBr := resolveProcessPollIntervals(pc)
			if gotPoll != tc.wantPoll || gotBr != tc.wantBr {
				t.Errorf("resolveProcessPollIntervals(poll=%d, bridge=%d) = (%d, %d), want (%d, %d)",
					tc.poll, tc.bridge, gotPoll, gotBr, tc.wantPoll, tc.wantBr)
			}
		})
	}
}
