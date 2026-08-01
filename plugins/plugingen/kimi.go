package main

import (
	"strings"

	"github.com/marmutapp/superbased-observer/internal/mcp"
)

// ---------------------------------------------------------------------
// Kimi Code plugin (coverage wave A — plan §7 row "kimi-code").
//
// Format grounding, first-party
// (moonshotai.github.io/kimi-code/en/customization/plugins.html, re-read
// 2026-07-31 to confirm the research note in
// docs/plans/plugin-coverage-research-2026-07-31.md §"Kimi Code"):
//
//   - The manifest is `kimi.plugin.json` at the plugin root, OR
//     `.kimi-plugin/plugin.json`; "When both files exist,
//     kimi.plugin.json takes precedence." We emit the former only —
//     shipping both would be two spellings of one wiring.
//   - `name` is REQUIRED and "serves as the plugin id. Must match
//     [a-z0-9][a-z0-9_-]{0,63}".
//   - Display metadata: version, description, keywords, author, homepage,
//     license, plus an `interface` object with displayName,
//     shortDescription, longDescription, developerName, websiteURL.
//   - `mcpServers` — "MCP server declarations, on by default", reusing the
//     standard MCP schema. The documented stdio example carries `command`
//     and `args` ONLY; no `env` key appears on that page.
//   - Install: `/plugins install <path-or-url>`, where a URL may be a bare
//     GitHub repo URL (latest release, falling back to the default
//     branch), a /tree/<ref>, a /releases/tag/<tag> or a /commit/<sha>.
//     Downloads go via github.com / codeload.github.com only.
//
// Deliberate omissions, each because the alternative would be a guess or
// a claim we cannot honour:
//
//   - `hooks`. The manifest documents a hooks vocabulary (event, matcher,
//     command, timeout), but observer has NO kimi-code hook receiver:
//     `observer hook <tool>` accepts claude-code, cursor, codex and hermes,
//     and internal/integration records HookMechanism None for kimi-code.
//     Declaring a hook would point Kimi at a command that does not exist.
//   - `author`. The field is documented, its OBJECT SHAPE is not (the page
//     lists it as display metadata without an example). `interface`
//     .developerName is documented and carries the same fact, so that is
//     what we fill.
//   - `skills`, `agents`, `commands`, `systemPrompt*`, `sessionStart` —
//     observer ships none of those components, and a manifest key pointing
//     at an absent path is a broken plugin.
//   - A custom marketplace catalog ({"version":"2","plugins":[{id,source}]}).
//     Documented, but redundant here: `/plugins install <github-url>` is
//     itself the first-party install channel for a single repo, and a
//     catalog would be a second place to keep the same one entry in sync.
//
// Root-placement note: because the documented remote install takes a
// GITHUB REPO URL, the manifest has to sit at the root of whatever is
// installed — the same constraint gemini-extension.json carries. The
// in-tree directory below is the source layout; the public transpose puts
// `kimi.plugin.json` at the repository root, where its name collides with
// nothing else that must live there.
// ---------------------------------------------------------------------

// kimiSurfaceDir is this surface's directory in the in-tree plugins/ layout.
const kimiSurfaceDir = "kimi-code"

// kimiInterface is the manifest's documented display block.
type kimiInterface struct {
	DisplayName      string `json:"displayName"`
	ShortDescription string `json:"shortDescription"`
	LongDescription  string `json:"longDescription"`
	DeveloperName    string `json:"developerName"`
	WebsiteURL       string `json:"websiteURL"`
}

// kimiPluginManifest is kimi.plugin.json. Only documented fields appear.
type kimiPluginManifest struct {
	Name        string               `json:"name"`
	Version     string               `json:"version"`
	Description string               `json:"description"`
	Keywords    []string             `json:"keywords"`
	Homepage    string               `json:"homepage"`
	License     string               `json:"license"`
	Interface   kimiInterface        `json:"interface"`
	MCPServers  map[string]mcpServer `json:"mcpServers"`
}

// kimiPluginDescription states the binary prerequisite plainly (§0).
const kimiPluginDescription = "Token, cost and cache observability for Kimi Code. " +
	"Wires the SuperBased Observer MCP server. " +
	binaryPrereqSentence + " " + installHint + " — this plugin is wiring only and installs no binary."

func renderKimiPluginJSON(entry mcpServer, version string) ([]byte, error) {
	return marshalJSON(kimiPluginManifest{
		Name:        pluginName,
		Version:     version,
		Description: kimiPluginDescription,
		Keywords:    pluginKeywords(),
		Homepage:    homepage,
		License:     license,
		Interface: kimiInterface{
			DisplayName:      "SuperBased Observer",
			ShortDescription: "Local-first token, cost and cache observability",
			LongDescription: "Query your own coding-agent history from inside Kimi Code: per-session " +
				"token and cost totals, cache behaviour, project patterns and past tool output. All data " +
				"stays in a local SQLite database. " + binaryPrereqSentence + ".",
			DeveloperName: "SuperBased",
			WebsiteURL:    homepage,
		},
		// The documented relative-path rule ("command ... must start with
		// ./ and be within the plugin root") applies to a path INSIDE the
		// plugin. `observer` is a bare PATH-resolved command, so it is not
		// a relative path and the rule does not bite — the same deviation
		// every surface here carries, for the same reason (a cache-copied
		// plugin cannot know the absolute path `observer init` writes).
		MCPServers: map[string]mcpServer{mcp.ServerName: entry},
	})
}

func kimiReadme(entry mcpServer) string {
	var b strings.Builder
	b.WriteString(generatedBanner)
	b.WriteString(`
# SuperBased Observer — Kimi Code plugin

Local-first token, cost and cache observability for Kimi Code.

**` + binaryPrereqSentence + `.** This plugin is
wiring only — it declares an MCP server that runs ` + "`observer`" + `; it does not
download or bundle it.

` + "```bash" + `
npm i -g @superbased/observer     # or: pipx install superbased-observer
observer start                    # run the local daemon
` + "```" + `

## Install

Inside Kimi Code:

` + "```" + `
/plugins install https://github.com/superbasedapp/plugins
` + "```" + `

A bare GitHub repository URL resolves to the latest release, falling back to
the default branch. The other documented pinned forms also work:
` + "`…/tree/<ref>`" + `, ` + "`…/releases/tag/<tag>`" + ` and ` + "`…/commit/<sha>`" + `.
Downloads go through ` + "`github.com`" + ` / ` + "`codeload.github.com`" + ` only.

Installed plugins live under ` + "`$KIMI_CODE_HOME/plugins/managed/<id>/`" + `.
Manage them with ` + "`/plugins list`" + `, ` + "`/plugins info " + pluginName + "`" + `,
` + "`/plugins enable|disable " + pluginName + "`" + `, ` + "`/plugins reload`" + ` and
` + "`/plugins remove " + pluginName + "`" + `. The MCP server this plugin declares can be
toggled on its own with ` + "`/plugins mcp enable|disable " + pluginName + " " + mcp.ServerName + "`" + `.

Kimi Code shows a trust badge per source; a third-party install (this one, until
it is listed in an official catalog) defaults the confirmation prompt to
**cancel**, so you have to confirm deliberately.

⚠️ **The manifest has to be at the root of whatever you install.** The
documented remote install takes a repository URL, so ` + "`kimi.plugin.json`" + `
belongs at the repository root — the same constraint
` + "`gemini-extension.json`" + ` carries. In this source tree it lives under
` + "`" + kimiSurfaceDir + "/" + pluginDir + "/`" + ` alongside the other surfaces; the
public repository puts it at the top level, where its name collides with
nothing else that must live there.

## What it wires

| Field | Value |
|---|---|
| ` + "`name`" + ` | ` + "`" + pluginName + "`" + ` — the plugin id, matching the documented ` + "`[a-z0-9][a-z0-9_-]{0,63}`" + ` pattern. |
| ` + "`version`" + ` | Stamped from the observer release tag. |
| ` + "`mcpServers." + mcp.ServerName + "`" + ` | ` + "`" + commandLine(entry) + "`" + ` — on-demand project/session/cost queries from inside Kimi Code. |

Captured data lands in the local ` + "`~/.observer/observer.db`" + `. The MCP server
makes no network calls of its own.

**No hooks are declared, and that is not an oversight.** Kimi's manifest
documents a ` + "`hooks`" + ` vocabulary, but observer has no Kimi hook receiver —
` + "`observer hook`" + ` accepts claude-code, cursor, codex and hermes only. A
declared hook would point Kimi at a command that does not exist. Kimi capture
works without hooks anyway: observer's watcher reads Kimi's own
` + "`wire.jsonl`" + ` traces, and ` + "`observer kimi -- …`" + ` routes turns through
the local proxy.

Nothing else is declared either — no skills, agents, commands or system-prompt
contribution — because observer ships none of those components, and a manifest
key pointing at a path that is not in the plugin is a broken plugin.

## Double-wiring

` + "`observer init`" + ` writes **no** Kimi MCP entry today. Kimi's only MCP config
surface is ` + "`~/.kimi-code/config.toml`" + `, a file observer deliberately never
reads (it holds a plaintext API key), so ` + "`internal/mcp`" + ` has no Kimi client
row and there is nothing for this plugin to duplicate.

` + "`observer init --kimi-code`" + `'s proxy-route step is unaffected and still worth
running: it adds a ` + "`base_url`" + ` under ` + "`[providers.openai]`" + ` so turns go
through the local proxy and token counts come from the wire rather than a
self-report. This plugin declares no proxy route.

The automatic detect-and-skip ` + "`observer init`" + ` performs for the Claude Code
plugin is **claude-code-only** (` + "`internal/claudeplugin`" + `); no equivalent probe
exists for Kimi. Documented, not built.

## Links

- Docs: ` + homepage + `
- Source: https://github.com/superbasedapp/observer
- License: ` + license + `
`)
	return b.String()
}
