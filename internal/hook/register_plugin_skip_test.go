package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/claudeplugin"
)

// TestRegisterClaudeCodeSkipsWhenPluginInstalled is the paired proof for
// the double-wiring guard: the SAME registrar call must write hooks on a
// fixture HOME with no plugin, and write NOTHING on one where the plugin
// artifact is present.
func TestRegisterClaudeCodeSkipsWhenPluginInstalled(t *testing.T) {
	cases := []struct {
		name string
		// seed prepares the fixture ~/.claude before registration.
		seed       func(t *testing.T, claudeDir string)
		force      bool
		wantSkip   bool
		wantReason string // substring
		// wantError marks rows where the registrar is EXPECTED to fail
		// after proceeding — the fail-open path still has to surface the
		// underlying settings error rather than swallow it.
		wantError bool
	}{
		{
			name:     "no plugin — registers normally",
			seed:     func(*testing.T, string) {},
			wantSkip: false,
		},
		{
			name: "marketplace added but plugin not installed — registers",
			seed: func(t *testing.T, dir string) {
				writeFixture(t, filepath.Join(dir, "plugins", "known_marketplaces.json"),
					`{"`+claudeplugin.Marketplace+`":{"source":{"source":"github","repo":"superbasedapp/observer-plugins"}}}`)
			},
			wantSkip: false,
		},
		{
			name: "another vendor's plugin enabled — registers",
			seed: func(t *testing.T, dir string) {
				writeFixture(t, filepath.Join(dir, "settings.json"),
					`{"enabledPlugins":{"someone-else@their-market":true}}`)
			},
			wantSkip: false,
		},
		{
			name: "our plugin enabled — skips",
			seed: func(t *testing.T, dir string) {
				writeFixture(t, filepath.Join(dir, "settings.json"),
					`{"enabledPlugins":{"`+claudeplugin.EnabledKey+`":true}}`)
			},
			wantSkip:   true,
			wantReason: claudeplugin.Name,
		},
		{
			name: "our plugin cached — skips",
			seed: func(t *testing.T, dir string) {
				seedVerifiedPluginCache(t, dir)
			},
			wantSkip:   true,
			wantReason: claudeplugin.Name,
		},
		{
			// H2: same plugin NAME, someone else's marketplace. Must NOT
			// suppress our wiring.
			name: "same-named plugin from a foreign marketplace — registers",
			seed: func(t *testing.T, dir string) {
				writeFixture(t, filepath.Join(dir, "settings.json"),
					`{"enabledPlugins":{"`+claudeplugin.Name+`@acme-internal":true}}`)
			},
			wantSkip: false,
		},
		{
			// H2: a bare/orphaned cache directory is not an install.
			name: "empty plugin cache directory — registers",
			seed: func(t *testing.T, dir string) {
				mkdirFixture(t, filepath.Join(dir, "plugins", "cache",
					claudeplugin.Marketplace, claudeplugin.Name, "1.29.0"))
			},
			wantSkip: false,
		},
		{
			// H3: a corrupt settings.json makes "not enabled" unprovable.
			// Fail OPEN to wiring — and the registrar surfaces the parse
			// error through res.Error, so this row asserts separately.
			name: "corrupt settings + verified cache — registers (fail-open)",
			seed: func(t *testing.T, dir string) {
				seedVerifiedPluginCache(t, dir)
				writeFixture(t, filepath.Join(dir, "settings.json"), `{not json`)
			},
			wantSkip:  false,
			wantError: true,
		},
		{
			name: "our plugin explicitly disabled — registers",
			seed: func(t *testing.T, dir string) {
				writeFixture(t, filepath.Join(dir, "settings.json"),
					`{"enabledPlugins":{"`+claudeplugin.EnabledKey+`":false}}`)
				seedVerifiedPluginCache(t, dir)
			},
			wantSkip: false,
		},
		{
			name: "--force overrides the skip",
			seed: func(t *testing.T, dir string) {
				writeFixture(t, filepath.Join(dir, "settings.json"),
					`{"enabledPlugins":{"`+claudeplugin.EnabledKey+`":true}}`)
			},
			force:    true,
			wantSkip: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			claudeDir := filepath.Join(home, ".claude")
			mkdirFixture(t, claudeDir)
			c.seed(t, claudeDir)

			reg, err := NewRegistry(Options{
				BinaryPath:    "/usr/local/bin/observer",
				HomeDir:       home,
				Force:         c.force,
				ChecksumsPath: filepath.Join(home, ".observer", "hook_checksums.json"),
			})
			if err != nil {
				t.Fatalf("NewRegistry: %v", err)
			}
			res := reg.Register("claude-code")
			if c.wantError {
				if res.Skipped {
					t.Fatal("Skipped despite an unreadable settings.json — must fail OPEN to wiring")
				}
				if res.Error == nil {
					t.Fatal("Error = nil; the settings parse failure must reach the user")
				}
				return
			}
			if res.Error != nil {
				t.Fatalf("Register: %v", res.Error)
			}

			if res.Skipped != c.wantSkip {
				t.Fatalf("Skipped = %v, want %v (reason %q)", res.Skipped, c.wantSkip, res.SkipReason)
			}

			settingsHooks := readClaudeSettingsHooks(t, filepath.Join(claudeDir, "settings.json"))
			if c.wantSkip {
				if len(res.HooksAdded) != 0 {
					t.Errorf("HooksAdded = %v on the skip path, want none", res.HooksAdded)
				}
				if !strings.Contains(res.SkipReason, c.wantReason) {
					t.Errorf("SkipReason = %q, want it to contain %q", res.SkipReason, c.wantReason)
				}
				if len(settingsHooks) != 0 {
					t.Errorf("registrar wrote %d hook event(s) to settings.json on the skip path", len(settingsHooks))
				}
				return
			}
			if len(res.HooksAdded) == 0 {
				t.Fatal("HooksAdded empty on the register path")
			}
			if res.SkipReason != "" {
				t.Errorf("SkipReason = %q on the register path, want empty", res.SkipReason)
			}
			if len(settingsHooks) != len(claudeCodeEvents) {
				t.Errorf("settings.json carries %d hook event(s), want %d",
					len(settingsHooks), len(claudeCodeEvents))
			}
		})
	}
}

// TestRegisterClaudeCodeSkipIsNonDestructive proves the skip leaves an
// EXISTING settings.json byte-identical — a skip must never be a partial
// write, and must never clobber the user's own hooks.
func TestRegisterClaudeCodeSkipIsNonDestructive(t *testing.T) {
	home := t.TempDir()
	claudeDir := filepath.Join(home, ".claude")
	path := filepath.Join(claudeDir, "settings.json")
	body := `{"enabledPlugins":{"` + claudeplugin.Name + `@` + claudeplugin.Marketplace + `":true},` +
		`"hooks":{"PreToolUse":[{"matcher":"*","hooks":[{"type":"command","command":"my-own-linter"}]}]}}`
	writeFixture(t, path, body)

	reg, err := NewRegistry(Options{
		BinaryPath:    "/usr/local/bin/observer",
		HomeDir:       home,
		ChecksumsPath: filepath.Join(home, ".observer", "hook_checksums.json"),
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if res := reg.Register("claude-code"); !res.Skipped {
		t.Fatalf("Register did not skip (err=%v added=%v)", res.Error, res.HooksAdded)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != body {
		t.Errorf("settings.json mutated on the skip path:\n got: %s\nwant: %s", got, body)
	}
}

func readClaudeSettingsHooks(t *testing.T, path string) map[string][]claudeHookGroup {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc struct {
		Hooks map[string][]claudeHookGroup `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return doc.Hooks
}

// seedVerifiedPluginCache writes the cache layout a REAL install
// produces: <claude>/plugins/cache/<marketplace>/<plugin>/<version>/
// .claude-plugin/plugin.json naming our plugin. A bare directory is
// deliberately NOT enough evidence — see findVerifiedCacheDirs.
func seedVerifiedPluginCache(t *testing.T, claudeDir string) {
	t.Helper()
	writeFixture(t, filepath.Join(claudeDir, "plugins", "cache",
		claudeplugin.Marketplace, claudeplugin.Name, "1.29.0",
		".claude-plugin", "plugin.json"),
		`{"name":"`+claudeplugin.Name+`","version":"1.29.0"}`)
}

func mkdirFixture(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func writeFixture(t *testing.T, path, body string) {
	t.Helper()
	mkdirFixture(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
