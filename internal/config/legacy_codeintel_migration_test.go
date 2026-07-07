package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
)

// TestLegacyCodeGraphMigration_MapsAndWarns pins the Phase 4
// decommission back-compat: the deprecated [compression.code_graph] /
// [intelligence.code_graph] blocks map onto [codeintel] and emit exactly
// one deprecation warning per legacy key. See
// docs/codeintel/migration-from-codegraph.md.
func TestLegacyCodeGraphMigration_MapsAndWarns(t *testing.T) {
	body := `
[compression.code_graph]
enabled = false
auto_index = false
auto_install = true
path = "/some/graph.db"

[intelligence.code_graph]
enabled = false
`
	cfg := Default()
	meta, err := toml.Decode(body, &cfg)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	warnings := migrateLegacyCodeGraph(&cfg, []toml.MetaData{meta})

	// One warning per legacy key present: enabled, auto_index,
	// auto_install, path (compression) + enabled (intelligence) = 5.
	if len(warnings) != 5 {
		t.Fatalf("warning count: got %d, want 5\n%v", len(warnings), warnings)
	}

	// enabled=false maps onto codeintel.enabled (no [codeintel] override).
	if cfg.CodeIntel.Enabled {
		t.Errorf("codeintel.enabled should map from legacy enabled=false")
	}
	// auto_index=false maps onto codeintel.index.on_start.
	if cfg.CodeIntel.Index.OnStart {
		t.Errorf("codeintel.index.on_start should map from legacy auto_index=false")
	}
}

// TestLegacyCodeGraphMigration_CodeIntelWins pins that an explicit
// [codeintel] key stays authoritative even when a legacy block is also
// present (the legacy value does NOT clobber it), and the deprecation
// warning still fires.
func TestLegacyCodeGraphMigration_CodeIntelWins(t *testing.T) {
	body := `
[codeintel]
enabled = true

[compression.code_graph]
enabled = false
`
	cfg := Default()
	meta, err := toml.Decode(body, &cfg)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	warnings := migrateLegacyCodeGraph(&cfg, []toml.MetaData{meta})
	if len(warnings) != 1 {
		t.Fatalf("warning count: got %d, want 1\n%v", len(warnings), warnings)
	}
	if !cfg.CodeIntel.Enabled {
		t.Errorf("explicit codeintel.enabled=true must win over legacy enabled=false")
	}
}

// TestLegacyCodeGraphMigration_NoLegacyNoWarn pins that a config without
// any legacy block produces no warnings and leaves [codeintel] at its
// Default() values.
func TestLegacyCodeGraphMigration_NoLegacyNoWarn(t *testing.T) {
	body := `
[proxy]
port = 8820
`
	cfg := Default()
	meta, err := toml.Decode(body, &cfg)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if w := migrateLegacyCodeGraph(&cfg, []toml.MetaData{meta}); len(w) != 0 {
		t.Errorf("expected no warnings for a legacy-free config, got %v", w)
	}
	if !cfg.CodeIntel.Enabled {
		t.Errorf("codeintel should keep its Default() enabled=true")
	}
}

// TestLegacyCodeGraphMigration_ThroughLoad pins the end-to-end wiring:
// Load() applies the migration so a legacy-only config still disables
// the in-process module.
func TestLegacyCodeGraphMigration_ThroughLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	body := `
[compression.code_graph]
enabled = false
`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(LoadOptions{GlobalPath: p, Env: func(string) string { return "" }})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CodeIntel.Enabled {
		t.Errorf("legacy compression.code_graph.enabled=false should disable codeintel via Load")
	}
}
