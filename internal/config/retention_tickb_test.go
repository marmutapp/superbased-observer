package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestTicketBRetentionDefaults pins the Ticket-B retention knobs
// (docs/plans/claude-code-hook-stall-ticket-and-db-prune-plan-2026-07-12.md):
// [codeintel].retention_days and [observer.retention].interval_hours are
// seeded by Default() and survive the loader's partial merge (the
// CacheTrack rule — a partial section keeps unset fields at their
// Default() values), and an explicit value overrides.
func TestTicketBRetentionDefaults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                  string
		toml                  string // "" = no config file at all
		wantCodeIntelDays     int
		wantRetentionInterval int
	}{
		{
			name:                  "no config file uses Default()",
			toml:                  "",
			wantCodeIntelDays:     90,
			wantRetentionInterval: 24,
		},
		{
			name: "partial [codeintel] keeps retention default",
			toml: "[codeintel]\nenabled = true\n",
			// A [codeintel] section that never mentions retention_days
			// must inherit 90, never a zero that disables the sweep.
			wantCodeIntelDays:     90,
			wantRetentionInterval: 24,
		},
		{
			name:                  "partial [observer.retention] keeps interval default",
			toml:                  "[observer.retention]\nmax_age_days = 30\n",
			wantCodeIntelDays:     90,
			wantRetentionInterval: 24,
		},
		{
			name:                  "explicit values override",
			toml:                  "[codeintel]\nretention_days = 7\n\n[observer.retention]\ninterval_hours = 6\n",
			wantCodeIntelDays:     7,
			wantRetentionInterval: 6,
		},
		{
			name:                  "explicit zero disables",
			toml:                  "[codeintel]\nretention_days = 0\n\n[observer.retention]\ninterval_hours = 0\n",
			wantCodeIntelDays:     0,
			wantRetentionInterval: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "config.toml")
			if tc.toml != "" {
				if err := os.WriteFile(path, []byte(tc.toml), 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}
			}
			cfg, err := Load(LoadOptions{GlobalPath: path, Env: func(string) string { return "" }})
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.CodeIntel.RetentionDays != tc.wantCodeIntelDays {
				t.Errorf("codeintel.retention_days = %d, want %d",
					cfg.CodeIntel.RetentionDays, tc.wantCodeIntelDays)
			}
			if cfg.Observer.Retention.IntervalHours != tc.wantRetentionInterval {
				t.Errorf("observer.retention.interval_hours = %d, want %d",
					cfg.Observer.Retention.IntervalHours, tc.wantRetentionInterval)
			}
		})
	}
}
