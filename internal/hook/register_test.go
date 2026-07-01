package hook

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupRegistry(t *testing.T) *Registry {
	t.Helper()
	return setupRegistryWithConfig(t, "")
}

func setupRegistryWithConfig(t *testing.T, configPath string) *Registry {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".cursor"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := NewRegistry(Options{
		BinaryPath:    "/opt/observer/bin/observer",
		HomeDir:       home,
		ChecksumsPath: filepath.Join(home, ".observer", "hook_checksums.json"),
		ConfigPath:    configPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRegisterClaudeCodeFreshInstall(t *testing.T) {
	t.Parallel()
	r := setupRegistry(t)
	res := r.Register("claude-code")
	if res.Error != nil {
		t.Fatalf("Register: %v", res.Error)
	}
	if len(res.HooksAdded) != len(claudeCodeEvents) {
		t.Errorf("added %d want %d", len(res.HooksAdded), len(claudeCodeEvents))
	}

	body, err := os.ReadFile(res.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(body, &settings); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, body)
	}
	hooksBlock, ok := settings["hooks"].(map[string]any)
	if !ok {
		t.Fatal("hooks block missing")
	}
	for _, event := range claudeCodeEvents {
		groups, ok := hooksBlock[event].([]any)
		if !ok || len(groups) == 0 {
			t.Errorf("event %s missing", event)
		}
	}

	// Checksum file should be written.
	csPath := filepath.Join(r.opts.HomeDir, ".observer", "hook_checksums.json")
	if _, err := os.Stat(csPath); err != nil {
		t.Errorf("checksum file not created: %v", err)
	}
}

func TestRegisterClaudeCodeIdempotent(t *testing.T) {
	t.Parallel()
	r := setupRegistry(t)
	if res := r.Register("claude-code"); res.Error != nil {
		t.Fatalf("first: %v", res.Error)
	}
	res := r.Register("claude-code")
	if res.Error != nil {
		t.Fatalf("second: %v", res.Error)
	}
	if len(res.HooksAdded) != 0 {
		t.Errorf("second register added %d (want 0): %v", len(res.HooksAdded), res.HooksAdded)
	}
	if len(res.AlreadySet) != len(claudeCodeEvents) {
		t.Errorf("AlreadySet %d want %d", len(res.AlreadySet), len(claudeCodeEvents))
	}
}

func TestRegisterClaudeCodePreservesOtherKeys(t *testing.T) {
	t.Parallel()
	r := setupRegistry(t)
	// Write a settings.json with unrelated fields that we must not clobber.
	pre := `{"theme":"dark","permissions":{"allow":["bash"]}}`
	path := filepath.Join(r.opts.HomeDir, ".claude", "settings.json")
	if err := os.WriteFile(path, []byte(pre), 0o600); err != nil {
		t.Fatal(err)
	}

	res := r.Register("claude-code")
	if res.Error != nil {
		t.Fatalf("Register: %v", res.Error)
	}
	body, _ := os.ReadFile(path)
	var got map[string]any
	_ = json.Unmarshal(body, &got)
	if got["theme"] != "dark" {
		t.Errorf("theme lost: %v", got["theme"])
	}
	if _, ok := got["permissions"]; !ok {
		t.Error("permissions lost")
	}
}

func TestRegisterClaudeCodeConflict(t *testing.T) {
	t.Parallel()
	r := setupRegistry(t)
	path := filepath.Join(r.opts.HomeDir, ".claude", "settings.json")
	pre := `{"hooks":{"PreToolUse":[{"matcher":"*","hooks":[{"type":"command","command":"/usr/local/bin/other-hook"}]}]}}`
	if err := os.WriteFile(path, []byte(pre), 0o600); err != nil {
		t.Fatal(err)
	}
	res := r.Register("claude-code")
	if res.Error == nil {
		t.Fatal("expected conflict error")
	}

	// With Force, should succeed and add our hook alongside.
	r.opts.Force = true
	res = r.Register("claude-code")
	if res.Error != nil {
		t.Fatalf("force register: %v", res.Error)
	}
}

// TestRegisterClaudeCodeQuotesWindowsBinaryPath pins the v1.8.2+
// Windows-shaped-path normalization: when BinaryPath contains
// backslashes (the canonical Windows shape, e.g.
// `D:\programsx\...\observer.exe`), the written command MUST emit
// the forward-slash equivalent (`D:/programsx/...\observer.exe`)
// so the path survives any shell wrapping the Claude Code harness
// applies. Background: the v1.6.25 fix at this site single-quoted
// the backslash path so Git Bash on Windows wouldn't strip
// backslashes as escape sequences (`\p` → `p`, `\s` → `s`, etc.).
// That worked when Claude Code spawned the hook directly. But the
// harness's per-tool-call Bash wrapper can strip the single quotes
// upstream, leaving the unquoted backslash path for bash to
// escape-strip — symptom is the canonical
// `D:programsxsuperbased-observerbinobserver-hermes.exe: command
// not found` 127 exit, sidecar stays empty, dashboard effort column
// never populates. Forward-slash normalization removes the only
// character any shell layer interprets specially, so the path
// arrives at exec unmodified regardless of how many wrappers
// stripped or re-quoted it. Bonus: still safe for paths with spaces
// (e.g. `C:/Program Files/observer/...`) — shellQuoteIfNeeded
// re-applies a quote there, but the path inside the quote no longer
// has backslashes to escape-strip.
func TestRegisterClaudeCodeQuotesWindowsBinaryPath(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := NewRegistry(Options{
		BinaryPath:    `D:\programsx\superbased-observer\bin\observer.exe`,
		HomeDir:       home,
		ChecksumsPath: filepath.Join(home, ".observer", "hook_checksums.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res := r.Register("claude-code"); res.Error != nil {
		t.Fatalf("Register: %v", res.Error)
	}
	body, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Parse the JSON and pull out the PreToolUse command so the
	// assertion is decoupled from JSON-escape details. Forward-slash
	// shape: no quoting (the path has no shell-special chars), no
	// backslashes (so no escape-stripping vulnerability).
	got := extractClaudeCommand(t, body, "PreToolUse")
	wantPrefix := `D:/programsx/superbased-observer/bin/observer.exe hook claude-code `
	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("PreToolUse command = %q, want prefix %q", got, wantPrefix)
	}
	// Regression guard: no backslash from the original Windows path
	// must leak into the written command. If this fails, the
	// forward-slash normalization regressed and the harness-wrapper
	// 127 symptom will return.
	if strings.Contains(got, `\`) {
		t.Errorf("PreToolUse command leaks backslash: %q", got)
	}
}

// TestRegisterClaudeCodeForwardSlashesBothPaths explicitly pins
// that BOTH the binary path AND the --config path are emitted in
// forward-slash form when Windows-shaped. The bug this prevents:
// Claude Code's harness wrapping the hook command in `bash -c
// '<hook>; <user-cmd>'` and the inner single quote around the
// config path closes the outer single quote, leaving the inner
// backslash path for bash to escape-strip. Forward-slash config
// path means no backslashes can be escape-stripped no matter how
// many shell wrappers strip the quoting upstream.
func TestRegisterClaudeCodeForwardSlashesBothPaths(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := NewRegistry(Options{
		BinaryPath:    `D:\programsx\superbased-observer\bin\observer.exe`,
		ConfigPath:    `C:\Users\marmu\.observer\config.toml`,
		HomeDir:       home,
		ChecksumsPath: filepath.Join(home, ".observer", "hook_checksums.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res := r.Register("claude-code"); res.Error != nil {
		t.Fatalf("Register: %v", res.Error)
	}
	body, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	got := extractClaudeCommand(t, body, "PreToolUse")
	wantPrefix := `D:/programsx/superbased-observer/bin/observer.exe hook claude-code pre-tool --config 'C:/Users/marmu/.observer/config.toml'`
	if got != wantPrefix {
		t.Errorf("PreToolUse command = %q, want %q", got, wantPrefix)
	}
	// Anti-regression: zero backslashes anywhere in the command —
	// neither the binary nor the config path may carry a Windows
	// separator, since each one defeats the bash-escape-strip
	// invariant the v1.6.25 single-quote fix relied on (and which
	// some harness wrappers undo).
	if strings.Contains(got, `\`) {
		t.Errorf("PreToolUse command leaks backslash (v1.8.2+ regression): %q", got)
	}
}

// extractClaudeCommand decodes settings.json and returns the first
// hook command for the given event under the "*" matcher group.
// Test helper for the Windows-quoting assertions.
func extractClaudeCommand(t *testing.T, body []byte, event string) string {
	t.Helper()
	var s struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		t.Fatalf("unmarshal settings.json: %v\n%s", err, body)
	}
	groups, ok := s.Hooks[event]
	if !ok || len(groups) == 0 {
		t.Fatalf("event %s missing in settings.json:\n%s", event, body)
	}
	for _, g := range groups {
		for _, h := range g.Hooks {
			if h.Type == "command" {
				return h.Command
			}
		}
	}
	t.Fatalf("event %s has no command-type hook:\n%s", event, body)
	return ""
}

// TestRegisterCursorQuotesWindowsBinaryPath mirrors the claude-code
// variant for cursor.
func TestRegisterCursorQuotesWindowsBinaryPath(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".cursor"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := NewRegistry(Options{
		BinaryPath:    `D:\programsx\superbased-observer\bin\observer.exe`,
		HomeDir:       home,
		ChecksumsPath: filepath.Join(home, ".observer", "hook_checksums.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res := r.Register("cursor"); res.Error != nil {
		t.Fatalf("Register: %v", res.Error)
	}
	body, err := os.ReadFile(filepath.Join(home, ".cursor", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	// cursor hooks.json has shape: {"hooks": {<event>: [{"command": "..."}]}}
	var s struct {
		Hooks map[string][]struct {
			Command string `json:"command"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		t.Fatalf("unmarshal hooks.json: %v\n%s", err, body)
	}
	entries, ok := s.Hooks["beforeShellExecution"]
	if !ok || len(entries) == 0 {
		t.Fatalf("beforeShellExecution missing:\n%s", body)
	}
	// Forward-slash shape — mirrors TestRegisterClaudeCodeQuotesWindowsBinaryPath.
	// See forwardSlashPath in register.go for the rationale.
	wantPrefix := `D:/programsx/superbased-observer/bin/observer.exe hook cursor `
	if !strings.HasPrefix(entries[0].Command, wantPrefix) {
		t.Errorf("beforeShellExecution command = %q, want prefix %q", entries[0].Command, wantPrefix)
	}
	if strings.Contains(entries[0].Command, `\`) {
		t.Errorf("beforeShellExecution command leaks backslash: %q", entries[0].Command)
	}
}

// extractCodexCommand pulls the first command-type hook for the
// given event out of a codex hooks.json body. Helper for the
// codex-on-Windows quoting assertions below — decouples them from
// JSON-escape details (\\ vs \) by parsing first and comparing
// against the runtime string value.
func extractCodexCommand(t *testing.T, body []byte, event string) string {
	t.Helper()
	var s struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		t.Fatalf("unmarshal hooks.json: %v\n%s", err, body)
	}
	groups, ok := s.Hooks[event]
	if !ok || len(groups) == 0 || len(groups[0].Hooks) == 0 {
		t.Fatalf("event %s missing in hooks.json:\n%s", event, body)
	}
	for _, g := range groups {
		for _, h := range g.Hooks {
			if h.Type == "command" {
				return h.Command
			}
		}
	}
	t.Fatalf("event %s has no command-type hook:\n%s", event, body)
	return ""
}

// TestRegisterCodexUsesCmdSafeQuotingForWindowsBinaryPath pins the
// v1.6.28 regression fix: Codex on Windows spawns hooks through
// cmd.exe, which interprets `'...'` literally — the v1.6.25
// single-quote shape made every codex hook fire exit 1 with the
// error `'C:\\...\\observer.exe' is not recognised as an internal
// or external command`. Operator-reported 2026-05-23 against npm
// @superbased/observer 1.6.27 + codex CLI 0.133.0 on Windows.
//
// Post-fix the codex registrar uses codexCmdQuoteIfNeeded — paths
// with backslash trigger the cmd.exe-safe double-quote variant,
// which wraps only when the path contains special characters
// (space, &, <, >, |, etc.). The user's reported path has no
// spaces, so the post-fix command is unquoted and runs cleanly
// under cmd.exe (verified workaround in the bug report).
//
// Anti-regression: must NOT contain `'D:\` anywhere (the old
// single-quote prefix), since that's what triggered exit 1.
func TestRegisterCodexUsesCmdSafeQuotingForWindowsBinaryPath(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := NewRegistry(Options{
		BinaryPath:    `D:\programsx\superbased-observer\bin\observer.exe`,
		HomeDir:       home,
		ChecksumsPath: filepath.Join(home, ".observer", "hook_checksums.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res := r.Register("codex"); res.Error != nil {
		t.Fatalf("Register: %v", res.Error)
	}
	body, err := os.ReadFile(filepath.Join(home, ".codex", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	got := extractCodexCommand(t, body, "PreToolUse")
	// Path has no spaces / cmd.exe-special chars → unwrapped.
	wantPrefix := `D:\programsx\superbased-observer\bin\observer.exe hook codex `
	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("PreToolUse command = %q, want prefix %q", got, wantPrefix)
	}
	// Anti-regression: no POSIX single-quote wrapper.
	if strings.Contains(got, `'D:\`) || strings.Contains(got, `'C:\`) {
		t.Errorf("codex command on Windows path still has POSIX single-quote wrapper (cmd.exe will misparse): %q", got)
	}
}

// TestRegisterCodexWrapsWindowsPathWithSpacesInDoubleQuotes pins the
// other half of the cmd.exe-safe quoter — paths with spaces (like
// `C:\Program Files\observer\observer.exe`) DO need quoting, but in
// cmd.exe-style double quotes rather than POSIX single quotes.
func TestRegisterCodexWrapsWindowsPathWithSpacesInDoubleQuotes(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := NewRegistry(Options{
		BinaryPath:    `C:\Program Files\observer\observer.exe`,
		HomeDir:       home,
		ChecksumsPath: filepath.Join(home, ".observer", "hook_checksums.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res := r.Register("codex"); res.Error != nil {
		t.Fatalf("Register: %v", res.Error)
	}
	body, err := os.ReadFile(filepath.Join(home, ".codex", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	got := extractCodexCommand(t, body, "PreToolUse")
	wantPrefix := `"C:\Program Files\observer\observer.exe" hook codex `
	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("PreToolUse command = %q, want prefix %q", got, wantPrefix)
	}
}

// TestRegisterCodexLinuxPathStaysPosixQuoted pins that Linux/macOS
// codex hooks keep the POSIX single-quote behaviour — codex on
// non-Windows hosts spawns hooks via /bin/sh, where `'...'` is the
// stricter no-expansion quoting we want. Path without special
// chars stays unwrapped (matching shellQuoteIfNeeded's behaviour);
// path with spaces gets single-quoted.
func TestRegisterCodexLinuxPathStaysPosixQuoted(t *testing.T) {
	t.Parallel()
	// Clean Linux path → unwrapped.
	{
		home := t.TempDir()
		if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
			t.Fatal(err)
		}
		r, err := NewRegistry(Options{
			BinaryPath:    "/usr/local/bin/observer",
			HomeDir:       home,
			ChecksumsPath: filepath.Join(home, ".observer", "hook_checksums.json"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if res := r.Register("codex"); res.Error != nil {
			t.Fatalf("Register: %v", res.Error)
		}
		body, _ := os.ReadFile(filepath.Join(home, ".codex", "hooks.json"))
		got := extractCodexCommand(t, body, "PreToolUse")
		wantPrefix := "/usr/local/bin/observer hook codex "
		if !strings.HasPrefix(got, wantPrefix) {
			t.Errorf("clean linux path: got %q want prefix %q", got, wantPrefix)
		}
	}
	// Linux path with space → POSIX single-quote.
	{
		home := t.TempDir()
		if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
			t.Fatal(err)
		}
		r, err := NewRegistry(Options{
			BinaryPath:    "/home/user/My Stuff/observer",
			HomeDir:       home,
			ChecksumsPath: filepath.Join(home, ".observer", "hook_checksums.json"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if res := r.Register("codex"); res.Error != nil {
			t.Fatalf("Register: %v", res.Error)
		}
		body, _ := os.ReadFile(filepath.Join(home, ".codex", "hooks.json"))
		got := extractCodexCommand(t, body, "PreToolUse")
		wantPrefix := `'/home/user/My Stuff/observer' hook codex `
		if !strings.HasPrefix(got, wantPrefix) {
			t.Errorf("linux path with space: got %q want prefix %q", got, wantPrefix)
		}
	}
}

// TestRegisterCodexHonoursCmdSafeConfigPath pins that the --config
// path embedded in the codex hook command ALSO uses cmd.exe-safe
// quoting when codex+config are on Windows-shaped paths. Pre-fix
// (and pre-v1.6.25, even) the config path was wrapped in POSIX
// single-quote via configFlagSuffix() which independently broke
// codex on Windows.
func TestRegisterCodexHonoursCmdSafeConfigPath(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := NewRegistry(Options{
		BinaryPath:    `D:\bin\observer.exe`,
		HomeDir:       home,
		ChecksumsPath: filepath.Join(home, ".observer", "hook_checksums.json"),
		ConfigPath:    `C:\Users\Administrator\.observer\config.toml`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res := r.Register("codex"); res.Error != nil {
		t.Fatalf("Register: %v", res.Error)
	}
	body, _ := os.ReadFile(filepath.Join(home, ".codex", "hooks.json"))
	got := extractCodexCommand(t, body, "PreToolUse")
	// Neither path has spaces → both should be unwrapped.
	wantSubstr := ` --config C:\Users\Administrator\.observer\config.toml`
	if !strings.Contains(got, wantSubstr) {
		t.Errorf("codex command = %q, want config substring %q", got, wantSubstr)
	}
	if strings.Contains(got, `'C:\`) {
		t.Errorf("config path still POSIX single-quoted (cmd.exe will misparse): %q", got)
	}
}

// TestRegisterClaudeCodeCrossBinaryPathRefresh pins the
// content-heuristic upgrade path: when settings.json holds an
// observer-shaped hook entry pointing at a DIFFERENT binary path
// (e.g. an npm-bundled observer in node_modules, a previous install
// under a renamed home dir), the next register pass should silently
// refresh the entry to the current binary path — NOT trip the
// conflict guard with "non-observer hook; pass --force." This is
// the bug class that surfaced when a user had Claude Code's hooks
// registered to a stale binary path and observer's v1.6.22 effort
// sidecar silently stayed empty because the registered (old) binary
// pre-dated the effort sidecar and never wrote to it. The Linux
// path-prefix detector (replaced by isObserverClaudeEntry) couldn't
// recognise the stale entry as ours; the content-heuristic does.
func TestRegisterClaudeCodeCrossBinaryPathRefresh(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}

	// First install: observer at /tmp/observer-A.
	first, err := NewRegistry(Options{
		BinaryPath:    "/tmp/observer-A",
		HomeDir:       home,
		ChecksumsPath: filepath.Join(home, ".observer", "hook_checksums.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res := first.Register("claude-code"); res.Error != nil {
		t.Fatalf("first: %v", res.Error)
	}

	// Second install: SAME home dir, NEW binary path. Must silently
	// refresh — no Force, no conflict error.
	second, err := NewRegistry(Options{
		BinaryPath:    "/usr/local/bin/observer",
		HomeDir:       home,
		ChecksumsPath: filepath.Join(home, ".observer", "hook_checksums.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	res := second.Register("claude-code")
	if res.Error != nil {
		t.Fatalf("second (cross-binary refresh): %v", res.Error)
	}
	if len(res.HooksAdded) != len(claudeCodeEvents) {
		t.Errorf("HooksAdded = %d want %d (every event should be re-added)",
			len(res.HooksAdded), len(claudeCodeEvents))
	}

	// Verify the file ONLY references the new binary now.
	body, _ := os.ReadFile(res.ConfigPath)
	if strings.Contains(string(body), "/tmp/observer-A") {
		t.Errorf("stale binary path leaked into refreshed settings.json:\n%s", body)
	}
	if !strings.Contains(string(body), "/usr/local/bin/observer") {
		t.Errorf("new binary path missing from refreshed settings.json:\n%s", body)
	}
}

// TestUnregisterClaudeCodeRemovesCrossBinaryEntries pins the
// Unregister-side counterpart to the v1.6.25 register-side cross-
// binary refresh: when settings.json holds an observer-shaped hook
// entry pointing at a DIFFERENT binary path (e.g. npm-bundled
// observer in node_modules) and the user uninstalls from the
// current binary, filterClaudeGroups MUST recognise the stale
// entry as ours via isObserverClaudeEntry and remove it — NOT
// leave it orphaned because the byte-exact prefix-match misses.
func TestUnregisterClaudeCodeRemovesCrossBinaryEntries(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}

	first, err := NewRegistry(Options{
		BinaryPath:    "/tmp/observer-A",
		HomeDir:       home,
		ChecksumsPath: filepath.Join(home, ".observer", "hook_checksums.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res := first.Register("claude-code"); res.Error != nil {
		t.Fatalf("first: %v", res.Error)
	}

	second, err := NewRegistry(Options{
		BinaryPath:    "/usr/local/bin/observer",
		HomeDir:       home,
		ChecksumsPath: filepath.Join(home, ".observer", "hook_checksums.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	res := second.Unregister("claude-code")
	if res.Error != nil {
		t.Fatalf("unregister: %v", res.Error)
	}
	if len(res.HooksRemoved) != len(claudeCodeEvents) {
		t.Errorf("HooksRemoved = %d want %d (every cross-binary entry should be removed)",
			len(res.HooksRemoved), len(claudeCodeEvents))
	}

	body, _ := os.ReadFile(res.ConfigPath)
	if bytes.Contains(body, []byte("/tmp/observer-A")) {
		t.Errorf("stale observer-A entries left behind after cross-binary unregister:\n%s", body)
	}
}

func TestRegisterCursorFreshInstall(t *testing.T) {
	t.Parallel()
	r := setupRegistry(t)
	res := r.Register("cursor")
	if res.Error != nil {
		t.Fatalf("Register: %v", res.Error)
	}
	body, err := os.ReadFile(res.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, body)
	}
	if got["version"] == nil {
		t.Error("cursor version missing")
	}
	hooks, ok := got["hooks"].(map[string]any)
	if !ok {
		t.Fatal("hooks block missing")
	}
	for _, event := range cursorEvents {
		if _, ok := hooks[event]; !ok {
			t.Errorf("event %s missing", event)
		}
	}
}

func TestRegisterCursorIdempotent(t *testing.T) {
	t.Parallel()
	r := setupRegistry(t)
	_ = r.Register("cursor")
	res := r.Register("cursor")
	if res.Error != nil {
		t.Fatal(res.Error)
	}
	if len(res.HooksAdded) != 0 {
		t.Errorf("second add added %d", len(res.HooksAdded))
	}
}

// TestRegisterCursorCrossBinaryPathRefresh mirrors
// TestRegisterClaudeCodeCrossBinaryPathRefresh for the cursor
// registrar: a hooks.json carrying an observer-shaped entry that
// points at a different binary path must be recognised as ours via
// the ` hook cursor ` content-heuristic and silently refreshed, not
// flagged as a foreign-tool conflict.
func TestRegisterCursorCrossBinaryPathRefresh(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".cursor"), 0o755); err != nil {
		t.Fatal(err)
	}

	first, err := NewRegistry(Options{
		BinaryPath:    "/tmp/observer-A",
		HomeDir:       home,
		ChecksumsPath: filepath.Join(home, ".observer", "hook_checksums.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res := first.Register("cursor"); res.Error != nil {
		t.Fatalf("first: %v", res.Error)
	}

	second, err := NewRegistry(Options{
		BinaryPath:    "/usr/local/bin/observer",
		HomeDir:       home,
		ChecksumsPath: filepath.Join(home, ".observer", "hook_checksums.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	res := second.Register("cursor")
	if res.Error != nil {
		t.Fatalf("second (cross-binary refresh): %v", res.Error)
	}
	if len(res.HooksAdded) != len(cursorEvents) {
		t.Errorf("HooksAdded = %d want %d", len(res.HooksAdded), len(cursorEvents))
	}

	body, _ := os.ReadFile(res.ConfigPath)
	if strings.Contains(string(body), "/tmp/observer-A") {
		t.Errorf("stale binary path leaked into refreshed hooks.json:\n%s", body)
	}
	if !strings.Contains(string(body), "/usr/local/bin/observer") {
		t.Errorf("new binary path missing from refreshed hooks.json:\n%s", body)
	}
}

// TestUnregisterCursorRemovesCrossBinaryEntries mirrors the claude-
// code variant: cursor hooks.json carrying entries written by a
// different observer binary path must be cleaned up via the content-
// heuristic, not left behind because the byte-exact prefix-match
// missed.
func TestUnregisterCursorRemovesCrossBinaryEntries(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".cursor"), 0o755); err != nil {
		t.Fatal(err)
	}

	first, err := NewRegistry(Options{
		BinaryPath:    "/tmp/observer-A",
		HomeDir:       home,
		ChecksumsPath: filepath.Join(home, ".observer", "hook_checksums.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res := first.Register("cursor"); res.Error != nil {
		t.Fatalf("first: %v", res.Error)
	}

	second, err := NewRegistry(Options{
		BinaryPath:    "/usr/local/bin/observer",
		HomeDir:       home,
		ChecksumsPath: filepath.Join(home, ".observer", "hook_checksums.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	res := second.Unregister("cursor")
	if res.Error != nil {
		t.Fatalf("unregister: %v", res.Error)
	}
	if len(res.HooksRemoved) == 0 {
		t.Errorf("HooksRemoved = 0; expected cross-binary entries to be removed")
	}
	if _, err := os.Stat(res.ConfigPath); err == nil {
		// File still exists — verify no observer-A entries remain.
		body, _ := os.ReadFile(res.ConfigPath)
		if bytes.Contains(body, []byte("/tmp/observer-A")) {
			t.Errorf("stale observer-A entries left behind after cross-binary unregister:\n%s", body)
		}
	}
}

func TestRegisterUnknownTool(t *testing.T) {
	t.Parallel()
	r := setupRegistry(t)
	res := r.Register("notarealthing")
	if res.Error == nil {
		t.Fatal("expected error for unknown tool")
	}
}

func TestDryRunDoesNotWrite(t *testing.T) {
	t.Parallel()
	r := setupRegistry(t)
	r.opts.DryRun = true
	res := r.Register("claude-code")
	if res.Error != nil {
		t.Fatal(res.Error)
	}
	_, err := os.Stat(res.ConfigPath)
	if err == nil {
		t.Error("dry run wrote the config")
	}
	if !res.DryRun {
		t.Error("result should flag dry run")
	}
}

func TestInstalledDetectsDirs(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := NewRegistry(Options{
		BinaryPath: "/x",
		HomeDir:    home,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := r.Installed()
	// "claude-code" must be present (the .claude/ in HomeDir was the
	// signal). Other entries (notably "cursor-windows" on WSL hosts
	// where crossmount detects a Windows-side .cursor/) are platform-
	// dependent — the test only asserts the .claude/ trigger, not the
	// absence of cross-mount auto-detection.
	if !containsString(got, "claude-code") {
		t.Errorf("Installed = %v, want it to contain claude-code", got)
	}
}

func containsString(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

func TestCommandContainsBinaryPath(t *testing.T) {
	t.Parallel()
	r := setupRegistry(t)
	if res := r.Register("claude-code"); res.Error != nil {
		t.Fatal(res.Error)
	}
	body, _ := os.ReadFile(filepath.Join(r.opts.HomeDir, ".claude", "settings.json"))
	if !strings.Contains(string(body), r.opts.BinaryPath) {
		t.Errorf("binary path not in settings: %s", body)
	}
}

// TestRegisterClaudeCodeWithConfigPath pins the v1.4.43+ hook→DB-wiring
// fix: when Options.ConfigPath is set, every registered hook command
// gets `--config <path>` appended so the spawned hook handler reads
// the same DB the proxy writes to. Without this, /compact rows always
// land on ~/.observer/observer.db regardless of which proxy daemon
// fired the hook — D23's Injector then queries the proxy's DB and
// finds nothing. Surfaced 2026-05-08 dogfood.
func TestRegisterClaudeCodeWithConfigPath(t *testing.T) {
	t.Parallel()
	r := setupRegistryWithConfig(t, "/tmp/ab-claude/on/observer-config.toml")
	res := r.Register("claude-code")
	if res.Error != nil {
		t.Fatalf("Register: %v", res.Error)
	}
	body, err := os.ReadFile(res.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	want := r.opts.BinaryPath + " hook claude-code pre-compact --config '/tmp/ab-claude/on/observer-config.toml'"
	if !strings.Contains(string(body), want) {
		t.Errorf("expected hook command to include --config; got:\n%s", body)
	}
}

// TestRegisterClaudeCodeRefreshesOnConfigPathChange pins that re-running
// init with a different config silently overwrites the registration
// instead of being treated as already-set. Without this, switching
// configs would leave the hook pointing at the old DB.
func TestRegisterClaudeCodeRefreshesOnConfigPathChange(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	first, err := NewRegistry(Options{
		BinaryPath:    "/opt/observer/bin/observer",
		HomeDir:       home,
		ChecksumsPath: filepath.Join(home, ".observer", "hook_checksums.json"),
		ConfigPath:    "/tmp/old.toml",
	})
	if err != nil {
		t.Fatal(err)
	}
	if r := first.Register("claude-code"); r.Error != nil {
		t.Fatalf("first: %v", r.Error)
	}

	second, err := NewRegistry(Options{
		BinaryPath:    "/opt/observer/bin/observer",
		HomeDir:       home,
		ChecksumsPath: filepath.Join(home, ".observer", "hook_checksums.json"),
		ConfigPath:    "/tmp/new.toml",
	})
	if err != nil {
		t.Fatal(err)
	}
	res := second.Register("claude-code")
	if res.Error != nil {
		t.Fatalf("second: %v", res.Error)
	}
	if len(res.HooksAdded) == 0 {
		t.Fatal("expected refresh to re-add events with new config")
	}
	body, _ := os.ReadFile(res.ConfigPath)
	if strings.Contains(string(body), "/tmp/old.toml") {
		t.Errorf("old config path leaked into refreshed registration: %s", body)
	}
	if !strings.Contains(string(body), "/tmp/new.toml") {
		t.Errorf("new config path missing: %s", body)
	}
}

// TestRegisterCursorWindowsFreshInstall pins the wsl.exe-prefixed
// hook command shape against a fake Windows-side .cursor/. The
// resulting hooks.json must carry every cursorEvents entry, each
// shaped `wsl.exe -d <distro> -- <linux-bin> hook cursor <event>
// [--config ...]`. Tests the explicit-WindowsCursorHome path so the
// crossmount auto-detect doesn't depend on /mnt/c presence.
func TestRegisterCursorWindowsFreshInstall(t *testing.T) {
	t.Parallel()
	winHome := t.TempDir() // stand-in for /mnt/c/Users/<u>
	cursorDir := filepath.Join(winHome, ".cursor")
	if err := os.MkdirAll(cursorDir, 0o755); err != nil {
		t.Fatal(err)
	}
	wslHome := t.TempDir()
	r, err := NewRegistry(Options{
		BinaryPath:        "/home/marmutapp/superbased-observer/bin/observer",
		HomeDir:           wslHome,
		ChecksumsPath:     filepath.Join(wslHome, ".observer", "hook_checksums.json"),
		WindowsCursorHome: winHome,
		WSLDistro:         "Ubuntu-20.04",
		ConfigPath:        "/home/marmutapp/.observer/config.toml",
	})
	if err != nil {
		t.Fatal(err)
	}
	res := r.Register("cursor-windows")
	if res.Error != nil {
		t.Fatalf("Register: %v", res.Error)
	}
	if len(res.HooksAdded) != len(cursorEvents) {
		t.Errorf("HooksAdded count = %d want %d", len(res.HooksAdded), len(cursorEvents))
	}
	if res.ConfigPath != filepath.Join(cursorDir, "hooks.json") {
		t.Errorf("ConfigPath = %q want %q", res.ConfigPath, filepath.Join(cursorDir, "hooks.json"))
	}

	body, err := os.ReadFile(res.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(body, &settings); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, body)
	}
	hooksBlock, ok := settings["hooks"].(map[string]any)
	if !ok {
		t.Fatal("hooks block missing")
	}
	for _, event := range cursorEvents {
		entries, ok := hooksBlock[event].([]any)
		if !ok || len(entries) == 0 {
			t.Errorf("event %s missing", event)
			continue
		}
		first, _ := entries[0].(map[string]any)
		cmd, _ := first["command"].(string)
		wantPrefix := "MSYS_NO_PATHCONV=1 wsl.exe -d Ubuntu-20.04 -- /home/marmutapp/superbased-observer/bin/observer hook cursor " + event
		if !strings.HasPrefix(cmd, wantPrefix) {
			t.Errorf("event %s cmd = %q want prefix %q", event, cmd, wantPrefix)
		}
		if !strings.Contains(cmd, "--config '/home/marmutapp/.observer/config.toml'") {
			t.Errorf("event %s cmd missing --config: %q", event, cmd)
		}
	}
}

// TestRegisterCursorWindowsRequiresDistro pins the
// "missing distro" error path. Without a distro, `wsl.exe` would be
// ambiguous on a host with multiple WSL distros — better to fail
// loudly at install time than write a broken hooks.json.
func TestRegisterCursorWindowsRequiresDistro(t *testing.T) {
	winHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(winHome, ".cursor"), 0o755); err != nil {
		t.Fatal(err)
	}
	wslHome := t.TempDir()
	t.Setenv("WSL_DISTRO_NAME", "")
	r, err := NewRegistry(Options{
		BinaryPath:        "/x",
		HomeDir:           wslHome,
		WindowsCursorHome: winHome,
	})
	if err != nil {
		t.Fatal(err)
	}
	res := r.Register("cursor-windows")
	if res.Error == nil {
		t.Fatal("expected error when distro unset, got nil")
	}
	if !strings.Contains(res.Error.Error(), "WSL distro unknown") {
		t.Errorf("error message = %q want it to mention 'WSL distro unknown'", res.Error)
	}
}

// TestRegisterCursorWindowsCrossBinaryPathRefresh pins the upgrade
// path: when a hooks.json was written by a previous observer
// install (different binary path), re-registering with a new
// binary should silently refresh the entries — not error with
// "non-observer hook" as if a foreign tool had written them. This
// is the staleness-detection bug that surfaced when the
// auto-register pass on `observer start` ran twice with different
// binary paths (e.g. /tmp smoke-test vs production install).
func TestRegisterCursorWindowsCrossBinaryPathRefresh(t *testing.T) {
	t.Parallel()
	winHome := t.TempDir()
	cursorDir := filepath.Join(winHome, ".cursor")
	if err := os.MkdirAll(cursorDir, 0o755); err != nil {
		t.Fatal(err)
	}
	wslHome := t.TempDir()

	// First install: binary at /tmp/observer-A.
	first, err := NewRegistry(Options{
		BinaryPath:        "/tmp/observer-A",
		HomeDir:           wslHome,
		ChecksumsPath:     filepath.Join(wslHome, ".observer", "hook_checksums.json"),
		WindowsCursorHome: winHome,
		WSLDistro:         "Ubuntu-20.04",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res := first.Register("cursor-windows"); res.Error != nil {
		t.Fatalf("first: %v", res.Error)
	}

	// Second install: same registry, NEW binary path. Should
	// silently refresh, not error.
	second, err := NewRegistry(Options{
		BinaryPath:        "/usr/local/bin/observer",
		HomeDir:           wslHome,
		ChecksumsPath:     filepath.Join(wslHome, ".observer", "hook_checksums.json"),
		WindowsCursorHome: winHome,
		WSLDistro:         "Ubuntu-20.04",
	})
	if err != nil {
		t.Fatal(err)
	}
	res := second.Register("cursor-windows")
	if res.Error != nil {
		t.Fatalf("second (cross-binary refresh): %v", res.Error)
	}
	if len(res.HooksAdded) != len(cursorEvents) {
		t.Errorf("HooksAdded = %d want %d", len(res.HooksAdded), len(cursorEvents))
	}

	// Verify the file ONLY references the new binary now.
	body, _ := os.ReadFile(res.ConfigPath)
	if strings.Contains(string(body), "/tmp/observer-A") {
		t.Errorf("stale binary path leaked into refreshed hooks.json:\n%s", body)
	}
	if !strings.Contains(string(body), "/usr/local/bin/observer") {
		t.Errorf("new binary path missing:\n%s", body)
	}
}

// TestRegisterCursorWindowsRespectsForeignEntry pins the
// safety-first behavior: a non-observer-shaped command (e.g. user
// has a different tool's hook wired in) must NOT be silently
// overwritten. Force=false → return error; Force=true → overwrite.
func TestRegisterCursorWindowsRespectsForeignEntry(t *testing.T) {
	t.Parallel()
	winHome := t.TempDir()
	cursorDir := filepath.Join(winHome, ".cursor")
	if err := os.MkdirAll(cursorDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Plant a foreign hook in beforeShellExecution.
	hooksPath := filepath.Join(cursorDir, "hooks.json")
	foreign := `{"version":1,"hooks":{"beforeShellExecution":[{"command":"powershell.exe Write-Host hi"}]}}`
	if err := os.WriteFile(hooksPath, []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}
	wslHome := t.TempDir()
	r, err := NewRegistry(Options{
		BinaryPath:        "/tmp/observer",
		HomeDir:           wslHome,
		WindowsCursorHome: winHome,
		WSLDistro:         "Ubuntu-20.04",
	})
	if err != nil {
		t.Fatal(err)
	}
	res := r.Register("cursor-windows")
	if res.Error == nil {
		t.Fatal("expected error when foreign hook present, got nil")
	}
	if !strings.Contains(res.Error.Error(), "non-observer") {
		t.Errorf("error message = %q want it to mention 'non-observer'", res.Error)
	}
}

// TestRegisterCursorWindowsIdempotent re-runs registration on an
// already-installed hooks.json and asserts no events get re-added.
func TestRegisterCursorWindowsIdempotent(t *testing.T) {
	t.Parallel()
	winHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(winHome, ".cursor"), 0o755); err != nil {
		t.Fatal(err)
	}
	wslHome := t.TempDir()
	r, err := NewRegistry(Options{
		BinaryPath:        "/opt/observer/bin/observer",
		HomeDir:           wslHome,
		ChecksumsPath:     filepath.Join(wslHome, ".observer", "hook_checksums.json"),
		WindowsCursorHome: winHome,
		WSLDistro:         "Ubuntu-22.04",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res := r.Register("cursor-windows"); res.Error != nil {
		t.Fatalf("first: %v", res.Error)
	}
	res := r.Register("cursor-windows")
	if res.Error != nil {
		t.Fatalf("second: %v", res.Error)
	}
	if len(res.HooksAdded) != 0 {
		t.Errorf("second register added %d (want 0): %v", len(res.HooksAdded), res.HooksAdded)
	}
	if len(res.AlreadySet) != len(cursorEvents) {
		t.Errorf("AlreadySet %d want %d", len(res.AlreadySet), len(cursorEvents))
	}
}

// TestShellQuoteEscapesSingleQuotes pins the shell-quoting helper so
// pathological config paths (with embedded ' or spaces) don't break
// the bash -c invocation Claude Code uses to run the hook command.
func TestShellQuoteEscapesSingleQuotes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"", "''"},
		{"/plain/path", "'/plain/path'"},
		{"/path with space", "'/path with space'"},
		{"/o'malley/x", `'/o'\''malley/x'`},
	}
	for _, c := range cases {
		if got := shellQuote(c.in); got != c.want {
			t.Errorf("shellQuote(%q) = %q want %q", c.in, got, c.want)
		}
	}
}

// codexRegistry mirrors setupRegistry but seeds ~/.codex instead of
// ~/.claude / ~/.cursor so codex registration has a directory to scan.
func codexRegistry(t *testing.T) *Registry {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := NewRegistry(Options{
		BinaryPath:    "/opt/observer/bin/observer",
		HomeDir:       home,
		ChecksumsPath: filepath.Join(home, ".observer", "hook_checksums.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRegisterCodexFreshInstall(t *testing.T) {
	t.Parallel()
	r := codexRegistry(t)
	res := r.Register("codex")
	if res.Error != nil {
		t.Fatalf("Register: %v", res.Error)
	}
	if len(res.HooksAdded) != len(codexEvents) {
		t.Errorf("added %d want %d", len(res.HooksAdded), len(codexEvents))
	}
	if got, want := res.ConfigPath, filepath.Join(r.opts.HomeDir, ".codex", "hooks.json"); got != want {
		t.Errorf("ConfigPath=%q want %q", got, want)
	}

	// hooks.json shape: {"hooks": {<event>: [<group>...]}}
	body, err := os.ReadFile(res.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("hooks.json not valid JSON: %v\n%s", err, body)
	}
	hooks, ok := got["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("hooks.json missing top-level hooks block: %s", body)
	}
	for _, event := range codexEvents {
		groups, ok := hooks[event].([]any)
		if !ok || len(groups) == 0 {
			t.Errorf("event %s missing in hooks.json", event)
		}
	}

	// config.toml feature flag: [features].hooks = true must be present.
	cfgRaw, err := os.ReadFile(filepath.Join(r.opts.HomeDir, ".codex", "config.toml"))
	if err != nil {
		t.Fatalf("config.toml not written: %v", err)
	}
	if !bytes.Contains(cfgRaw, []byte("hooks = true")) {
		t.Errorf("config.toml missing hooks=true:\n%s", cfgRaw)
	}
}

func TestRegisterCodexIdempotent(t *testing.T) {
	t.Parallel()
	r := codexRegistry(t)
	if res := r.Register("codex"); res.Error != nil {
		t.Fatalf("first: %v", res.Error)
	}
	res := r.Register("codex")
	if res.Error != nil {
		t.Fatalf("second: %v", res.Error)
	}
	if len(res.HooksAdded) != 0 {
		t.Errorf("second register added %d (want 0)", len(res.HooksAdded))
	}
	if len(res.AlreadySet) != len(codexEvents) {
		t.Errorf("AlreadySet %d want %d", len(res.AlreadySet), len(codexEvents))
	}
}

func TestRegisterCodexPreservesExistingConfigToml(t *testing.T) {
	t.Parallel()
	r := codexRegistry(t)
	// Pre-existing config with unrelated keys our flag insert must
	// preserve. Mirrors a real ~/.codex/config.toml shape.
	pre := `personality = "pragmatic"

[projects."/tmp/foo"]
trust_level = "trusted"
`
	cfgPath := filepath.Join(r.opts.HomeDir, ".codex", "config.toml")
	if err := os.WriteFile(cfgPath, []byte(pre), 0o600); err != nil {
		t.Fatal(err)
	}
	res := r.Register("codex")
	if res.Error != nil {
		t.Fatalf("Register: %v", res.Error)
	}
	body, _ := os.ReadFile(cfgPath)
	if !bytes.Contains(body, []byte("personality")) {
		t.Errorf("personality lost from config.toml:\n%s", body)
	}
	if !bytes.Contains(body, []byte("/tmp/foo")) {
		t.Errorf("projects table lost from config.toml:\n%s", body)
	}
	if !bytes.Contains(body, []byte("hooks = true")) {
		t.Errorf("hooks=true not added:\n%s", body)
	}
}

func TestRegisterCodexConflict(t *testing.T) {
	t.Parallel()
	r := codexRegistry(t)
	// Pre-existing user-authored hook on PreToolUse — our register
	// must refuse without --force.
	hooksPath := filepath.Join(r.opts.HomeDir, ".codex", "hooks.json")
	pre := `{"hooks":{"PreToolUse":[{"matcher":"*","hooks":[{"type":"command","command":"/usr/local/bin/my-policy"}]}]}}`
	if err := os.WriteFile(hooksPath, []byte(pre), 0o600); err != nil {
		t.Fatal(err)
	}
	res := r.Register("codex")
	if res.Error == nil {
		t.Fatalf("expected conflict error, got HooksAdded=%v", res.HooksAdded)
	}
	if !strings.Contains(res.Error.Error(), "non-observer hook") {
		t.Errorf("unexpected error: %v", res.Error)
	}
}

// TestRegisterCodexCrossBinaryPathRefresh mirrors
// TestRegisterClaudeCodeCrossBinaryPathRefresh for the codex
// registrar: a hooks.json carrying an observer-shaped entry that
// points at a different binary path must be recognised as ours via
// the ` hook codex ` content-heuristic and silently refreshed.
func TestRegisterCodexCrossBinaryPathRefresh(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}

	first, err := NewRegistry(Options{
		BinaryPath:    "/tmp/observer-A",
		HomeDir:       home,
		ChecksumsPath: filepath.Join(home, ".observer", "hook_checksums.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res := first.Register("codex"); res.Error != nil {
		t.Fatalf("first: %v", res.Error)
	}

	second, err := NewRegistry(Options{
		BinaryPath:    "/usr/local/bin/observer",
		HomeDir:       home,
		ChecksumsPath: filepath.Join(home, ".observer", "hook_checksums.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	res := second.Register("codex")
	if res.Error != nil {
		t.Fatalf("second (cross-binary refresh): %v", res.Error)
	}
	if len(res.HooksAdded) != len(codexEvents) {
		t.Errorf("HooksAdded = %d want %d", len(res.HooksAdded), len(codexEvents))
	}

	body, _ := os.ReadFile(res.ConfigPath)
	if strings.Contains(string(body), "/tmp/observer-A") {
		t.Errorf("stale binary path leaked into refreshed hooks.json:\n%s", body)
	}
	if !strings.Contains(string(body), "/usr/local/bin/observer") {
		t.Errorf("new binary path missing from refreshed hooks.json:\n%s", body)
	}
}

func TestUnregisterCodexRemovesObserverEntries(t *testing.T) {
	t.Parallel()
	r := codexRegistry(t)
	if res := r.Register("codex"); res.Error != nil {
		t.Fatalf("Register: %v", res.Error)
	}
	res := r.Unregister("codex")
	if res.Error != nil {
		t.Fatalf("Unregister: %v", res.Error)
	}
	if len(res.HooksRemoved) != len(codexEvents) {
		t.Errorf("removed %d want %d", len(res.HooksRemoved), len(codexEvents))
	}
	body, _ := os.ReadFile(res.ConfigPath)
	if bytes.Contains(body, []byte("/opt/observer/bin/observer")) {
		t.Errorf("observer entries still present after unregister:\n%s", body)
	}
}

// TestUnregisterCodexRemovesCrossBinaryEntries mirrors the claude-
// code variant: a codex hooks.json carrying entries from a
// different observer binary path must be cleaned up via the
// content-heuristic.
func TestUnregisterCodexRemovesCrossBinaryEntries(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}

	first, err := NewRegistry(Options{
		BinaryPath:    "/tmp/observer-A",
		HomeDir:       home,
		ChecksumsPath: filepath.Join(home, ".observer", "hook_checksums.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res := first.Register("codex"); res.Error != nil {
		t.Fatalf("first: %v", res.Error)
	}

	second, err := NewRegistry(Options{
		BinaryPath:    "/usr/local/bin/observer",
		HomeDir:       home,
		ChecksumsPath: filepath.Join(home, ".observer", "hook_checksums.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	res := second.Unregister("codex")
	if res.Error != nil {
		t.Fatalf("unregister: %v", res.Error)
	}
	if len(res.HooksRemoved) != len(codexEvents) {
		t.Errorf("HooksRemoved = %d want %d (every cross-binary entry should be removed)",
			len(res.HooksRemoved), len(codexEvents))
	}

	body, _ := os.ReadFile(res.ConfigPath)
	if bytes.Contains(body, []byte("/tmp/observer-A")) {
		t.Errorf("stale observer-A entries left behind after cross-binary unregister:\n%s", body)
	}
}

func TestInstalledDetectsCodex(t *testing.T) {
	t.Parallel()
	r := codexRegistry(t)
	got := r.Installed()
	found := false
	for _, t := range got {
		if t == "codex" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Installed()=%v does not include codex", got)
	}
}

// TestObserverEntryHeuristicsAreDisjoint pins the contract that the
// Linux/default and Windows-bridge heuristics match disjoint command
// shapes — so if both registrar paths write to the same settings.json
// (rare but possible after switching observer modes on the same
// host), `observer start` from one mode does not silently rewrite
// the other mode's entries. Without the wsl.exe-prefix guard added
// in v1.6.26, isObserverClaudeEntry / isObserverCursorEntry matched
// wsl-wrapped commands too, causing the Linux/default refresh path
// to overwrite WSL-bridge entries into native shape.
func TestObserverEntryHeuristicsAreDisjoint(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		cmd         string
		wantNative  bool // Linux/default heuristic
		wantWindows bool // wsl-bridge heuristic
	}{
		{
			name:        "claude native",
			cmd:         "/usr/local/bin/observer hook claude-code pre-tool",
			wantNative:  true,
			wantWindows: false,
		},
		{
			name:        "claude wsl bare",
			cmd:         "wsl.exe -d Ubuntu -- /home/u/observer hook claude-code pre-tool",
			wantNative:  false,
			wantWindows: true,
		},
		{
			name:        "claude wsl with msys prefix",
			cmd:         "MSYS_NO_PATHCONV=1 wsl.exe -d Ubuntu -- /home/u/observer hook claude-code pre-tool",
			wantNative:  false,
			wantWindows: true,
		},
		{
			name:        "claude foreign",
			cmd:         "/usr/local/bin/other-tool --flag value",
			wantNative:  false,
			wantWindows: false,
		},
	}
	for _, c := range cases {
		t.Run("claude:"+c.name, func(t *testing.T) {
			t.Parallel()
			if got := isObserverClaudeEntry(c.cmd); got != c.wantNative {
				t.Errorf("isObserverClaudeEntry(%q) = %v, want %v", c.cmd, got, c.wantNative)
			}
			if got := isObserverWindowsClaudeEntry(c.cmd); got != c.wantWindows {
				t.Errorf("isObserverWindowsClaudeEntry(%q) = %v, want %v", c.cmd, got, c.wantWindows)
			}
		})
	}

	cursorCases := []struct {
		name        string
		cmd         string
		wantNative  bool
		wantWindows bool
	}{
		{
			name:        "cursor native",
			cmd:         "/usr/local/bin/observer hook cursor beforeShellExecution",
			wantNative:  true,
			wantWindows: false,
		},
		{
			name:        "cursor wsl bare",
			cmd:         "wsl.exe -d Ubuntu -- /home/u/observer hook cursor beforeShellExecution",
			wantNative:  false,
			wantWindows: true,
		},
		{
			name:        "cursor wsl with msys prefix",
			cmd:         "MSYS_NO_PATHCONV=1 wsl.exe -d Ubuntu -- /home/u/observer hook cursor beforeShellExecution",
			wantNative:  false,
			wantWindows: true,
		},
	}
	for _, c := range cursorCases {
		t.Run("cursor:"+c.name, func(t *testing.T) {
			t.Parallel()
			if got := isObserverCursorEntry(c.cmd); got != c.wantNative {
				t.Errorf("isObserverCursorEntry(%q) = %v, want %v", c.cmd, got, c.wantNative)
			}
			if got := isObserverWindowsCursorEntry(c.cmd); got != c.wantWindows {
				t.Errorf("isObserverWindowsCursorEntry(%q) = %v, want %v", c.cmd, got, c.wantWindows)
			}
		})
	}
}

// TestRegisterClaudeCodeWindowsFreshInstall pins the wsl.exe-prefixed
// hook command shape against a fake Windows-side .claude/. Every entry
// in claudeCodeEvents must land in <home>/.claude/settings.json with
// the shape `wsl.exe -d <distro> -- <linux-bin> hook claude-code
// <event-arg> [--config <wsl-path>]`. The explicit
// WindowsClaudeHome option exercises the path that doesn't depend on
// /mnt/c presence at test time.
func TestRegisterClaudeCodeWindowsFreshInstall(t *testing.T) {
	t.Parallel()
	winHome := t.TempDir() // stand-in for /mnt/c/Users/<u>
	claudeDir := filepath.Join(winHome, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	wslHome := t.TempDir()
	r, err := NewRegistry(Options{
		BinaryPath:        "/home/marmutapp/superbased-observer/bin/observer",
		HomeDir:           wslHome,
		ChecksumsPath:     filepath.Join(wslHome, ".observer", "hook_checksums.json"),
		WindowsClaudeHome: winHome,
		WSLDistro:         "Ubuntu-20.04",
		ConfigPath:        "/home/marmutapp/.observer/config.toml",
	})
	if err != nil {
		t.Fatal(err)
	}
	res := r.Register("claude-code-windows")
	if res.Error != nil {
		t.Fatalf("Register: %v", res.Error)
	}
	if len(res.HooksAdded) != len(claudeCodeEvents) {
		t.Errorf("HooksAdded count = %d want %d", len(res.HooksAdded), len(claudeCodeEvents))
	}
	if res.ConfigPath != filepath.Join(claudeDir, "settings.json") {
		t.Errorf("ConfigPath = %q want %q", res.ConfigPath, filepath.Join(claudeDir, "settings.json"))
	}

	body, err := os.ReadFile(res.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(body, &settings); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, body)
	}
	hooksBlock, ok := settings["hooks"].(map[string]any)
	if !ok {
		t.Fatal("hooks block missing")
	}
	for _, event := range claudeCodeEvents {
		groups, ok := hooksBlock[event].([]any)
		if !ok || len(groups) == 0 {
			t.Errorf("event %s missing", event)
			continue
		}
		first, _ := groups[0].(map[string]any)
		hooks, _ := first["hooks"].([]any)
		if len(hooks) == 0 {
			t.Errorf("event %s has empty hooks slice", event)
			continue
		}
		hook0, _ := hooks[0].(map[string]any)
		cmd, _ := hook0["command"].(string)
		wantPrefix := "MSYS_NO_PATHCONV=1 wsl.exe -d Ubuntu-20.04 -- /home/marmutapp/superbased-observer/bin/observer hook claude-code " + hookEventArg(event)
		if !strings.HasPrefix(cmd, wantPrefix) {
			t.Errorf("event %s cmd = %q want prefix %q", event, cmd, wantPrefix)
		}
		if !strings.Contains(cmd, "--config '/home/marmutapp/.observer/config.toml'") {
			t.Errorf("event %s cmd missing --config: %q", event, cmd)
		}
	}
}

// TestRegisterClaudeCodeWindowsRequiresDistro pins the "missing distro"
// error path — without one, the wsl.exe wrapper would be ambiguous on
// a host with multiple distros and we'd write a broken settings.json.
func TestRegisterClaudeCodeWindowsRequiresDistro(t *testing.T) {
	winHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(winHome, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	wslHome := t.TempDir()
	t.Setenv("WSL_DISTRO_NAME", "")
	r, err := NewRegistry(Options{
		BinaryPath:        "/x",
		HomeDir:           wslHome,
		WindowsClaudeHome: winHome,
	})
	if err != nil {
		t.Fatal(err)
	}
	res := r.Register("claude-code-windows")
	if res.Error == nil {
		t.Fatal("expected error when distro unset, got nil")
	}
	if !strings.Contains(res.Error.Error(), "WSL distro unknown") {
		t.Errorf("error = %q want it to mention 'WSL distro unknown'", res.Error)
	}
}

// TestRegisterClaudeCodeWindowsIdempotent pins re-register safety: a
// second Register("claude-code-windows") with the same Options must
// surface every event as AlreadySet and NOT duplicate entries in the
// settings.json. Counts post-register groups under each event.
func TestRegisterClaudeCodeWindowsIdempotent(t *testing.T) {
	t.Parallel()
	winHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(winHome, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	wslHome := t.TempDir()
	opts := Options{
		BinaryPath:        "/bin/observer",
		HomeDir:           wslHome,
		ChecksumsPath:     filepath.Join(wslHome, ".observer", "hook_checksums.json"),
		WindowsClaudeHome: winHome,
		WSLDistro:         "u20",
	}
	r1, _ := NewRegistry(opts)
	if res := r1.Register("claude-code-windows"); res.Error != nil {
		t.Fatalf("first: %v", res.Error)
	}
	r2, _ := NewRegistry(opts)
	res := r2.Register("claude-code-windows")
	if res.Error != nil {
		t.Fatalf("second: %v", res.Error)
	}
	if len(res.HooksAdded) != 0 {
		t.Errorf("second Register added %d hooks, want 0 (all should be AlreadySet)", len(res.HooksAdded))
	}
	if len(res.AlreadySet) != len(claudeCodeEvents) {
		t.Errorf("AlreadySet = %d want %d", len(res.AlreadySet), len(claudeCodeEvents))
	}
	body, _ := os.ReadFile(res.ConfigPath)
	var settings map[string]any
	_ = json.Unmarshal(body, &settings)
	hooks, _ := settings["hooks"].(map[string]any)
	for _, event := range claudeCodeEvents {
		groups, _ := hooks[event].([]any)
		if len(groups) != 1 {
			t.Errorf("event %s: got %d groups after idempotent re-register, want 1", event, len(groups))
		}
	}
}

// TestRegisterClaudeCodeWindowsConflictGuard pins the user-hook
// protection: a pre-existing non-observer command on any event causes
// Register to fail loudly (no --force). User's hook is left untouched.
func TestRegisterClaudeCodeWindowsConflictGuard(t *testing.T) {
	t.Parallel()
	winHome := t.TempDir()
	claudeDir := filepath.Join(winHome, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(claudeDir, "settings.json")
	prior := `{"hooks":{"PreToolUse":[{"matcher":"*","hooks":[{"type":"command","command":"C:\\Users\\me\\my-hook.exe"}]}]}}`
	if err := os.WriteFile(settingsPath, []byte(prior), 0o644); err != nil {
		t.Fatal(err)
	}
	wslHome := t.TempDir()
	r, _ := NewRegistry(Options{
		BinaryPath:        "/bin/observer",
		HomeDir:           wslHome,
		WindowsClaudeHome: winHome,
		WSLDistro:         "u20",
	})
	res := r.Register("claude-code-windows")
	if res.Error == nil {
		t.Fatal("expected conflict error, got nil")
	}
	if !strings.Contains(res.Error.Error(), "non-observer hook") {
		t.Errorf("error = %q want it to mention 'non-observer hook'", res.Error)
	}
	// User's hook must still be there — Register should not partial-
	// write before refusing.
	body, _ := os.ReadFile(settingsPath)
	if !strings.Contains(string(body), "my-hook.exe") {
		t.Errorf("user hook lost after conflict-guard failure: %s", body)
	}
}

// TestUnregisterClaudeCodeWindowsRoundTrip pins the install → uninstall
// cycle. After Unregister, observer entries are gone, user-authored
// entries on other events survive, and the checksum guard accepts the
// matched-checksum case.
func TestUnregisterClaudeCodeWindowsRoundTrip(t *testing.T) {
	t.Parallel()
	winHome := t.TempDir()
	claudeDir := filepath.Join(winHome, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(claudeDir, "settings.json")
	// Pre-existing user hook on PreCompact — observer skips it via the
	// matcher and our event-list, so this entry must survive both
	// Register and Unregister.
	prior := `{"hooks":{"FileChanged":[{"matcher":"*","hooks":[{"type":"command","command":"C:\\my-user-hook.exe"}]}]}}`
	if err := os.WriteFile(settingsPath, []byte(prior), 0o644); err != nil {
		t.Fatal(err)
	}
	wslHome := t.TempDir()
	opts := Options{
		BinaryPath:        "/bin/observer",
		HomeDir:           wslHome,
		ChecksumsPath:     filepath.Join(wslHome, ".observer", "hook_checksums.json"),
		WindowsClaudeHome: winHome,
		WSLDistro:         "u20",
	}
	reg, _ := NewRegistry(opts)
	if res := reg.Register("claude-code-windows"); res.Error != nil {
		t.Fatalf("Register: %v", res.Error)
	}

	unreg, _ := NewRegistry(opts)
	ures := unreg.Unregister("claude-code-windows")
	if ures.Error != nil {
		t.Fatalf("Unregister: %v", ures.Error)
	}
	if len(ures.HooksRemoved) != len(claudeCodeEvents) {
		t.Errorf("HooksRemoved = %d want %d", len(ures.HooksRemoved), len(claudeCodeEvents))
	}
	body, _ := os.ReadFile(settingsPath)
	if !strings.Contains(string(body), "my-user-hook.exe") {
		t.Errorf("user hook lost during Unregister: %s", body)
	}
	if strings.Contains(string(body), "hook claude-code") {
		t.Errorf("observer entry survived Unregister: %s", body)
	}
}

// TestInstalledClaudeCodeWindows pins the auto-detect contract:
// Installed() includes "claude-code-windows" once
// WindowsClaudeHome resolves to an existing directory. The explicit
// option path is exercised so the test doesn't depend on /mnt/c.
func TestInstalledClaudeCodeWindows(t *testing.T) {
	t.Parallel()
	winHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(winHome, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, _ := NewRegistry(Options{
		BinaryPath:        "/bin/observer",
		HomeDir:           t.TempDir(),
		WindowsClaudeHome: winHome,
	})
	got := r.Installed()
	found := false
	for _, tool := range got {
		if tool == "claude-code-windows" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Installed()=%v does not include claude-code-windows", got)
	}
}
