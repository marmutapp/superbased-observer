package main

import (
	"strings"

	"github.com/marmutapp/superbased-observer/internal/mcp"
)

// ---------------------------------------------------------------------
// Devin CLI plugin (coverage wave A — plan §7 row "devin").
//
// Format grounding, first-party
// (docs.devin.ai/cli/extensibility/plugins/overview, re-read 2026-07-31;
// MCP config shapes from docs.devin.ai/cli/extensibility/mcp/configuration
// as recorded in docs/plans/plugin-coverage-research-2026-07-31.md):
//
//   - The manifest is `.devin-plugin/plugin.json`; only `name` is
//     mandatory ("must be unique among installed plugins"). Supported
//     metadata fields, quoted: name, version, description,
//     author ({name, email}), homepage, repository, license, keywords.
//     Also documented: requiredPlugins / optionalPlugins /
//     forbiddenPlugins, skills, and mcpServers.
//   - The plugin root (NOT `.devin-plugin/`) carries the components:
//     AGENTS.md, rules/, agents/, `hooks.json` ("optional lifecycle
//     hooks") and `mcp_config.json` ("optional MCP servers"), whose shape
//     is `{"mcpServers": {"<name>": {…}}}` and whose servers "start with
//     the session". Devin also honours the Claude-plugin conventions (a
//     root `.mcp.json`, and the manifest's own `mcpServers` field).
//   - Install: `devin plugins install acme/review-tools` (GitHub
//     owner/repo), `devin plugins install https://…/repo.git` (any git
//     URL), or `devin plugins install ./my-plugin` (local folder, linked
//     live). `-y`/`--yes` skips the confirmation prompt.
//   - There is no public plugin marketplace/catalog for the CLI (the
//     "MCP Marketplace" is Devin Cloud's settings page, a different
//     product), so distribution is git-URL install only.
//
// TWO deliberate omissions, both departures from the shape the task
// sketch assumed, each for a reason:
//
//   - `hooks.json` is NOT emitted. Devin's `.devin/hooks.v1.json` is
//     explicitly Claude-Code-compatible but UNWIRED in the shipped CLI
//     (live 3000.1.27, recorded on the devin row of
//     internal/integration), and observer has no Devin hook receiver
//     anyway: `observer hook <tool>` accepts claude-code, cursor, codex
//     and hermes. A hooks.json here would declare a command that cannot
//     run, on an event that does not fire.
//   - the manifest's `mcpServers` field is NOT set. The docs say it is
//     "read IN ADDITION to the root conventions" — and the root
//     convention `mcp_config.json` is exactly what this plugin ships.
//     Setting both would declare the same server twice, from one plugin,
//     by construction. One spelling per surface.
// ---------------------------------------------------------------------

// devinSurfaceDir is this surface's directory in the in-tree plugins/ layout.
const devinSurfaceDir = "devin"

// devinPluginManifest is .devin-plugin/plugin.json. Only documented fields
// appear, and `author` uses the documented {name, email} shape.
type devinPluginManifest struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Description string   `json:"description"`
	Author      author   `json:"author"`
	Homepage    string   `json:"homepage"`
	Repository  string   `json:"repository"`
	License     string   `json:"license"`
	Keywords    []string `json:"keywords"`
}

// devinPluginDescription states the binary prerequisite plainly (§0).
const devinPluginDescription = "Token, cost and cache observability for Devin. " +
	"Wires the SuperBased Observer MCP server. " +
	binaryPrereqSentence + " " + installHint + " — this plugin is wiring only and installs no binary."

func renderDevinPluginJSON(version string) ([]byte, error) {
	return marshalJSON(devinPluginManifest{
		Name:        pluginName,
		Version:     version,
		Description: devinPluginDescription,
		// The documented author shape is {name, email} — no url key — so
		// the shared author struct is filled to exactly that. Its `url` is
		// omitempty, which is what keeps the emitted object documented.
		Author:     author{Name: authorName, Email: authorMail},
		Homepage:   homepage,
		Repository: repository,
		License:    license,
		Keywords:   pluginKeywords(),
	})
}

// renderDevinMCPConfigJSON emits the plugin-root `mcp_config.json`. Devin's
// stdio entry keys are command (required), args, env and disabled; we set
// the two the registrars actually produce.
func renderDevinMCPConfigJSON(entry mcpServer) ([]byte, error) {
	return marshalJSON(struct {
		MCPServers map[string]mcpServer `json:"mcpServers"`
	}{MCPServers: map[string]mcpServer{mcp.ServerName: entry}})
}

func devinReadme(entry mcpServer) string {
	var b strings.Builder
	b.WriteString(generatedBanner)
	b.WriteString(`
# SuperBased Observer — Devin CLI plugin

Local-first token, cost and cache observability for Devin for Terminal.

**` + binaryPrereqSentence + `.** This plugin is
wiring only — it declares an MCP server that runs ` + "`observer`" + `; it does not
download or bundle it.

` + "```bash" + `
npm i -g @superbased/observer     # or: pipx install superbased-observer
observer start                    # run the local daemon
` + "```" + `

## Layout

` + "```" + `
` + pluginDir + `/
├── .devin-plugin/plugin.json   ← the manifest
└── mcp_config.json             ← the MCP server it ships, at the plugin ROOT
` + "```" + `

## Install

` + "```bash" + `
devin plugins install superbasedapp/plugins
` + "```" + `

A GitHub ` + "`owner/repo`" + `, any git URL, or a local folder are the three
documented sources; ` + "`-y`/`--yes`" + ` skips the confirmation prompt. Manage
with ` + "`devin plugins list|info|update|remove`" + `.

⚠️ **Repository-root placement.** A git-URL install takes the repository, so
the plugin's own files have to be at that repository's root. In this source
tree they live under ` + "`" + devinSurfaceDir + "/" + pluginDir + "/`" + ` next to the
other surfaces; the public repository puts ` + "`.devin-plugin/`" + ` and
` + "`mcp_config.json`" + ` at the top level, where neither name collides with
another surface's root entry. If you would rather not take the whole
repository, clone it and install the directory:

` + "```bash" + `
devin plugins install ./` + devinSurfaceDir + `/` + pluginDir + `
` + "```" + `

**There is no Devin plugin marketplace to list in.** Cognition documents no
public catalog for the CLI (the "MCP Marketplace" is Devin Cloud's settings
page — a different product surface), so a git-URL install is the whole
distribution channel, and no catalog file is generated.

## What it wires

| Component | What it declares |
|---|---|
| ` + "`.devin-plugin/plugin.json`" + ` | Identity only: ` + "`name`" + ` (` + "`" + pluginName + "`" + `), version, description, author, homepage, repository, license, keywords. |
| ` + "`mcp_config.json`" + ` | The ` + "`" + mcp.ServerName + "`" + ` MCP server: ` + "`" + commandLine(entry) + "`" + ` — on-demand project/session/cost queries from inside Devin. Servers in this file start with the session. |

**The manifest's own ` + "`mcpServers`" + ` field is deliberately empty.** Devin
reads it "in addition to the root conventions", and the root convention
` + "`mcp_config.json`" + ` is what this plugin ships — setting both would declare
the same server twice from one plugin.

**No ` + "`hooks.json`" + ` is declared.** Devin's Claude-Code-compatible
` + "`hooks.v1.json`" + ` is present but unwired in the shipped CLI (grounded live
against 3000.1.27), and observer has no Devin hook receiver either
(` + "`observer hook`" + ` accepts claude-code, cursor, codex and hermes only). A
hooks file here would declare a command that cannot run, on an event that does
not fire. Devin capture works without it: observer's watcher reads Devin's
` + "`sessions.db`" + ` message tree directly.

Captured data lands in the local ` + "`~/.observer/observer.db`" + `. The MCP server
makes no network calls of its own.

## Double-wiring

` + "`observer init`" + ` writes **no** Devin MCP entry today — ` + "`internal/mcp`" + `
has no Devin client row — so this plugin cannot duplicate anything observer
wrote. If you have separately run
` + "`devin mcp add " + mcp.ServerName + " -- observer serve`" + `, or hand-added the
server to ` + "`~/.config/devin/mcp_config.json`" + ` / ` + "`.devin/mcp_config.json`" + `,
remove one of the two — Devin would otherwise load the same tool schema twice
per turn.

The automatic detect-and-skip ` + "`observer init`" + ` performs for the Claude Code
plugin is **claude-code-only** (` + "`internal/claudeplugin`" + `); no equivalent probe
exists for Devin. Documented, not built.

## Links

- Docs: ` + homepage + `
- Source: https://github.com/superbasedapp/observer
- License: ` + license + `
`)
	return b.String()
}
