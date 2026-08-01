package claudeplugin

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func boolPtr(b bool) *bool { return &b }

// TestClassify pins the decision table. Each row is one shape of on-disk
// evidence; the rules are documented on classify itself.
func TestClassify(t *testing.T) {
	settings := "/h/.claude/settings.json"
	cases := []struct {
		name          string
		reads         []settingsRead
		cacheDirs     []string
		marketplaces  []string
		wantActive    bool
		wantUncertain bool
		wantEnabled   Enablement
	}{
		{
			name:        "nothing at all",
			wantEnabled: EnablementAbsent,
		},
		{
			name:         "marketplace added but no plugin installed",
			reads:        []settingsRead{{Path: settings}},
			marketplaces: []string{Marketplace},
			wantEnabled:  EnablementAbsent,
		},
		{
			name:        "enabled in user settings",
			reads:       []settingsRead{{Path: settings, Entry: boolPtr(true)}},
			wantActive:  true,
			wantEnabled: EnablementOn,
		},
		{
			name:        "explicitly disabled — observer's own wiring is NOT redundant",
			reads:       []settingsRead{{Path: settings, Entry: boolPtr(false)}},
			wantEnabled: EnablementOff,
		},
		{
			name:        "disabled entry wins over a verified cache dir",
			reads:       []settingsRead{{Path: settings, Entry: boolPtr(false)}},
			cacheDirs:   []string{"/h/.claude/plugins/cache/superbased/" + Name + "/1.0.0"},
			wantEnabled: EnablementOff,
		},
		{
			name:        "verified cache dir alone (project/local-scope install)",
			reads:       []settingsRead{{Path: settings}},
			cacheDirs:   []string{"/h/.claude/plugins/cache/superbased/" + Name + "/1.0.0"},
			wantActive:  true,
			wantEnabled: EnablementAbsent,
		},
		{
			name:        "no settings file at all, verified cache dir",
			cacheDirs:   []string{"/h/.claude/plugins/cache/superbased/" + Name + "/1.0.0"},
			wantActive:  true,
			wantEnabled: EnablementAbsent,
		},
		// H3: fail open to wiring. An unreadable settings.json makes
		// "not enabled" unprovable, so we must NOT skip.
		{
			name:          "unreadable settings + cache dir — MUST NOT skip",
			reads:         []settingsRead{{Path: settings, Err: errors.New("permission denied")}},
			cacheDirs:     []string{"/h/.claude/plugins/cache/superbased/" + Name + "/1.0.0"},
			wantActive:    false,
			wantUncertain: true,
			wantEnabled:   EnablementAbsent,
		},
		{
			name:          "corrupt settings, no cache — MUST NOT skip",
			reads:         []settingsRead{{Path: settings, Err: errors.New("parse: unexpected char")}},
			wantActive:    false,
			wantUncertain: true,
			wantEnabled:   EnablementAbsent,
		},
		{
			name: "affirmative enablement outranks a sibling read error",
			reads: []settingsRead{
				{Path: "/h/.claude/other.json", Err: errors.New("boom")},
				{Path: settings, Entry: boolPtr(true)},
			},
			wantActive:  true,
			wantEnabled: EnablementOn,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classify(c.reads, c.cacheDirs, c.marketplaces)
			if got.Active != c.wantActive {
				t.Errorf("Active = %v, want %v", got.Active, c.wantActive)
			}
			if got.Uncertain != c.wantUncertain {
				t.Errorf("Uncertain = %v, want %v", got.Uncertain, c.wantUncertain)
			}
			if got.Enabled != c.wantEnabled {
				t.Errorf("Enabled = %v, want %v", got.Enabled, c.wantEnabled)
			}
			if got.Uncertain && got.Warning() == "" {
				t.Error("Warning() empty for an uncertain detection")
			}
			if !got.Uncertain && got.Warning() != "" {
				t.Errorf("Warning() = %q for a conclusive detection, want empty", got.Warning())
			}
			if got.Active && got.Reason() == "" {
				t.Error("Reason() empty for an active detection")
			}
			if !got.Active && got.Reason() != "" {
				t.Errorf("Reason() = %q for an inactive detection, want empty", got.Reason())
			}
			// A skip must never rest on an unreadable file.
			if got.Active && got.Uncertain {
				t.Error("Active AND Uncertain — a skip must rest on affirmative evidence only")
			}
		})
	}
}

// TestDetectRejectsForeignPluginWithOurName is codex finding H2's exact
// scenario: `superbased@acme-internal` is SOMEBODY ELSE'S plugin that
// merely shares our name. Suppressing observer's wiring for it would
// leave that user with no capture at all.
func TestDetectRejectsForeignPluginWithOurName(t *testing.T) {
	cases := []struct {
		name       string
		key        string
		wantActive bool
	}{
		{"our exact key", EnabledKey, true},
		{"same name, foreign marketplace", Name + "@acme-internal", false},
		{"same marketplace, foreign name", "other-plugin@" + Marketplace, false},
		{"bare name, no marketplace", Name, false},
		{"our key with a suffix", EnabledKey + "-fork", false},
		{"unrelated", "firecrawl@claude-plugins-official", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), ".claude")
			mustMkdir(t, dir)
			mustWrite(t, filepath.Join(dir, "settings.json"),
				`{"enabledPlugins":{"`+c.key+`":true}}`)
			if got := DetectInClaudeDir(dir).Active; got != c.wantActive {
				t.Errorf("Active = %v for key %q, want %v", got, c.key, c.wantActive)
			}
		})
	}
}

// TestFindVerifiedCacheDirs is codex finding H2's other half: a bare or
// stale directory is not evidence a plugin is installed. Claude Code keeps
// orphaned version directories for ~14 days after an update or uninstall.
func TestFindVerifiedCacheDirs(t *testing.T) {
	ourManifest := `{"name":"` + Name + `","version":"1.29.0"}`

	cases := []struct {
		name     string
		seed     func(t *testing.T, cacheRoot string)
		wantDirs int
	}{
		{
			name:     "no cache root at all",
			seed:     func(*testing.T, string) {},
			wantDirs: 0,
		},
		{
			name: "bare plugin dir, no version children",
			seed: func(t *testing.T, root string) {
				mustMkdir(t, filepath.Join(root, Marketplace, Name))
			},
			wantDirs: 0,
		},
		{
			name: "empty version dir — orphaned/stale, NOT evidence",
			seed: func(t *testing.T, root string) {
				mustMkdir(t, filepath.Join(root, Marketplace, Name, "1.29.0"))
			},
			wantDirs: 0,
		},
		{
			name: "version dir with someone else's manifest",
			seed: func(t *testing.T, root string) {
				mustWrite(t, filepath.Join(root, Marketplace, Name, "1.29.0", ".claude-plugin", "plugin.json"),
					`{"name":"someone-else"}`)
			},
			wantDirs: 0,
		},
		{
			name: "version dir with a malformed manifest",
			seed: func(t *testing.T, root string) {
				mustWrite(t, filepath.Join(root, Marketplace, Name, "1.29.0", ".claude-plugin", "plugin.json"),
					`{not json`)
			},
			wantDirs: 0,
		},
		{
			name: "our plugin under a FOREIGN marketplace segment",
			seed: func(t *testing.T, root string) {
				mustWrite(t, filepath.Join(root, "acme-internal", Name, "1.29.0", ".claude-plugin", "plugin.json"),
					ourManifest)
			},
			wantDirs: 0,
		},
		{
			name: "verified install",
			seed: func(t *testing.T, root string) {
				mustWrite(t, filepath.Join(root, Marketplace, Name, "1.29.0", ".claude-plugin", "plugin.json"),
					ourManifest)
			},
			wantDirs: 1,
		},
		{
			name: "two versions, one orphaned",
			seed: func(t *testing.T, root string) {
				mustWrite(t, filepath.Join(root, Marketplace, Name, "1.29.0", ".claude-plugin", "plugin.json"),
					ourManifest)
				mustMkdir(t, filepath.Join(root, Marketplace, Name, "1.28.0"))
			},
			wantDirs: 1,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			claudeDir := filepath.Join(t.TempDir(), ".claude")
			cacheRoot := filepath.Join(claudeDir, "plugins", "cache")
			mustMkdir(t, claudeDir)
			c.seed(t, cacheRoot)

			got := findVerifiedCacheDirs(cacheRoot)
			if len(got) != c.wantDirs {
				t.Errorf("findVerifiedCacheDirs = %v (%d), want %d dirs", got, len(got), c.wantDirs)
			}
			// End-to-end: cache evidence is what decides Active here.
			if active := DetectInClaudeDir(claudeDir).Active; active != (c.wantDirs > 0) {
				t.Errorf("DetectInClaudeDir Active = %v, want %v", active, c.wantDirs > 0)
			}
		})
	}
}

// TestDetectFailsOpenOnUnreadableSettings is codex finding H3's exact
// scenario: a cached install plus a corrupt settings.json must NOT skip.
func TestDetectFailsOpenOnUnreadableSettings(t *testing.T) {
	seedVerifiedCache := func(t *testing.T, claudeDir string) {
		mustWrite(t, filepath.Join(claudeDir, "plugins", "cache", Marketplace, Name, "1.29.0",
			".claude-plugin", "plugin.json"), `{"name":"`+Name+`"}`)
	}

	t.Run("corrupt settings + verified cache", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), ".claude")
		mustMkdir(t, dir)
		seedVerifiedCache(t, dir)
		mustWrite(t, filepath.Join(dir, "settings.json"), `{not json`)

		d := DetectInClaudeDir(dir)
		if d.Active {
			t.Error("Active with an unparseable settings.json — must fail OPEN to wiring")
		}
		if !d.Uncertain {
			t.Error("Uncertain = false; the caller cannot tell the probe failed")
		}
		if d.Err == nil {
			t.Error("Err = nil; the underlying failure must be reportable")
		}
		if !strings.Contains(d.Warning(), "registering anyway") {
			t.Errorf("Warning() = %q; must say registration proceeds", d.Warning())
		}
	})

	t.Run("unreadable settings + verified cache", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root: chmod 000 does not deny reads")
		}
		dir := filepath.Join(t.TempDir(), ".claude")
		mustMkdir(t, dir)
		seedVerifiedCache(t, dir)
		path := filepath.Join(dir, "settings.json")
		mustWrite(t, path, `{"enabledPlugins":{}}`)
		if err := os.Chmod(path, 0o000); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

		d := DetectInClaudeDir(dir)
		if d.Active {
			t.Error("Active with an unreadable settings.json — must fail OPEN to wiring")
		}
		if !d.Uncertain {
			t.Error("Uncertain = false for an unreadable settings.json")
		}
	})

	t.Run("absent settings is conclusive, not uncertain", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), ".claude")
		mustMkdir(t, dir)
		seedVerifiedCache(t, dir)

		d := DetectInClaudeDir(dir)
		if d.Uncertain {
			t.Error("Uncertain for a MISSING settings.json — absence is a conclusive 'no entry'")
		}
		if !d.Active {
			t.Error("not Active: a verified cache dir with no settings entry is the project-scope case")
		}
	})
}

// TestDetectInClaudeDirOnFixtureHomes runs the real I/O path against
// fixture .claude directories.
func TestDetectInClaudeDirOnFixtureHomes(t *testing.T) {
	t.Run("absent home", func(t *testing.T) {
		if DetectInClaudeDir(filepath.Join(t.TempDir(), "nope", ".claude")).Active {
			t.Error("Active on a nonexistent .claude dir")
		}
	})

	t.Run("empty claude dir", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), ".claude")
		mustMkdir(t, dir)
		if DetectInClaudeDir(dir).Active {
			t.Error("Active on an empty .claude dir")
		}
	})

	t.Run("hooks registered but no plugin", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), ".claude")
		mustMkdir(t, dir)
		mustWrite(t, filepath.Join(dir, "settings.json"),
			`{"hooks":{"PreToolUse":[{"matcher":"*","hooks":[{"type":"command","command":"/usr/bin/observer hook claude-code pre-tool"}]}]}}`)
		if DetectInClaudeDir(dir).Active {
			t.Error("Active with only init-written hooks present")
		}
	})

	t.Run("our plugin enabled", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), ".claude")
		mustMkdir(t, dir)
		mustWrite(t, filepath.Join(dir, "settings.json"),
			`{"enabledPlugins":{"`+EnabledKey+`":true},"hooks":{}}`)
		d := DetectInClaudeDir(dir)
		if !d.Active || d.Enabled != EnablementOn {
			t.Fatalf("Active=%v Enabled=%v; want true / EnablementOn", d.Active, d.Enabled)
		}
		if !strings.Contains(d.Reason(), Name) {
			t.Errorf("Reason() = %q, want it to name the plugin", d.Reason())
		}
	})

	t.Run("marketplace known but plugin not installed", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), ".claude")
		mustMkdir(t, filepath.Join(dir, "plugins"))
		mustWrite(t, filepath.Join(dir, "plugins", "known_marketplaces.json"),
			`{"`+Marketplace+`":{"source":{"source":"github","repo":"superbasedapp/plugins"}}}`)
		d := DetectInClaudeDir(dir)
		if d.Active {
			t.Error("Active from a known marketplace alone — a catalog is not an install")
		}
		if len(d.Marketplaces) != 1 || d.Marketplaces[0] != Marketplace {
			t.Errorf("Marketplaces = %v, want [%s]", d.Marketplaces, Marketplace)
		}
	})
}

// TestDetectEmptyHome pins the guard against an empty home, which would
// otherwise probe "/.claude".
func TestDetectEmptyHome(t *testing.T) {
	if Detect("").Active {
		t.Error("Active for an empty homeDir")
	}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	mustMkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
