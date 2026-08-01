package diag

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/claudeplugin"
)

const (
	fixtureObserverHooks = `"hooks":{` +
		`"PreToolUse":[{"matcher":"*","hooks":[{"type":"command","command":"/usr/local/bin/observer hook claude-code pre-tool"}]}],` +
		`"Stop":[{"matcher":"*","hooks":[{"type":"command","command":"/usr/local/bin/observer hook claude-code stop"}]}]}`
	fixtureForeignHooks = `"hooks":{` +
		`"PreToolUse":[{"matcher":"*","hooks":[{"type":"command","command":"my-own-linter"}]}]}`
	// codex L6: a third-party binary that takes the same subcommand shape.
	fixtureAcmeHooks = `"hooks":{` +
		`"PreToolUse":[{"matcher":"*","hooks":[{"type":"command","command":"/opt/acme hook claude-code audit"}]}]}`
	fixtureWSLBridgeHooks = `"hooks":{` +
		`"Stop":[{"matcher":"*","hooks":[{"type":"command",` +
		`"command":"MSYS_NO_PATHCONV=1 wsl.exe -d Ubuntu-20.04 -- /home/u/bin/observer hook claude-code stop"}]}]}`
	fixturePluginOn = `"enabledPlugins":{"` + claudeplugin.EnabledKey + `":true}`
	fixtureObsMCP   = `{"mcpServers":{"observer":{"command":"observer","args":["serve"]}}}`
)

// noWindowsSide pins the crossmount seam OFF for a test. Without it, a
// test running on a WSL host reads the operator's real Windows-side
// .claude — the containment lesson from the 2026-07-31 pollution
// incident, applied to the doctor probe's own resolver.
func noWindowsSide(t *testing.T) {
	t.Helper()
	prev := windowsClaudeDir
	windowsClaudeDir = func(string) string { return "" }
	t.Cleanup(func() { windowsClaudeDir = prev })
}

// fakeWindowsSide points the crossmount seam at a fixture directory.
func fakeWindowsSide(t *testing.T, dir string) {
	t.Helper()
	prev := windowsClaudeDir
	windowsClaudeDir = func(string) string { return dir }
	t.Cleanup(func() { windowsClaudeDir = prev })
}

// TestCheckClaudeCodePlugin pins the doctor probe: it WARNs only when the
// plugin AND `observer init`'s own wiring are both present on a side.
func TestCheckClaudeCodePlugin(t *testing.T) {
	verifiedCache := func(t *testing.T, claudeDir string) {
		mustWriteFile(t, filepath.Join(claudeDir, "plugins", "cache",
			claudeplugin.Marketplace, claudeplugin.Name, "1.29.0",
			".claude-plugin", "plugin.json"), `{"name":"`+claudeplugin.Name+`"}`)
	}

	cases := []struct {
		name       string
		settings   string // ~/.claude/settings.json body ("" = absent)
		claudeJSON string // ~/.claude.json body ("" = absent)
		cached     bool
		wantStatus Status
		wantDetail string
	}{
		{
			name:       "nothing wired at all",
			wantStatus: StatusOK,
		},
		{
			name:       "init hooks only, no plugin",
			settings:   "{" + fixtureObserverHooks + "}",
			claudeJSON: fixtureObsMCP,
			wantStatus: StatusOK,
		},
		{
			name:       "plugin only",
			settings:   "{" + fixturePluginOn + "}",
			wantStatus: StatusOK,
		},
		{
			name:       "plugin only, user's own unrelated hooks present",
			settings:   "{" + fixturePluginOn + "," + fixtureForeignHooks + "}",
			wantStatus: StatusOK,
		},
		{
			// codex L6: `/opt/acme hook claude-code audit` must NOT be
			// mistaken for observer wiring.
			name:       "plugin only, a third-party acme hook present",
			settings:   "{" + fixturePluginOn + "," + fixtureAcmeHooks + "}",
			wantStatus: StatusOK,
		},
		{
			name:       "BOTH — plugin enabled and init hooks",
			settings:   "{" + fixturePluginOn + "," + fixtureObserverHooks + "}",
			wantStatus: StatusWarn,
			wantDetail: "hook event(s)",
		},
		{
			// The wsl.exe bridge shape must still be recognised as ours.
			name:       "BOTH — plugin enabled and wsl-bridged init hooks",
			settings:   "{" + fixturePluginOn + "," + fixtureWSLBridgeHooks + "}",
			wantStatus: StatusWarn,
			wantDetail: "hook event(s)",
		},
		{
			name:       "BOTH — plugin cached and init MCP entry",
			cached:     true,
			claudeJSON: fixtureObsMCP,
			wantStatus: StatusWarn,
			wantDetail: "MCP server",
		},
		{
			name:       "plugin cached, someone else's MCP entry only",
			cached:     true,
			claudeJSON: `{"mcpServers":{"other":{"command":"other"}}}`,
			wantStatus: StatusOK,
		},
		{
			name:       "plugin explicitly disabled with init hooks — no double fire",
			settings:   `{"enabledPlugins":{"` + claudeplugin.EnabledKey + `":false},` + fixtureObserverHooks + `}`,
			cached:     true,
			wantStatus: StatusOK,
		},
		{
			// H2: same name, foreign marketplace — not our plugin.
			name:       "foreign same-named plugin with init hooks",
			settings:   `{"enabledPlugins":{"` + claudeplugin.Name + `@acme-internal":true},` + fixtureObserverHooks + `}`,
			wantStatus: StatusOK,
		},
		{
			// H3: unreadable settings must be REPORTED, not silently
			// treated as "no plugin".
			name:       "corrupt settings is reported as uncertain",
			settings:   `{not json`,
			cached:     true,
			wantStatus: StatusOK,
			wantDetail: "could not check",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			noWindowsSide(t)
			home := t.TempDir()
			claudeDir := filepath.Join(home, ".claude")
			mustMkdirAll(t, claudeDir)
			if c.settings != "" {
				mustWriteFile(t, filepath.Join(claudeDir, "settings.json"), c.settings)
			}
			if c.claudeJSON != "" {
				mustWriteFile(t, filepath.Join(home, ".claude.json"), c.claudeJSON)
			}
			if c.cached {
				verifiedCache(t, claudeDir)
			}

			got := checkClaudeCodePlugin(home, "")
			if got.Status != c.wantStatus {
				t.Fatalf("Status = %v, want %v (message %q, details %v)",
					got.Status, c.wantStatus, got.Message, got.Details)
			}
			joined := strings.Join(got.Details, "\n")
			if c.wantDetail != "" && !strings.Contains(joined, c.wantDetail) {
				t.Errorf("details %v missing %q", got.Details, c.wantDetail)
			}
			if got.Status == StatusWarn {
				if !strings.Contains(got.Message, claudeplugin.Name) {
					t.Errorf("warn message %q does not name the plugin", got.Message)
				}
				if !strings.Contains(joined, "observer uninstall --claude-code") ||
					!strings.Contains(joined, "/plugin uninstall") {
					t.Errorf("warn details do not tell the user which one to remove: %v", got.Details)
				}
				// H1 residue must be stated wherever the cost is described.
				if !strings.Contains(joined, "compaction_events") {
					t.Errorf("warn details omit the compaction_events residue: %v", got.Details)
				}
			}
		})
	}
}

// TestCheckClaudeCodePluginWindowsSide is codex finding M5: a WSL daemon
// serving a Windows Claude Code must have its WINDOWS-side .claude
// examined too, and reported by name.
func TestCheckClaudeCodePluginWindowsSide(t *testing.T) {
	t.Run("windows side double-wired, native clean", func(t *testing.T) {
		home := t.TempDir()
		mustMkdirAll(t, filepath.Join(home, ".claude"))

		winDir := filepath.Join(t.TempDir(), ".claude")
		mustWriteFile(t, filepath.Join(winDir, "settings.json"),
			"{"+fixturePluginOn+","+fixtureWSLBridgeHooks+"}")
		fakeWindowsSide(t, winDir)

		got := checkClaudeCodePlugin(home, "")
		if got.Status != StatusWarn {
			t.Fatalf("Status = %v, want warn (message %q details %v)", got.Status, got.Message, got.Details)
		}
		if !strings.Contains(got.Message, "windows") {
			t.Errorf("message %q does not name the windows side", got.Message)
		}
		if !strings.Contains(strings.Join(got.Details, "\n"), winDir) {
			t.Errorf("details do not name the windows .claude dir %q: %v", winDir, got.Details)
		}
	})

	t.Run("windows side clean, native double-wired", func(t *testing.T) {
		home := t.TempDir()
		mustWriteFile(t, filepath.Join(home, ".claude", "settings.json"),
			"{"+fixturePluginOn+","+fixtureObserverHooks+"}")

		winDir := filepath.Join(t.TempDir(), ".claude")
		mustMkdirAll(t, winDir)
		fakeWindowsSide(t, winDir)

		got := checkClaudeCodePlugin(home, "")
		if got.Status != StatusWarn {
			t.Fatalf("Status = %v, want warn", got.Status)
		}
		if !strings.Contains(got.Message, "native") {
			t.Errorf("message %q does not name the native side", got.Message)
		}
		if strings.Contains(got.Message, "windows") {
			t.Errorf("message %q blames the windows side, which is clean", got.Message)
		}
	})

	t.Run("no windows side resolved — native only", func(t *testing.T) {
		noWindowsSide(t)
		home := t.TempDir()
		mustWriteFile(t, filepath.Join(home, ".claude", "settings.json"),
			"{"+fixturePluginOn+","+fixtureObserverHooks+"}")
		got := checkClaudeCodePlugin(home, "")
		if got.Status != StatusWarn {
			t.Fatalf("Status = %v, want warn", got.Status)
		}
		if strings.Contains(got.Message, "windows") {
			t.Errorf("message %q mentions a windows side that does not exist", got.Message)
		}
	})
}

// TestCheckClaudeCodePluginIsFilterable proves `observer doctor
// claude-code` scopes to the probe — Report.Filter matches on Name.
func TestCheckClaudeCodePluginIsFilterable(t *testing.T) {
	noWindowsSide(t)
	r := Report{Checks: []Check{checkClaudeCodePlugin(t.TempDir(), ""), {Name: "db.size"}}}
	got := r.Filter("claude-code")
	if len(got.Checks) != 1 || got.Checks[0].Name != "claude-code.plugin" {
		t.Fatalf("Filter(\"claude-code\") = %v, want just claude-code.plugin", got.Checks)
	}
}

// TestObserverHookEventsInClaudeSettings pins the ownership matcher used
// for reporting. codex L6: a third-party command must not count.
func TestObserverHookEventsInClaudeSettings(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"absent file", "", nil},
		{"malformed", `{nope`, nil},
		{"no hooks key", `{}`, nil},
		{"foreign hooks only", "{" + fixtureForeignHooks + "}", nil},
		{"acme hook with the same subcommand shape", "{" + fixtureAcmeHooks + "}", nil},
		{"observer hooks", "{" + fixtureObserverHooks + "}", []string{"PreToolUse", "Stop"}},
		{"wsl-bridged observer hooks", "{" + fixtureWSLBridgeHooks + "}", []string{"Stop"}},
		{
			"observer alongside a foreign hook on the same event",
			`{"hooks":{"PreToolUse":[` +
				`{"matcher":"*","hooks":[{"type":"command","command":"my-own-linter"}]},` +
				`{"matcher":"*","hooks":[{"type":"command","command":"/opt/observer hook claude-code pre-tool"}]}]}}`,
			[]string{"PreToolUse"},
		},
		{
			"a cursor hook command must not count",
			`{"hooks":{"PreToolUse":[{"matcher":"*","hooks":[{"type":"command","command":"/opt/observer hook cursor pre-tool"}]}]}}`,
			nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			if c.body != "" {
				mustWriteFile(t, path, c.body)
			}
			got := observerHookEventsInClaudeSettings(path)
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Errorf("events = %v, want %v", got, c.want)
			}
		})
	}
}

func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func mustWriteFile(t *testing.T, path, body string) {
	t.Helper()
	mustMkdirAll(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestCheckClaudeCodePluginRespectsDoctorSandbox is the read-side half of the
// 2026-07-31 incident fix (codex MEDIUM). DoctorOptions.HomeDir documents
// itself as the sandbox override, but the Windows-side probe used to call the
// zero-argument resolver — so a sandboxed doctor run still read the
// operator's real /mnt/c/Users/<u>/.claude, breaking the sandbox and making
// results machine-dependent.
//
// The seam is NOT replaced here: this drives the REAL resolver on purpose,
// recording every path it is asked for, and asserts (a) it is never invoked
// for a foreign home when the run is sandboxed, and (b) the check says so
// instead of silently pretending there is no Windows side.
func TestCheckClaudeCodePluginRespectsDoctorSandbox(t *testing.T) {
	var asked []string
	prev := windowsClaudeDir
	real := prev
	windowsClaudeDir = func(homeOverride string) string {
		dir := real(homeOverride) // the production resolver, gate included
		if dir != "" {
			asked = append(asked, dir)
		}
		return dir
	}
	t.Cleanup(func() { windowsClaudeDir = prev })

	sandbox := t.TempDir()
	got := checkClaudeCodePlugin(sandbox, sandbox) // homeOverride == the sandbox

	if len(asked) != 0 {
		t.Errorf("sandboxed doctor run resolved foreign-OS homes %v — it must read nothing outside %s", asked, sandbox)
	}
	if got.Status != StatusOK {
		t.Errorf("empty sandbox should be OK, got %v: %s", got.Status, got.Message)
	}
	var noted bool
	for _, d := range got.Details {
		if strings.Contains(d, "windows: not inspected") {
			noted = true
		}
		// Nothing in the report may name a path outside the sandbox.
		if strings.Contains(d, "/mnt/") {
			t.Errorf("detail names a foreign-OS path: %q", d)
		}
	}
	if !noted {
		t.Errorf("sandboxed run must SAY the windows side was not inspected, details = %v", got.Details)
	}

	// Production shape (homeOverride ""): the resolver runs unchanged. Assert
	// only that it is reached — whether a Windows home exists is host-specific.
	asked = nil
	_ = checkClaudeCodePlugin(sandbox, "")
	for _, d := range asked {
		if d == "" {
			t.Error("production resolver returned an empty dir into the asked list")
		}
	}
}
