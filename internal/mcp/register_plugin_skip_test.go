package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/claudeplugin"
	"github.com/marmutapp/superbased-observer/internal/mcp/locate"
)

// TestRegisterClaudeCodeMCPSkipsWhenPluginInstalled is the MCP half of the
// double-wiring guard. The plugin bundles a .mcp.json declaring this same
// stdio server; Claude Code namespaces a plugin server separately from a
// user-config one, so both present means the observer tool schema loads
// twice per turn.
func TestRegisterClaudeCodeMCPSkipsWhenPluginInstalled(t *testing.T) {
	cases := []struct {
		name        string
		seed        func(t *testing.T, claudeDir string)
		force       bool
		tool        string
		wantSkip    bool
		wantWarning bool
	}{
		{
			name:     "no plugin — registers",
			seed:     func(*testing.T, string) {},
			tool:     "claude-code",
			wantSkip: false,
		},
		{
			name: "our plugin enabled — skips",
			seed: func(t *testing.T, dir string) {
				writeMCPFixture(t, filepath.Join(dir, "settings.json"),
					`{"enabledPlugins":{"`+claudeplugin.EnabledKey+`":true}}`)
			},
			tool:     "claude-code",
			wantSkip: true,
		},
		{
			name: "our plugin cached (verified) — skips",
			seed: func(t *testing.T, dir string) {
				writeMCPFixture(t, filepath.Join(dir, "plugins", "cache",
					claudeplugin.Marketplace, claudeplugin.Name, "1.29.0",
					".claude-plugin", "plugin.json"),
					`{"name":"`+claudeplugin.Name+`"}`)
			},
			tool:     "claude-code",
			wantSkip: true,
		},
		{
			// H2: same name, foreign marketplace — must NOT suppress.
			name: "same-named plugin from a foreign marketplace — registers",
			seed: func(t *testing.T, dir string) {
				writeMCPFixture(t, filepath.Join(dir, "settings.json"),
					`{"enabledPlugins":{"`+claudeplugin.Name+`@acme-internal":true}}`)
			},
			tool:     "claude-code",
			wantSkip: false,
		},
		{
			// H2: an empty cache dir is not an install.
			name: "empty plugin cache directory — registers",
			seed: func(t *testing.T, dir string) {
				mkdirMCPFixture(t, filepath.Join(dir, "plugins", "cache",
					claudeplugin.Marketplace, claudeplugin.Name, "1.29.0"))
			},
			tool:     "claude-code",
			wantSkip: false,
		},
		{
			// H3: fail open to wiring, and SAY why through ProbeWarning
			// (the MCP registrar reads ~/.claude.json, so it would
			// otherwise never surface a ~/.claude/settings.json failure).
			name: "corrupt settings + verified cache — registers with a warning",
			seed: func(t *testing.T, dir string) {
				writeMCPFixture(t, filepath.Join(dir, "plugins", "cache",
					claudeplugin.Marketplace, claudeplugin.Name, "1.29.0",
					".claude-plugin", "plugin.json"), `{"name":"`+claudeplugin.Name+`"}`)
				writeMCPFixture(t, filepath.Join(dir, "settings.json"), `{not json`)
			},
			tool:        "claude-code",
			wantSkip:    false,
			wantWarning: true,
		},
		{
			name: "--force overrides",
			seed: func(t *testing.T, dir string) {
				writeMCPFixture(t, filepath.Join(dir, "settings.json"),
					`{"enabledPlugins":{"`+claudeplugin.EnabledKey+`":true}}`)
			},
			force:    true,
			tool:     "claude-code",
			wantSkip: false,
		},
		{
			name: "cursor is unaffected by a Claude Code plugin",
			seed: func(t *testing.T, dir string) {
				writeMCPFixture(t, filepath.Join(dir, "settings.json"),
					`{"enabledPlugins":{"`+claudeplugin.EnabledKey+`":true}}`)
			},
			tool:     "cursor",
			wantSkip: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			mkdirMCPFixture(t, filepath.Join(home, ".claude"))
			c.seed(t, filepath.Join(home, ".claude"))

			reg, err := NewRegistrar(RegisterOptions{
				BinaryPath: "/usr/local/bin/observer",
				HomeDir:    home,
				Force:      c.force,
			})
			if err != nil {
				t.Fatalf("NewRegistrar: %v", err)
			}
			res := reg.Register(c.tool)
			if res.Error != nil {
				t.Fatalf("Register(%s): %v", c.tool, res.Error)
			}
			if res.Skipped != c.wantSkip {
				t.Fatalf("Skipped = %v, want %v (reason %q)", res.Skipped, c.wantSkip, res.SkipReason)
			}
			if got := res.ProbeWarning != ""; got != c.wantWarning {
				t.Errorf("ProbeWarning present = %v, want %v (%q)", got, c.wantWarning, res.ProbeWarning)
			}

			loc, ok := locate.ForClient(c.tool, home)
			if !ok {
				t.Fatalf("locate.ForClient(%q) not supported", c.tool)
			}
			entryWritten := mcpEntryPresent(t, loc.Path)

			if c.wantSkip {
				if res.Added {
					t.Error("Added = true on the skip path")
				}
				if !strings.Contains(res.SkipReason, claudeplugin.Name) {
					t.Errorf("SkipReason = %q, want it to name the plugin", res.SkipReason)
				}
				if res.ConfigPath != loc.Path {
					t.Errorf("ConfigPath = %q, want %q", res.ConfigPath, loc.Path)
				}
				if entryWritten {
					t.Errorf("registrar wrote an %q entry into %s on the skip path", ServerName, loc.Path)
				}
				return
			}
			if !res.Added {
				t.Error("Added = false on the register path")
			}
			if !entryWritten {
				t.Errorf("no %q entry in %s after registering", ServerName, loc.Path)
			}
		})
	}
}

func mcpEntryPresent(t *testing.T, path string) bool {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var doc struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	_, ok := doc.MCPServers[ServerName]
	return ok
}

func mkdirMCPFixture(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func writeMCPFixture(t *testing.T, path, body string) {
	t.Helper()
	mkdirMCPFixture(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
