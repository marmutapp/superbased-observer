package diag

import (
	"context"
	"testing"
)

func TestCheckProcessObservability_DisabledIsOK(t *testing.T) {
	cfg, database, _, _ := newTestEnv(t)
	// Default config: the feature is opt-in / off.
	c := checkProcessObservability(context.Background(), database, cfg)
	if c.Status != StatusOK {
		t.Errorf("disabled feature should be OK, got %s (%q)", c.Status, c.Message)
	}
}

func TestCheckProcessObservability_EnabledNoRowsWarns(t *testing.T) {
	cfg, database, _, _ := newTestEnv(t)
	cfg.Observer.Process.Enabled = true
	cfg.Observer.Process.Backend = "linux_ebpf"
	c := checkProcessObservability(context.Background(), database, cfg)
	if c.Status != StatusWarn {
		t.Errorf("enabled-but-empty should WARN (verify backend availability), got %s (%q)", c.Status, c.Message)
	}
}

func TestCheckProcessObservability_EnabledWithRowsOK(t *testing.T) {
	cfg, database, _, _ := newTestEnv(t)
	cfg.Observer.Process.Enabled = true
	// Minimal unattributed row (NULL session_id is FK-safe) just to make
	// the count non-zero.
	if _, err := database.ExecContext(context.Background(),
		`INSERT INTO process_runs
		   (process_key, pid, attribution_source, attribution_confidence, started_at, last_seen_at)
		 VALUES ('pk-doctor', 7, 'none', 'none', '2026-06-16T12:00:00Z', '2026-06-16T12:00:00Z')`); err != nil {
		t.Fatalf("seed process_runs: %v", err)
	}
	c := checkProcessObservability(context.Background(), database, cfg)
	if c.Status != StatusOK {
		t.Errorf("enabled with rows should be OK, got %s (%q)", c.Status, c.Message)
	}
}
