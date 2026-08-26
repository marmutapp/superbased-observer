package main

import (
	"testing"
	"time"
)

func TestWALWatchdogInterval(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		alertMB    int
		minutes    int
		want       time.Duration
		wantEnable bool
	}{
		{name: "enabled", alertMB: 1024, minutes: 10, want: 10 * time.Minute, wantEnable: true},
		{name: "alert disabled", alertMB: 0, minutes: 10},
		{name: "cadence zero disables", alertMB: 1024, minutes: 0},
		{name: "cadence negative disables", alertMB: 1024, minutes: -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, enabled := walWatchdogInterval(tc.alertMB, tc.minutes)
			if got != tc.want || enabled != tc.wantEnable {
				t.Fatalf("walWatchdogInterval(%d, %d) = (%s, %v), want (%s, %v)",
					tc.alertMB, tc.minutes, got, enabled, tc.want, tc.wantEnable)
			}
		})
	}
}
