package main

import (
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/processobs"
)

// TestResolveMetricPolicy pins the [observer.process.metrics] mapping,
// including the "0 = inherit the default" contract and the two knobs whose
// zero is MEANINGFUL (persist every refresh / store the whole ring).
func TestResolveMetricPolicy(t *testing.T) {
	t.Parallel()
	def := processobs.DefaultMetricPolicy()
	tests := []struct {
		name string
		in   config.ProcessMetricsConfig
		want processobs.MetricPolicy
	}{
		{
			name: "empty section inherits every default",
			in:   config.ProcessMetricsConfig{},
			want: def,
		},
		{
			name: "shipped defaults resolve to the same policy",
			in: config.ProcessMetricsConfig{
				SampleIntervalMS: 2000, WindowSeconds: 300, MaxSamples: 300,
				PersistIntervalMS: 15000, PersistMaxSamples: 60,
			},
			want: def,
		},
		{
			name: "one-second sampling with a ten-minute window",
			in: config.ProcessMetricsConfig{
				SampleIntervalMS: 1000, WindowSeconds: 600, MaxSamples: 1200,
				PersistIntervalMS: 30000, PersistMaxSamples: 120,
			},
			want: processobs.MetricPolicy{
				SampleInterval: time.Second, Window: 10 * time.Minute, MaxSamples: 1200,
				PersistInterval: 30 * time.Second, PersistMaxSamples: 120,
			},
		},
		{
			name: "negative persist knobs mean persist-every-refresh / store-everything",
			in: config.ProcessMetricsConfig{
				SampleIntervalMS: 2000, WindowSeconds: 300, MaxSamples: 300,
				PersistIntervalMS: -1, PersistMaxSamples: -1,
			},
			want: processobs.MetricPolicy{
				SampleInterval: 2 * time.Second, Window: 5 * time.Minute, MaxSamples: 300,
				PersistInterval: -1 * time.Millisecond, PersistMaxSamples: -1,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := resolveMetricPolicy(config.ProcessConfig{Metrics: tt.in}, nil)
			if got != tt.want {
				t.Errorf("resolveMetricPolicy = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestResolveMetricPolicySamplingCeiling documents the poll-rate ceiling: a
// sample interval below the poll interval is HONOURED (we never silently clamp
// the operator's config) but is warned about at start, because the poller
// cannot actually produce data that fresh.
func TestResolveMetricPolicySamplingCeiling(t *testing.T) {
	t.Parallel()
	pc := config.ProcessConfig{
		PollIntervalMS: 2000,
		Metrics:        config.ProcessMetricsConfig{SampleIntervalMS: 500},
	}
	got := resolveMetricPolicy(pc, nil)
	if got.SampleInterval != 500*time.Millisecond {
		t.Fatalf("sample interval was silently clamped to %s", got.SampleInterval)
	}
}
