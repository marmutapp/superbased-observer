package oneshot

import (
	"strings"
	"testing"
	"time"
)

func TestWindow(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		spec      string
		wantSince time.Time
		wantLabel string
		wantErr   bool
	}{
		{
			name:      "empty defaults to 30 days",
			spec:      "",
			wantSince: now.Add(-30 * 24 * time.Hour),
			wantLabel: "last 30 days",
		},
		{
			name:      "7d",
			spec:      "7d",
			wantSince: now.Add(-7 * 24 * time.Hour),
			wantLabel: "last 7 days",
		},
		{
			name:      "30d",
			spec:      "30d",
			wantSince: now.Add(-30 * 24 * time.Hour),
			wantLabel: "last 30 days",
		},
		{
			name:      "90d",
			spec:      "90d",
			wantSince: now.Add(-90 * 24 * time.Hour),
			wantLabel: "last 90 days",
		},
		{
			name:      "arbitrary N",
			spec:      "14d",
			wantSince: now.Add(-14 * 24 * time.Hour),
			wantLabel: "last 14 days",
		},
		{
			name:      "all",
			spec:      "all",
			wantSince: time.Time{},
			wantLabel: "all time",
		},
		{
			name:      "RFC3339 timestamp",
			spec:      "2026-06-01T00:00:00Z",
			wantSince: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
			wantLabel: "since 2026-06-01",
		},
		{
			name:    "negative days rejected",
			spec:    "-1d",
			wantErr: true,
		},
		{
			name:    "zero days rejected",
			spec:    "0d",
			wantErr: true,
		},
		{
			name:    "garbage rejected",
			spec:    "banana",
			wantErr: true,
		},
		{
			name:    "non-numeric Nd rejected",
			spec:    "abcd",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			since, label, err := Window(tt.spec, now)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Window(%q) = %v, %q, nil; want error", tt.spec, since, label)
				}
				if !strings.Contains(err.Error(), "oneshot.Window") {
					t.Errorf("error %q missing oneshot.Window: prefix", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("Window(%q) unexpected error: %v", tt.spec, err)
			}
			if !since.Equal(tt.wantSince) {
				t.Errorf("Window(%q) since = %v, want %v", tt.spec, since, tt.wantSince)
			}
			if label != tt.wantLabel {
				t.Errorf("Window(%q) label = %q, want %q", tt.spec, label, tt.wantLabel)
			}
		})
	}
}

func TestWindowAllHasZeroTime(t *testing.T) {
	since, _, err := Window("all", time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !since.IsZero() {
		t.Errorf("Window(\"all\") since = %v, want zero time", since)
	}
}
