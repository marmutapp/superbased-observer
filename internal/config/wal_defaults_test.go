package config

import "testing"

// §1e follow-through (2026-08-22): the WAL watchdog keys default ON with a
// bounded alert (1 GiB) and 10-minute cadence — the 15.5 GB silent-WAL
// incident must trip this within one tick, not require an operator stat.
func TestWALWatchdogDefaults(t *testing.T) {
	cfg, err := Load(LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Observer.Retention.WALAlertMB != 1024 {
		t.Fatalf("wal_alert_mb default = %d, want 1024", cfg.Observer.Retention.WALAlertMB)
	}
	if cfg.Observer.Retention.WALWatchMinutes != 10 {
		t.Fatalf("wal_watch_minutes default = %d, want 10", cfg.Observer.Retention.WALWatchMinutes)
	}
}
