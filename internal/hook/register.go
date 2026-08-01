package hook

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/marmutapp/superbased-observer/internal/claudeplugin"
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

	// Skipped reports that the registrar deliberately wrote nothing
	// because the tool already carries observer's wiring through its own
	// packaging surface — today, the Claude Code plugin, which declares
	// the same hook events. Not an error: the user is already covered,
	// and registering again would fire every event twice. SkipReason
	// names the artifact that proved it. Both zero on every other path.
	Skipped    bool
	SkipReason string

	// SkipAdvice is the operator-facing next step that goes with
	// SkipReason. Empty means "the default plugin advice applies" (the
	// printer supplies it), so the plugin skip path is unchanged; the
	// cross-OS sandbox skip (see crossmount.AutoDetectSuppressed) sets it
	// because the --force escape hatch deliberately does NOT apply there.
	SkipAdvice string

	// ProbeWarning is a NON-FATAL note: the plugin probe could not read
	// a file it needed, so the registrar registered without being able
	// to rule out a plugin already providing this wiring. Registration
	// still happened (fail-open to wiring) — this is a reporting
	// channel, never control flow. Empty on every conclusive path.
	ProbeWarning string
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
	// homeOverride is the caller-supplied Options.HomeDir VERBATIM, kept
	// before NewRegistry defaults it to the real $HOME. Non-empty means
	// "the caller pinned this registry's home", which switches OFF
	// crossmount auto-detection for the cross-OS targets — see
	// crossmount.AutoDetectSuppressed for the 2026-07-31 incident that
	// makes this load-bearing.
	homeOverride string
}

// NewRegistry returns a registry ready to install hooks.
func NewRegistry(opts Options) (*Registry, error) {
	if opts.BinaryPath == "" {
		return nil, errors.New("hook.NewRegistry: BinaryPath is required")
	}
	homeOverride := opts.HomeDir
	if opts.HomeDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("hook.NewRegistry: UserHomeDir: %w", err)
		}
		opts.HomeDir = home
	}
	return &Registry{opts: opts, homeOverride: homeOverride}, nil
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

// allHomes and homeOwnedByCurrentWindowsUser are the crossmount seams (package
// vars, mirroring internal/proxyroute) so tests can inject a fixed multi-home
// layout AND a deterministic ownership verdict without a real /mnt/c mount or
// a cmd.exe interop shell (restore them in a defer).
var (
	allHomes                      = crossmount.AllHomes
	homeOwnedByCurrentWindowsUser = crossmount.HomeOwnedByCurrentWindowsUser
)

// detectWindowsClaudeHome returns the resolved Windows-side .claude
// directory used by the claude-code-windows registration target, or
// "" if none. Honors Options.WindowsClaudeHome when set; otherwise
// accepts an auto-detected OS=windows home that has a `.claude/`
// subdirectory ONLY when crossmount can prove it belongs to the current
// Windows user. Mirrors detectWindowsCursorHome and the proxy-route
// side's resolveWindowsHome R1 guard.
func (r *Registry) detectWindowsClaudeHome() string {
	return r.detectWindowsHome(r.opts.WindowsClaudeHome, ".claude")
}

// WindowsClaudeDir exposes the resolved Windows-side .claude directory
// the claude-code-windows registration target writes into, or "" when
// there is none. Exported so `observer doctor` can inspect the SAME
// directory this registrar would write to, instead of re-deriving the
// crossmount + ownership rules and drifting from them (one owner). Its
// argument mirrors Options.WindowsClaudeHome: an explicit Windows USER
// home override, or "" to auto-detect.
func WindowsClaudeDir(override string) string {
	r := &Registry{opts: Options{WindowsClaudeHome: override}}
	return r.detectWindowsClaudeHome()
}

// detectWindowsCursorHome returns the resolved Windows-side .cursor
// directory used by the cursor-windows registration target, or "" if
// none. Honors Options.WindowsCursorHome when set; otherwise accepts an
// auto-detected OS=windows home carrying `.cursor/` only when
// crossmount-ownership-verified.
func (r *Registry) detectWindowsCursorHome() string {
	return r.detectWindowsHome(r.opts.WindowsCursorHome, ".cursor")
}

// detectWindowsHome resolves the Windows-side <subdir> directory for a
// cross-OS registration target. Ownership discipline mirrors the proxy-route
// writer's resolveWindowsHome (security finding R1/F1): a WSL daemon must NOT
// install hooks into another Windows user's config just because theirs is the
// only `.claude`/`.cursor` mounted.
//
//   - An explicit override wins unconditionally — the operator named the home,
//     so ownership verification is moot; returned even if the dir doesn't exist
//     yet (the registrar mkdir's on first install).
//   - Otherwise an auto-detected OS=windows home carrying <subdir> is accepted
//     ONLY when crossmount proves it belongs to the current Windows user
//     (base name matches %USERNAME%). Zero owned homes — or an ambiguous
//     several — resolve to "" so the virtual target simply doesn't surface in
//     Installed() (the honest floor: no behaviour change on a single-user
//     machine where the name matches; a refusal to guess otherwise).
func (r *Registry) detectWindowsHome(override, subdir string) string {
	// Sandbox gate FIRST — before the override branch, because an override
	// that points OUTSIDE the pinned home is exactly the escape hatch that
	// re-opened this hole. See crossmount.AutoDetectSuppressed (incident
	// 2026-07-31). Production never pins HomeDir, so this is false on every
	// real path and a bare override still wins unconditionally.
	if r.foreignAutoDetectSuppressed(override) {
		return ""
	}
	if override != "" {
		return filepath.Join(override, subdir)
	}
	var owned []string
	for _, h := range allHomes() {
		if h.OS != crossmount.OSWindows {
			continue
		}
		dir := filepath.Join(h.Path, subdir)
		if !r.dirExists(dir) {
			continue
		}
		if homeOwnedByCurrentWindowsUser(h.Path) {
			owned = append(owned, dir)
		}
	}
	if len(owned) == 1 {
		return owned[0]
	}
	return ""
}

func (r *Registry) dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// foreignAutoDetectSuppressed reports whether this registry's caller pinned
// HomeDir (a sandbox) without naming the Windows-side home for the target
// being resolved. See crossmount.AutoDetectSuppressed for the rule and the
// 2026-07-31 incident it exists to make structurally impossible.
func (r *Registry) foreignAutoDetectSuppressed(override string) bool {
	return crossmount.AutoDetectSuppressed(r.homeOverride, override)
}

// sandboxSkipResult fills res as a deliberate no-write for a cross-OS target
// the sandbox gate suppressed. It is a SKIP, not an error: nothing is wrong
// with the host — the caller pinned a home and either never said which
// Windows-side home it wanted, or named one OUTSIDE that sandbox, so there is
// no path this registry is allowed to touch. optionName is the Options field
// that unlocks the target; its value must resolve INSIDE the pinned home.
func (r *Registry) sandboxSkipResult(res *RegistrationResult, subdir, optionName, override string) {
	res.Skipped = true
	if override == "" {
		res.SkipReason = fmt.Sprintf(
			"HomeDir was pinned by the caller but no %s was given — cross-OS %s/ resolution is suppressed (incident 2026-07-31)",
			optionName, subdir,
		)
	} else {
		res.SkipReason = fmt.Sprintf(
			"%s (%s) resolves OUTSIDE the pinned HomeDir (%s) — cross-OS %s/ resolution is suppressed (incident 2026-07-31)",
			optionName, override, r.homeOverride, subdir,
		)
	}
	res.SkipAdvice = fmt.Sprintf(
		"nothing written; a sandboxed caller must set %s to a home UNDER its own HomeDir to wire this target (--force does not lift this).",
		optionName,
	)
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

	// Double-wiring guard: the Claude Code plugin declares exactly the
	// events below, and Claude Code merges hook config from every source,
	// so registering on top of an installed plugin fires each event
	// twice. Skip instead. --force still writes, for the operator who
	// wants both (e.g. mid-migration off the plugin).
	//
	// A skip needs AFFIRMATIVE evidence. When the probe could not read
	// settings.json it reports Uncertain and we fall through to register:
	// losing capture because a config file was corrupt would be a far
	// worse failure than a doubled event. The read below hits the same
	// file and surfaces the underlying error through res.Error.
	pd := claudeplugin.DetectInClaudeDir(settingsDir)
	if pd.Active && !r.opts.Force {
		res.Skipped = true
		res.SkipReason = pd.Reason()
		return res
	}
	res.ProbeWarning = pd.Warning()

	// Serialize observer's own writers of this file, THEN read — the
	// snapshot we mutate has to be taken after the lock, or a
	// concurrent observer's committed write is lost.
	unlock, err := r.lockSettings(path)
	if err != nil {
		res.Error = fmt.Errorf("hook.registerClaudeCode: %w", err)
		return res
	}
	defer unlock()

	// Pin the write target now, before the read — see pinnedTarget (F6).
	pinned := pinWriteTarget(path)

	raw, err := readSettingsFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		res.Error = fmt.Errorf("hook.registerClaudeCode: read: %w", err)
		return res
	}
	// Preserve unknown top-level fields via map[string]json.RawMessage.
	settings := map[string]json.RawMessage{}
	if len(raw) > 0 {
		settings, err = decodeSettingsObject(path, raw)
		if err != nil {
			res.Error = fmt.Errorf("hook.registerClaudeCode: %w", err)
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
	if err := writeJSONIndented(settingsDir, pinned, settings); err != nil {
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
		if r.foreignAutoDetectSuppressed(r.opts.WindowsClaudeHome) {
			r.sandboxSkipResult(&res, ".claude", "WindowsClaudeHome", r.opts.WindowsClaudeHome)
			return res
		}
		res.Error = errors.New("hook.registerClaudeCodeWindows: no Windows-side .claude/ detected (set WindowsClaudeHome explicitly or run on a host where crossmount sees /mnt/c/Users/<u>/.claude/)")
		return res
	}
	res.ConfigPath = filepath.Join(claudeDir, "settings.json")

	// Same double-wiring guard as the native registrar, applied to the
	// Windows-side .claude the cross-OS bridge writes into: that install
	// keeps its own plugin state there. Same fail-open-to-wiring rule.
	pdWin := claudeplugin.DetectInClaudeDir(claudeDir)
	if pdWin.Active && !r.opts.Force {
		res.Skipped = true
		res.SkipReason = pdWin.Reason()
		return res
	}
	res.ProbeWarning = pdWin.Warning()

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

	unlock, err := r.lockSettings(res.ConfigPath)
	if err != nil {
		res.Error = fmt.Errorf("hook.registerClaudeCodeWindows: %w", err)
		return res
	}
	defer unlock()

	// Pin the write target now, before the read — see pinnedTarget (F6).
	pinned := pinWriteTarget(res.ConfigPath)

	raw, err := readSettingsFile(res.ConfigPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		res.Error = fmt.Errorf("hook.registerClaudeCodeWindows: read: %w", err)
		return res
	}
	settings := map[string]json.RawMessage{}
	if len(raw) > 0 {
		settings, err = decodeSettingsObject(res.ConfigPath, raw)
		if err != nil {
			res.Error = fmt.Errorf("hook.registerClaudeCodeWindows: %w", err)
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
		cmd := wrapperPrefix + shellQuoteIfNeeded(r.opts.BinaryPath) + " hook claude-code " + hookEventArg(event) + r.configFlagSuffix()
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
	if err := writeJSONIndented(claudeDir, pinned, settings); err != nil {
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

// IsObserverClaudeCodeHookCommand reports whether cmd is an observer
// `hook claude-code <event>` invocation, in either the native shape or
// the wsl.exe cross-OS bridge shape this package writes.
//
// This is the STRICT counterpart of isObserverClaudeEntry. The two exist
// for opposite reasons and must not be merged:
//
//   - isObserverClaudeEntry is deliberately loose (a bare
//     ` hook claude-code ` substring test) because the REGISTRAR has to
//     recognise its own stale entries in order to refresh them; a
//     false negative there strands a user on an old binary forever.
//   - this one is for REPORTING (the `claude-code.plugin` doctor probe),
//     where a false POSITIVE is the harm: a third-party command such as
//     `/opt/acme hook claude-code audit` would otherwise raise a
//     double-wiring warning naming wiring observer never wrote.
//
// So this tokenizes and checks an allow-list: after stripping any
// leading environment assignments and the `wsl.exe -d <distro> --`
// bridge prefix, argv[0] must NAME an observer binary
// (isObserverBinaryToken — basename `observer`/`superbased`, optional
// `.exe`, optional `-suffix`) and be followed by exactly the tokens
// `hook` `claude-code`.
func IsObserverClaudeCodeHookCommand(cmd string) bool {
	toks := stripHookCommandPrefix(splitCommandTokens(cmd))
	if len(toks) < 3 {
		return false
	}
	return isObserverBinaryToken(toks[0]) && toks[1] == "hook" && toks[2] == "claude-code"
}

// stripHookCommandPrefix removes the leading `KEY=VALUE` environment
// assignments and, if present, the `wsl.exe -d <distro> --` bridge that
// registerClaudeCodeWindows writes, returning the tokens of the command
// actually executed. Returns nil when a bridge prefix is present but
// malformed (no `--` separator), so a half-understood command is never
// treated as ours.
func stripHookCommandPrefix(toks []string) []string {
	for len(toks) > 0 && strings.Contains(toks[0], "=") &&
		!strings.ContainsAny(toks[0], `/\`) {
		toks = toks[1:]
	}
	if len(toks) == 0 {
		return nil
	}
	base := strings.ToLower(strings.TrimSuffix(commandBaseName(toks[0]), ".exe"))
	if base != "wsl" {
		return toks
	}
	for i, t := range toks {
		if t == "--" {
			return toks[i+1:]
		}
	}
	return nil
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

	unlock, err := r.lockSettings(path)
	if err != nil {
		res.Error = fmt.Errorf("hook.registerCursor: %w", err)
		return res
	}
	defer unlock()

	// Pin the write target now, before the read — see pinnedTarget (F6).
	pinned := pinWriteTarget(path)

	raw, err := readSettingsFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		res.Error = fmt.Errorf("hook.registerCursor: read: %w", err)
		return res
	}
	settings := map[string]json.RawMessage{}
	if len(raw) > 0 {
		settings, err = decodeSettingsObject(path, raw)
		if err != nil {
			res.Error = fmt.Errorf("hook.registerCursor: %w", err)
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
	if err := writeJSONIndented(cursorDir, pinned, settings); err != nil {
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
		if r.foreignAutoDetectSuppressed(r.opts.WindowsCursorHome) {
			r.sandboxSkipResult(&res, ".cursor", "WindowsCursorHome", r.opts.WindowsCursorHome)
			return res
		}
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

	unlock, err := r.lockSettings(res.ConfigPath)
	if err != nil {
		res.Error = fmt.Errorf("hook.registerCursorWindows: %w", err)
		return res
	}
	defer unlock()

	// Pin the write target now, before the read — see pinnedTarget (F6).
	pinned := pinWriteTarget(res.ConfigPath)

	raw, err := readSettingsFile(res.ConfigPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		res.Error = fmt.Errorf("hook.registerCursorWindows: read: %w", err)
		return res
	}
	settings := map[string]json.RawMessage{}
	if len(raw) > 0 {
		settings, err = decodeSettingsObject(res.ConfigPath, raw)
		if err != nil {
			res.Error = fmt.Errorf("hook.registerCursorWindows: %w", err)
			return res
		}
	}
	hooks := map[string][]cursorHookEntry{}
	if existing, ok := settings["hooks"]; ok {
		_ = json.Unmarshal(existing, &hooks)
	}

	// NO MSYS_NO_PATHCONV=1 prefix here: Cursor executes hook commands via
	// PowerShell on Windows (it runs `Get-Content <payload> -Raw | & {
	// $input | <command> }`), where a bash-style `VAR=value cmd` env-prefix
	// is not a command — PowerShell fails with "The term
	// 'MSYS_NO_PATHCONV=1' is not recognized" and the hook never fires
	// (live-confirmed against Cursor 3.9.16, session 77fefbb3). PowerShell
	// (and cmd.exe) pass the /home/... arg to wsl.exe verbatim — they don't
	// do MSYS path translation — so the prefix that Git Bash needs is both
	// unnecessary AND fatal here. The v1.6.22 MSYS prefix targeted an older
	// Cursor that ran hooks through /bin/bash (Git Bash); the current
	// surface is PowerShell. isObserverWindowsCursorEntry still recognises
	// the legacy MSYS-prefixed shape, so refresh-on-drift replaces it.
	// (claude-code's Windows bridge may share this once its hook shell is
	// confirmed — tracked separately.)
	wrapperPrefix := fmt.Sprintf("wsl.exe -d %s -- ", shellQuoteIfNeeded(distro))
	for _, event := range cursorEvents {
		cmd := wrapperPrefix + shellQuoteIfNeeded(r.opts.BinaryPath) + " hook cursor " + event + r.configFlagSuffix()
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
	if err := writeJSONIndented(cursorDir, pinned, settings); err != nil {
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

// maxSettingsFileBytes caps how large a JSON settings/config file we are
// willing to read into memory before patching it. A real
// `settings.json` is kilobytes; anything past this is either a mistake
// (a log accidentally redirected onto the path) or hostile, and reading
// it unbounded would be a trivial memory-exhaustion foot-gun on a
// process the user runs on their own machine. Over-cap files are
// REFUSED (never truncated, never partially parsed) so we can't
// silently rewrite a file we only half-understood.
const maxSettingsFileBytes int64 = 5 << 20 // 5 MiB

// readSettingsFile reads a JSON settings/config file with the
// maxSettingsFileBytes guard applied via Stat BEFORE the read. Callers
// keep their own os.ErrNotExist handling — a missing file returns the
// wrapped fs.ErrNotExist from Stat, exactly as os.ReadFile would.
func readSettingsFile(path string) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Size() > maxSettingsFileBytes {
		return nil, fmt.Errorf("%s is %d bytes, over observer's %d-byte settings-file limit; refusing to read or modify it", path, fi.Size(), maxSettingsFileBytes)
	}
	return os.ReadFile(path)
}

// decodeSettingsObject decodes a settings/config file body into the
// top-level-key-preserving map every registrar patches, refusing two
// shapes that a plain json.Unmarshal accepts but that we cannot
// round-trip honestly:
//
//   - An explicit JSON `null`. Unmarshalling null into a map sets the
//     map to NIL (encoding/json's documented behaviour for maps,
//     slices, pointers and interfaces), so the very next
//     `settings[key] = ...` in a registrar PANICS with "assignment to
//     entry in nil map". Treating null as `{}` would be the other
//     obvious fix, but it isn't honest: an explicit null is not a
//     settings object, and silently replacing it with our own object
//     discards a deliberate (if odd) user statement about the file.
//     Refuse and name the file.
//   - Duplicate top-level keys. encoding/json keeps the LAST
//     occurrence, so a read-modify-write silently collapses the file
//     and destroys whichever duplicate the user's other tool was
//     reading. Detected by token-walking the top level (json.Decoder)
//     rather than by parsing into a map, which is exactly the
//     information json.Unmarshal throws away.
//
// Error text keeps the existing `parse <path>: <json error>` shape for
// genuinely malformed JSON so registrar error messages are unchanged.
func decodeSettingsObject(path string, raw []byte) (map[string]json.RawMessage, error) {
	settings := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &settings); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if settings == nil {
		return nil, fmt.Errorf("%s contains an explicit JSON null, not a settings object; refusing to modify it (replace its contents with {} or delete the file)", path)
	}
	if key, ok := duplicateTopLevelKey(raw); ok {
		return nil, fmt.Errorf("%s has a duplicate top-level %q key; refusing to modify it because rewriting would silently keep only the last one (de-duplicate the file by hand first)", path, key)
	}
	return settings, nil
}

// duplicateTopLevelKey token-walks the top-level object of raw and
// reports the first key that appears twice. Syntax errors are reported
// as "no duplicate" — decodeSettingsObject has already surfaced them
// through json.Unmarshal, and this helper's only job is the one thing
// Unmarshal cannot tell us.
func duplicateTopLevelKey(raw []byte) (string, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return "", false
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return "", false
	}
	seen := make(map[string]struct{})
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return "", false
		}
		key, ok := kt.(string)
		if !ok {
			return "", false
		}
		if _, dup := seen[key]; dup {
			return key, true
		}
		seen[key] = struct{}{}
		// Consume the value wholesale (any nesting) without decoding it.
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return "", false
		}
	}
	return "", false
}

// settingsLockSuffix is appended to a settings/config path to form its
// advisory lock file. Deliberately NOT ".lock" alone: the file sits
// beside the real config in a directory the AI tool also reads, so the
// name has to be unmistakably ours.
const settingsLockSuffix = ".observer-lock"

// settingsLockStale is how long a lock file may go untouched before a
// waiting process treats it as abandoned (a crashed observer) and
// breaks it. Registration is a sub-millisecond read-modify-write, so
// anything this old is dead by definition.
const settingsLockStale = 30 * time.Second

// settingsLockTimeout bounds how long we wait for another observer
// process to finish its read-modify-write before giving up with an
// error rather than racing it.
const settingsLockTimeout = 5 * time.Second

// lockSettings acquires the advisory lock for a settings/config file
// this Registry is about to read-modify-write, returning the releaser.
// Dry runs never write, so they never lock (and never leave a lock
// file behind in a directory a --dry-run must not touch).
func (r *Registry) lockSettings(path string) (func(), error) {
	if r.opts.DryRun {
		return func() {}, nil
	}
	return lockSettingsFile(path)
}

// lockSettingsFile takes a cross-process advisory lock on path by
// O_CREATE|O_EXCL-creating `<path>.observer-lock`, breaking locks older
// than settingsLockStale, and giving up after settingsLockTimeout. The
// O_EXCL sentinel (rather than flock/LockFileEx) is deliberate: it is
// one code path on every OS this ships to, with no syscall build tags,
// and matches internal/diag/lockfile.go's existing file-based
// convention.
//
// FAIL-OPEN: any error other than "already exists" (a read-only
// directory, an exotic filesystem that rejects O_EXCL) returns a no-op
// releaser and no error — registration proceeds unserialized rather
// than breaking on filesystems where the lock cannot be represented.
// Losing serialization is strictly better than losing the ability to
// register hooks at all.
//
// RESIDUAL, disclosed honestly: this serializes OBSERVER's own writers
// against each other (two `observer init` runs, an init racing a
// `observer start` auto-register). It cannot serialize us against
// Claude Code / Cursor themselves — they write settings.json without
// consulting any lock, so a genuinely concurrent edit by the AI tool
// can still be lost. Closing that would need the tool's cooperation;
// the atomic temp+rename below at least guarantees the file is never
// observed half-written.
func lockSettingsFile(path string) (func(), error) {
	lockPath := path + settingsLockSuffix
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return func() {}, nil // fail-open: see docstring
	}
	ownerID := lockOwnerToken()
	deadline := time.Now().Add(settingsLockTimeout)
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = f.WriteString(ownerID)
			_ = f.Close()
			return func() { unlockSettingsFile(lockPath, ownerID) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return func() {}, nil // fail-open: see docstring
		}
		// Held by someone else. Break it if it looks abandoned —
		// ownership-verified so a concurrent breaker can't win an ABA
		// race against a legitimate new owner (see breakStaleLock).
		// observeStaleLock pairs the mtime check and the content read
		// through ONE open file description, so what we hand to
		// breakStaleLock as "the specific lock instance we observed as
		// stale" is exactly that — not a fresh read taken later, which
		// would instead describe whatever happens to be at lockPath by
		// the time breakStaleLock runs (possibly a legitimate new
		// owner's lock, acquired in the gap between this check and the
		// break).
		if observed, stale := observeStaleLock(lockPath); stale {
			breakStaleLock(lockPath, observed)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out after %s waiting for %s (another observer process is writing %s; delete the lock file if no observer is running)", settingsLockTimeout, lockPath, path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// lockOwnerToken returns a fresh, unique owner identity for a
// newly-acquired settings lock, written verbatim as the lock file's
// entire body: "pid=<pid> nonce=<hex> acquired=<RFC3339Nano>\n". The
// pid+timestamp give a human inspecting a stray lock file the same
// debugging context the previous "pid=%d acquired=%s" body gave; the
// random nonce is what makes the returned string usable as a
// compare-and-delete identity (F7) — two different acquisitions of
// the SAME lock path, even by the same pid moments apart, get
// different identities, so "does the file still hold this exact
// string" is a reliable test for "is this still the lock instance I
// created/observed".
func lockOwnerToken() string {
	return fmt.Sprintf("pid=%d nonce=%s acquired=%s\n", os.Getpid(), randomHex(16), time.Now().UTC().Format(time.RFC3339Nano))
}

// randomHex returns n random bytes hex-encoded. crypto/rand.Read is
// documented to never fail on any platform Go supports; the
// time-derived fallback only exists so a theoretical read error can't
// turn a lock/quarantine helper into a panicking code path — it is
// still unique-enough for a same-process, single-call-site use to
// avoid a same-nanosecond collision.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// observeStaleLock pairs "is lockPath abandoned" with "what does it
// currently contain" into a single atomic observation, by checking
// mtime and reading content off the SAME open file description rather
// than a separate os.Stat + a later os.ReadFile against the pathname.
// Two syscalls issued against a pathname a moment apart can straddle
// a concurrent unlock+relock (the file at that path is a different
// inode by the second syscall); reading from the fd returned by Open
// pins us to the exact inode that mtime was measured against. The
// remaining pathname-level race — a different file existing at
// lockPath by the time Open runs than whatever failed our O_EXCL
// attempt moments earlier — is inherent to any check-then-act sequence
// over a path and is what breakStaleLock's rename-and-reverify closes.
func observeStaleLock(lockPath string) (content []byte, stale bool) {
	f, err := os.Open(lockPath)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || time.Since(fi.ModTime()) <= settingsLockStale {
		return nil, false
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, false
	}
	return data, true
}

// breakStaleLock attempts to remove a settings lock that
// observeStaleLock has already confirmed looks abandoned, using an
// ownership-verified rename-then-recheck protocol that closes F7's
// ABA race: two contenders observing the same stale lock must not
// both conclude they broke it, and neither may remove a lock that was
// legitimately (re-)acquired between the staleness observation and
// the break.
//
// observed is exactly what observeStaleLock captured — "the specific
// lock instance we observed as stale" — NOT a fresh read taken here;
// re-reading at break time would instead describe whatever happens to
// occupy lockPath by the time this function runs, which could by then
// be a brand new, legitimately-held lock.
//
//  1. Atomically rename lockPath to a unique quarantine path beside
//     it. Rename only succeeds if a source still exists at that
//     instant, so of any number of concurrent breakers racing the
//     same stale lock, AT MOST ONE wins the rename — everyone else
//     gets ENOENT and falls through having removed nothing (no
//     double-break, no partial state).
//  2. Read the quarantined file and compare its bytes to observed.
//     Equal confirms the rename moved the SAME lock instance we
//     originally observed as stale — lock files are write-once
//     (created via O_EXCL, never rewritten in place, only unlinked on
//     release), so identical content after an atomic rename can only
//     mean nothing replaced it between our observation and our
//     rename. Safe to discard for good.
//  3. Unequal (or the source was already gone) means either another
//     breaker won first, or the rename actually moved a DIFFERENT,
//     freshly (and legitimately) acquired lock — the process we
//     originally observed as stale must have cleanly unlocked
//     (identity-verified by unlockSettingsFile) right as we moved to
//     break it, and a new owner raced in before our rename executed.
//     We did not "consume" anything that was ours to take;
//     best-effort restore it — only when the path is free again,
//     never clobbering whatever a still-newer owner may have created
//     since — and log the near-miss rather than silently discarding a
//     live lock.
func breakStaleLock(lockPath string, observed []byte) {
	quarantine := fmt.Sprintf("%s.broken-%d-%s", lockPath, os.Getpid(), randomHex(8))
	if err := os.Rename(lockPath, quarantine); err != nil {
		// Lost the race: someone else's break (or a legitimate
		// unlock+recreate) already moved/removed the source first.
		return
	}
	current, readErr := os.ReadFile(quarantine)
	if readErr == nil && bytes.Equal(current, observed) {
		// Confirmed: this was genuinely the same stale-lock instance
		// we decided to break. Discard it for good.
		_ = os.Remove(quarantine)
		return
	}
	// We quarantined a lock we can't prove is the one we observed —
	// almost certainly a legitimate new owner that raced in between
	// our staleness read and our rename. Restore it if the path is
	// free again so that owner's eventual unlock still finds its own
	// lock; never overwrite whatever a still-newer owner has since
	// created there.
	if _, statErr := os.Lstat(lockPath); errors.Is(statErr, os.ErrNotExist) {
		if renameErr := os.Rename(quarantine, lockPath); renameErr == nil {
			slog.Default().Warn("hook: stale-lock break caught a live lock instead of an abandoned one; restored it", "lock_path", lockPath)
			return
		}
	}
	slog.Default().Warn("hook: stale-lock break caught a live lock instead of an abandoned one and could not restore it; its owner may need to retry", "lock_path", lockPath)
	_ = os.Remove(quarantine)
}

// unlockSettingsFile releases a lock previously acquired by
// lockSettingsFile, but only after verifying the lock file still
// holds the exact identity string this holder wrote at acquisition
// time — closing F7's second half. Without this, an unconditional
// os.Remove here would delete a SUCCESSOR's lock out from under it
// whenever this holder's own lock was (mistakenly or not) broken
// while still held — e.g. a slow read-modify-write that ran past
// settingsLockStale and got broken by a waiting contender, then
// finished its work and called unlock believing it still owned the
// file. A mismatch (or the file already being gone) means this holder
// no longer owns the lock: log and skip, never remove.
func unlockSettingsFile(lockPath, ownerID string) {
	current, err := os.ReadFile(lockPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Default().Warn("hook: could not verify lock ownership before unlock", "lock_path", lockPath, "error", err)
		}
		return
	}
	if string(current) != ownerID {
		slog.Default().Warn("hook: lock file no longer matches this holder's identity at unlock time; leaving it alone (owned by a successor)", "lock_path", lockPath)
		return
	}
	_ = os.Remove(lockPath)
}

// pinnedTarget captures, at a single instant right after a settings
// lock is acquired and BEFORE the file is read, which physical file a
// write to `path` will land on — closing F6's symlink-retarget race.
// Resolving the symlink target separately at write time (the old
// resolveWriteTarget, called fresh inside writeJSONIndented /
// atomicWriteFile) let a link retargeted between the read and the
// write silently redirect a patch built from target A's content onto
// target B. Callers now resolve ONCE via pinWriteTarget immediately
// after locking, thread the same pinnedTarget through the read and
// the write, and call verifyUnmoved() immediately before the final
// rename so a link that moved during the read/build window is caught
// and refused rather than clobbered.
type pinnedTarget struct {
	path      string // original path (possibly a symlink)
	target    string // resolved file to actually read/write; == path when not a symlink
	isSymlink bool
	linkValue string // os.Readlink(path) at pin time; empty when !isSymlink
}

// pinWriteTarget resolves path's write target once. Must be called
// while the caller holds path's advisory settings lock, before the
// first read of the file the caller is about to patch.
func pinWriteTarget(path string) pinnedTarget {
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return pinnedTarget{path: path, target: path}
	}
	link, err := os.Readlink(path)
	if err != nil {
		return pinnedTarget{path: path, target: path}
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved == "" {
		// Dangling link: same fallback the old resolveWriteTarget used
		// (write through the link path itself).
		return pinnedTarget{path: path, target: path, isSymlink: true, linkValue: link}
	}
	return pinnedTarget{path: path, target: resolved, isSymlink: true, linkValue: link}
}

// verifyUnmoved re-checks, immediately before the final rename, that
// path is still the same symlink (or still a plain file) it was when
// pinned. A mismatch means the link was retargeted (or a plain file
// was replaced with a symlink, or vice versa) while this write was in
// flight against the OLD target; refusing here is the only way to
// avoid silently overwriting a file the caller never read.
func (p pinnedTarget) verifyUnmoved() error {
	fi, err := os.Lstat(p.path)
	if err != nil {
		if p.isSymlink {
			return fmt.Errorf("%s: symlink disappeared while observer was preparing to write it; refusing to write (it may have been replaced)", p.path)
		}
		return nil
	}
	isLink := fi.Mode()&os.ModeSymlink != 0
	if isLink != p.isSymlink {
		return fmt.Errorf("%s: changed between read and write (symlink-ness changed); refusing to write — re-run to pick up the new state", p.path)
	}
	if !p.isSymlink {
		return nil
	}
	link, err := os.Readlink(p.path)
	if err != nil || link != p.linkValue {
		return fmt.Errorf("%s: symlink was retargeted while observer was preparing to write it (was -> %s); refusing to write a patch built against the old target", p.path, p.linkValue)
	}
	return nil
}

// writeJSONIndented writes a map[string]json.RawMessage as stable-keyed,
// 2-space-indented JSON. Creates the parent dir if missing.
//
// Callers own the advisory lock (see lockSettings) — this function must
// never take it itself, or a caller that already holds it would
// deadlock against its own O_EXCL sentinel.
//
// pinned must have been produced by pinWriteTarget against path, right
// after the lock was acquired and before path was first read — see
// pinnedTarget's doc comment for why (F6).
func writeJSONIndented(dir string, pinned pinnedTarget, settings map[string]json.RawMessage) error {
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
		// Re-indent the value for readability — on the RAW bytes, never
		// by decoding through `any`. Decoding a value we do not own and
		// re-marshalling it is lossy in at least three ways that all
		// silently corrupt a user's file: integers beyond 2^53 are
		// mangled through float64 (9007199254740993 -> ...992), number
		// formatting is rewritten (1.0 -> 1, 1e3 -> 1000), and `<`/`>`/`&`
		// inside strings get \u-escaped. json.Indent is a pure
		// whitespace transform, so every unrelated top-level key
		// round-trips byte-identically.
		//
		// One deliberate cosmetic consequence: nested object keys keep
		// their SOURCE order instead of being alphabetized by Go's map
		// marshalling. Semantics are identical; the blocks we write
		// ourselves ("hooks", "statusLine", ...) now serialize in
		// struct-field order.
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, settings[k], "  ", "  "); err == nil {
			buf = append(buf, pretty.Bytes()...)
		} else {
			buf = append(buf, settings[k]...)
		}
		if i < len(keys)-1 {
			buf = append(buf, ',')
		}
		buf = append(buf, '\n')
	}
	buf = append(buf, '}', '\n')

	// Write through a UNIQUE temp file, not a fixed `<path>.tmp`: two
	// observer processes patching the same settings.json would otherwise
	// stomp each other's half-written temp file and rename a spliced
	// body into place. Rename onto the symlink TARGET pinned at lock
	// time when path is a link, so dotfile-managed setups keep their
	// link.
	target := pinned.target
	tmpf, err := os.CreateTemp(filepath.Dir(target), filepath.Base(target)+".tmp-*")
	if err != nil {
		return fmt.Errorf("hook.write: temp: %w", err)
	}
	tmp := tmpf.Name()
	if _, err := tmpf.Write(buf); err != nil {
		_ = tmpf.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("hook.write: %w", err)
	}
	if err := tmpf.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("hook.write: %w", err)
	}
	// Re-verify immediately before the rename: the link must still
	// resolve to the target this write was built against (F6).
	if err := pinned.verifyUnmoved(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("hook.write: %w", err)
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("hook.write: rename: %w", err)
	}
	return nil
}

// atomicWriteFile writes data to path via a UNIQUE temp file — never a
// fixed `<path>.tmp`, which two observer processes racing to patch the
// same file would otherwise stomp: whichever opens second truncates
// the first's still-open temp file out from under it (same inode, same
// path, no O_EXCL), so the eventual rename can land a spliced/torn
// body. Same rationale as writeJSONIndented's temp file, generalized
// here so every fixed-name writer in this package (hook_checksums.json,
// Codex's hooks.json and config.toml) can share one hardened
// implementation instead of re-deriving it.
//
// Renames onto the SYMLINK TARGET pinned at lock time when path is a
// symlink (pinWriteTarget), so a dotfile-managed config keeps its
// link — matching writeJSONIndented's symlink stance.
//
// Callers that need cross-process serialization own the advisory lock
// (see lockSettings) — this function never takes it itself, so a
// caller already holding it can't deadlock against its own O_EXCL
// sentinel.
//
// pinned must have been produced by pinWriteTarget against path, right
// after the lock was acquired and before path was first read — see
// pinnedTarget's doc comment for why (F6).
func atomicWriteFile(pinned pinnedTarget, data []byte) error {
	target := pinned.target
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("hook.atomicWriteFile: mkdir: %w", err)
	}
	tmpf, err := os.CreateTemp(dir, filepath.Base(target)+".tmp-*")
	if err != nil {
		return fmt.Errorf("hook.atomicWriteFile: temp: %w", err)
	}
	tmp := tmpf.Name()
	if _, err := tmpf.Write(data); err != nil {
		_ = tmpf.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("hook.atomicWriteFile: write: %w", err)
	}
	if err := tmpf.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("hook.atomicWriteFile: close: %w", err)
	}
	// Re-verify immediately before the rename: the link must still
	// resolve to the target this write was built against (F6).
	if err := pinned.verifyUnmoved(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("hook.atomicWriteFile: %w", err)
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("hook.atomicWriteFile: rename: %w", err)
	}
	return nil
}

// removeEmptyConfigFile deletes path after a registrar has emptied a
// config file's last observer-owned content (its last top-level key,
// or the whole document). Honors the same symlink-preserving stance
// as writeJSONIndented/atomicWriteFile: a settings.json a dotfile
// manager maintains as a SYMLINK into a tracked repo must never be
// silently os.Remove'd. Unlinking the symlink only unlinks the
// dirent — the TARGET file (the one the AI tool, and any dotfile
// repo, actually reads) survives untouched with its stale content
// still in place, while the caller believes the removal succeeded.
// Deleting the resolved TARGET instead would be just as dishonest in
// the other direction: silently destroying a file this package does
// not own, tracked or not, on the operator's behalf.
//
// So a symlinked config is REFUSED with an error naming the file —
// the same "refuse and name it" stance readSettingsFile and
// decodeSettingsObject already take for file shapes this package
// cannot honestly rewrite. A missing file (already gone, or a race
// with another remover) is not an error.
//
// expected is the exact byte content the caller read and decided was
// empty of observer-owned content, captured BEFORE it made that
// decision. Closing F5's check-then-delete TOCTOU: the caller holds
// path's advisory settings lock across its whole
// read-decide-write-or-delete window, but that lock only serializes
// against OTHER observer processes — it does nothing against the AI
// tool itself (or the user, or a dotfile-manager sync) replacing the
// file with fresh content of its own between the caller's read and
// this delete. Re-reading path's current bytes here, immediately
// before the actual unlink, and refusing unless they still match
// exactly what the caller decided to delete turns that window into a
// safe no-op instead of a silent destructive race: whatever replaced
// the file survives, and the caller's stale delete decision is
// discarded instead of acted on.
func removeEmptyConfigFile(path string, expected []byte) error {
	fi, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; refusing to delete it — unlinking it would silently detach it while its target file survives untouched, and deleting the target could destroy a file tracked elsewhere; remove it by hand if that's what you want", path)
	}
	current, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if !bytes.Equal(current, expected) {
		return fmt.Errorf("%s changed since observer decided to delete it (likely written by the AI tool, or another process, in between); refusing to delete a file observer never actually read — re-run to make a fresh decision against the current content", path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// recordChecksum computes SHA256 of the config file and records it in the
// checksums registry so `observer doctor` can detect drift.
//
// hook_checksums.json is shared by every registrar (claude-code,
// cursor, codex, statusline, ...), so this takes its OWN advisory lock
// on the checksums file — a claude-code registration and a cursor
// registration each hold a DIFFERENT settings-file lock but must still
// serialize here, or their concurrent read-modify-write of the shared
// registry loses each other's entries (and, pre-atomic-write, could
// splice their bodies together).
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

	unlock, err := r.lockSettings(csPath)
	if err != nil {
		return fmt.Errorf("hook.recordChecksum: %w", err)
	}
	defer unlock()

	// Pin the write target now, before the read — see pinnedTarget (F6).
	pinned := pinWriteTarget(csPath)

	current := map[string]any{}
	if raw, err := os.ReadFile(csPath); err == nil {
		_ = json.Unmarshal(raw, &current)
	}
	current[path] = entry
	body, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return fmt.Errorf("hook.recordChecksum: marshal: %w", err)
	}
	if err := atomicWriteFile(pinned, body); err != nil {
		return fmt.Errorf("hook.recordChecksum: %w", err)
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

	unlock, err := r.lockSettings(path)
	if err != nil {
		res.Error = fmt.Errorf("hook.unregisterClaudeCode: %w", err)
		return res
	}
	defer unlock()

	// Pin the write target now, before the read — see pinnedTarget (F6).
	pinned := pinWriteTarget(path)

	raw, err := readSettingsFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			res.Skipped = true
			return res
		}
		res.Error = fmt.Errorf("hook.unregisterClaudeCode: read: %w", err)
		return res
	}

	settings, err := decodeSettingsObject(path, raw)
	if err != nil {
		res.Error = fmt.Errorf("hook.unregisterClaudeCode: %w", err)
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
		if err := removeEmptyConfigFile(path, raw); err != nil {
			res.Error = fmt.Errorf("hook.unregisterClaudeCode: remove empty %s: %w", path, err)
			return res
		}
	} else {
		if err := writeJSONIndented(settingsDir, pinned, settings); err != nil {
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

	unlock, err := r.lockSettings(res.ConfigPath)
	if err != nil {
		res.Error = fmt.Errorf("hook.unregisterClaudeCodeWindows: %w", err)
		return res
	}
	defer unlock()

	// Pin the write target now, before the read — see pinnedTarget (F6).
	pinned := pinWriteTarget(res.ConfigPath)

	raw, err := readSettingsFile(res.ConfigPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			res.Skipped = true
			return res
		}
		res.Error = fmt.Errorf("hook.unregisterClaudeCodeWindows: read: %w", err)
		return res
	}

	settings, err := decodeSettingsObject(res.ConfigPath, raw)
	if err != nil {
		res.Error = fmt.Errorf("hook.unregisterClaudeCodeWindows: %w", err)
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
		if err := removeEmptyConfigFile(res.ConfigPath, raw); err != nil {
			res.Error = fmt.Errorf("hook.unregisterClaudeCodeWindows: remove empty %s: %w", res.ConfigPath, err)
			return res
		}
	} else {
		if err := writeJSONIndented(claudeDir, pinned, settings); err != nil {
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

	unlock, err := r.lockSettings(path)
	if err != nil {
		res.Error = fmt.Errorf("hook.unregisterCursor: %w", err)
		return res
	}
	defer unlock()

	// Pin the write target now, before the read — see pinnedTarget (F6).
	pinned := pinWriteTarget(path)

	raw, err := readSettingsFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			res.Skipped = true
			return res
		}
		res.Error = fmt.Errorf("hook.unregisterCursor: read: %w", err)
		return res
	}

	settings, err := decodeSettingsObject(path, raw)
	if err != nil {
		res.Error = fmt.Errorf("hook.unregisterCursor: %w", err)
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
		if err := removeEmptyConfigFile(path, raw); err != nil {
			res.Error = fmt.Errorf("hook.unregisterCursor: remove %s: %w", path, err)
			return res
		}
	} else {
		if err := writeJSONIndented(cursorDir, pinned, settings); err != nil {
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
//
// Takes the same per-checksums-file advisory lock as recordChecksum —
// see that docstring for why a shared registry needs its own lock
// independent of whatever settings-file lock the caller may be
// holding.
func (r *Registry) removeChecksum(path string) error {
	csPath := r.opts.ChecksumsPath
	if csPath == "" {
		csPath = filepath.Join(r.opts.HomeDir, ".observer", "hook_checksums.json")
	}

	unlock, err := r.lockSettings(csPath)
	if err != nil {
		return fmt.Errorf("hook.removeChecksum: %w", err)
	}
	defer unlock()

	// Pin the write target now, before the read — see pinnedTarget (F6).
	pinned := pinWriteTarget(csPath)

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
	if err := atomicWriteFile(pinned, body); err != nil {
		return fmt.Errorf("hook.removeChecksum: %w", err)
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

	// Serialize observer's own writers of this file, THEN read — same
	// contract as registerClaudeCode/registerCursor (see lockSettings).
	// Codex's hooks.json previously took NO lock at all here.
	unlock, err := r.lockSettings(hooksPath)
	if err != nil {
		res.Error = fmt.Errorf("hook.registerCodex: %w", err)
		return res
	}
	defer unlock()

	// Pin the write target now, before the read — see pinnedTarget (F6).
	pinned := pinWriteTarget(hooksPath)

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

	if err := writeCodexHooks(dir, pinned, cfg); err != nil {
		res.Error = err
		return res
	}
	if err := r.recordChecksum(hooksPath); err != nil {
		res.Error = err
		return res
	}
	if err := r.ensureCodexHooksFeatureFlag(dir); err != nil {
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

	unlock, err := r.lockSettings(hooksPath)
	if err != nil {
		res.Error = fmt.Errorf("hook.unregisterCodex: %w", err)
		return res
	}
	defer unlock()

	// Pin the write target now, before the read — see pinnedTarget (F6).
	pinned := pinWriteTarget(hooksPath)

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
	if err := writeCodexHooks(dir, pinned, cfg); err != nil {
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
// event ordering. Creates the parent dir if missing. Callers own the
// advisory lock (see lockSettings) — this function must never take it
// itself, matching writeJSONIndented's contract.
//
// pinned must have been produced by pinWriteTarget against the hooks
// file, right after the lock was acquired and before it was first
// read — see pinnedTarget's doc comment for why (F6).
func writeCodexHooks(dir string, pinned pinnedTarget, cfg codexHooksConfig) error {
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

	if err := atomicWriteFile(pinned, buf.Bytes()); err != nil {
		return fmt.Errorf("hook.writeCodexHooks: %w", err)
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
// A method (not a free function) so it can take its own advisory lock
// on config.toml — a DIFFERENT file than the hooks.json lock
// registerCodex already holds for the rest of its body, so this is a
// second, independent lockSettings/unlock pair, not a re-entrant one.
func (r *Registry) ensureCodexHooksFeatureFlag(dir string) error {
	path := filepath.Join(dir, "config.toml")

	// Serialize observer's own writers of this file, THEN read — same
	// contract as every other settings/config writer since WP9
	// (lockSettings' docstring). config.toml previously took no lock
	// and wrote through a FIXED `<path>.tmp`, so two concurrent
	// `observer init --codex` runs could splice a torn config.toml.
	unlock, err := r.lockSettings(path)
	if err != nil {
		return fmt.Errorf("hook.ensureCodexHooksFeatureFlag: %w", err)
	}
	defer unlock()

	// Pin the write target now, before the read — see pinnedTarget (F6).
	pinned := pinWriteTarget(path)

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

	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(root); err != nil {
		return fmt.Errorf("hook.ensureCodexHooksFeatureFlag: encode: %w", err)
	}
	if err := atomicWriteFile(pinned, buf.Bytes()); err != nil {
		return fmt.Errorf("hook.ensureCodexHooksFeatureFlag: %w", err)
	}
	return nil
}

// --- Statusline registration (docs/plans/observer-statusline-plan-2026-07-30.md §5.1) ---
//
// This is a NEW, independent registration surface — a sibling of the hook
// registration above, not a variant of it. It patches a different
// top-level key ("statusLine") in the exact same ~/.claude/settings.json
// file that registerClaudeCode patches ("hooks"), using the identical
// read-preserve-write map[string]json.RawMessage shape so unknown keys
// (and the "hooks" key itself) survive byte-for-byte. registerClaudeCode
// and every other existing function in this file are NOT modified by
// this addition — only new sibling functions are added below, and the
// entry points (RegisterClaudeCodeStatusline / UnregisterClaudeCodeStatusline)
// are deliberately NOT wired into the tool-name-dispatched Register/
// Unregister switches above, so those two functions also stay untouched.
//
// v1 scope is claude-code (native OS) ONLY — see
// runStatuslineInit's doc comment in cmd/observer/init.go for the
// cross-OS (WSL bridge) decision: a Windows-side Claude Code reached
// only via crossmount is deliberately NOT bridged here (unlike hooks,
// a statusLine command runs on every render tick, and a wsl.exe
// subprocess spawn per tick risks the plan's <100ms budget); the
// caller WARNs instead of silently doing nothing.

// claudeStatuslineEntry is the shape written into settings.json's
// top-level "statusLine" key: `{"type":"command","command":"<bin>
// statusline","padding":0}`. Distinct from claudeHookGroup/
// claudeHookCommand — the "statusLine" key is never merged with
// "hooks" (CLAUDE.md #4: one owner per piece of state; this struct
// owns exactly the "statusLine" value, nothing else in the file).
type claudeStatuslineEntry struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Padding int    `json:"padding"`
}

// isObserverStatuslineEntry recognises a "statusLine" command as one
// previously written by ANY observer registrar. Unlike
// isObserverClaudeEntry — whose ` hook claude-code ` signature is
// observer-internal syntax nobody else writes — "statusline" is a
// GENERIC word other statusline tools use as a subcommand, so a bare
// substring test is not a safe ownership signal: `node /opt/acme
// statusline --theme compact` would have been silently overwritten by
// register (or uninstalled by unregister) as if it were ours.
//
// Ownership therefore requires BOTH halves of the command shape we
// actually write (`<quoted-bin-path> statusline`):
//
//  1. the executable token's basename is an observer binary
//     (isObserverBinaryToken), and
//  2. the very next argument is exactly "statusline".
//
// Anything else — a foreign executable, "statusline" appearing only as
// a later flag value, an unparseable command — is FOREIGN, which means
// register blocks without --force and unregister leaves it alone.
// Trailing arguments after "statusline" are tolerated so a future
// flag-carrying variant of our own command still refreshes rather than
// tripping the conflict guard.
func isObserverStatuslineEntry(cmd string) bool {
	toks := splitCommandTokens(cmd)
	if len(toks) < 2 || toks[1] != "statusline" {
		return false
	}
	return isObserverBinaryToken(toks[0])
}

// isObserverBinaryToken reports whether an executable token names an
// observer binary, by basename. Accepts the plain names (`observer`,
// `superbased`, with or without a `.exe` tail) plus the
// `observer-<something>` / `superbased-<something>` family, because
// real installs DO carry suffixed names (`observer-v1.8.3` from a
// versioned download, `observer-hermes.exe`, a `/tmp/observer-A` A/B
// build) and a path-shape-strict test would misclassify our own
// earlier registration as foreign — the exact failure mode
// isObserverClaudeEntry's content-heuristic exists to avoid (a stale
// entry that can no longer be refreshed without --force).
// Case-insensitive: Windows paths are.
func isObserverBinaryToken(tok string) bool {
	base := strings.ToLower(commandBaseName(tok))
	base = strings.TrimSuffix(base, ".exe")
	if base == "observer" || base == "superbased" {
		return true
	}
	return strings.HasPrefix(base, "observer-") || strings.HasPrefix(base, "superbased-")
}

// commandBaseName returns the final path element of an executable
// token, splitting on EITHER separator — the token may be a
// forward-slash-normalized Windows path (see forwardSlashPath) or a
// native backslash one, and filepath.Base only knows the host's
// separator.
func commandBaseName(tok string) string {
	if i := strings.LastIndexAny(tok, `/\`); i >= 0 {
		return tok[i+1:]
	}
	return tok
}

// splitCommandTokens splits a registered hook/statusline command string
// into argv-ish tokens, honouring the quoting our own writers emit
// (shellQuote's POSIX single quotes, including the close-escape-reopen
// form it emits for an embedded quote character — see shellQuote) plus
// double quotes and backslash escapes for
// hand-written entries. Deliberately small: it exists to answer "what
// is the executable, and what is its first argument", not to be a
// shell. Unbalanced quotes simply yield whatever tokens were closed,
// which the callers treat as ambiguous ⇒ foreign.
func splitCommandTokens(cmd string) []string {
	var (
		toks    []string
		cur     []byte
		started bool
		inSing  bool
		inDoub  bool
	)
	flush := func() {
		if started {
			toks = append(toks, string(cur))
			cur = cur[:0]
			started = false
		}
	}
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case inSing:
			if c == '\'' {
				inSing = false
				continue
			}
			cur = append(cur, c)
		case inDoub:
			if c == '"' {
				inDoub = false
				continue
			}
			if c == '\\' && i+1 < len(cmd) {
				i++
				cur = append(cur, cmd[i])
				continue
			}
			cur = append(cur, c)
		case c == '\'':
			inSing, started = true, true
		case c == '"':
			inDoub, started = true, true
		case c == '\\' && i+1 < len(cmd):
			i++
			cur = append(cur, cmd[i])
			started = true
		case c == ' ' || c == '\t':
			flush()
		default:
			cur = append(cur, c)
			started = true
		}
	}
	flush()
	return toks
}

// registerClaudeCodeStatusline installs (or refreshes) the
// "statusLine" top-level key in ~/.claude/settings.json, wired to
// `<binary> statusline`. Conflict discipline mirrors
// hasConflictingClaudeHook: a foreign (non-observer) existing value
// blocks without --force — more conservative here than for hooks,
// because "statusLine" is a single, visible UI slot (unlike hooks,
// which fan out across many named events with no single-slot
// contention), so clobbering a user's own statusline silently would
// be a materially worse experience than clobbering one hook event.
// An observer-recognised entry (by isObserverStatuslineEntry)
// refreshes silently on drift (binary path moved, quoting changed).
// An entry that already matches byte-for-byte is reported via
// AlreadySet and the file is NOT rewritten at all (idempotent
// re-run — no needless mtime/checksum churn), mirroring how
// registerClaudeCode's observerCmdMatches short-circuits per event.
func (r *Registry) registerClaudeCodeStatusline() RegistrationResult {
	res := RegistrationResult{Tool: "claude-code-statusline", DryRun: r.opts.DryRun}
	settingsDir := filepath.Join(r.opts.HomeDir, ".claude")
	path := filepath.Join(settingsDir, "settings.json")
	res.ConfigPath = path

	unlock, err := r.lockSettings(path)
	if err != nil {
		res.Error = fmt.Errorf("hook.registerClaudeCodeStatusline: %w", err)
		return res
	}
	defer unlock()

	// Pin the write target now, before the read — see pinnedTarget (F6).
	pinned := pinWriteTarget(path)

	raw, err := readSettingsFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		res.Error = fmt.Errorf("hook.registerClaudeCodeStatusline: read: %w", err)
		return res
	}
	// Preserve unknown top-level fields (including "hooks") via
	// map[string]json.RawMessage — identical shape to registerClaudeCode.
	settings := map[string]json.RawMessage{}
	if len(raw) > 0 {
		settings, err = decodeSettingsObject(path, raw)
		if err != nil {
			res.Error = fmt.Errorf("hook.registerClaudeCodeStatusline: %w", err)
			return res
		}
	}

	// Same forward-slash + shell-quote binary-path handling the hook
	// registrars use — see forwardSlashPath's docstring for the Git
	// Bash / harness-wrapper rationale.
	binPath := forwardSlashPath(r.opts.BinaryPath)
	desired := claudeStatuslineEntry{
		Type:    "command",
		Command: shellQuoteIfNeeded(binPath) + " statusline",
		Padding: 0,
	}

	if existing, ok := settings["statusLine"]; ok {
		var cur claudeStatuslineEntry
		parsed := json.Unmarshal(existing, &cur) == nil
		switch {
		case parsed && cur == desired:
			res.AlreadySet = append(res.AlreadySet, "statusLine")
			return res
		case parsed && isObserverStatuslineEntry(cur.Command):
			// Observer-owned but stale (binary path/quoting drifted) —
			// silently refresh; fall through to overwrite below.
		case r.opts.Force:
			// Foreign (or unparseable) value, but the operator passed
			// --force — fall through to overwrite below.
		default:
			shape := string(existing)
			if parsed {
				shape = cur.Command
			}
			res.Error = fmt.Errorf("hook.registerClaudeCodeStatusline: %s already has a non-observer \"statusLine\" command %q; pass --force to overwrite", path, shape)
			return res
		}
	}

	patched, err := json.Marshal(desired)
	if err != nil {
		res.Error = fmt.Errorf("hook.registerClaudeCodeStatusline: marshal: %w", err)
		return res
	}
	settings["statusLine"] = patched
	res.HooksAdded = append(res.HooksAdded, "statusLine")

	if r.opts.DryRun {
		return res
	}
	if err := writeJSONIndented(settingsDir, pinned, settings); err != nil {
		res.Error = err
		return res
	}
	if err := r.recordChecksum(path); err != nil {
		res.Error = err
		return res
	}
	return res
}

// unregisterClaudeCodeStatusline removes the "statusLine" top-level
// key from ~/.claude/settings.json ENTIRELY (not blanked) — so a user
// who opts out gets Claude Code's own default statusline back, not an
// empty one. Only an entry recognised as observer-written (via
// isObserverStatuslineEntry) is ever removed; a foreign/user-authored
// "statusLine" is left untouched and reported via HooksKept, mirroring
// how unregisterClaudeCode preserves non-observer hook entries without
// requiring --force to do so (Force here gates ONLY the checksum-drift
// guard below, exactly like the hook unregistrars).
func (r *Registry) unregisterClaudeCodeStatusline() UnregistrationResult {
	res := UnregistrationResult{Tool: "claude-code-statusline", DryRun: r.opts.DryRun}
	settingsDir := filepath.Join(r.opts.HomeDir, ".claude")
	path := filepath.Join(settingsDir, "settings.json")
	res.ConfigPath = path

	unlock, err := r.lockSettings(path)
	if err != nil {
		res.Error = fmt.Errorf("hook.unregisterClaudeCodeStatusline: %w", err)
		return res
	}
	defer unlock()

	// Pin the write target now, before the read — see pinnedTarget (F6).
	pinned := pinWriteTarget(path)

	raw, err := readSettingsFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			res.Skipped = true
			return res
		}
		res.Error = fmt.Errorf("hook.unregisterClaudeCodeStatusline: read: %w", err)
		return res
	}

	settings, err := decodeSettingsObject(path, raw)
	if err != nil {
		res.Error = fmt.Errorf("hook.unregisterClaudeCodeStatusline: %w", err)
		return res
	}

	existing, ok := settings["statusLine"]
	if !ok {
		res.Skipped = true
		return res
	}
	var cur claudeStatuslineEntry
	if err := json.Unmarshal(existing, &cur); err != nil || !isObserverStatuslineEntry(cur.Command) {
		// Foreign (or unparseable) "statusLine" — a user-authored
		// statusline (or another tool's). Never removed by uninstall;
		// no --force override for this case (only for checksum drift
		// below), mirroring the hook unregistrars' HooksKept posture.
		res.HooksKept = append(res.HooksKept, "statusLine")
		res.Skipped = true
		return res
	}

	// There is real work to do — verify the file hasn't drifted since
	// we installed, so we don't clobber user edits made to OTHER keys
	// in the same file. Passing --force bypasses the guard. Note the
	// checksum registry tracks this whole FILE (not the "statusLine"
	// key specifically) — the same coarse, file-granularity contract
	// registerClaudeCode's recordChecksum already uses for "hooks".
	match, err := r.checksumMatches(path, raw)
	if err != nil {
		res.Error = fmt.Errorf("hook.unregisterClaudeCodeStatusline: checksum: %w", err)
		return res
	}
	res.ChecksumMatch = match
	if !match && !r.opts.Force {
		res.Error = fmt.Errorf("hook.unregisterClaudeCodeStatusline: %s has been modified since install (checksum mismatch); pass --force to remove anyway", path)
		return res
	}

	delete(settings, "statusLine")
	res.HooksRemoved = append(res.HooksRemoved, "statusLine")

	if r.opts.DryRun {
		return res
	}

	if len(settings) == 0 {
		if err := removeEmptyConfigFile(path, raw); err != nil {
			res.Error = fmt.Errorf("hook.unregisterClaudeCodeStatusline: remove empty %s: %w", path, err)
			return res
		}
	} else {
		if err := writeJSONIndented(settingsDir, pinned, settings); err != nil {
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

// RegisterClaudeCodeStatusline is the exported entry point for the
// statusline registration path. Deliberately NOT reached through the
// tool-name-dispatched Register(tool string) switch above (adding a
// case there would touch an existing function this WP must leave
// byte-identical) — callers (cmd/observer/init.go's `observer init
// --statusline`) call this directly instead. Opt-in only: never
// called by `observer start`'s hook auto-register path, matching the
// MCP/proxy-route precedent (CLAUDE.md's `observer start` invariant).
func (r *Registry) RegisterClaudeCodeStatusline() RegistrationResult {
	return r.registerClaudeCodeStatusline()
}

// UnregisterClaudeCodeStatusline is the exported entry point for
// removing the statusline registration — the `observer init
// --statusline --uninstall` path. See RegisterClaudeCodeStatusline's
// doc comment for why this is a direct call rather than a Unregister
// switch case.
func (r *Registry) UnregisterClaudeCodeStatusline() UnregistrationResult {
	return r.unregisterClaudeCodeStatusline()
}
