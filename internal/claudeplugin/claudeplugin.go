package claudeplugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Plugin identity. These constants are the ONE owner of how the
// SuperBased Claude Code plugin names itself: plugins/plugingen stamps
// the generated manifests from them, and the detectors below look for
// the same strings on disk. Changing Name breaks every existing install
// (Claude Code keys enabledPlugins on it), so it is append-only history
// in practice — see the `renames` mechanism in
// code.claude.com/docs/en/plugin-marketplaces.
//
// The 2026-07-31 rename from `superbased-observer` to `superbased` was
// taken while NOTHING was published: no marketplace existed, so no
// enabledPlugins key and no cache directory carrying the old name could
// exist on any user's disk, and the rename cost exactly zero compat
// debt. That window is CLOSED the moment the public catalog ships.
const (
	// Name is the plugin's stable identifier — the `name` field of
	// .claude-plugin/plugin.json and of the marketplace entry, and the
	// left half of an enabledPlugins key.
	Name = "superbased"

	// Marketplace is the catalog name users type after the `@` in
	// `/plugin install <plugin>@<marketplace>`. It deliberately equals
	// Name: one brand, one catalog, one plugin — so the install line is
	// `/plugin install superbased@superbased`.
	Marketplace = "superbased"

	// Dir is the plugin's directory name inside the marketplace root.
	// A marketplace entry's relative source resolves against that root
	// and `../` is forbidden, so the plugin lives beneath it.
	Dir = "superbased"
)

// EnabledKey is the EXACT enabledPlugins key an install of OUR plugin
// from OUR marketplace writes. Detection matches this whole string, not
// just its plugin half: `superbased@acme-internal` is somebody else's
// plugin that merely shares our name, and suppressing observer's wiring
// for it would leave that user with no capture at all.
//
// The deliberate cost: a user who vendors our plugin into a marketplace
// of their own is NOT detected, so `observer init` registers on top and
// they get the documented double-fire. That is the safe direction to be
// wrong in — the failure is redundant capture, never absent capture.
const EnabledKey = Name + "@" + Marketplace

// Enablement is the tri-state a settings file's enabledPlugins map can
// report for the plugin.
type Enablement int

const (
	// EnablementAbsent means the settings we could read do not mention
	// the plugin. It may still be installed at project or local scope,
	// whose settings files live in a workspace we don't know.
	EnablementAbsent Enablement = iota
	// EnablementOn means a readable settings scope enables the plugin.
	EnablementOn
	// EnablementOff means a readable settings scope explicitly disables
	// it. Claude Code then loads none of its components, so its hooks do
	// not fire and observer's own registration is NOT redundant.
	EnablementOff
)

// Detection is what the probes found. The zero value means "no evidence
// of the plugin", which is the correct fallback for every I/O failure.
type Detection struct {
	// Active reports whether the plugin's components (its hooks and its
	// MCP server) are expected to load in Claude Code on this host. This
	// is the predicate callers act on: Active means "skip, observer is
	// already wired here".
	//
	// Active is only ever set from AFFIRMATIVE, trustworthy evidence.
	// Anything unreadable or ambiguous leaves it false, so the caller
	// registers — see Uncertain.
	Active bool

	// Uncertain reports that a settings file exists but could not be
	// read or parsed, so "the plugin is not enabled here" could NOT be
	// established. Callers MUST treat this as "register anyway": a
	// corrupt settings.json must never cost the user their capture
	// wiring. Err carries the underlying failure for reporting.
	Uncertain bool

	// Err is the settings read/parse failure behind Uncertain. Nil
	// otherwise. It is a REPORTING channel, not a control-flow one —
	// detection has already failed open by leaving Active false.
	Err error

	// Enabled is the enabledPlugins verdict from the settings scopes we
	// could read.
	Enabled Enablement

	// SettingsPath is the settings file Enabled came from. Empty when no
	// scope mentioned the plugin.
	SettingsPath string

	// CacheDirs are the VERIFIED plugin cache directories — a version
	// directory under ~/.claude/plugins/cache/<Marketplace>/<Name>/ whose
	// .claude-plugin/plugin.json identifies our plugin by name. Sorted.
	// A bare or empty directory never appears here.
	CacheDirs []string

	// Marketplaces are the marketplace names from
	// ~/.claude/plugins/known_marketplaces.json, sorted. Informational
	// ONLY: having added a marketplace is not having installed a plugin,
	// so this never decides Active.
	Marketplaces []string
}

// Reason renders a one-line, user-facing explanation of why Active is
// true, naming the concrete artifact that proved it. Empty when the
// plugin was not detected.
func (d Detection) Reason() string {
	if !d.Active {
		return ""
	}
	switch {
	case d.Enabled == EnablementOn && d.SettingsPath != "":
		return fmt.Sprintf("the %s Claude Code plugin is enabled (%q in %s)", Name, EnabledKey, d.SettingsPath)
	case len(d.CacheDirs) > 0:
		return fmt.Sprintf("the %s Claude Code plugin is installed (%s)", Name, d.CacheDirs[0])
	default:
		return fmt.Sprintf("the %s Claude Code plugin is installed", Name)
	}
}

// Warning renders the non-fatal probe failure behind Uncertain, for a
// caller that wants to tell the user why it registered without being
// able to check. Empty when the probe was conclusive.
func (d Detection) Warning() string {
	if !d.Uncertain || d.Err == nil {
		return ""
	}
	return fmt.Sprintf("could not check for the %s Claude Code plugin: %v"+
		" — registering anyway (a plugin install would then double every event;"+
		" run `observer doctor claude-code` once the file is readable)", Name, d.Err)
}

// Detect probes the Claude Code state under homeDir.
func Detect(homeDir string) Detection {
	if homeDir == "" {
		return Detection{}
	}
	return DetectInClaudeDir(filepath.Join(homeDir, ".claude"))
}

// DetectInClaudeDir probes an explicit .claude directory. Used directly
// by the cross-OS registration target and the cross-OS doctor probe,
// where a WSL-side daemon reaches a Windows-side .claude via crossmount.
func DetectInClaudeDir(claudeDir string) Detection {
	if claudeDir == "" {
		return Detection{}
	}
	return classify(
		readSettingsScopes(settingsScopes(claudeDir)),
		findVerifiedCacheDirs(filepath.Join(claudeDir, "plugins", "cache")),
		readKnownMarketplaces(filepath.Join(claudeDir, "plugins", "known_marketplaces.json")),
	)
}

// settingsRead is one settings file's contribution to the decision.
// Exactly one of Err / Entry is meaningful: Err non-nil means the file
// exists but could not be trusted; Entry nil with Err nil means the file
// was read successfully and simply has no entry for EnabledKey.
type settingsRead struct {
	Path  string
	Err   error
	Entry *bool
}

// classify is the pure predicate. Split from the I/O above so the
// decision table can be exercised without a filesystem.
//
// The rules, in order:
//
//  1. An explicit `enabledPlugins[EnabledKey] = true` in a readable
//     scope is affirmative: the plugin loads, so observer's own wiring
//     would double it. Skip.
//
//  2. A settings file we could not read or parse makes "not enabled"
//     unprovable. Fail OPEN to wiring: Active stays false and Uncertain
//     is set, so the caller registers and reports the failure. A corrupt
//     settings.json must never silently cost the user their capture —
//     the same principle as hook fail-open. This outranks cache
//     evidence, because cache evidence only means the FILES are present;
//     whether the plugin is enabled lives in the file we just failed to
//     read.
//
//  3. An explicit `false` means the user turned the plugin off, so its
//     components do not load and observer's wiring is the only one that
//     fires. Register.
//
//  4. Settings read cleanly with no entry at all: a VERIFIED cache
//     directory is then the evidence. Claude Code only populates
//     cache/<marketplace>/<plugin>/<version>/ when that plugin was
//     actually installed, and we additionally require the version
//     directory's own manifest to name our plugin — a bare, empty or
//     stale directory does not count. The enabling entry may live in a
//     project-scoped settings file we cannot see from $HOME, which is
//     exactly the case this rule covers.
//
//  5. Nothing else counts. A registered marketplace is a catalog the
//     user can browse, not a plugin they installed.
func classify(reads []settingsRead, cacheDirs, marketplaces []string) Detection {
	d := Detection{CacheDirs: cacheDirs, Marketplaces: marketplaces}

	// Rule 1 — affirmative enablement wins over everything, including a
	// sibling scope we failed to read.
	for _, r := range reads {
		if r.Entry != nil && *r.Entry {
			d.Enabled, d.SettingsPath = EnablementOn, r.Path
			d.Active = true
			return d
		}
	}

	// Rule 2 — any unreadable scope makes the negative unprovable.
	for _, r := range reads {
		if r.Err != nil {
			d.Uncertain, d.Err, d.SettingsPath = true, r.Err, r.Path
			d.Active = false
			return d
		}
	}

	// Rule 3 — explicit off.
	for _, r := range reads {
		if r.Entry != nil {
			d.Enabled, d.SettingsPath = EnablementOff, r.Path
			d.Active = false
			return d
		}
	}

	// Rule 4 — verified cache evidence, only on a clean read.
	d.Active = len(cacheDirs) > 0
	return d
}

// settingsScopes lists the settings files under a .claude directory that
// can carry enabledPlugins. Only the USER scope lives here; project and
// local scopes live in a workspace, which `observer init` (a global
// operation) has no business reading — and which a cloned repository
// could supply, the same reasoning that made Claude Code stop reading
// pluginConfigs from those files.
func settingsScopes(claudeDir string) []string {
	return []string{filepath.Join(claudeDir, "settings.json")}
}

// readSettingsScopes reads each settings file and extracts the EXACT
// EnabledKey entry. A file that does not exist contributes nothing (that
// is a conclusive "no entry", not a failure). A file that exists but
// cannot be read or parsed contributes an Err, which fails detection
// open — see classify rule 2.
func readSettingsScopes(paths []string) []settingsRead {
	var out []settingsRead
	for _, p := range paths {
		raw, err := os.ReadFile(p) //nolint:gosec // G304: an operator-owned config path derived from $HOME, not user input.
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			out = append(out, settingsRead{Path: p, Err: err})
			continue
		}
		var doc struct {
			EnabledPlugins map[string]bool `json:"enabledPlugins"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			out = append(out, settingsRead{Path: p, Err: fmt.Errorf("parse %s: %w", p, err)})
			continue
		}
		if on, ok := doc.EnabledPlugins[EnabledKey]; ok {
			out = append(out, settingsRead{Path: p, Entry: &on})
			continue
		}
		out = append(out, settingsRead{Path: p})
	}
	return out
}

// findVerifiedCacheDirs returns the version directories under
// <cacheRoot>/<Marketplace>/<Name>/ whose own .claude-plugin/plugin.json
// identifies OUR plugin.
//
// The layout is documented as cache/<marketplace>/<plugin>/<version>/
// (the plugin-seed directory structure in
// code.claude.com/docs/en/plugin-marketplaces). Two deliberate
// tightenings over "a directory exists":
//
//   - the marketplace segment must be OURS, so a same-named plugin from
//     another catalog cannot suppress our wiring;
//   - the version directory must carry a manifest naming our plugin, so
//     an empty, half-deleted or orphaned directory is not evidence.
//     Claude Code keeps orphaned version directories for ~14 days after
//     an update or uninstall, so "a directory is present" genuinely does
//     not imply "a plugin is installed".
func findVerifiedCacheDirs(cacheRoot string) []string {
	pluginRoot := filepath.Join(cacheRoot, Marketplace, Name)
	versions, err := os.ReadDir(pluginRoot)
	if err != nil {
		return nil
	}
	var out []string
	for _, v := range versions {
		if !v.IsDir() {
			continue
		}
		dir := filepath.Join(pluginRoot, v.Name())
		if manifestNamesOurPlugin(filepath.Join(dir, ".claude-plugin", "plugin.json")) {
			out = append(out, dir)
		}
	}
	sort.Strings(out)
	return out
}

// manifestNamesOurPlugin reports whether path is a plugin.json whose
// `name` is ours. This is exactly the field plugins/plugingen emits, so
// the check is matched to what we actually ship. Any read or parse
// failure is a NO: cache evidence has to be affirmative.
func manifestNamesOurPlugin(path string) bool {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: a path built from $HOME plus fixed segments, not user input.
	if err != nil {
		return false
	}
	var doc struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return false
	}
	return doc.Name == Name
}

// readKnownMarketplaces returns the marketplace names Claude Code has
// registered for this user. Informational only — see Detection.
func readKnownMarketplaces(path string) []string {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: an operator-owned config path derived from $HOME, not user input.
	if err != nil {
		return nil
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	out := make([]string, 0, len(doc))
	for name := range doc {
		out = append(out, name)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil
	}
	return out
}
