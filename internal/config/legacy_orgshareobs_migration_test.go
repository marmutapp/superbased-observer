package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
)

// TestLegacyOrgShareObsMigration_MapsAndWarns pins the plane-separation
// audit M1 back-compat: the deprecated flat [org_client.share] obs_* keys
// map onto the nested [org_client.share.obs] sub-table and emit exactly one
// deprecation warning per legacy key present.
func TestLegacyOrgShareObsMigration_MapsAndWarns(t *testing.T) {
	body := `
[org_client.share]
full_content = true
obs_summary = true
obs_traces = true
obs_content = false
obs_eval_summary = true
`
	cfg := Default()
	meta, err := toml.Decode(body, &cfg)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	warnings := migrateLegacyOrgShareObs(&cfg, []toml.MetaData{meta})

	// One warning per flat obs_* key present = 4.
	if len(warnings) != 4 {
		t.Fatalf("warning count: got %d, want 4\n%v", len(warnings), warnings)
	}

	sh := cfg.OrgClient.Share
	if !sh.Obs.Summary || !sh.Obs.Traces || sh.Obs.Content || !sh.Obs.EvalSummary {
		t.Errorf("nested obs flags not mapped from flat keys: %+v", sh.Obs)
	}
	// Unrelated share flag untouched.
	if !sh.FullContent {
		t.Errorf("full_content should be preserved")
	}
}

// TestLegacyOrgShareObsMigration_NestedWins pins that an explicit nested
// [org_client.share.obs] key stays authoritative even when the flat key is
// also present (the flat value does NOT clobber it), and the deprecation
// warning still fires.
func TestLegacyOrgShareObsMigration_NestedWins(t *testing.T) {
	body := `
[org_client.share.obs]
summary = true

[org_client.share]
obs_summary = false
`
	cfg := Default()
	meta, err := toml.Decode(body, &cfg)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	warnings := migrateLegacyOrgShareObs(&cfg, []toml.MetaData{meta})
	if len(warnings) != 1 {
		t.Fatalf("warning count: got %d, want 1\n%v", len(warnings), warnings)
	}
	if !cfg.OrgClient.Share.Obs.Summary {
		t.Errorf("explicit obs.summary=true must win over legacy obs_summary=false")
	}
}

// TestLegacyOrgShareObsMigration_NoLegacyNoWarn pins that a config without
// any flat obs_* key produces no warnings and leaves the nested flags at
// their zero (opt-out) default.
func TestLegacyOrgShareObsMigration_NoLegacyNoWarn(t *testing.T) {
	body := `
[org_client.share.obs]
traces = true
`
	cfg := Default()
	meta, err := toml.Decode(body, &cfg)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if w := migrateLegacyOrgShareObs(&cfg, []toml.MetaData{meta}); len(w) != 0 {
		t.Errorf("expected no warnings for a flat-key-free config, got %v", w)
	}
	if !cfg.OrgClient.Share.Obs.Traces {
		t.Errorf("nested obs.traces=true should be honored")
	}
	if cfg.OrgClient.Share.Obs.Summary {
		t.Errorf("unset nested obs.summary should default false")
	}
}

// TestLegacyOrgShareObsMigration_ThroughLoad pins the end-to-end wiring:
// Load() applies the migration so a flat-only config still opts the node
// into the obs summary tier via the nested field consumers read.
func TestLegacyOrgShareObsMigration_ThroughLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	body := `
[org_client.share]
obs_summary = true
`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(LoadOptions{GlobalPath: p, Env: func(string) string { return "" }})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.OrgClient.Share.Obs.Summary {
		t.Errorf("legacy org_client.share.obs_summary=true should set obs.Summary via Load")
	}
}
