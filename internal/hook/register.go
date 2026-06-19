package hook

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
)

// RegistrationResult summarizes a single tool registration.
type RegistrationResult struct {
	Tool       string   // claude-code | cursor | codex
	ConfigPath string   // absolute path to the patched config file
	HooksAdded []string // event names that now point at the observer binary
	AlreadySet []string // events that already pointed at the observer (skipped)
	DryRun     bool
	Error      error
}

// Options parameterize RegisterAll.
type Options struct {
	// BinaryPath is the absolute path to the running observer binary
	// that hook commands will invoke. Required.
	BinaryPath string
	// DryRun, when true, computes the result without touching any files.
	DryRun bool
	// Force, when true, overwrites existing non-observer hook entries for
	// the events we manage. When false, conflicts are reported as errors.
	Force bool
	// HomeDir, when non-empty, overrides the default user home — used by
	// tests to sandbox registration in a temp directory.
	HomeDir string
	// ChecksumsPath overrides ~/.observer/hook_checksums.json. Empty
	// means use the default.
	ChecksumsPath string
	// ConfigPath, when non-empty, is appended to the registered hook
	// command as `--config <path>`. Used to keep the hook handler's view
	// of config (and therefore which DB it writes compaction_events /
	// pidbridge rows into) aligned with whichever proxy the user is
	// running. Without this, the hook handler always reads
	// ~/.observer/config.toml and writes to ~/.observer/observer.db,
	// even when the proxy is running against a different config (e.g.
	// the A/B harness's /tmp/ab-claude/on/observer-config.toml). D23's
	// Injector then queries the proxy's DB and finds nothing because
	// the row landed elsewhere. Surfaced 2026-05-08 dogfood.
	ConfigPath string

	// WSLDistro names the WSL distribution to invoke via wsl.exe when
	// registering cursor hooks against a Windows-side ~/.cursor (the
	// "cursor-windows" tool). Required for that registration target;
	// ignored elsewhere. Empty defaults to $WSL_DISTRO_NAME at
	// registration time when running inside WSL.
	WSLDistro string

	// WindowsCursorHome, when non-empty, overrides the auto-detected
	// Windows-side .cursor directory used by the cursor-windows
	// registration target. Default: the first crossmount-detected
	// Windows home with a .cursor/ subdirectory (`<home>/.cursor`).
	WindowsCursorHome string

	// WindowsClaudeHome, when non-empty, overrides the auto-detected
	// Windows-side .claude directory used by the claude-code-windows
	// registration target. Same shape as WindowsCursorHome: pass the
	// Windows USER home (e.g. /mnt/c/Users/<u>) — the registrar
	// appends `.claude` itself. Default: the first crossmount-detected
	// Windows home with a `.claude/` subdirectory.
	WindowsClaudeHome string
}

// Registry is the per-tool registration dispatcher.
type Registry struct {
	opts Options
}

// NewRegistry returns a registry ready to install hooks.
func NewRegistry(opts Options) (*Registry, error) {
	if opts.BinaryPath == "" {
		return nil, errors.New("hook.NewRegistry: BinaryPath is required")
	}
	if opts.HomeDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("hook.NewRegistry: UserHomeDir: %w", err)
		}
		opts.HomeDir = home
	}
	return &Registry{opts: opts}, nil
}

// Installed reports which supported tools appear to be installed, based on
// the presence of their config directories. "cursor-windows" surfaces
// when crossmount detects a Windows-side .cursor/ directory (the
// observer is running in WSL while Cursor IDE runs on Windows) — it
// registers wsl.exe-launched hooks at that Windows path so the
// Windows-Cursor process can invoke the WSL-side observer binary.
func (r *Registry) Installed() []string {
	var tools []string
	if r.dirExists(filepath.Join(r.opts.HomeDir, ".claude")) {
		tools = append(tools, "claude-code")
	}
	if r.dirExists(filepath.Join(r.opts.HomeDir, ".cursor")) {
		tools = append(tools, "cursor")
	}
	if r.dirExists(filepath.Join(r.opts.HomeDir, ".codex")) {
		tools = append(tools, "codex")
	}
	if r.detectWindowsCursorHome() != "" {
		tools = append(tools, "cursor-windows")
	}
	if r.detectWindowsClaudeHome() != "" {
		tools = append(tools, "claude-code-windows")
	}
	return tools
}

// detectWindowsClaudeHome returns the resolved Windows-side .claude
// directory used by the claude-code-windows registration target, or
// "" if none. Honors Options.WindowsClaudeHome when set; otherwise
// picks the first crossmount-detected OS=windows home that has a
// `.claude/` subdirectory. Mirrors detectWindowsCursorHome.
func (r *Registry) detectWindowsClaudeHome() string {
	if r.opts.WindowsClaudeHome != "" {
		dir := filepath.Join(r.opts.WindowsClaudeHome, ".claude")
		if r.dirExists(dir) {
			return dir
		}
		// Treat the option as authoritative even if the dir doesn't
		// exist yet — the registrar can mkdir on first install.
		return dir
	}
	for _, h := range crossmount.AllHomes() {
		if h.OS != crossmount.OSWindows {
			continue
		}
		dir := filepath.Join(h.Path, ".claude")
		if r.dirExists(dir) {
			return dir
		}
	}
	return ""
}

// detectWindowsCursorHome returns the resolved Windows-side .cursor
// directory used by the cursor-windows registration target, or "" if
// none. Honors Options.WindowsCursorHome when set; otherwise picks the
// first crossmount-detected OS=windows home that has a `.cursor/`
// subdirectory.
func (r *Registry) detectWindowsCursorHome() string {
	if r.opts.WindowsCursorHome != "" {
		dir := filepath.Join(r.opts.WindowsCursorHome, ".cursor")
		if r.dirExists(dir) {
			return dir
		}
		// Treat it as authoritative even when missing — the caller
		// may want to bootstrap a fresh hook config there. The
		// register path will mkdir the parent.
		return dir
	}
	for _, h := range crossmount.AllHomes() {
		if h.OS != crossmount.OSWindows {
			continue
		}
		dir := filepath.Join(h.Path, ".cursor")
		if r.dirExists(dir) {
			return dir
		}
	}
	return ""
}

func (r *Registry) dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// Register installs observer hooks into the config file for tool. Supported
// values: "claude-code", "claude-code-windows", "cursor", "cursor-windows",
// "codex". Unknown tools return an error.
func (r *Registry) Register(tool string) RegistrationResult {
	switch tool {
	case "claude-code":
		return r.registerClaudeCode()
	case "claude-code-windows":
		return r.registerClaudeCodeWindows()
	case "cursor":
		return r.registerCursor()
	case "cursor-windows":
		return r.registerCursorWindows()
	case "codex":
		return r.registerCodex()
	default:
		return RegistrationResult{
			Tool:   tool,
			Error:  fmt.Errorf("hook.Register: tool %q not supported for hook registration", tool),
			DryRun: r.opts.DryRun,
		}
	}
}

// claudeCodeEvents is the set of events we register for. The matcher "*"
// catches every tool; downstream handlers filter by tool_name.
//
// Tier 1 additions (2026-05): SessionEnd, UserPromptSubmit,
// PostToolUseFailure, StopFailure, SubagentStart, SubagentStop,
// Notification, CwdChanged. Each maps to a row in the actions table
// via cmd/observer/hook.go::handleClaudeCodeHook so dashboards can
// surface failures, sub-agent fan-out, lifecycle exit reasons, host
// notifications and cwd changes that the JSONL transcript either
// doesn't carry or carries less directly.
//
// Tier 2/3 additions (2026-05-11): Setup, UserPromptExpansion,
// PostToolBatch, PermissionRequest, PermissionDenied, InstructionsLoaded,
// ConfigChange. Shapes verified against
// docs.claude.com/docs/en/hooks. FileChanged deferred — its matcher
// takes literal filenames (split on `|`), not the wildcard `*` shape
// all other events use, so it needs a separate per-project config
// surface before registration.
var claudeCodeEvents = []string{
	"SessionStart",
	"SessionEnd",
	"Setup",
	"UserPromptSubmit",
	"UserPromptExpansion",
	"PreToolUse",
	"PostToolUse",
	"PostToolUseFailure",
	"PostToolBatch",
	"PermissionRequest",
	"PermissionDenied",
	"Stop",
	"StopFailure",
	"PreCompact",
	"PostCompact",
	"SubagentStart",
	"SubagentStop",
	"Notification",
	"CwdChanged",
	"InstructionsLoaded",
	"ConfigChange",
	// WorktreeRemove is non-blocking (logging only) so it's safe to
	// register by default. WorktreeCreate (its blocking pair) is NOT
	// in this list — see docs/claude-worktree-hook.md for the opt-in
	// procedure. Wiring it as a default hook without a verified
	// path-echo contract risks breaking every Agent spawn with
	// isolation: "worktree".
	"WorktreeRemove",
}

type claudeHookGroup struct {
	Matcher string              `json:"matcher,omitempty"`
	Hooks   []claudeHookCommand `json:"hooks"`
}

type claudeHookCommand struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

func (r *Registry) registerClaudeCode() RegistrationResult {
	res := RegistrationResult{Tool: "claude-code", DryRun: r.opts.DryRun}
	settingsDir := filepath.Join(r.opts.HomeDir, ".claude")
	path := filepath.Join(settingsDir, "settings.json")
	res.ConfigPath = path

	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		res.Error = fmt.Errorf("hook.registerClaudeCode: read: %w", err)
		return res
	}
	// Preserve unknown top-level fields via map[string]json.RawMessage.
	settings := map[string]json.RawMessage{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &settings); err != nil {
			res.Error = fmt.Errorf("hook.registerClaudeCode: parse %s: %w", path, err)
			return res
		}
	}
	var hooks map[string][]claudeHookGroup
	if existing, ok := settings["hooks"]; ok {
		if err := json.Unmarshal(existing, &hooks); err != nil {
			res.Error = fmt.Errorf("hook.registerClaudeCode: parse hooks: %w", err)
			return res
		}
	}
	if hooks == nil {
		hooks = map[string][]claudeHookGroup{}
	}

	for _, event := range claudeCodeEvents {
		// Normalize Windows-shaped paths to forward slashes so the
		// hook command survives any shell wrapping the harness applies.
		// Background: the v1.6.25 fix at this site single-quoted the
		// backslash path (`'D:\programsx\...\observer.exe'`) so Git
		// Bash on Windows wouldn't strip backslashes as escape
		// sequences. That worked when Claude Code spawned the hook
		// directly. But the harness's per-tool-call Bash wrapper can
		// strip the single quotes on intermittent invocation patterns
		// (operator-reported 2026-06-06; reliably reproduces on
		// `wsl.exe`-shaped Bash-tool calls), leaving the unquoted
		// backslash path for bash to escape-strip — symptom is the
		// canonical `D:programsxsuperbased-observerbinobserver-hermes.exe:
		// command not found` 127 exit. Forward-slash normalization is
		// a stronger guarantee than single-quoting: `D:/programsx/...`
		// has no character any shell layer interprets specially, so
		// the path arrives at the exec syscall unmodified regardless
		// of how many wrappers stripped or re-quoted it. Same effect
		// for the --config path (see configFlagSuffixForwardSlash).
		// shellQuoteIfNeeded is still called for the safety of paths
		// with spaces (`C:\Program Files\...`); forward-slash variants
		// of those still need quoting, just with whatever quote style
		// survives without escape interpretation.
		binPath := forwardSlashPath(r.opts.BinaryPath)
		cmd := shellQuoteIfNeeded(binPath) + " hook claude-code " + hookEventArg(event) + r.configFlagSuffixForwardSlash()
		groups := hooks[event]
		idx := findClaudeGroupWithObserver(groups)
		if idx >= 0 {
			if observerCmdMatches(groups[idx], cmd) {
				res.AlreadySet = append(res.AlreadySet, event)
				continue
			}
			// Args drifted but the entry is already ours (recognised
			// by content-heuristic — see isObserverClaudeEntry).
			// Silently refresh; covers same-binary arg drift AND
			// cross-binary upgrade (e.g. an npm-bundled observer in
			// node_modules being replaced by a fresh local build).
			// Drop the stale group and fall through to append the
			// up-to-date one.
			groups = append(groups[:idx], groups[idx+1:]...)
		}
		// Conflict check: a non-observer hook command on "*" matcher
		// counts as an unmanaged entry.
		if !r.opts.Force && hasConflictingClaudeHook(groups) {
			res.Error = fmt.Errorf("hook.registerClaudeCode: event %s already has a non-observer hook; pass --force to overwrite", event)
			return res
		}
		groups = append(groups, claudeHookGroup{
			Matcher: "*",
			Hooks:   []claudeHookCommand{{Type: "command", Command: cmd}},
		})
		hooks[event] = groups
		res.HooksAdded = append(res.HooksAdded, event)
	}

	patched, err := json.Marshal(hooks)
	if err != nil {
		res.Error = fmt.Errorf("hook.registerClaudeCode: marshal hooks: %w", err)
		return res
	}
	settings["hooks"] = patched

	if r.opts.DryRun {
		return res
	}
	if err := writeJSONIndented(settingsDir, path, settings); err != nil {
		res.Error = err
		return res
	}
	if err := r.recordChecksum(path); err != nil {
		res.Error = err
		return res
	}
	return res
}

// findClaudeGroupWithObserver returns the index of a group whose
// single hook command is recognised as observer-written by
// isObserverClaudeEntry, or -1. Detection is content-based (matches
// the ` hook claude-code ` token sequence) rather than binary-path-
// prefix, so an entry left behind by a differently-installed
// observer (e.g. an npm-bundled binary in node_modules, a Linux
// build under a renamed home dir) is still recognised as ours and
// silently refreshed on the next register pass. The Windows
// registrar's findClaudeGroupWithObserverWindows has the same shape;
// this is its Linux/default counterpart added in the v1.6.25
// drift-refresh fix.
func findClaudeGroupWithObserver(groups []claudeHookGroup) int {
	for i, g := range groups {
		for _, h := range g.Hooks {
			if h.Type == "command" && isObserverClaudeEntry(h.Command) {
				return i
			}
		}
	}
	return -1
}

// registerClaudeCodeWindows installs Claude Code hooks into a Windows-
// side `.claude/settings.json` (typically
// `/mnt/c/Users/<u>/.claude/settings.json`) with each command wrapped
// in
//
//	MSYS_NO_PATHCONV=1 wsl.exe -d <distro> -- <linux-bin> hook
//	claude-code <event> [--config <wsl-path>]
//
// so the Windows Claude Desktop process can fire it from Git Bash (the
// shell Claude Code uses on Windows per code.claude.com/docs/en/hooks:
// "Git Bash on Windows"). The MSYS_NO_PATHCONV=1 prefix is load-
// bearing — without it, Git Bash's MSYS layer auto-translates the
// Linux `/home/...` path arguments into `C:/Program Files/Git/home/...`
// before they reach wsl.exe, the inner binary can't be found, and
// every hook fires exit-127. Symptom in the JSONL:
//
//	{"type":"hook_non_blocking_error","exitCode":127,
//	 "stderr":"/bin/bash: C:/Program Files/Git/home/.../observer:
//	 No such file or directory"}
//
// confirmed on Claude Desktop v2.1.138 (2026-05-20).
//
// MSYS_NO_PATHCONV is bash-only (silently ignored by macOS/Linux `sh -c`
// and by cmd.exe), so the prefix is safe to set unconditionally on
// the Windows registrar without runtime branching.
//
// Distro lookup: Options.WSLDistro → $WSL_DISTRO_NAME. Empty distro
// is an error; the command would be ambiguous on a host with multiple
// distros. Same contract as registerCursorWindows.
func (r *Registry) registerClaudeCodeWindows() RegistrationResult {
	res := RegistrationResult{Tool: "claude-code-windows", DryRun: r.opts.DryRun}

	claudeDir := r.detectWindowsClaudeHome()
	if claudeDir == "" {
		res.Error = errors.New("hook.registerClaudeCodeWindows: no Windows-side .claude/ detected (set WindowsClaudeHome explicitly or run on a host where crossmount sees /mnt/c/Users/<u>/.claude/)")
		return res
	}
	res.ConfigPath = filepath.Join(claudeDir, "settings.json")

	distro := r.opts.WSLDistro
	if distro == "" {
		distro = os.Getenv("WSL_DISTRO_NAME")
	}
	if distro == "" {
		res.Error = errors.New("hook.registerClaudeCodeWindows: WSL distro unknown — set Options.WSLDistro or run inside WSL (so $WSL_DISTRO_NAME is set)")
		return res
	}
	// MSYS_NO_PATHCONV=1 inline-env prefix — see registerClaudeCodeWindows
	// docstring for the Git Bash path-translation rationale.
	wrapperPrefix := fmt.Sprintf("MSYS_NO_PATHCONV=1 wsl.exe -d %s -- ", shellQuoteIfNeeded(distro))

	raw, err := os.ReadFile(res.ConfigPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		res.Error = fmt.Errorf("hook.registerClaudeCodeWindows: read: %w", err)
		return res
	}
	settings := map[string]json.RawMessage{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &settings); err != nil {
			res.Error = fmt.Errorf("hook.registerClaudeCodeWindows: parse %s: %w", res.ConfigPath, err)
			return res
		}
	}
	var hooks map[string][]claudeHookGroup
	if existing, ok := settings["hooks"]; ok {
		if err := json.Unmarshal(existing, &hooks); err != nil {
			res.Error = fmt.Errorf("hook.registerClaudeCodeWindows: parse hooks: %w", err)
			return res
		}
	}
	if hooks == nil {
		hooks = map[string][]claudeHookGroup{}
	}

	for _, event := range claudeCodeEvents {
		cmd := wrapperPrefix + r.opts.BinaryPath + " hook claude-code " + hookEventArg(event) + r.configFlagSuffix()
		groups := hooks[event]
		idx := findClaudeGroupWithObserverWindows(groups)
		if idx >= 0 {
			if observerCmdMatches(groups[idx], cmd) {
				res.AlreadySet = append(res.AlreadySet, event)
				continue
			}
			// Refresh-on-drift: binary path or config flag changed but
			// the wsl-wrapped shape is still ours. Drop the stale group
			// and fall through to append the current one.
			groups = append(groups[:idx], groups[idx+1:]...)
		}
		if !r.opts.Force && hasConflictingClaudeHookWindows(groups) {
			res.Error = fmt.Errorf("hook.registerClaudeCodeWindows: event %s already has a non-observer hook; pass --force to overwrite", event)
			return res
		}
		groups = append(groups, claudeHookGroup{
			Matcher: "*",
			Hooks:   []claudeHookCommand{{Type: "command", Command: cmd}},
		})
		hooks[event] = groups
		res.HooksAdded = append(res.HooksAdded, event)
	}

	patched, err := json.Marshal(hooks)
	if err != nil {
		res.Error = fmt.Errorf("hook.registerClaudeCodeWindows: marshal hooks: %w", err)
		return res
	}
	settings["hooks"] = patched

	if r.opts.DryRun {
		return res
	}
	if err := writeJSONIndented(claudeDir, res.ConfigPath, settings); err != nil {
		res.Error = err
		return res
	}
	if err := r.recordChecksum(res.ConfigPath); err != nil {
		res.Error = err
		return res
	}
	return res
}

// isObserverClaudeEntry recognises a hook command as one previously
// written by ANY observer claude-code (Linux/default) registrar.
// The ` hook claude-code ` token sequence is the stable signature,
// regardless of which observer binary path prefixes it.
// Content-heuristic rather than binary-path-prefix so a hook left
// behind by a differently-installed observer (e.g. an npm-bundled
// binary in node_modules, an upgrade that moved the install root,
// a Linux build with a renamed `$HOME`) is still recognised as ours
// and silently refreshed on the next `observer start` auto-register
// pass. Without this, the conflict guard would mis-classify those
// stale-but-ours entries as foreign third-party hooks and refuse to
// touch them without `--force`, leaving Claude Code firing the OLD
// observer binary forever — exactly the bug class that caused the
// v1.6.22 claude-code effort sidecar to silently stay empty for
// users who had a node_modules-bundled observer registered before
// upgrading to a local build.
//
// The Windows registrar's isObserverWindowsClaudeEntry is the same
// idea with an additional wsl.exe-wrapper guard; this is its
// Linux/default counterpart. To keep the two heuristics matching
// disjoint shapes (so the Linux/default registrar doesn't rewrite
// wsl-bridge entries into native-shape on hosts that have both
// configurations registered against the same settings.json), this
// helper EXCLUDES wsl.exe-wrapped commands. Any cmd that
// isObserverWindowsClaudeEntry would accept is rejected here.
//
// Trade-off: a third-party command containing the literal
// ` hook claude-code ` substring would be misclassified as observer
// and silently rewritten. The risk is acceptable — the syntax is
// distinctive enough that an accidental collision is essentially
// impossible, and the same trade-off the Windows registrar already
// accepts in production since v1.6.22.
func isObserverClaudeEntry(cmd string) bool {
	if !strings.Contains(cmd, " hook claude-code ") {
		return false
	}
	if strings.HasPrefix(cmd, "wsl.exe ") || strings.HasPrefix(cmd, "MSYS_NO_PATHCONV=1 wsl.exe ") {
		return false
	}
	return true
}

// isObserverWindowsClaudeEntry recognises a hook command as one
// previously written by this registrar: a `wsl.exe ` invocation (with
// or without a leading MSYS env-var prefix) that ultimately calls
// `<bin> hook claude-code <event>`. The MSYS_NO_PATHCONV=1 prefix
// shipped in v1.6.22+; we match either shape so refresh-on-drift
// picks up the older prefix-free entries and rewrites them with the
// fixed wrapper. The `hook claude-code` token is the stable
// signature; anything else is treated as a user-authored hook.
func isObserverWindowsClaudeEntry(cmd string) bool {
	if !strings.Contains(cmd, " hook claude-code ") {
		return false
	}
	if strings.HasPrefix(cmd, "wsl.exe ") {
		return true
	}
	if strings.HasPrefix(cmd, "MSYS_NO_PATHCONV=1 wsl.exe ") {
		return true
	}
	return false
}

// findClaudeGroupWithObserverWindows returns the index of a group
// whose single hook command is a wsl-wrapped observer claude-code
// invocation, or -1.
func findClaudeGroupWithObserverWindows(groups []claudeHookGroup) int {
	for i, g := range groups {
		for _, h := range g.Hooks {
			if h.Type == "command" && isObserverWindowsClaudeEntry(h.Command) {
				return i
			}
		}
	}
	return -1
}

// hasConflictingClaudeHookWindows reports whether any group contains a
// non-observer command. Used by the Windows registrar's --force-less
// path to refuse silent overwrite of user-authored hooks.
func hasConflictingClaudeHookWindows(groups []claudeHookGroup) bool {
	for _, g := range groups {
		for _, h := range g.Hooks {
			if h.Type != "command" {
				continue
			}
			if !isObserverWindowsClaudeEntry(h.Command) {
				return true
			}
		}
	}
	return false
}

// hasConflictingClaudeHook reports whether any group on the "*"
// matcher carries a command that isn't observer-shaped. Used by the
// force-less path to refuse silent overwrite of user-authored hooks.
// Mirror of hasConflictingClaudeHookWindows; content-heuristic so an
// observer entry from a different install path (npm, cross-binary
// upgrade) is recognised as ours and falls through to the
// refresh-by-overwrite path rather than tripping the guard.
func hasConflictingClaudeHook(groups []claudeHookGroup) bool {
	for _, g := range groups {
		if g.Matcher != "" && g.Matcher != "*" {
			continue
		}
		for _, h := range g.Hooks {
			if h.Type != "command" {
				continue
			}
			if !isObserverClaudeEntry(h.Command) {
				return true
			}
		}
	}
	return false
}

// cursorEvents is the set of Cursor hook events we register for. The first
// 5 are the original Tier 1 set (shell, MCP, file edits, prompt submit,
// stop). Tier 2 (v1.4.45) extends coverage to file reads (closing audit
// C2), tool failures, session-lifecycle markers, sub-agent fan-out, and
// pre-compact dispatch. Tier 3 (v1.4.45) adds the universal preToolUse
// to fill the long-tail-tool gap (Glob/Grep/Search/Write/etc. — tools
// the per-tool before* hooks miss) plus three paired-after observers
// registered no-row pending update-in-place: postToolUse,
// afterShellExecution, afterMCPExecution. Tier 4 (v1.6.18) adds
// afterAgentThought + afterAgentResponse: Cursor 3.4+ stopped writing
// agent-transcripts/*.jsonl files (the JSONL walker BuildStopTranscriptEvents
// relied on as a fallback for finalized assistant prose is now
// dead-code), and live-captured payloads confirmed the events fire
// once-per-finalized-block (not per-token-delta as the v1.4.45
// docstring claimed). See internal/adapter/cursor/adapter.go for the
// full rationale. Tab events (beforeTabFileRead, afterTabFileEdit)
// remain out of scope.
var cursorEvents = []string{
	"beforeSubmitPrompt", "beforeShellExecution", "afterFileEdit", "beforeMCPExecution", "stop",
	"beforeReadFile", "postToolUseFailure", "sessionStart", "sessionEnd",
	"subagentStart", "subagentStop", "preCompact",
	"preToolUse", "postToolUse", "afterShellExecution", "afterMCPExecution",
	"afterAgentThought", "afterAgentResponse",
}

type cursorHookEntry struct {
	Command string `json:"command"`
}

func (r *Registry) registerCursor() RegistrationResult {
	res := RegistrationResult{Tool: "cursor", DryRun: r.opts.DryRun}
	cursorDir := filepath.Join(r.opts.HomeDir, ".cursor")
	path := filepath.Join(cursorDir, "hooks.json")
	res.ConfigPath = path

	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		res.Error = fmt.Errorf("hook.registerCursor: read: %w", err)
		return res
	}
	settings := map[string]json.RawMessage{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &settings); err != nil {
			res.Error = fmt.Errorf("hook.registerCursor: parse: %w", err)
			return res
		}
	}
	hooks := map[string][]cursorHookEntry{}
	if existing, ok := settings["hooks"]; ok {
		_ = json.Unmarshal(existing, &hooks)
	}

	for _, event := range cursorEvents {
		// Forward-slash-normalize the binary + config path before
		// quoting — see registerClaudeCode for the full rationale
		// (the harness can strip the single quotes upstream, leaving
		// unquoted-Windows-path backslashes for Git Bash to escape-
		// strip). Forward slashes survive any shell wrapping.
		binPath := forwardSlashPath(r.opts.BinaryPath)
		cmd := shellQuoteIfNeeded(binPath) + " hook cursor " + event + r.configFlagSuffixForwardSlash()
		if slicesContainsCommand(hooks[event], cmd) {
			res.AlreadySet = append(res.AlreadySet, event)
			continue
		}
		// Stale-observer-args case: existing command is recognised as
		// observer-written (by content-heuristic — see
		// isObserverCursorEntry) but has different args. Covers
		// same-binary arg drift (e.g. missing --config after we add a
		// new flag) AND cross-binary upgrade (e.g. an npm-bundled
		// observer in node_modules being replaced by a fresh local
		// build). Silently refresh; non-observer entries are still
		// treated as conflicts via hasCursorConflict below.
		if hasStaleObserverEntry(hooks[event], cmd) {
			hooks[event] = filterStaleObserverEntries(hooks[event], cmd)
		}
		if !r.opts.Force && hasCursorConflict(hooks[event]) {
			res.Error = fmt.Errorf("hook.registerCursor: event %s already has a non-observer hook; pass --force to overwrite", event)
			return res
		}
		hooks[event] = append(hooks[event], cursorHookEntry{Command: cmd})
		res.HooksAdded = append(res.HooksAdded, event)
	}

	settings["version"] = json.RawMessage("1")
	hookJSON, err := json.Marshal(hooks)
	if err != nil {
		res.Error = fmt.Errorf("hook.registerCursor: marshal hooks: %w", err)
		return res
	}
	settings["hooks"] = hookJSON

	if r.opts.DryRun {
		return res
	}
	if err := writeJSONIndented(cursorDir, path, settings); err != nil {
		res.Error = err
		return res
	}
	if err := r.recordChecksum(path); err != nil {
		res.Error = err
		return res
	}
	return res
}

// registerCursorWindows installs Cursor hooks into a Windows-side
// .cursor/hooks.json (typically `/mnt/c/Users/<u>/.cursor/hooks.json`)
// with each command wrapped in `wsl.exe -d <distro> -- <linux-bin>
// hook cursor <event> [--config <wsl-path>]`. The Windows-Cursor
// process spawns wsl.exe; wsl.exe routes stdin/stdout to the WSL-side
// observer binary; the binary processes the hook payload exactly as
// it would on a native Linux install. Uses double-dash (`--`)
// separator so wsl.exe stops parsing its own flags before the linux
// binary path; this keeps any future wsl.exe flags additions from
// silently consuming an arg meant for observer.
//
// The WSL distro name comes from Options.WSLDistro, falling back to
// $WSL_DISTRO_NAME at registration time. Empty distro is an error —
// without it, the registered command would be ambiguous on a host
// with multiple WSL distros.
func (r *Registry) registerCursorWindows() RegistrationResult {
	res := RegistrationResult{Tool: "cursor-windows", DryRun: r.opts.DryRun}

	cursorDir := r.detectWindowsCursorHome()
	if cursorDir == "" {
		res.Error = errors.New("hook.registerCursorWindows: no Windows-side .cursor/ detected (set WindowsCursorHome explicitly or run on a host where crossmount sees /mnt/c/Users/<u>/.cursor/)")
		return res
	}
	res.ConfigPath = filepath.Join(cursorDir, "hooks.json")

	distro := r.opts.WSLDistro
	if distro == "" {
		distro = os.Getenv("WSL_DISTRO_NAME")
	}
	if distro == "" {
		res.Error = errors.New("hook.registerCursorWindows: WSL distro unknown — set Options.WSLDistro or run inside WSL (so $WSL_DISTRO_NAME is set)")
		return res
	}

	raw, err := os.ReadFile(res.ConfigPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		res.Error = fmt.Errorf("hook.registerCursorWindows: read: %w", err)
		return res
	}
	settings := map[string]json.RawMessage{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &settings); err != nil {
			res.Error = fmt.Errorf("hook.registerCursorWindows: parse: %w", err)
			return res
		}
	}
	hooks := map[string][]cursorHookEntry{}
	if existing, ok := settings["hooks"]; ok {
		_ = json.Unmarshal(existing, &hooks)
	}

	// MSYS_NO_PATHCONV=1 inline-env prefix prevents Git Bash on Windows
	// from auto-translating the /home/... arg into C:/Program Files/Git/
	// home/... before wsl.exe receives it. Without it, every hook fire
	// exits 127 with the JSONL attachment stderr
	// "/bin/bash: C:/Program Files/Git/home/.../observer: No such file
	// or directory". Same gotcha as registerClaudeCodeWindows — see
	// that function's docstring for the full diagnostic.
	wrapperPrefix := fmt.Sprintf("MSYS_NO_PATHCONV=1 wsl.exe -d %s -- ", shellQuoteIfNeeded(distro))
	for _, event := range cursorEvents {
		cmd := wrapperPrefix + r.opts.BinaryPath + " hook cursor " + event + r.configFlagSuffix()
		if slicesContainsCommand(hooks[event], cmd) {
			res.AlreadySet = append(res.AlreadySet, event)
			continue
		}
		// Stale-observer-entry case: any command starting with `wsl.exe`
		// that contains ` hook cursor ` was clearly written by a prior
		// observer install — we own it regardless of which observer
		// binary path it points at. This matters across upgrades,
		// distro changes, or smoke-test artifacts: the binary path
		// changes but the entry is still ours and should be refreshed,
		// not treated as a foreign conflict.
		hooks[event] = filterStaleObserverWindowsCursorEntries(hooks[event], cmd)
		if !r.opts.Force && hasNonObserverWindowsCursorEntry(hooks[event]) {
			res.Error = fmt.Errorf("hook.registerCursorWindows: event %s already has a non-observer hook; pass --force to overwrite", event)
			return res
		}
		hooks[event] = append(hooks[event], cursorHookEntry{Command: cmd})
		res.HooksAdded = append(res.HooksAdded, event)
	}

	settings["version"] = json.RawMessage("1")
	hookJSON, err := json.Marshal(hooks)
	if err != nil {
		res.Error = fmt.Errorf("hook.registerCursorWindows: marshal hooks: %w", err)
		return res
	}
	settings["hooks"] = hookJSON

	if r.opts.DryRun {
		return res
	}
	if err := writeJSONIndented(cursorDir, res.ConfigPath, settings); err != nil {
		res.Error = err
		return res
	}
	if err := r.recordChecksum(res.ConfigPath); err != nil {
		res.Error = err
		return res
	}
	return res
}

// isObserverWindowsCursorEntry recognises an entry as one we
// previously wrote: a `wsl.exe ...` invocation (with or without a
// leading MSYS env-var prefix) that ultimately calls `<bin> hook
// cursor <event> ...`. The MSYS_NO_PATHCONV=1 prefix shipped in
// v1.6.22+ — matching both shapes lets refresh-on-drift upgrade
// older prefix-free entries to the fixed wrapper. The `hook cursor`
// token is the stable signature; anything else is foreign and
// treated as a user-authored conflict.
func isObserverWindowsCursorEntry(cmd string) bool {
	if !strings.Contains(cmd, " hook cursor ") {
		return false
	}
	if strings.HasPrefix(cmd, "wsl.exe ") {
		return true
	}
	if strings.HasPrefix(cmd, "MSYS_NO_PATHCONV=1 wsl.exe ") {
		return true
	}
	return false
}

// filterStaleObserverWindowsCursorEntries drops any
// observer-recognised stale entry that doesn't match `want`. Used
// by the cursor-windows registrar to clear prior registrations
// before appending the canonical command. Non-observer entries pass
// through untouched so the conflict check below can flag them.
func filterStaleObserverWindowsCursorEntries(entries []cursorHookEntry, want string) []cursorHookEntry {
	out := make([]cursorHookEntry, 0, len(entries))
	for _, e := range entries {
		if isObserverWindowsCursorEntry(e.Command) && e.Command != want {
			continue
		}
		out = append(out, e)
	}
	return out
}

// hasNonObserverWindowsCursorEntry reports whether the slice has any
// entry we don't recognise as observer-authored. Anything not
// matching the wsl.exe + `hook cursor` shape counts as foreign.
func hasNonObserverWindowsCursorEntry(entries []cursorHookEntry) bool {
	for _, e := range entries {
		if !isObserverWindowsCursorEntry(e.Command) {
			return true
		}
	}
	return false
}

// shellQuoteIfNeeded wraps s in single quotes when it contains
// shell-meaningful characters, otherwise returns it verbatim. Used
// for the WSL distro name in the registered hook command — distro
// names like "Ubuntu-20.04" are bare-safe; weirder names (with
// spaces or quotes) would need escaping.
//
// POSIX-strict — single-quote disables ALL shell interpretation
// (no `$VAR` expansion, no backtick substitution). Correct for
// Claude Code (always Git Bash on Windows; POSIX everywhere else)
// and the WSL-bridge registrars. WRONG for cmd.exe — cmd interprets
// `'...'` literally as part of the argument, which is why Codex on
// Windows needs the codex-specific quoter below.
func shellQuoteIfNeeded(s string) string {
	if s == "" {
		return "''"
	}
	for _, r := range s {
		if r == ' ' || r == '\'' || r == '"' || r == '`' || r == '\\' || r == '$' {
			return shellQuote(s)
		}
	}
	if strings.ContainsAny(s, "*?[](){};&|<>") {
		return shellQuote(s)
	}
	return s
}

// isWindowsPath returns true if s looks Windows-shaped (contains a
// backslash separator). Used by the codex registrar to pick a
// cmd.exe-safe quoter on Windows paths and the POSIX single-quote
// quoter on Linux/macOS paths. Path-shape rather than runtime.GOOS
// so cross-platform tests can exercise both shapes without OS
// mocking — and so a Linux host writing hooks for a Windows-side
// codex (via shared mounts, hypothetical) would also pick up the
// right quoting.
func isWindowsPath(s string) bool {
	return strings.Contains(s, `\`)
}

// forwardSlashPath converts a Windows-shaped path (backslash
// separators) into the forward-slash equivalent, leaving non-
// Windows-shaped paths untouched. Used by registerClaudeCode +
// registerCursor — both write hook commands that Claude Code on
// Windows invokes through Git Bash (per code.claude.com/docs/en/hooks:
// "Git Bash on Windows") OR through whatever inner shell the harness
// uses to wrap a single Bash-tool call (which may strip the
// single-quote wrapping the registrar applies). With backslash paths,
// every layer that loses the single quotes turns the unquoted
// backslashes into shell escape sequences and strips them —
// `D:\programsx\...` collapses to `D:programsx...` and bash exits 127
// "command not found". Forward-slash paths survive every shell
// wrapping intact: Git Bash, PowerShell, and cmd.exe all accept
// `D:/programsx/...` as a Windows file path, and forward slashes are
// never escape characters in any of those shells, so the path is
// untouched even when an upstream wrapper strips the registrar's
// quoting. This is the v1.8.2+ evolution of the v1.6.25 single-quote
// fix (see TestRegisterClaudeCodeQuotesWindowsBinaryPath for the
// original symptom); the single-quote fix worked when Claude Code
// ran the hook command directly in Git Bash but the harness's
// per-tool-call Bash wrapper would still strip the outer single
// quotes on intermittent invocation patterns (operator-reported
// 2026-06-06 across `wsl.exe`-style Bash-tool calls). Forward-slash
// normalization is the bulletproof fix because it removes the only
// character that any shell layer interprets specially.
func forwardSlashPath(s string) string {
	if !isWindowsPath(s) {
		return s
	}
	return strings.ReplaceAll(s, `\`, `/`)
}

// cmdQuoteIfNeeded wraps s in DOUBLE quotes when it contains
// cmd.exe-meaningful characters, otherwise returns it verbatim.
// Used by the codex registrar for Windows-shaped paths because
// Codex 0.133+ on Windows spawns hooks through cmd.exe, which only
// understands `"..."` quoting — `'...'` is interpreted literally
// as part of the argument (operator-reported 2026-05-23: codex
// hook fires exit 1 with `'C:\\...\\observer.exe'` because cmd
// can't find a binary whose name starts with a literal `'`).
//
// cmd.exe doesn't have backslash-as-escape inside `"..."`, so
// Windows paths round-trip cleanly without further escaping.
// Windows file names disallow `"` (NTFS-enforced), so we don't
// need to escape embedded quotes either.
//
// Trade-off vs shellQuoteIfNeeded: double-quote allows `$VAR`
// expansion in POSIX shells. Not a concern here — codex's
// codexCmdQuoteIfNeeded only routes to this when the path is
// Windows-shaped (the path will be evaluated by cmd.exe, not by
// /bin/sh).
func cmdQuoteIfNeeded(s string) string {
	if s == "" {
		return `""`
	}
	for _, r := range s {
		if r == ' ' || r == '"' || r == '&' || r == '<' || r == '>' || r == '|' || r == '^' || r == '(' || r == ')' || r == '%' {
			return `"` + s + `"`
		}
	}
	return s
}

// codexCmdQuoteIfNeeded picks between POSIX single-quote and
// cmd.exe double-quote based on the path shape. Used only by the
// codex registrar — claudecode + cursor stay on shellQuoteIfNeeded
// because Claude Code on Windows always uses Git Bash (POSIX) and
// cursor's hook execution shell on Windows-native is unverified
// (operator can re-target it if a regression appears there too).
//
// Path-shape detection rather than runtime.GOOS so cross-platform
// tests can pin both shapes without OS mocking.
func codexCmdQuoteIfNeeded(s string) string {
	if isWindowsPath(s) {
		return cmdQuoteIfNeeded(s)
	}
	return shellQuoteIfNeeded(s)
}

// hasCursorConflict reports whether any entry carries a command
// that isn't recognised as observer-shaped. Used by the force-less
// path to refuse silent overwrite of user-authored hooks.
// Content-heuristic via isObserverCursorEntry so entries from a
// different observer install path (npm bundle, cross-binary upgrade)
// fall through to the refresh path rather than being flagged as
// foreign.
func hasCursorConflict(entries []cursorHookEntry) bool {
	for _, e := range entries {
		if !isObserverCursorEntry(e.Command) {
			return true
		}
	}
	return false
}

func hookEventArg(event string) string {
	// Claude Code event names are CamelCase; we use lower-kebab on the CLI.
	switch event {
	case "SessionStart":
		return "session-start"
	case "SessionEnd":
		return "session-end"
	case "UserPromptSubmit":
		return "user-prompt-submit"
	case "PreToolUse":
		return "pre-tool"
	case "PostToolUse":
		return "post-tool"
	case "PostToolUseFailure":
		return "post-tool-failure"
	case "Stop":
		return "stop"
	case "StopFailure":
		return "stop-failure"
	case "PreCompact":
		return "pre-compact"
	case "PostCompact":
		return "post-compact"
	case "SubagentStart":
		return "subagent-start"
	case "SubagentStop":
		return "subagent-stop"
	case "Notification":
		return "notification"
	case "CwdChanged":
		return "cwd-changed"
	case "Setup":
		return "setup"
	case "UserPromptExpansion":
		return "user-prompt-expansion"
	case "PostToolBatch":
		return "post-tool-batch"
	case "PermissionRequest":
		return "permission-request"
	case "PermissionDenied":
		return "permission-denied"
	case "InstructionsLoaded":
		return "instructions-loaded"
	case "ConfigChange":
		return "config-change"
	case "WorktreeCreate":
		return "worktree-create"
	case "WorktreeRemove":
		return "worktree-remove"
	}
	return event
}

// writeJSONIndented writes a map[string]json.RawMessage as stable-keyed,
// 2-space-indented JSON. Creates the parent dir if missing.
func writeJSONIndented(dir, path string, settings map[string]json.RawMessage) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("hook.write: mkdir: %w", err)
	}
	keys := make([]string, 0, len(settings))
	for k := range settings {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Manually emit with sorted keys so JSON diffs stay clean.
	var buf []byte
	buf = append(buf, '{', '\n')
	for i, k := range keys {
		buf = append(buf, ' ', ' ')
		kk, _ := json.Marshal(k)
		buf = append(buf, kk...)
		buf = append(buf, ':', ' ')
		// Re-indent the value for readability.
		var tmp any
		if err := json.Unmarshal(settings[k], &tmp); err == nil {
			pretty, _ := json.MarshalIndent(tmp, "  ", "  ")
			buf = append(buf, pretty...)
		} else {
			buf = append(buf, settings[k]...)
		}
		if i < len(keys)-1 {
			buf = append(buf, ',')
		}
		buf = append(buf, '\n')
	}
	buf = append(buf, '}', '\n')

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return fmt.Errorf("hook.write: %w", err)
	}
	return os.Rename(tmp, path)
}

// recordChecksum computes SHA256 of the config file and records it in the
// checksums registry so `observer doctor` can detect drift.
func (r *Registry) recordChecksum(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("hook.recordChecksum: %w", err)
	}
	sum := sha256.Sum256(data)
	entry := map[string]any{
		"sha256":      hex.EncodeToString(sum[:]),
		"registered":  time.Now().UTC().Format(time.RFC3339),
		"binary_path": r.opts.BinaryPath,
	}

	csPath := r.opts.ChecksumsPath
	if csPath == "" {
		csPath = filepath.Join(r.opts.HomeDir, ".observer", "hook_checksums.json")
	}

	current := map[string]any{}
	if raw, err := os.ReadFile(csPath); err == nil {
		_ = json.Unmarshal(raw, &current)
	}
	current[path] = entry
	body, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return fmt.Errorf("hook.recordChecksum: marshal: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(csPath), 0o755); err != nil {
		return fmt.Errorf("hook.recordChecksum: mkdir: %w", err)
	}
	if err := os.WriteFile(csPath, body, 0o600); err != nil {
		return fmt.Errorf("hook.recordChecksum: write: %w", err)
	}
	return nil
}

// configFlagSuffix returns ` --config <path>` when ConfigPath is set,
// or empty string otherwise. The leading space lets callers concatenate
// directly onto the binary+event command. Path is single-quote shell-
// escaped (POSIX-style: every `'` becomes `'\”`) so paths containing
// spaces or quotes round-trip safely through `/bin/bash -c`. Used by
// the claudecode + cursor registrars (Git Bash / POSIX targets).
func (r *Registry) configFlagSuffix() string {
	return r.configFlagSuffixWith(shellQuote)
}

// configFlagSuffixForwardSlash mirrors configFlagSuffix but normalizes
// the config path to forward slashes before quoting. Used by
// registerClaudeCode + registerCursor — see forwardSlashPath for the
// shell-wrapper rationale. Non-Windows-shaped paths pass through
// unchanged so Linux/macOS hook commands and their test fixtures are
// unaffected.
func (r *Registry) configFlagSuffixForwardSlash() string {
	if r.opts.ConfigPath == "" {
		return ""
	}
	return " --config " + shellQuote(forwardSlashPath(r.opts.ConfigPath))
}

// configFlagSuffixWith mirrors configFlagSuffix but lets the caller
// pick its own quoter. Used by registerCodex to apply cmd.exe-safe
// double-quote on Windows-shaped paths — `'...'` is invalid quoting
// in cmd.exe (Codex on Windows spawns hooks via cmd.exe, not Git
// Bash), so a single-quoted --config path would surface as a literal
// argument and confuse codex's flag parser.
func (r *Registry) configFlagSuffixWith(quote func(string) string) string {
	if r.opts.ConfigPath == "" {
		return ""
	}
	return " --config " + quote(r.opts.ConfigPath)
}

// shellQuote returns a single-quoted POSIX-shell literal of s with any
// embedded single-quote escaped via the standard `'\”` sequence.
// Conservative — wraps unconditionally so even sane paths get quotes,
// at the cost of two extra bytes. Callers feed the result into a
// command string that goes through bash -c, where single-quotes turn
// off all interpretation.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	var b []byte
	b = append(b, '\'')
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			b = append(b, '\'', '\\', '\'', '\'')
			continue
		}
		b = append(b, s[i])
	}
	b = append(b, '\'')
	return string(b)
}

// observerCmdMatches reports whether group has exactly one hook command
// equal to want. Anything else (different command, multiple hooks,
// non-command type) returns false.
func observerCmdMatches(group claudeHookGroup, want string) bool {
	if len(group.Hooks) != 1 {
		return false
	}
	h := group.Hooks[0]
	return h.Type == "command" && h.Command == want
}

// slicesContainsCommand reports whether entries holds a hook with
// Command == want.
func slicesContainsCommand(entries []cursorHookEntry, want string) bool {
	for _, e := range entries {
		if e.Command == want {
			return true
		}
	}
	return false
}

// isObserverCursorEntry recognises a hook command as one previously
// written by ANY observer cursor (Linux/default) registrar. The
// ` hook cursor ` token sequence is the stable signature, regardless
// of which observer binary path prefixes it. Same content-heuristic
// rationale as isObserverClaudeEntry — lets refresh-on-drift upgrade
// entries left behind by a differently-installed observer (npm
// bundle in node_modules, cross-binary upgrade, renamed $HOME)
// without --force. See isObserverClaudeEntry for the full trade-off
// discussion; the cursor entry shape uses the same observer-internal
// syntax so the collision risk is equally negligible.
//
// Excludes wsl.exe-wrapped commands so this and
// isObserverWindowsCursorEntry match disjoint shapes — see
// isObserverClaudeEntry's docstring for the same rationale.
func isObserverCursorEntry(cmd string) bool {
	if !strings.Contains(cmd, " hook cursor ") {
		return false
	}
	if strings.HasPrefix(cmd, "wsl.exe ") || strings.HasPrefix(cmd, "MSYS_NO_PATHCONV=1 wsl.exe ") {
		return false
	}
	return true
}

// hasStaleObserverEntry reports whether entries holds an observer-
// recognised hook command that doesn't match want (i.e. an old
// registration — possibly from a different observer binary — that
// needs refreshing). Different from a non-observer conflict — those
// go through hasCursorConflict.
func hasStaleObserverEntry(entries []cursorHookEntry, want string) bool {
	for _, e := range entries {
		if isObserverCursorEntry(e.Command) && e.Command != want {
			return true
		}
	}
	return false
}

// filterStaleObserverEntries drops entries recognised as observer-
// written that don't match the canonical want. Used by the refresh
// path to clear a previous registration (including ones from a
// different observer binary path) before appending the fresh one.
// Non-observer entries (other tools the user wired in by hand) pass
// through untouched.
func filterStaleObserverEntries(entries []cursorHookEntry, want string) []cursorHookEntry {
	out := make([]cursorHookEntry, 0, len(entries))
	for _, e := range entries {
		if isObserverCursorEntry(e.Command) && e.Command != want {
			continue
		}
		out = append(out, e)
	}
	return out
}

// UnregistrationResult summarizes a single tool unregistration.
type UnregistrationResult struct {
	Tool          string   // claude-code | cursor
	ConfigPath    string   // absolute path to the patched config file
	HooksRemoved  []string // event names where observer entries were removed
	HooksKept     []string // events where non-observer (user-authored) hooks remain
	DryRun        bool
	Skipped       bool // true when the config file does not exist — nothing to do
	ChecksumMatch bool // true when the stored install-time checksum matched pre-mutation
	Error         error
}

// Unregister removes observer hook entries from tool's config file. Only
// entries whose Command starts with opts.BinaryPath are removed; any
// user-authored hooks in the same file are preserved. If the file's
// checksum doesn't match the one recorded at install time, returns an
// error unless opts.Force is set.
//
// Supported tools: "claude-code", "claude-code-windows", "cursor",
// "codex".
func (r *Registry) Unregister(tool string) UnregistrationResult {
	switch tool {
	case "claude-code":
		return r.unregisterClaudeCode()
	case "claude-code-windows":
		return r.unregisterClaudeCodeWindows()
	case "cursor":
		return r.unregisterCursor()
	case "codex":
		return r.unregisterCodex()
	default:
		return UnregistrationResult{
			Tool:   tool,
			Error:  fmt.Errorf("hook.Unregister: tool %q not supported", tool),
			DryRun: r.opts.DryRun,
		}
	}
}

func (r *Registry) unregisterClaudeCode() UnregistrationResult {
	res := UnregistrationResult{Tool: "claude-code", DryRun: r.opts.DryRun}
	settingsDir := filepath.Join(r.opts.HomeDir, ".claude")
	path := filepath.Join(settingsDir, "settings.json")
	res.ConfigPath = path

	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			res.Skipped = true
			return res
		}
		res.Error = fmt.Errorf("hook.unregisterClaudeCode: read: %w", err)
		return res
	}

	settings := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &settings); err != nil {
		res.Error = fmt.Errorf("hook.unregisterClaudeCode: parse %s: %w", path, err)
		return res
	}
	hooks := map[string][]claudeHookGroup{}
	if existing, ok := settings["hooks"]; ok {
		if err := json.Unmarshal(existing, &hooks); err != nil {
			res.Error = fmt.Errorf("hook.unregisterClaudeCode: parse hooks: %w", err)
			return res
		}
	}

	for event, groups := range hooks {
		newGroups, removed, kept := filterClaudeGroups(groups)
		if removed > 0 {
			res.HooksRemoved = append(res.HooksRemoved, event)
		}
		if kept > 0 {
			res.HooksKept = append(res.HooksKept, event)
		}
		if len(newGroups) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = newGroups
		}
	}
	sort.Strings(res.HooksRemoved)
	sort.Strings(res.HooksKept)

	// No observer entries to remove — skip the checksum guard entirely
	// and treat this as a no-op regardless of file drift.
	if len(res.HooksRemoved) == 0 {
		res.Skipped = true
		return res
	}

	// There is real work to do — now verify the file hasn't drifted since
	// we installed, so we don't clobber user edits. Passing --force
	// bypasses the guard.
	match, err := r.checksumMatches(path, raw)
	if err != nil {
		res.Error = fmt.Errorf("hook.unregisterClaudeCode: checksum: %w", err)
		return res
	}
	res.ChecksumMatch = match
	if !match && !r.opts.Force {
		res.Error = fmt.Errorf("hook.unregisterClaudeCode: %s has been modified since install (checksum mismatch); pass --force to remove anyway", path)
		return res
	}

	if len(hooks) == 0 {
		delete(settings, "hooks")
	} else {
		patched, err := json.Marshal(hooks)
		if err != nil {
			res.Error = fmt.Errorf("hook.unregisterClaudeCode: marshal hooks: %w", err)
			return res
		}
		settings["hooks"] = patched
	}

	if r.opts.DryRun {
		return res
	}

	if len(settings) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			res.Error = fmt.Errorf("hook.unregisterClaudeCode: remove empty %s: %w", path, err)
			return res
		}
	} else {
		if err := writeJSONIndented(settingsDir, path, settings); err != nil {
			res.Error = err
			return res
		}
	}
	if err := r.removeChecksum(path); err != nil {
		res.Error = err
		return res
	}
	return res
}

// unregisterClaudeCodeWindows removes hook entries this registrar
// previously wrote into the Windows-side .claude/settings.json. Mirrors
// unregisterClaudeCode but matches on the wsl.exe-wrapped signature
// rather than a binary-path prefix, so user-authored hooks (and stale
// observer entries from a different distro / binary path) are still
// recognised as ours. The user's non-observer entries are preserved.
func (r *Registry) unregisterClaudeCodeWindows() UnregistrationResult {
	res := UnregistrationResult{Tool: "claude-code-windows", DryRun: r.opts.DryRun}

	claudeDir := r.detectWindowsClaudeHome()
	if claudeDir == "" {
		res.Skipped = true
		return res
	}
	res.ConfigPath = filepath.Join(claudeDir, "settings.json")

	raw, err := os.ReadFile(res.ConfigPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			res.Skipped = true
			return res
		}
		res.Error = fmt.Errorf("hook.unregisterClaudeCodeWindows: read: %w", err)
		return res
	}

	settings := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &settings); err != nil {
		res.Error = fmt.Errorf("hook.unregisterClaudeCodeWindows: parse %s: %w", res.ConfigPath, err)
		return res
	}
	hooks := map[string][]claudeHookGroup{}
	if existing, ok := settings["hooks"]; ok {
		if err := json.Unmarshal(existing, &hooks); err != nil {
			res.Error = fmt.Errorf("hook.unregisterClaudeCodeWindows: parse hooks: %w", err)
			return res
		}
	}

	for event, groups := range hooks {
		newGroups, removed, kept := filterClaudeGroupsWindows(groups)
		if removed > 0 {
			res.HooksRemoved = append(res.HooksRemoved, event)
		}
		if kept > 0 {
			res.HooksKept = append(res.HooksKept, event)
		}
		if len(newGroups) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = newGroups
		}
	}
	sort.Strings(res.HooksRemoved)
	sort.Strings(res.HooksKept)

	if len(res.HooksRemoved) == 0 {
		res.Skipped = true
		return res
	}

	match, err := r.checksumMatches(res.ConfigPath, raw)
	if err != nil {
		res.Error = fmt.Errorf("hook.unregisterClaudeCodeWindows: checksum: %w", err)
		return res
	}
	res.ChecksumMatch = match
	if !match && !r.opts.Force {
		res.Error = fmt.Errorf("hook.unregisterClaudeCodeWindows: %s has been modified since install (checksum mismatch); pass --force to remove anyway", res.ConfigPath)
		return res
	}

	if len(hooks) == 0 {
		delete(settings, "hooks")
	} else {
		patched, err := json.Marshal(hooks)
		if err != nil {
			res.Error = fmt.Errorf("hook.unregisterClaudeCodeWindows: marshal hooks: %w", err)
			return res
		}
		settings["hooks"] = patched
	}

	if r.opts.DryRun {
		return res
	}

	if len(settings) == 0 {
		if err := os.Remove(res.ConfigPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			res.Error = fmt.Errorf("hook.unregisterClaudeCodeWindows: remove empty %s: %w", res.ConfigPath, err)
			return res
		}
	} else {
		if err := writeJSONIndented(claudeDir, res.ConfigPath, settings); err != nil {
			res.Error = err
			return res
		}
	}
	if err := r.removeChecksum(res.ConfigPath); err != nil {
		res.Error = err
		return res
	}
	return res
}

// filterClaudeGroupsWindows walks groups, drops any command our
// Windows registrar previously wrote (wsl.exe-wrapped observer
// invocation), and discards groups left empty. Returns the survivors
// plus removed / kept counts so the caller can decide whether to
// touch the file at all.
func filterClaudeGroupsWindows(groups []claudeHookGroup) (out []claudeHookGroup, removed, kept int) {
	for _, g := range groups {
		var survivors []claudeHookCommand
		for _, h := range g.Hooks {
			if h.Type == "command" && isObserverWindowsClaudeEntry(h.Command) {
				removed++
				continue
			}
			survivors = append(survivors, h)
		}
		if len(survivors) == 0 {
			continue
		}
		kept += len(survivors)
		out = append(out, claudeHookGroup{Matcher: g.Matcher, Hooks: survivors})
	}
	return out, removed, kept
}

func (r *Registry) unregisterCursor() UnregistrationResult {
	res := UnregistrationResult{Tool: "cursor", DryRun: r.opts.DryRun}
	cursorDir := filepath.Join(r.opts.HomeDir, ".cursor")
	path := filepath.Join(cursorDir, "hooks.json")
	res.ConfigPath = path

	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			res.Skipped = true
			return res
		}
		res.Error = fmt.Errorf("hook.unregisterCursor: read: %w", err)
		return res
	}

	settings := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &settings); err != nil {
		res.Error = fmt.Errorf("hook.unregisterCursor: parse: %w", err)
		return res
	}
	hooks := map[string][]cursorHookEntry{}
	if existing, ok := settings["hooks"]; ok {
		_ = json.Unmarshal(existing, &hooks)
	}

	for event, entries := range hooks {
		var survivors []cursorHookEntry
		removed := 0
		for _, e := range entries {
			// Content-heuristic so cross-binary installs (npm bundle,
			// renamed $HOME, prior worktree binary) get cleaned up
			// when uninstalling from a different observer build.
			// Mirrors the register-side isObserverCursorEntry usage —
			// without this, byte-exact prefix-match would leave
			// orphaned entries behind on `observer uninstall cursor`.
			if isObserverCursorEntry(e.Command) {
				removed++
				continue
			}
			survivors = append(survivors, e)
		}
		if removed > 0 {
			res.HooksRemoved = append(res.HooksRemoved, event)
		}
		if len(survivors) > 0 {
			res.HooksKept = append(res.HooksKept, event)
		}
		if len(survivors) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = survivors
		}
	}
	sort.Strings(res.HooksRemoved)
	sort.Strings(res.HooksKept)

	if len(res.HooksRemoved) == 0 {
		res.Skipped = true
		return res
	}

	match, err := r.checksumMatches(path, raw)
	if err != nil {
		res.Error = fmt.Errorf("hook.unregisterCursor: checksum: %w", err)
		return res
	}
	res.ChecksumMatch = match
	if !match && !r.opts.Force {
		res.Error = fmt.Errorf("hook.unregisterCursor: %s has been modified since install (checksum mismatch); pass --force to remove anyway", path)
		return res
	}

	if len(hooks) == 0 {
		delete(settings, "hooks")
	} else {
		hookJSON, err := json.Marshal(hooks)
		if err != nil {
			res.Error = fmt.Errorf("hook.unregisterCursor: marshal hooks: %w", err)
			return res
		}
		settings["hooks"] = hookJSON
	}

	if r.opts.DryRun {
		return res
	}

	// If the only surviving keys are the "version" we manufactured at
	// install time, remove the file entirely so uninstall leaves no trace.
	if len(settings) == 1 {
		if _, onlyVersion := settings["version"]; onlyVersion {
			delete(settings, "version")
		}
	}
	if len(settings) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			res.Error = fmt.Errorf("hook.unregisterCursor: remove %s: %w", path, err)
			return res
		}
	} else {
		if err := writeJSONIndented(cursorDir, path, settings); err != nil {
			res.Error = err
			return res
		}
	}
	if err := r.removeChecksum(path); err != nil {
		res.Error = err
		return res
	}
	return res
}

// filterClaudeGroups walks groups, drops any command recognised as
// observer-written via isObserverClaudeEntry, and cleans up any
// group left empty. Returns the surviving groups, the count of
// removed observer entries, and the count of surviving non-observer
// entries. Content-heuristic (vs byte-exact binary-path prefix) so
// cross-binary stale entries — npm bundle in node_modules, renamed
// $HOME, prior worktree build — also get cleaned up when uninstalling
// from a different observer binary. Mirrors the register-side
// findClaudeGroupWithObserver / hasConflictingClaudeHook usage.
func filterClaudeGroups(groups []claudeHookGroup) (out []claudeHookGroup, removed, kept int) {
	for _, g := range groups {
		var survivors []claudeHookCommand
		for _, h := range g.Hooks {
			if h.Type == "command" && isObserverClaudeEntry(h.Command) {
				removed++
				continue
			}
			survivors = append(survivors, h)
		}
		if len(survivors) == 0 {
			continue
		}
		kept += len(survivors)
		out = append(out, claudeHookGroup{Matcher: g.Matcher, Hooks: survivors})
	}
	return out, removed, kept
}

// checksumMatches reports whether the hash stored for path in the
// checksums registry matches SHA256(data). A missing entry or missing
// registry file returns (false, nil) — caller decides policy.
func (r *Registry) checksumMatches(path string, data []byte) (bool, error) {
	csPath := r.opts.ChecksumsPath
	if csPath == "" {
		csPath = filepath.Join(r.opts.HomeDir, ".observer", "hook_checksums.json")
	}
	raw, err := os.ReadFile(csPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	current := map[string]map[string]any{}
	if err := json.Unmarshal(raw, &current); err != nil {
		return false, err
	}
	entry, ok := current[path]
	if !ok {
		return false, nil
	}
	stored, _ := entry["sha256"].(string)
	sum := sha256.Sum256(data)
	return stored == hex.EncodeToString(sum[:]), nil
}

// removeChecksum deletes path's entry from the checksums registry. When
// the registry becomes empty it is removed entirely. Missing registry is
// not an error.
func (r *Registry) removeChecksum(path string) error {
	csPath := r.opts.ChecksumsPath
	if csPath == "" {
		csPath = filepath.Join(r.opts.HomeDir, ".observer", "hook_checksums.json")
	}
	raw, err := os.ReadFile(csPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("hook.removeChecksum: read: %w", err)
	}
	current := map[string]any{}
	if err := json.Unmarshal(raw, &current); err != nil {
		return fmt.Errorf("hook.removeChecksum: parse: %w", err)
	}
	delete(current, path)
	if len(current) == 0 {
		if err := os.Remove(csPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("hook.removeChecksum: remove %s: %w", csPath, err)
		}
		return nil
	}
	body, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return fmt.Errorf("hook.removeChecksum: marshal: %w", err)
	}
	if err := os.WriteFile(csPath, body, 0o600); err != nil {
		return fmt.Errorf("hook.removeChecksum: write: %w", err)
	}
	return nil
}

// codexEvents is the set of Codex hook events we register for. Codex's
// hook event names are CamelCase identical to Claude Code's, but codex
// fires its own subset (no Pre/Post compact, no SubagentStart/Stop, etc.).
// Per developers.openai.com/codex/hooks (snapshot 2026-05-09), codex
// exposes 6 events; we register all 6 so observer's hook handler is the
// single point of capture / dispatch.
var codexEvents = []string{
	"SessionStart",
	"UserPromptSubmit",
	"PreToolUse",
	"PermissionRequest",
	"PostToolUse",
	"Stop",
}

// codexHooksFile is the per-event-list config; we keep hooks in
// ~/.codex/hooks.json (codex also accepts [hooks.<Event>] inside
// config.toml but the JSON file path is canonical and the diff stays
// scoped to a single file). codex 0.129.0 requires
// `[features].hooks = true` in config.toml separately to actually
// dispatch hooks — registerCodex sets both. See
// ensureCodexHooksFeatureFlag for the verified flag-name history.
const codexHooksFile = "hooks.json"

// codexHookGroup mirrors claudeHookGroup — codex inherited Claude Code's
// hooks-config schema verbatim (top-level `hooks` map → event-name
// arrays → group with matcher + nested hooks list).
type codexHookGroup struct {
	Matcher string              `json:"matcher,omitempty"`
	Hooks   []claudeHookCommand `json:"hooks"`
}

// codexHooksFile body is `{"hooks": {<event>: [<group>...]}}`.
type codexHooksConfig struct {
	Hooks map[string][]codexHookGroup `json:"hooks"`
}

// registerCodex installs observer hooks into ~/.codex/hooks.json AND
// ensures `[features].hooks = true` in ~/.codex/config.toml so codex
// actually dispatches them. Idempotent — re-running with the same
// binary path returns AlreadySet.
//
// Trust caveat: codex requires per-hook user trust approval the first
// time each entry is seen (security feature). The user must run codex
// once after registration and use `/hooks` to mark our entries trusted;
// there is no documented programmatic shortcut as of codex 0.129.0
// (the trust hash algorithm is opaque and not exposed via any
// `codex` subcommand). The CLI prints a hint after registration.
func (r *Registry) registerCodex() RegistrationResult {
	res := RegistrationResult{Tool: "codex", DryRun: r.opts.DryRun}
	dir := filepath.Join(r.opts.HomeDir, ".codex")
	hooksPath := filepath.Join(dir, codexHooksFile)
	res.ConfigPath = hooksPath

	cfg, err := readCodexHooks(hooksPath)
	if err != nil {
		res.Error = err
		return res
	}

	// codexCmdQuoteIfNeeded picks the right quoter based on path
	// shape: POSIX single-quote for Linux/macOS paths (codex spawns
	// hooks via /bin/sh there), cmd.exe double-quote for Windows
	// paths (codex 0.133+ on Windows spawns via cmd.exe — single
	// quotes are interpreted literally and the command fails). The
	// v1.6.25 single-quote fix here was correct for Claude Code (Git
	// Bash always) but wrong for Codex on Windows; operator report
	// 2026-05-23 surfaced the regression. See codexCmdQuoteIfNeeded
	// docstring for the full rationale + trade-off discussion.
	quote := codexCmdQuoteIfNeeded
	for _, event := range codexEvents {
		cmd := quote(r.opts.BinaryPath) + " hook codex " + event + r.configFlagSuffixWith(quote)
		groups := cfg.Hooks[event]
		idx := findCodexGroupWithObserver(groups)
		if idx >= 0 {
			if observerCodexCmdMatches(groups[idx], cmd) {
				res.AlreadySet = append(res.AlreadySet, event)
				continue
			}
			// Stale-observer-args / cross-binary refresh — recognised
			// as ours via content-heuristic (isObserverCodexEntry).
			// Drop the stale group; the fresh append below restores.
			groups = append(groups[:idx], groups[idx+1:]...)
		}
		if !r.opts.Force && hasConflictingCodexHook(groups) {
			res.Error = fmt.Errorf("hook.registerCodex: event %s already has a non-observer hook; pass --force to overwrite", event)
			return res
		}
		groups = append(groups, codexHookGroup{
			Matcher: "*",
			Hooks:   []claudeHookCommand{{Type: "command", Command: cmd}},
		})
		cfg.Hooks[event] = groups
		res.HooksAdded = append(res.HooksAdded, event)
	}

	if r.opts.DryRun {
		// Still report whether the feature flag would be flipped.
		return res
	}

	if err := writeCodexHooks(dir, hooksPath, cfg); err != nil {
		res.Error = err
		return res
	}
	if err := r.recordChecksum(hooksPath); err != nil {
		res.Error = err
		return res
	}
	if err := ensureCodexHooksFeatureFlag(dir); err != nil {
		res.Error = err
		return res
	}
	return res
}

func (r *Registry) unregisterCodex() UnregistrationResult {
	res := UnregistrationResult{Tool: "codex", DryRun: r.opts.DryRun}
	dir := filepath.Join(r.opts.HomeDir, ".codex")
	hooksPath := filepath.Join(dir, codexHooksFile)
	res.ConfigPath = hooksPath

	raw, err := os.ReadFile(hooksPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			res.Skipped = true
			return res
		}
		res.Error = fmt.Errorf("hook.unregisterCodex: read: %w", err)
		return res
	}
	var cfg codexHooksConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		res.Error = fmt.Errorf("hook.unregisterCodex: parse %s: %w", hooksPath, err)
		return res
	}
	if cfg.Hooks == nil {
		cfg.Hooks = map[string][]codexHookGroup{}
	}

	for event, groups := range cfg.Hooks {
		newGroups, removed, kept := filterCodexGroups(groups)
		if removed > 0 {
			res.HooksRemoved = append(res.HooksRemoved, event)
		}
		if kept > 0 {
			res.HooksKept = append(res.HooksKept, event)
		}
		if len(newGroups) == 0 {
			delete(cfg.Hooks, event)
		} else {
			cfg.Hooks[event] = newGroups
		}
	}
	sort.Strings(res.HooksRemoved)
	sort.Strings(res.HooksKept)

	if len(res.HooksRemoved) == 0 {
		res.Skipped = true
		return res
	}

	match, err := r.checksumMatches(hooksPath, raw)
	if err != nil {
		res.Error = fmt.Errorf("hook.unregisterCodex: checksum: %w", err)
		return res
	}
	if !match && !r.opts.Force {
		res.Error = fmt.Errorf("hook.unregisterCodex: %s changed since install; pass --force to overwrite", hooksPath)
		return res
	}

	if r.opts.DryRun {
		return res
	}
	// When no entries remain we still write an empty `{"hooks":{}}` rather
	// than removing the file entirely; codex tolerates the empty object and
	// the user may have added their own entries we don't know about
	// (kept-rows above already short-circuit if any non-observer entries
	// remain).
	if err := writeCodexHooks(dir, hooksPath, cfg); err != nil {
		res.Error = err
		return res
	}
	if err := r.removeChecksum(hooksPath); err != nil {
		res.Error = err
		return res
	}
	// Note: we DO NOT flip [features].hooks = false on uninstall —
	// the user may have other hooks registered through that flag.
	return res
}

// readCodexHooks loads ~/.codex/hooks.json, returning an empty config
// when the file doesn't exist.
func readCodexHooks(path string) (codexHooksConfig, error) {
	cfg := codexHooksConfig{Hooks: map[string][]codexHookGroup{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("hook.readCodexHooks: read: %w", err)
	}
	if len(raw) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("hook.readCodexHooks: parse %s: %w", path, err)
	}
	if cfg.Hooks == nil {
		cfg.Hooks = map[string][]codexHookGroup{}
	}
	return cfg, nil
}

// writeCodexHooks marshals cfg as 2-space-indented JSON with stable
// event ordering. Creates the parent dir if missing.
func writeCodexHooks(dir, path string, cfg codexHooksConfig) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("hook.writeCodexHooks: mkdir: %w", err)
	}
	// Emit with sorted event keys so JSON diffs stay clean across
	// re-registrations.
	keys := make([]string, 0, len(cfg.Hooks))
	for k := range cfg.Hooks {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var buf bytes.Buffer
	buf.WriteString("{\n  \"hooks\": {")
	for i, k := range keys {
		if i > 0 {
			buf.WriteString(",")
		}
		buf.WriteString("\n    ")
		ke, _ := json.Marshal(k)
		buf.Write(ke)
		buf.WriteString(": ")
		ge, err := json.MarshalIndent(cfg.Hooks[k], "    ", "  ")
		if err != nil {
			return fmt.Errorf("hook.writeCodexHooks: encode %s: %w", k, err)
		}
		buf.Write(ge)
	}
	if len(keys) > 0 {
		buf.WriteString("\n  ")
	}
	buf.WriteString("}\n}\n")

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("hook.writeCodexHooks: write: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("hook.writeCodexHooks: rename: %w", err)
	}
	return nil
}

// isObserverCodexEntry recognises a hook command as one previously
// written by ANY observer codex registrar. Same content-heuristic
// rationale as isObserverClaudeEntry and isObserverCursorEntry — the
// ` hook codex ` token sequence is the stable signature regardless
// of which observer binary path prefixes it. Lets refresh-on-drift
// upgrade entries left behind by a differently-installed observer
// (npm bundle in node_modules, cross-binary upgrade, renamed
// $HOME) without --force.
func isObserverCodexEntry(cmd string) bool {
	return strings.Contains(cmd, " hook codex ")
}

// findCodexGroupWithObserver returns the index of a codex hook
// group whose single entry is recognised as observer-written by
// isObserverCodexEntry, or -1. Content-heuristic (see
// isObserverCodexEntry) so cross-binary stale entries are still
// detected as ours and refreshed.
func findCodexGroupWithObserver(groups []codexHookGroup) int {
	for i, g := range groups {
		for _, h := range g.Hooks {
			if h.Type == "command" && isObserverCodexEntry(h.Command) {
				return i
			}
		}
	}
	return -1
}

// observerCodexCmdMatches reports whether g already encodes the observer
// hook entry shape we'd write — same matcher ("*"), single command of
// type "command", same command body. Drift in any of these fields means
// the entry is stale and registerCodex should refresh it.
func observerCodexCmdMatches(g codexHookGroup, cmd string) bool {
	if g.Matcher != "*" {
		return false
	}
	if len(g.Hooks) != 1 {
		return false
	}
	h := g.Hooks[0]
	return h.Type == "command" && h.Command == cmd
}

// hasConflictingCodexHook reports whether any group carries a
// command that isn't observer-shaped. Force-less guard against
// silently overwriting user-authored hooks. Content-heuristic via
// isObserverCodexEntry — cross-binary stale entries fall through
// to the refresh path.
func hasConflictingCodexHook(groups []codexHookGroup) bool {
	for _, g := range groups {
		for _, h := range g.Hooks {
			if h.Type != "command" {
				continue
			}
			if !isObserverCodexEntry(h.Command) {
				return true
			}
		}
	}
	return false
}

// filterCodexGroups returns groups with all observer-owned entries
// (recognised via isObserverCodexEntry) stripped, plus counts
// (removed, kept) for the result struct. Content-heuristic so
// cross-binary stale entries are also cleaned up on uninstall —
// mirrors the register-side findCodexGroupWithObserver /
// hasConflictingCodexHook usage.
func filterCodexGroups(groups []codexHookGroup) ([]codexHookGroup, int, int) {
	var out []codexHookGroup
	var removed, kept int
	for _, g := range groups {
		var keepHooks []claudeHookCommand
		for _, h := range g.Hooks {
			if h.Type == "command" && isObserverCodexEntry(h.Command) {
				removed++
				continue
			}
			keepHooks = append(keepHooks, h)
		}
		if len(keepHooks) == 0 {
			continue
		}
		kept += len(keepHooks)
		g.Hooks = keepHooks
		out = append(out, g)
	}
	return out, removed, kept
}

// ensureCodexHooksFeatureFlag patches `[features].hooks = true` into
// ~/.codex/config.toml if not already set. Codex gates the hook
// dispatcher behind this flag — without it, hooks.json is read but
// never invoked.
//
// **Verified flag name (2026-05-11):** codex 0.129.0 prints
// "`[features].codex_hooks` is deprecated. Use `[features].hooks`
// instead." when it sees the longer form. So `hooks = true` is the
// canonical name despite some published docs (e.g. the local
// reference at tmp/codex-hooks.md) showing `codex_hooks`. Observer's
// long-standing choice of `hooks = true` is correct.
//
// **Tool-context hook coverage caveat:** even with this flag set,
// codex's `PreToolUse` / `PostToolUse` hooks only intercept "simple
// Bash calls, apply_patch edits, and MCP tool calls" (per
// docs.codex.com hooks reference). Modern codex shell calls route
// through `unified_exec` which is NOT intercepted yet. Result:
// tool-using prompts produce JSONL rows but rarely emit hook rows.
// This is a codex design limitation, not an observer bug. See
// docs/codex-hook-capture.md for the maintainer's 2026-05-11
// dogfood notes.
func ensureCodexHooksFeatureFlag(dir string) error {
	path := filepath.Join(dir, "config.toml")
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("hook.ensureCodexHooksFeatureFlag: read: %w", err)
	}
	root := map[string]any{}
	if len(raw) > 0 {
		if err := toml.Unmarshal(raw, &root); err != nil {
			return fmt.Errorf("hook.ensureCodexHooksFeatureFlag: parse %s: %w", path, err)
		}
	}
	features, _ := root["features"].(map[string]any)
	if features == nil {
		features = map[string]any{}
	}
	if v, ok := features["hooks"].(bool); ok && v {
		return nil
	}
	features["hooks"] = true
	root["features"] = features

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("hook.ensureCodexHooksFeatureFlag: mkdir: %w", err)
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(root); err != nil {
		return fmt.Errorf("hook.ensureCodexHooksFeatureFlag: encode: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("hook.ensureCodexHooksFeatureFlag: write: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("hook.ensureCodexHooksFeatureFlag: rename: %w", err)
	}
	return nil
}
