package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/mcp"
)

// ---------------------------------------------------------------------
// Qoder CLI plugin (coverage wave A — plan §7 row "qoder").
//
// Format grounding, first-party (docs.qoder.com/en/cli/plugins, re-read
// 2026-07-31 to settle one thing the research note left open — see the
// mcpServers paragraph below):
//
//   - The manifest is `.qoder-plugin/plugin.json`. "Only `name` is
//     required in plugin.json; other fields are optional." Documented
//     fields: name ("Unique plugin identifier; cannot contain spaces;
//     kebab-case recommended"), version, description, author, homepage,
//     repository, license (SPDX id), keywords.
//   - The documented plugin layout puts every COMPONENT at the plugin
//     root and only the manifest inside `.qoder-plugin/`:
//     commands/, agents/, skills/, hooks/hooks.json, output-styles/,
//     bin/ and `.mcp.json` ("MCP servers shipped with this plugin").
//     Convention directories are "auto-discovered when present,
//     otherwise ignored".
//   - Install: `qodercli plugins install <path>` (absolute, relative or
//     `~/…`), `-s/--scope user|project|local` (user is the default),
//     plus `qodercli plugins marketplace add <git-url | org/repo |
//     local dir | marketplace.json URL>` and then
//     `qodercli plugins install <name>`. `/plugins reload` in the TUI
//     picks up changes without a restart.
//
// THE ONE CORRECTION to the research note: there is NO `mcpServers`
// manifest field. The research verdict wrote "a .qoder-plugin/plugin.json
// declaring an mcpServers entry (or .mcp.json at project root)"; the
// documented manifest field list has no such key, and the "advanced
// manifest" overrides cover commands / agents / skills / hooks /
// outputStyles only. MCP is bundled by FILE — `.mcp.json` at the plugin
// root (with the leading dot; a plain `mcp.json` is not mentioned
// anywhere on that page, unlike droid's, which is the reverse). So this
// surface ships the manifest plus a sibling `.mcp.json`, and puts nothing
// MCP-shaped in the manifest.
//
// Deliberate omissions:
//
//   - `hooks` / `hooks/hooks.json`. `qodercli hooks` exists, but no
//     firing hook envelope has ever been grounded for Qoder
//     (internal/integration records HookMechanism None) and observer has
//     no Qoder hook receiver. Declaring one would name a command that
//     cannot run.
//   - `author`. The field is documented; its object shape is not (the
//     Quick Start example omits it). homepage + repository carry the same
//     attribution unambiguously.
//
// ── THE ROOT CATALOG, AND WHY IT IS NOT OPTIONAL ─────────────────────
//
// This surface used to ship NO `marketplace.json`, on the grounds that
// the catalog schema is undocumented. That reasoning was right about the
// docs and wrong about the CONSEQUENCE: shipping nothing does not make
// `qodercli plugins marketplace add <this repo>` fail, it makes it
// resolve SOMEBODY ELSE'S catalog.
//
// qodercli 1.1.5's marketplace loader tries, in this exact order:
//
//	.qoder-plugin/marketplace.json → .claude-plugin/marketplace.json
//	→ marketplace.json → the path itself
//
// (read out of the shipped binary: the loader is a four-element array
// literal over those paths, the same fallback chain Droid documents in
// prose). The assembled public repo carries `.claude-plugin/marketplace.json`
// at its root for Claude Code, so with no catalog of ours the SECOND entry
// wins and Qoder installs the CLAUDE CODE plugin — `hooks/hooks.json`
// full of `observer hook claude-code …` commands and all. That was
// verified live, not reasoned about: adding the assembled tree under a
// throwaway HOME and running `qodercli plugins install superbased@superbased`
// staged `.claude-plugin/plugin.json` + `hooks/hooks.json` into the Qoder
// plugin cache. Exactly the Droid fall-through, one vendor over.
//
// So the catalog IS emitted, and it shadows that fallback with a plugin
// that declares only an MCP server. Its schema is no longer a guess
// either — the loader validates against a schema carried in the same
// binary, read out of it field by field:
//
//   - `name` (required): `^[a-z0-9][-a-z0-9._]*$`, ≤100 chars, no spaces,
//     and refused if it impersonates an official Qoder marketplace
//     (`qoder-marketplace` / `qoder-plugins` / `qoder-plugins-official`,
//     an `official…qoder` pattern, the `qoder-enterprise-*` and
//     `qoderwork-enterprise-*` prefixes) or is `inline` / `builtin` /
//     `local` / `flag`. "superbased" is none of those.
//   - `owner` (REQUIRED, unlike Droid's): an object `{name, email?, url?}`.
//   - `plugins[]`: each entry is the plugin-manifest field set (partial)
//     extended with `name` (required, no spaces), `source` (required),
//     `category?`, `tags?` and `strict?` (default true — "require plugin
//     manifest in the plugin folder", which ours has).
//   - `metadata?`: `{pluginRoot?, version?, description?}`. NOTE the
//     catalog's own prose description lives HERE, not at the top level —
//     that is the one shape difference from the Claude catalog, and why
//     this file does not simply reuse marketplaceManifest.
//   - A string `source` is documented in-schema as "Path relative to
//     marketplace root" and is refused unless it starts with `./`.
//
// Deliberate omission: a `version` on the catalog entry. The install was
// run BOTH ways under a throwaway HOME; with no entry version qodercli
// resolves the pin from the plugin's own `.qoder-plugin/plugin.json`
// (`qodercli plugins list` printed `superbased@superbased v1.28.0`, and
// the cache path ends in `/1.28.0/`). That is the same "the plugin's own
// manifest is the pin" model the Codex and Droid catalogs use, and it
// keeps this file off the release stamper's list.
// ---------------------------------------------------------------------

// qoderSurfaceDir is this surface's directory in the in-tree plugins/
// layout; qoderPluginPath is the plugin's path INSIDE the marketplace
// root, and therefore the `./`-relative source the catalog carries.
//
// ── WHICH ROOT THAT SOURCE RESOLVES AGAINST ─────────────────────────
//
// The marketplace root is the PUBLIC repository root once assembled
// (scripts/assemble-plugins-repo.sh), where this surface's plugin sits at
// `qoder/<plugin>` — a subdirectory, because Qoder's own documented
// install is a local path and nothing about it resolves from a root. The
// catalog is the one piece of this surface that MUST be at the root (see
// the fall-through note above), so its source is `./qoder/<plugin>`.
//
// In THIS tree that makes the catalog the one file whose relative source
// resolves against `plugins/` rather than against its own surface
// directory: `plugins/qoder/<plugin>` is exactly `plugins/` + the source.
// qoderCatalogSourceResolves is called with that root so the rule is
// checked rather than asserted in prose. (Droid keeps the frames aligned
// instead, by nesting its plugin under `factory/`; doing the same here
// would mean `plugins/qoder/qoder/<plugin>`, which buys nothing and reads
// worse.)
const (
	qoderSurfaceDir = "qoder"
	qoderPluginPath = qoderSurfaceDir + "/" + pluginDir
)

// qoderCategory is a catalog-entry display hint — the same word the Codex
// and Droid entries use, one vocabulary across surfaces.
const qoderCategory = "observability"

// qoderPluginManifest is .qoder-plugin/plugin.json. Only documented fields.
type qoderPluginManifest struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Description string   `json:"description"`
	Homepage    string   `json:"homepage"`
	Repository  string   `json:"repository"`
	License     string   `json:"license"`
	Keywords    []string `json:"keywords"`
}

// qoderPluginDescription states the binary prerequisite plainly (§0).
const qoderPluginDescription = "Token, cost and cache observability for Qoder. " +
	"Wires the SuperBased Observer MCP server. " +
	binaryPrereqSentence + " " + installHint + " — this plugin is wiring only and installs no binary."

func renderQoderPluginJSON(version string) ([]byte, error) {
	return marshalJSON(qoderPluginManifest{
		Name:        pluginName,
		Version:     version,
		Description: qoderPluginDescription,
		Homepage:    homepage,
		Repository:  repository,
		License:     license,
		Keywords:    pluginKeywords(),
	})
}

// renderQoderMCPJSON emits the plugin-root `.mcp.json` — Qoder's documented
// "MCP servers shipped with this plugin" file, in the standard
// {"mcpServers": {…}} stdio shape.
func renderQoderMCPJSON(entry mcpServer) ([]byte, error) {
	return marshalJSON(struct {
		MCPServers map[string]mcpServer `json:"mcpServers"`
	}{MCPServers: map[string]mcpServer{mcp.ServerName: entry}})
}

// qoderMarketplaceMetadata is the catalog's optional `metadata` object.
// Qoder puts the marketplace's own prose description in here, NOT at the
// top level (the Claude catalog is the other way round).
type qoderMarketplaceMetadata struct {
	Description string `json:"description,omitempty"`
}

// qoderMarketplaceEntry is one row of the catalog's "plugins" array.
// `source` is the documented relative-path string form ("Path relative to
// marketplace root"); the source OBJECT forms (npm / pip / url / github /
// git-subdir) are for plugins that live elsewhere, which ours does not.
type qoderMarketplaceEntry struct {
	Name        string   `json:"name"`
	Source      string   `json:"source"`
	Description string   `json:"description,omitempty"`
	Category    string   `json:"category,omitempty"`
	Homepage    string   `json:"homepage,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

// qoderMarketplaceManifest is .qoder-plugin/marketplace.json.
type qoderMarketplaceManifest struct {
	Name     string                    `json:"name"`
	Owner    author                    `json:"owner"`
	Metadata *qoderMarketplaceMetadata `json:"metadata,omitempty"`
	Plugins  []qoderMarketplaceEntry   `json:"plugins"`
}

func renderQoderMarketplaceJSON() ([]byte, error) {
	return marshalJSON(qoderMarketplaceManifest{
		Name:  marketplaceName,
		Owner: author{Name: "SuperBased", Email: authorMail, URL: authorURL},
		// A catalog's own description is a shelf label, not a per-plugin
		// listing — the same exemption the Claude Code, Codex and Droid
		// catalogs take. The plugin's prerequisite sentence lives on its
		// entry below.
		Metadata: &qoderMarketplaceMetadata{
			Description: "SuperBased plugins for AI coding agents — local-first token, cost and cache observability.",
		},
		Plugins: []qoderMarketplaceEntry{{
			Name:        pluginName,
			Source:      "./" + qoderPluginPath,
			Description: qoderPluginDescription,
			Category:    qoderCategory,
			Homepage:    homepage,
			Tags:        pluginKeywords(),
		}},
	})
}

// qoderCatalogSourceResolves reports whether the catalog's relative source
// points at a real Qoder plugin manifest under root — the schema's
// "Path relative to marketplace root" rule, checked the same way the
// Claude Code, Codex and Droid catalogs check theirs. Kept here (rather
// than inline in the test) so the rule and the emitted path have one
// owner. Note root is the MARKETPLACE root, which in this tree is
// `plugins/` — see the qoderPluginPath comment.
func qoderCatalogSourceResolves(root, source string) error {
	if !strings.HasPrefix(source, "./") || strings.Contains(source, "..") {
		return fmt.Errorf("plugingen: qoder catalog source %q must be a \"./\"-relative path that never escapes the marketplace root", source)
	}
	manifest := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(source, "./")), ".qoder-plugin", "plugin.json")
	raw, err := os.ReadFile(manifest) //nolint:gosec // G304: path is derived from the generator's own emitted catalog under the repo tree.
	if err != nil {
		return fmt.Errorf("plugingen: qoder catalog source %q does not resolve to a plugin manifest: %w", source, err)
	}
	var m struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("plugingen: qoder plugin manifest at %s is not valid JSON: %w", manifest, err)
	}
	if m.Name != pluginName {
		return fmt.Errorf("plugingen: qoder catalog source %q resolves to plugin %q, want %q", source, m.Name, pluginName)
	}
	return nil
}

func qoderReadme(entry mcpServer) string {
	var b strings.Builder
	b.WriteString(generatedBanner)
	b.WriteString(`
# SuperBased Observer — Qoder CLI plugin

Local-first token, cost and cache observability for Qoder (` + "`qodercli`" + `).

**` + binaryPrereqSentence + `.** This plugin is
wiring only — it declares an MCP server that runs ` + "`observer`" + `; it does not
download or bundle it.

` + "```bash" + `
npm i -g @superbased/observer     # or: pipx install superbased-observer
observer start                    # run the local daemon
` + "```" + `

## Layout

` + "```" + `
.qoder-plugin/marketplace.json   ← the catalog, at the marketplace ROOT
` + qoderPluginPath + `/
├── .qoder-plugin/plugin.json    ← the manifest (only the manifest goes here)
└── .mcp.json                    ← the MCP server it ships, at the plugin ROOT
` + "```" + `

Qoder auto-discovers convention paths at the plugin root and ignores the ones
that are absent, so a two-file plugin is a complete plugin. The catalog's
` + "`source`" + ` is ` + "`./" + qoderPluginPath + "`" + ` — relative to the
marketplace root, and inside it, as the schema requires.

## Install

Add this repository as a marketplace, then install from it:

` + "```bash" + `
qodercli plugins marketplace add superbasedapp/plugins
qodercli plugins install ` + pluginName + `@` + marketplaceName + `
` + "```" + `

` + "`marketplace add`" + ` also takes a git URL, a local directory or a
` + "`marketplace.json`" + ` URL, and ` + "`--scope`" + ` takes ` + "`user`" + `
(the default), ` + "`project`" + ` or ` + "`local`" + `.

Or install the plugin directory straight from a clone, with no marketplace at
all:

` + "```bash" + `
qodercli plugins install ./` + qoderPluginPath + `
` + "```" + `

An absolute path or a ` + "`~/…`" + ` path works too, and ` + "`-s/--scope`" + ` takes
the same three values. Restart the CLI
or run ` + "`/plugins reload`" + ` afterwards. Locally-installed plugins carry the
id ` + "`" + pluginName + "@local`" + `; manage them with
` + "`qodercli plugins list|enable|disable|validate|update`" + ` and
` + "`qodercli plugins uninstall " + pluginName + "@local`" + `.

### Why there is a catalog here at all

Qoder looks for a marketplace manifest in a fixed order —
` + "`.qoder-plugin/marketplace.json`" + `, then
` + "`.claude-plugin/marketplace.json`" + `, then ` + "`marketplace.json`" + ` — and
this repository carries a ` + "`.claude-plugin/marketplace.json`" + ` at its root
for Claude Code. Without a Qoder catalog to take the first slot,
` + "`qodercli plugins marketplace add`" + ` on this repository would fall through
to the Claude Code entry and install **that** plugin — which bundles
` + "`hooks/hooks.json`" + ` full of ` + "`observer hook claude-code …`" + ` commands
written for a different tool. The catalog above takes the first slot and
resolves to the Qoder plugin, which declares an MCP server and nothing else.
(Droid documents the same fallback and gets the same treatment; see
` + "`../droid/`" + `.)

The catalog entry carries no ` + "`version`" + `: Qoder reads the pin from the
plugin's own ` + "`.qoder-plugin/plugin.json`" + `, exactly like the Codex and
Droid catalogs.

## What it wires

| Component | What it declares |
|---|---|
| ` + "`.qoder-plugin/marketplace.json`" + ` | The catalog: one entry, ` + "`" + pluginName + "`" + `, sourced at ` + "`./" + qoderPluginPath + "`" + `. |
| ` + "`.qoder-plugin/plugin.json`" + ` | Identity only: ` + "`name`" + ` (` + "`" + pluginName + "`" + `), version, description, homepage, repository, license, keywords. |
| ` + "`.mcp.json`" + ` | The ` + "`" + mcp.ServerName + "`" + ` MCP server: ` + "`" + commandLine(entry) + "`" + ` — on-demand project/session/cost queries from inside Qoder. |

There is deliberately **no ` + "`mcpServers`" + ` key in the manifest.** Qoder's
documented manifest field list does not have one — MCP is bundled by file, and
the manifest's advanced overrides cover commands / agents / skills / hooks /
outputStyles only.

**No hooks are declared.** ` + "`qodercli hooks`" + ` exists, but no firing hook
envelope has been grounded for Qoder and observer has no Qoder hook receiver
(` + "`observer hook`" + ` accepts claude-code, cursor, codex and hermes only), so a
declared hook would name a command that cannot run.

Captured data lands in the local ` + "`~/.observer/observer.db`" + `. The MCP server
makes no network calls of its own.

## Double-wiring

` + "`observer init`" + ` writes **no** Qoder MCP entry today — ` + "`internal/mcp`" + `
has no Qoder client row — so this plugin cannot duplicate anything observer
wrote. If you have separately added the same server by hand with
` + "`qodercli mcp add`" + `, or into ` + "`~/.qoder/settings.json`" + ` or a project
` + "`.mcp.json`" + `, remove one of the two: Qoder would otherwise load the same
tool schema twice per turn.

The automatic detect-and-skip ` + "`observer init`" + ` performs for the Claude Code
plugin is **claude-code-only** (` + "`internal/claudeplugin`" + `); no equivalent probe
exists for Qoder. Documented, not built.

## What observer can and cannot see for Qoder

Qoder's local stores carry neither a model name nor token counts — usage is
server-side only, and Qoder has no base-URL knob to route through the local
proxy. So observer records Qoder **activity** (sessions, tool calls, project
patterns) but not Qoder **spend**. This plugin does not change that; it makes
observer's own database queryable from inside Qoder.

## Links

- Docs: ` + homepage + `
- Source: https://github.com/superbasedapp/observer
- License: ` + license + `
`)
	return b.String()
}
