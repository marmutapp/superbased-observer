package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/claudeplugin"
)

// TestWireAIClientsSkipsClaudeCodeWhenPluginInstalled is the end-to-end
// proof of the init-side double-wiring guard: `observer init
// --claude-code` on a fixture HOME that carries the plugin must write no
// hooks and no MCP entry, and must SAY why — while the same call on a
// fixture HOME without the plugin writes both.
//
// The proxy-route step is deliberately skipped here: it needs no plugin
// guard (the plugin declares no proxy route) and writing it would drag a
// port/registrar into a test about the skip.
//
// CONTAINMENT (belt and braces since the structural fix): a pinned
// HomeDir now suppresses cross-OS auto-detection at the registrars
// themselves (crossmount.AutoDetectSuppressed — the 2026-07-31 incident,
// where this test's "claude-code-windows" target resolved the operator's
// REAL /mnt/c/Users/<u>/.claude and wrote 22 hook entries into it). The
// explicit WindowsClaudeHome / WindowsCursorHome pins below are kept
// deliberately: they take the unconditional override branch of
// hook.Registry.detectWindowsHome, so every write this test can reach
// lands under t.TempDir() even if the gate were ever weakened. The
// standing guard is TestWireAIClients_SandboxHomeNeverEscapes.
func TestWireAIClientsSkipsClaudeCodeWhenPluginInstalled(t *testing.T) {
	cases := []struct {
		name     string
		seed     func(t *testing.T, claudeDir string)
		force    bool
		wantSkip bool
	}{
		{
			name: "no plugin — wires hooks and MCP",
			seed: func(*testing.T, string) {},
		},
		{
			name: "plugin enabled — skips both",
			seed: func(t *testing.T, dir string) {
				writeInitFixture(t, filepath.Join(dir, "settings.json"),
					`{"enabledPlugins":{"`+claudeplugin.EnabledKey+`":true}}`)
			},
			wantSkip: true,
		},
		{
			name: "plugin cached (verified manifest) — skips both",
			seed: func(t *testing.T, dir string) {
				writeInitFixture(t, filepath.Join(dir, "plugins", "cache",
					claudeplugin.Marketplace, claudeplugin.Name, "1.29.0",
					".claude-plugin", "plugin.json"),
					`{"name":"`+claudeplugin.Name+`"}`)
			},
			wantSkip: true,
		},
		{
			// H2: a bare cache directory is not an install.
			name: "empty plugin cache directory — wires normally",
			seed: func(t *testing.T, dir string) {
				mkdirInitFixture(t, filepath.Join(dir, "plugins", "cache",
					claudeplugin.Marketplace, claudeplugin.Name, "1.29.0"))
			},
			wantSkip: false,
		},
		{
			// H2: same name, foreign marketplace.
			name: "foreign same-named plugin — wires normally",
			seed: func(t *testing.T, dir string) {
				writeInitFixture(t, filepath.Join(dir, "settings.json"),
					`{"enabledPlugins":{"`+claudeplugin.Name+`@acme-internal":true}}`)
			},
			wantSkip: false,
		},
		{
			name: "--force wires anyway",
			seed: func(t *testing.T, dir string) {
				writeInitFixture(t, filepath.Join(dir, "settings.json"),
					`{"enabledPlugins":{"`+claudeplugin.EnabledKey+`":true}}`)
			},
			force: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			claudeDir := filepath.Join(home, ".claude")
			mkdirInitFixture(t, claudeDir)
			c.seed(t, claudeDir)

			// See the CONTAINMENT note above: these two overrides are what
			// keep the cross-OS targets inside the sandbox — and they must
			// live UNDER `home`, or the gate refuses them as uncontained.
			winHome := filepath.Join(home, "mnt", "c", "Users", "tester")
			mkdirInitFixture(t, winHome)

			lines, _, _, _, err := wireAIClients(WireAIClientsOptions{
				HomeDir:           home,
				OnlyClaudeCode:    true,
				SkipProxy:         true,
				SkipExtension:     true,
				Force:             c.force,
				WindowsClaudeHome: winHome,
				WindowsCursorHome: winHome,
			})
			if err != nil {
				t.Fatalf("wireAIClients: %v", err)
			}
			out := strings.Join(lines, "\n")

			hooksWritten := fileContains(t, filepath.Join(claudeDir, "settings.json"), " hook claude-code ")
			mcpWritten := fileContains(t, filepath.Join(home, ".claude.json"), `"observer"`)

			if c.wantSkip {
				if hooksWritten {
					t.Error("hooks were written into ~/.claude/settings.json despite the plugin")
				}
				if mcpWritten {
					t.Error("an MCP entry was written into ~/.claude.json despite the plugin")
				}
				if !strings.Contains(out, "hook skipped") {
					t.Errorf("output does not report the hook skip:\n%s", out)
				}
				if !strings.Contains(out, "mcp  skipped") {
					t.Errorf("output does not report the MCP skip:\n%s", out)
				}
				if !strings.Contains(out, claudeplugin.Name) {
					t.Errorf("output does not name the plugin as the reason:\n%s", out)
				}
				if !strings.Contains(out, "--force") {
					t.Errorf("output does not offer the --force escape hatch:\n%s", out)
				}
				return
			}
			if !hooksWritten {
				t.Errorf("no hooks written on the non-skip path:\n%s", out)
			}
			if !mcpWritten {
				t.Errorf("no MCP entry written on the non-skip path:\n%s", out)
			}
			if strings.Contains(out, "skipped") {
				t.Errorf("output reports a skip on the non-skip path:\n%s", out)
			}
		})
	}
}

func fileContains(t *testing.T, path, needle string) bool {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(raw), needle)
}

func mkdirInitFixture(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func writeInitFixture(t *testing.T, path, body string) {
	t.Helper()
	mkdirInitFixture(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
