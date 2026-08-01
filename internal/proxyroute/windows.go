package proxyroute

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
)

// isWSL is the WSL-detection seam. It is a package var (not a direct
// crossmount.IsWSL call) so a test can force the WSL / not-WSL branch without a
// real /proc probe (restore it in a defer). F4: the cross-OS localhost route
// depends on WSL2 localhost forwarding, so a native Linux box that merely has a
// disk mounted at /mnt/c/Users must NEVER get localhost routes written.
var isWSL = crossmount.IsWSL

// SetWSLForTest overrides the WSL-detection gate and returns a restore func the
// caller defers. It exists so tests in OTHER packages (cmd/observer's init-wire
// test) can drive the cross-OS Windows writers deterministically on any host —
// pinning them ON where a native CI box would otherwise skip. Test-only by
// contract; production code never calls it. Mirrors the in-package isWSL seam
// the proxyroute tests swap directly.
func SetWSLForTest(v bool) func() {
	prev := isWSL
	isWSL = func() bool { return v }
	return func() { isWSL = prev }
}

// allHomes is the crossmount-home enumeration seam, a package var for the same
// reason as isWSL: a test can inject a fixed multi-home layout to exercise the
// F3 ambiguity branch without a real /mnt/c mount (restore it in a defer).
var allHomes = crossmount.AllHomes

// windowsUserName resolves the CURRENT Windows user's login name via WSL
// interop. R1: a WSL daemon must NOT auto-rewrite an arbitrary Windows-side
// home just because it is the only one mounted — the daemon running as
// WSL-Bob would happily rewrite /mnt/c/Users/Alice/.claude if Alice's is the
// only detected `.claude`. resolveWindowsHome accepts an auto-detected home
// only when its user-home base matches this name (case-insensitive), proving
// ownership. An unknown name ("") is treated as "ownership unverifiable" and
// the route is refused rather than guessed.
//
// The primitive itself now lives in internal/platform/crossmount
// (crossmount.WindowsUserName — memoized, WSL-gated, the ONE owner shared by
// this proxy-route path AND the hook registrar's parallel Windows detection).
// This stays a package var (like isWSL / allHomes) so the existing proxyroute
// tests can swap it for a deterministic identity without shelling out (restore
// it in a defer).
var windowsUserName = crossmount.WindowsUserName

// Cross-OS proxy-route writers.
//
// When the observer daemon runs inside WSL2 but the operator installed
// Claude Code / Codex from Windows, the tool's config lives in a
// Windows-side home (e.g. /mnt/c/Users/<u>/.claude), and pointing it at
// the loopback 127.0.0.1:<port> the native writers use does NOT reach
// the WSL proxy — 127.0.0.1 on the Windows side is the Windows loopback,
// a different network namespace. WSL2's default NAT networking DOES
// forward Windows-side `localhost` to the WSL guest's listeners
// (localhostForwarding, on by default), so the cross-OS route writes
// `http://localhost:<port>` instead of the loopback IP.
//
// Caveats (surfaced honestly by the doctor check in
// internal/diag/windowsroute.go, never assumed here):
//   - `.wslconfig` [wsl2] localhostForwarding=false disables the
//     forwarding — the Windows tool then can't reach the proxy.
//   - Mirrored networking mode (Windows 11 22H2+) changes the model but
//     still makes localhost reach the guest; only an explicitly firewalled
//     setup breaks it.
//
// The write itself is a plain settings.json / config.toml file write over
// the DrvFs 9p mount, which is safe (unlike SQLite over DrvFs, which fails
// with "disk I/O error (10)"): these are small text files written with the
// same temp-file+rename dance the native writers use. That is why the
// route is solved at the config-write layer, not by any storage bridge.

// windowsBaseURL is the base URL the cross-OS writers point a Windows-side
// tool at. Unlike the native loopback URL (claudeBaseURL / codexBaseURL),
// it uses `localhost` so WSL2 NAT localhost forwarding routes the request
// from the Windows host into the WSL guest's proxy listener. Codex appends
// the "/v1" segment its OpenAI-compatible wire needs (mirrors the native
// codexBaseURL shape, host-swapped).
func windowsBaseURL(port int) string {
	return fmt.Sprintf("http://localhost:%d", port)
}

// windowsHomesFor returns EVERY Windows-side home directory that holds subdir
// (".claude" or ".codex") for a cross-OS route write. override-first: a
// non-empty override is authoritative — returned as the single candidate joined
// with subdir even if it doesn't exist yet (the writer mkdir's on first
// install), mirroring hook.detectWindowsClaudeHome. Otherwise it walks
// crossmount.AllHomes() and collects ALL OS==windows homes that already carry
// subdir. F3: a multi-user Windows machine can expose several
// /mnt/c/Users/<u>/.claude — collecting all of them lets the caller refuse to
// auto-pick rather than silently rewrite another user's config.
func windowsHomesFor(override, subdir string) []string {
	if override != "" {
		return []string{filepath.Join(override, subdir)}
	}
	var out []string
	for _, h := range allHomes() {
		if h.OS != crossmount.OSWindows {
			continue
		}
		dir := filepath.Join(h.Path, subdir)
		if dirIsPresent(dir) {
			out = append(out, dir)
		}
	}
	return out
}

// resolveWindowsHome classifies the Windows-side homes for subdir into a single
// unambiguous target OWNED by the current operator, or an honest reason it can't
// be resolved:
//   - dir != "" — an explicit override forced one, OR exactly one auto-detected
//     home whose ownership was PROVEN (its user-home base matches the current
//     Windows username): use it.
//   - dir == "" and refuse != nil — one or more homes carry subdir but none can
//     be safely picked: the caller must refuse and name the override option.
//     This covers F3 (several homes, no override to disambiguate) AND R1
//     (ownership couldn't be verified — the username is unknown because interop
//     is off, or no candidate's base matches it). Even a SINGLE candidate is
//     refused when ownership is unproven, because auto-rewriting another
//     Windows user's config is exactly the danger R1 closes.
//   - dir == "" and refuse == nil — NONE found: the caller reports "not
//     detected".
//
// An explicit override wins unconditionally (the operator named the home, so
// ownership verification is moot). Auto-detection is trusted only after the
// R1 ownership check.
func resolveWindowsHome(override, subdir string) (dir string, refuse []string) {
	// Explicit override is authoritative — skip the ownership check entirely.
	if override != "" {
		return filepath.Join(override, subdir), nil
	}
	homes := windowsHomesFor("", subdir)
	if len(homes) == 0 {
		return "", nil // none detected
	}
	// R1: accept an auto-detected home only when it demonstrably belongs to
	// the operator running the daemon. Each candidate dir is `<home>/<subdir>`,
	// so filepath.Base(filepath.Dir(dir)) recovers the user-home leaf (the
	// Windows username) to compare against the interop-resolved current user.
	// Unknown user ("" — interop off) or no unique match ⇒ refuse and list the
	// candidates so the operator disambiguates via the override.
	user := windowsUserName()
	if user != "" {
		var owned []string
		for _, d := range homes {
			if strings.EqualFold(filepath.Base(filepath.Dir(d)), user) {
				owned = append(owned, d)
			}
		}
		if len(owned) == 1 {
			return owned[0], nil
		}
	}
	return "", homes
}

// resolveWindowsHomeFor is the Registrar-scoped resolveWindowsHome: it applies
// the SANDBOX GATE before any crossmount walk, then delegates.
//
// The gate (crossmount.AutoDetectSuppressed, incident 2026-07-31): a caller
// that pinned RegisterOptions.HomeDir and did NOT name this target's Windows
// home gets NO auto-detection — it resolves to "not detected" (dir "", no
// refusal candidates) so the cross-OS writers skip instead of silently
// resolving the REAL /mnt/c/Users/<u>/.claude. Production never pins HomeDir,
// so every real `observer init` run takes the delegate path unchanged.
//
// EVERY caller of resolveWindowsHome inside this Registrar must go through
// here — that is what makes the hazard structural rather than per-call
// discipline.
func (r *Registrar) resolveWindowsHomeFor(override, subdir string) (dir string, refuse []string) {
	if crossmount.AutoDetectSuppressed(r.homeOverride, override) {
		return "", nil
	}
	return resolveWindowsHome(override, subdir)
}

// sandboxRouteSkip fills a benign, explicit skip result for a cross-OS route
// write the sandbox gate suppressed: the caller pinned HomeDir and either
// named no Windows-side home, or named one OUTSIDE that sandbox. optionName is
// the RegisterOptions field that unlocks the target; its value must resolve
// INSIDE the pinned home.
func (r *Registrar) sandboxRouteSkip(tool, subdir, optionName, override string) RegistrationResult {
	reason := fmt.Sprintf(
		"HomeDir was pinned by the caller but no %s was given — cross-OS %s/ resolution is suppressed (incident 2026-07-31); set %s to a home UNDER the pinned HomeDir to wire this target",
		optionName, subdir, optionName,
	)
	if override != "" {
		reason = fmt.Sprintf(
			"%s (%s) resolves OUTSIDE the pinned HomeDir (%s) — cross-OS %s/ resolution is suppressed (incident 2026-07-31)",
			optionName, override, r.homeOverride, subdir,
		)
	}
	return RegistrationResult{
		Tool:          tool,
		DryRun:        r.opts.DryRun,
		ConfigMissing: true,
		SkipReason:    reason,
	}
}

// windowsRouteRefusalError builds the honest refusal error for a cross-OS route
// write that could not resolve a single OWNED Windows home. It distinguishes
// two shapes so the message is precise rather than falsely implying plurality:
//
//   - exactly ONE candidate (len(refuse)==1): a single Windows-side <subdir>/
//     home was found, but ownership could not be verified (its user-home leaf
//     didn't match %USERNAME%, or interop is unavailable so the username is
//     unknown). Name the exact home found and the flag that confirms it.
//   - SEVERAL candidates: multiple homes carry the config and none could be
//     verified as the operator's — refuse to auto-pick and name the flag.
//
// fn is the caller for the wrapped-error prefix, subdir the config dir
// (".claude"/".codex"), flag the `observer init` flag that disambiguates, and
// optionName the registrar option the flag feeds (kept in the multi message so
// callers that key off the option name still match).
func windowsRouteRefusalError(fn, subdir, flag, optionName string, refuse []string) error {
	if len(refuse) == 1 {
		return fmt.Errorf(
			"%s: found %s but could not verify it belongs to the current Windows user (%%USERNAME%% mismatch or interop unavailable) — pass %s to confirm",
			fn, refuse[0], flag,
		)
	}
	return fmt.Errorf(
		"%s: multiple Windows-side %s/ homes were detected (%s) but none could be verified as the current Windows user's — refusing to auto-pick; pass %s (%s) to choose",
		fn, subdir, strings.Join(refuse, ", "), flag, optionName,
	)
}

// dirIsPresent reports whether p exists and is a directory.
func dirIsPresent(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// RegisterClaudeCodeWindows writes env.ANTHROPIC_BASE_URL into a
// Windows-side .claude/settings.json (detected via crossmount, or the
// WindowsClaudeHome override) pointing Claude Code at the WSL proxy over
// localhost forwarding. Delegates to registerClaudeCodeAt so behaviour is
// identical to the native writer except for the target dir, the localhost
// URL, and the "claude-code-windows" tool label. Errors (Error set) when
// no Windows-side .claude/ is detected.
func (r *Registrar) RegisterClaudeCodeWindows() RegistrationResult {
	if !isWSL() {
		// F4: no WSL2 localhost forwarding off WSL, so a localhost route could
		// never reach the guest proxy. Benign skip (nothing to route), not an
		// error — mirrors the ConfigMissing skip signal `observer init` iterates
		// over.
		return RegistrationResult{Tool: "claude-code-windows", DryRun: r.opts.DryRun, ConfigMissing: true}
	}
	if crossmount.AutoDetectSuppressed(r.homeOverride, r.opts.WindowsClaudeHome) {
		return r.sandboxRouteSkip("claude-code-windows", ".claude", "WindowsClaudeHome", r.opts.WindowsClaudeHome)
	}
	dir, refuse := r.resolveWindowsHomeFor(r.opts.WindowsClaudeHome, ".claude")
	if len(refuse) > 0 {
		return RegistrationResult{
			Tool:   "claude-code-windows",
			DryRun: r.opts.DryRun,
			Error: windowsRouteRefusalError(
				"proxyroute.RegisterClaudeCodeWindows", ".claude",
				"--windows-claude-home", "WindowsClaudeHome", refuse,
			),
		}
	}
	if dir == "" {
		return RegistrationResult{
			Tool:   "claude-code-windows",
			DryRun: r.opts.DryRun,
			Error:  errors.New("proxyroute.RegisterClaudeCodeWindows: no Windows-side .claude/ detected (set WindowsClaudeHome or run in WSL where crossmount sees /mnt/c/Users/<u>/.claude/)"),
		}
	}
	return r.registerClaudeCodeAt(dir, windowsBaseURL(r.opts.ProxyPort), "claude-code-windows")
}

// RegisterCodexWindows writes the custom model provider + base_url into a
// Windows-side .codex/config.toml (detected via crossmount, or the
// WindowsCodexHome override) pointing Codex at the WSL proxy over
// localhost forwarding. Delegates to registerCodexAt; the localhost URL
// carries the same "/v1" segment the native codexBaseURL uses. Errors
// (Error set) when no Windows-side .codex/ is detected.
func (r *Registrar) RegisterCodexWindows() RegistrationResult {
	if !isWSL() {
		// F4: benign skip off WSL (see RegisterClaudeCodeWindows).
		return RegistrationResult{Tool: "codex-windows", DryRun: r.opts.DryRun, ConfigMissing: true}
	}
	if crossmount.AutoDetectSuppressed(r.homeOverride, r.opts.WindowsCodexHome) {
		return r.sandboxRouteSkip("codex-windows", ".codex", "WindowsCodexHome", r.opts.WindowsCodexHome)
	}
	dir, refuse := r.resolveWindowsHomeFor(r.opts.WindowsCodexHome, ".codex")
	if len(refuse) > 0 {
		return RegistrationResult{
			Tool:   "codex-windows",
			DryRun: r.opts.DryRun,
			Error: windowsRouteRefusalError(
				"proxyroute.RegisterCodexWindows", ".codex",
				"--windows-codex-home", "WindowsCodexHome", refuse,
			),
		}
	}
	if dir == "" {
		return RegistrationResult{
			Tool:   "codex-windows",
			DryRun: r.opts.DryRun,
			Error:  errors.New("proxyroute.RegisterCodexWindows: no Windows-side .codex/ detected (set WindowsCodexHome or run in WSL where crossmount sees /mnt/c/Users/<u>/.codex/)"),
		}
	}
	return r.registerCodexAt(dir, windowsBaseURL(r.opts.ProxyPort)+"/v1", "codex-windows")
}

// WindowsRouteTargets returns the `<tool>-windows` virtual targets whose
// Windows-side config dir is present AND unambiguous (exactly one home, or one
// forced via an override), so `observer init` can union them into its
// installed-tools set the same way hook.Registry.Installed surfaces
// "claude-code-windows". Empty on a native (non-cross-OS) host (F4: gated on
// isWSL). An AMBIGUOUS tool — more than one Windows home carrying its config —
// is EXCLUDED (F3): batch init must not offer a target it would refuse to
// auto-pick; the operator disambiguates via WindowsClaudeHome / WindowsCodexHome
// (the doctor check WARNs with the candidates). Order is stable:
// claude-code-windows then codex-windows.
func (r *Registrar) WindowsRouteTargets() []string {
	if !isWSL() {
		return nil
	}
	var out []string
	if dir, _ := r.resolveWindowsHomeFor(r.opts.WindowsClaudeHome, ".claude"); dir != "" {
		out = append(out, "claude-code-windows")
	}
	if dir, _ := r.resolveWindowsHomeFor(r.opts.WindowsCodexHome, ".codex"); dir != "" {
		out = append(out, "codex-windows")
	}
	return out
}

// WindowsRouteCandidates reports, per cross-OS route tool label, the Windows
// USER-home directories that were DETECTED but could NOT be auto-resolved —
// either several homes carry the config (F3) or ownership could not be proven
// against the current Windows user (R1). It is the disambiguation counterpart
// to WindowsRouteTargets (which lists only the cleanly-resolved targets): an
// interactive `observer init` presents these as a numbered picker and feeds
// the chosen home back as the WindowsClaudeHome / WindowsCodexHome override.
//
// Keyed by "claude-code-windows" / "codex-windows"; a label is ABSENT when its
// route resolved cleanly (a single owned home, or an override) or nothing was
// detected. Empty on a native (non-WSL) host (gated on isWSL, like
// WindowsRouteTargets). The returned paths are USER homes — the exact value an
// override wants — not the ".claude"/".codex" config dir the registrar
// appends.
func (r *Registrar) WindowsRouteCandidates() map[string][]string {
	if !isWSL() {
		return nil
	}
	out := map[string][]string{}
	add := func(label, subdir, override string) {
		dir, refuse := r.resolveWindowsHomeFor(override, subdir)
		if dir != "" || len(refuse) == 0 {
			return // resolved cleanly, or nothing detected — no disambiguation needed
		}
		homes := make([]string, 0, len(refuse))
		for _, d := range refuse {
			homes = append(homes, filepath.Dir(d)) // <home>/<subdir> → <home>
		}
		out[label] = homes
	}
	add("claude-code-windows", ".claude", r.opts.WindowsClaudeHome)
	add("codex-windows", ".codex", r.opts.WindowsCodexHome)
	if len(out) == 0 {
		return nil
	}
	return out
}
