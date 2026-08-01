package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/mcp"
	"github.com/marmutapp/superbased-observer/internal/mcp/locate"
)

// ---------------------------------------------------------------------
// Droid plugin + local marketplace catalog (coverage wave A — plan §7 row
// "droid", Factory AI).
//
// This is the ONE surface in wave A with a REAL observer registrar behind
// it: internal/mcp/locate carries a `droid` row (~/.factory/mcp.json,
// FormatMCPServersJSON) and internal/integration records
// MCP{Format: MCPServersJSON, Implemented: true}. So — like claude-code,
// cursor, codex and opencode — this generator runs the real registrar
// against the sandbox HOME and transposes what it wrote, rather than
// carrying canonicalStdio. That is the strongest form of the §3
// one-owner rule available here.
//
// Format grounding, first-party (docs.factory.ai/guides/building/
// building-plugins, read 2026-07-31 — it supplies the field list the
// research note in docs/plans/plugin-coverage-research-2026-07-31.md did
// not enumerate):
//
//   - Manifest `.factory-plugin/plugin.json`: `name` ("for native Droid
//     and Claude Code compatibility"), `description` ("Shown in browsing
//     and installation surfaces when available"), `version`
//     ("documentation metadata only" — a git install tracks the installed
//     commit hash), and `author` (object with `name`), `homepage`,
//     `repository`, `license`, `keywords` as "Optional metadata for
//     attribution and discovery".
//   - MCP IS NOT A MANIFEST FIELD. There is no `mcpServers` key in
//     plugin.json; a plugin bundles MCP servers via a root **`mcp.json`**
//     (no leading dot) containing an `mcpServers` object with
//     command/args/env. The docs note that for Claude Code compatibility
//     "`.mcp.json` is translated to `mcp.json`" when the plugin is copied
//     into Droid's cache — so `mcp.json` is the native spelling and the
//     one we emit. (Qoder is the exact reverse: dotted `.mcp.json`, no
//     `mcp.json` anywhere in its docs. Two neighbouring formats, opposite
//     file names; this comment exists so nobody "fixes" one to match the
//     other.)
//   - Components live at the plugin ROOT: commands/, skills/, droids/,
//     hooks/, mcp.json, README.md. The docs warn: "Do not put them inside
//     .factory-plugin/; that directory is for plugin metadata."
//   - Catalog `.factory-plugin/marketplace.json` (with a fallback to
//     `.claude-plugin/marketplace.json`): `name` required ("Marketplace
//     identifier"), optional `description` and `owner` ("Contact
//     metadata", an object with `name`), and `plugins[]` whose entries
//     require `name` and `source` ("Relative path string or source
//     object") with optional description, category, homepage, tags.
//     Relative path sources "must stay inside the marketplace directory".
//   - Install: `droid plugin marketplace add <source>` then
//     `droid plugin install <name>@<marketplace>` (or the `/plugins` TUI).
//     Factory maintains github.com/Factory-AI/factory-plugins as its own
//     official marketplace; submitting there is an operator-gated public
//     action, not something this generator does.
//
// Deliberate omission: `hooks/hooks.json`. Droid documents plugin hooks
// (with a ${DROID_PLUGIN_ROOT} path convention), but observer has no
// droid hook receiver — `observer hook <tool>` accepts claude-code,
// cursor, codex and hermes, and internal/integration records
// HookMechanism None for droid ("no hook subcommand found in
// `droid --help`"). A hooks.json would declare a command that does not
// exist.
// ---------------------------------------------------------------------

// droidSurfaceDir is this surface's directory in the in-tree plugins/
// layout; droidPluginDir is the plugin's directory INSIDE the marketplace
// root, and therefore the `./`-relative source the catalog carries.
//
// ── WHY IT IS NESTED UNDER `factory/` AND NOT JUST `<pluginDir>` ─────
//
// The marketplace root is a REPOSITORY root once published
// (scripts/assemble-plugins-repo.sh), and two other catalogs already
// resolve their own plugin directories from that same root: Claude Code's
// `./superbased` and Codex's `./plugins/superbased`. A bare `./superbased`
// here would land Droid's plugin in the SAME directory as Claude Code's —
// and that directory carries `hooks/hooks.json` full of
// `observer hook claude-code …` commands. Droid reads plugin hooks from
// exactly that path, so the merge would hand Droid a hook roster built for
// another tool. Nesting keeps the three apart with no assumption about
// what any vendor does with a foreign sibling file. Factory's own rule —
// a relative source "must stay inside the marketplace directory" — is
// satisfied either way.
//
// Placing the catalog at the repository ROOT is deliberate for the same
// reason: Factory documents `.claude-plugin/marketplace.json` as a
// FALLBACK when `.factory-plugin/marketplace.json` is absent, so a repo
// carrying only the Claude catalog would answer
// `droid plugin marketplace add superbasedapp/plugins` with the Claude
// Code plugin — hooks and all. Shipping our own catalog at the root
// shadows that fallback with a plugin that declares only an MCP server.
const (
	droidSurfaceDir = "droid"
	droidPluginDir  = "factory/" + pluginName
)

// droidCategory is a catalog-entry display hint. Factory documents
// `category` as a free-form optional string rather than an enum, so this
// is the same word the Codex entry uses — one vocabulary across surfaces.
const droidCategory = "observability"

// readDroidEntry pulls the "observer" entry out of ~/.factory/mcp.json
// after the real registrar wrote it. It reuses readMCPEntry, which already
// rejects any key the mcpServer struct does not model — the guard that
// stops a registrar field being silently dropped on the way into a
// published manifest.
func readDroidEntry(home string) (mcpServer, error) {
	if _, ok := locate.ForClient("droid", home); !ok {
		return mcpServer{}, fmt.Errorf("plugingen: locate has no droid row — the droid plugin surface has no registrar to transpose")
	}
	return readMCPEntry("droid", home)
}

// droidPluginManifest is .factory-plugin/plugin.json.
type droidPluginManifest struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Version     string   `json:"version"`
	Author      author   `json:"author"`
	Homepage    string   `json:"homepage"`
	Repository  string   `json:"repository"`
	License     string   `json:"license"`
	Keywords    []string `json:"keywords"`
}

// droidPluginDescription states the binary prerequisite plainly (§0).
const droidPluginDescription = "Token, cost and cache observability for Droid. " +
	"Wires the SuperBased Observer MCP server. " +
	binaryPrereqSentence + " " + installHint + " — this plugin is wiring only and installs no binary."

func renderDroidPluginJSON(version string) ([]byte, error) {
	return marshalJSON(droidPluginManifest{
		Name:        pluginName,
		Description: droidPluginDescription,
		Version:     version,
		// Factory documents `author` as "object with name" and nothing
		// else, so only `name` is filled; the shared struct's email/url are
		// omitempty. Attribution that the docs do ground lives in the
		// homepage and repository fields below.
		Author:     author{Name: authorName},
		Homepage:   homepage,
		Repository: repository,
		License:    license,
		Keywords:   pluginKeywords(),
	})
}

// renderDroidMCPJSON emits the plugin-root `mcp.json` — Factory's NATIVE
// spelling (see the file comment: `.mcp.json` is the Claude-compat alias
// Droid translates on copy).
func renderDroidMCPJSON(entry mcpServer) ([]byte, error) {
	return marshalJSON(struct {
		MCPServers map[string]mcpServer `json:"mcpServers"`
	}{MCPServers: map[string]mcpServer{mcp.ServerName: entry}})
}

// droidMarketplaceEntry is one row of the catalog's "plugins" array. Source
// is the documented relative-path string form; the source-OBJECT forms
// (github / url / git-subdir / npm) are for plugins that live elsewhere,
// which ours does not.
type droidMarketplaceEntry struct {
	Name        string   `json:"name"`
	Source      string   `json:"source"`
	Description string   `json:"description,omitempty"`
	Category    string   `json:"category,omitempty"`
	Homepage    string   `json:"homepage,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

// droidMarketplaceManifest is .factory-plugin/marketplace.json.
type droidMarketplaceManifest struct {
	Name        string                  `json:"name"`
	Description string                  `json:"description,omitempty"`
	Owner       author                  `json:"owner"`
	Plugins     []droidMarketplaceEntry `json:"plugins"`
}

func renderDroidMarketplaceJSON() ([]byte, error) {
	return marshalJSON(droidMarketplaceManifest{
		Name: marketplaceName,
		// A catalog's own description is a shelf label, not a per-plugin
		// listing — the same exemption the Claude Code and Codex catalogs
		// take. The plugin's prerequisite sentence lives on its entry.
		Description: "SuperBased plugins for AI coding agents — local-first token, cost and cache observability.",
		Owner:       author{Name: "SuperBased"},
		Plugins: []droidMarketplaceEntry{{
			Name:        pluginName,
			Source:      "./" + droidPluginDir,
			Description: droidPluginDescription,
			Category:    droidCategory,
			Homepage:    homepage,
			Tags:        pluginKeywords(),
		}},
	})
}

// droidCatalogSourceResolves reports whether the catalog's relative source
// points at a real plugin manifest under root — the "must stay inside the
// marketplace directory" rule, checked the same way the Claude Code and
// Codex tests check theirs. Kept here (rather than inline in the test) so
// the rule and the emitted path have one owner.
func droidCatalogSourceResolves(root, source string) error {
	if !strings.HasPrefix(source, "./") || strings.Contains(source, "..") {
		return fmt.Errorf("plugingen: droid catalog source %q must be a \"./\"-relative path that never escapes the marketplace root", source)
	}
	manifest := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(source, "./")), ".factory-plugin", "plugin.json")
	raw, err := os.ReadFile(manifest) //nolint:gosec // G304: path is derived from the generator's own emitted catalog under the repo tree.
	if err != nil {
		return fmt.Errorf("plugingen: droid catalog source %q does not resolve to a plugin manifest: %w", source, err)
	}
	var m struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("plugingen: droid plugin manifest at %s is not valid JSON: %w", manifest, err)
	}
	if m.Name != pluginName {
		return fmt.Errorf("plugingen: droid catalog source %q resolves to plugin %q, want %q", source, m.Name, pluginName)
	}
	return nil
}

func droidReadme(entry mcpServer) string {
	var b strings.Builder
	b.WriteString(generatedBanner)
	b.WriteString(`
# SuperBased Observer — Droid plugin

Local-first token, cost and cache observability for Factory AI's ` + "`droid`" + `.

**` + binaryPrereqSentence + `.** This plugin is
wiring only — it declares an MCP server that runs ` + "`observer`" + `; it does not
download or bundle it.

` + "```bash" + `
npm i -g @superbased/observer     # or: pipx install superbased-observer
observer start                    # run the local daemon
` + "```" + `

## Layout

` + "```" + `
` + droidSurfaceDir + `/                                ← marketplace root
├── .factory-plugin/marketplace.json    ← the catalog
└── ` + droidPluginDir + `/
    ├── .factory-plugin/plugin.json      ← the plugin manifest (metadata only)
    └── mcp.json                         ← the MCP server it bundles, at the plugin ROOT
` + "```" + `

Factory's own docs are explicit that only the manifest belongs inside
` + "`.factory-plugin/`" + `: "Do not put them inside ` + "`.factory-plugin/`" + `; that
directory is for plugin metadata." The catalog entry's ` + "`source`" + ` is
` + "`./" + droidPluginDir + "`" + ` — relative to the marketplace root, and inside it,
as the documented rule requires.

Note the file name: Droid's native MCP file is ` + "`mcp.json`" + `, **not**
` + "`.mcp.json`" + `. The dotted form is the Claude Code compatibility alias, which
Droid translates to ` + "`mcp.json`" + ` when it copies a plugin into its cache. (Qoder,
in ` + "`../qoder/`" + `, is the exact reverse — dotted only. The two are not typos.)

## Install

` + "```bash" + `
droid plugin marketplace add https://github.com/superbasedapp/plugins
droid plugin install ` + pluginName + `@` + marketplaceName + `
` + "```" + `

Or browse with ` + "`/plugins`" + ` inside the CLI. A local checkout works too —
` + "`droid plugin marketplace add ./" + droidSurfaceDir + "`" + ` then the same
install line.

Factory maintains its own official marketplace at
` + "`github.com/Factory-AI/factory-plugins`" + `. Listing there is a public
submission, not something this repository does on its own; the catalog above is
the self-hosted form, exactly like the Codex surface.

## What it wires

| Component | What it declares |
|---|---|
| ` + "`.factory-plugin/plugin.json`" + ` | Identity only — Droid's manifest has no MCP field. |
| ` + "`mcp.json`" + ` | The ` + "`" + mcp.ServerName + "`" + ` MCP server: ` + "`" + commandLine(entry) + "`" + ` — the same launch ` + "`observer init`" + ` writes into ` + "`~/.factory/mcp.json`" + `, transposed into the plugin's own file. |

**No hooks are declared.** Droid documents plugin hooks, but observer has no
Droid hook receiver (` + "`observer hook`" + ` accepts claude-code, cursor, codex and
hermes only), so a ` + "`hooks/hooks.json`" + ` here would declare a command that
does not exist. Droid capture works without hooks: observer's watcher reads the
JSONL transcripts under ` + "`~/.factory/sessions`" + ` and their settings sidecars.

Captured data lands in the local ` + "`~/.observer/observer.db`" + `. The MCP server
makes no network calls of its own.

## Don't double-wire

Droid is one of the few tools here where ` + "`observer init`" + ` **does** write an
MCP entry: ` + "`~/.factory/mcp.json`" + `, under the same ` + "`" + mcp.ServerName + "`" + `
key. Carrying both that entry and this plugin loads observer's MCP tool schema
twice per turn — wasted context, no data corruption (observer's rows are keyed
and upsert).

**Droid has no ` + "`--droid`" + ` init flag.** It is selected by auto-detection —
` + "`observer init`" + ` with no tool flags, or ` + "`observer init --all`" + ` — so the
MCP step runs for it whenever ` + "`~/.factory`" + ` is present.

**And Droid is not auto-detected the other way either.** The detect-and-skip
` + "`observer init`" + ` performs is ` + "`internal/claudeplugin`" + ` — claude-code-only,
by design — so nothing notices this plugin. With it installed, either skip the
MCP step at init time:

` + "```bash" + `
observer init --skip-mcp     # hooks + proxy routes only, for every selected tool
` + "```" + `

or, if it is already wired, delete the ` + "`\"" + mcp.ServerName + "\"`" + ` key from
` + "`~/.factory/mcp.json`" + ` (the file is otherwise left untouched — observer only
ever adds or removes its own entry there). This is documented, not built.

## Links

- Docs: ` + homepage + `
- Source: https://github.com/superbasedapp/observer
- License: ` + license + `
`)
	return b.String()
}
