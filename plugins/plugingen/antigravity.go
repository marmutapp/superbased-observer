package main

import (
	"strings"

	"github.com/marmutapp/superbased-observer/internal/mcp"
)

// ---------------------------------------------------------------------
// Antigravity plugin (coverage wave A — plan §7 row "antigravity (agy)").
//
// Format grounding, first-party (antigravity.google/docs/plugins,
// /docs/cli/plugins, /docs/mcp and /docs/hooks, all read 2026-07-31):
//
//   - A plugin is a DIRECTORY whose root carries `plugin.json` — "Required
//     marker file". The only documented manifest field is `name`, and
//     "The name field is optional and defaults to the directory name if
//     omitted"; the CLI page adds that it "must match the pattern
//     ^[a-zA-Z0-9-_]+$". Optional siblings at the plugin root:
//     `mcp_config.json`, `hooks.json`, `skills/`, `agents/`, `rules/`.
//   - `mcp_config.json` uses "a single mcpServers object". stdio entry
//     keys: `command` (required), `args`, `env`, `cwd`. Remote entries use
//     `serverUrl` — with an explicit warning that "url or httpUrl are not
//     supported", which is the cross-vendor trap on this format. Optional
//     either way: `disabled`, `disabledTools`.
//   - Plugin directories: workspace `.agents/plugins/` or
//     `_agents/plugins/`, global `~/.gemini/config/plugins/`. The CLI's
//     `agy plugin install /path/to/local/plugin` stages a package
//     directory into `~/.gemini/antigravity-cli/plugins/<plugin_name>/`;
//     `agy plugin list|enable|disable|uninstall` manage it.
//   - This is NOT the Gemini CLI extension format. Gemini CLI installs a
//     `gemini-extension.json` into `~/.gemini/extensions/<name>/`; that
//     surface is generated in gemini.go and is a different artifact.
//
// ── ⚠️ THE `.agents/plugins/` COLLISION — investigated, and AVOIDED ──
//
// Antigravity's WORKSPACE plugin directory is `.agents/plugins/`. Codex's
// repo-scoped marketplace catalog is the FILE
// `$REPO_ROOT/.agents/plugins/marketplace.json` (codex.go), and
// scripts/assemble-plugins-repo.sh already places it at the public
// repository root. Same directory, two vendors.
//
// What is actually true, and what is only inferred:
//
//   - GROUNDED: Codex reads one named file, `marketplace.json`, and its
//     catalog names its plugins explicitly by `source.path`. A sibling
//     DIRECTORY in `.agents/plugins/` is not something Codex ingests.
//     So the Codex side of the collision is safe.
//   - NOT GROUNDED: what Antigravity does with a loose FILE sitting in a
//     plugins directory. Its docs say only that it "automatically scans
//     these directories to discover and load your customizations", and
//     define a plugin as a directory containing `plugin.json`. Whether a
//     stray `marketplace.json` is silently ignored, warned about, or
//     treated as a malformed entry is simply not documented. We will not
//     assert "safe" on an inference.
//
// The decision that makes the question moot: **we do not put the
// Antigravity plugin under `.agents/plugins/` at all.** We can afford
// not to, because — unlike Claude Code, Codex and Gemini CLI —
// Antigravity's documented install path does NOT resolve anything from a
// repository root. `agy plugin install <local path>` takes a directory
// and stages it into `~/.gemini/antigravity-cli/plugins/<name>/`; the
// alternative is copying the folder into the user's OWN
// `~/.gemini/config/plugins/` (global) or their own workspace's
// `.agents/plugins/`. Neither needs our repository to imitate a user's
// workspace. So this surface ships as a plain directory
// (plugins/antigravity/<plugin>/), the README tells the user which of
// THEIR directories to place it in, and the public repository root's
// `.agents/plugins/` keeps containing exactly the one file Codex
// documents.
//
// The residual, stated rather than hidden: a user who CLONES the public
// plugins repository and opens it as an Antigravity workspace has a
// `.agents/plugins/marketplace.json` in scope. That file is Codex's and
// predates this surface; the mitigation is that we add nothing to that
// directory and never tell anyone to open the repository as a workspace.
// If Google ever documents the enumeration rule, this decision can be
// revisited with evidence — until then, coexistence is UNVERIFIED, not
// disproven, and the layout simply does not depend on it.
//
// Deliberate omissions:
//
//   - `hooks.json`. Antigravity documents plugin-root hooks (events
//     PreToolUse / PostToolUse / PreInvocation / PostInvocation / Stop,
//     handlers {type, command, timeout}). Observer has NO Antigravity
//     hook receiver: `observer hook <tool>` accepts claude-code, cursor,
//     codex and hermes, and internal/integration records HookMechanism
//     None for both `antigravity` and `antigravity-cli`. Emitting a
//     hooks.json would declare a command that does not exist — the exact
//     fabrication the honesty rule forbids. Antigravity capture does not
//     need it: the `agy` CLI writes a plaintext-protobuf SQLite `.db`
//     that observer's watcher parses directly.
//   - `version`. plugin.json documents exactly one field, `name`. There is
//     no version key to stamp, so this is the one wave-A surface that
//     carries no release number at all.
//   - `skills/`, `agents/`, `rules/`. Observer ships none of them.
// ---------------------------------------------------------------------

// antigravitySurfaceDir is this surface's directory in the in-tree layout.
const antigravitySurfaceDir = "antigravity"

// antigravityPluginManifest is plugin.json. `name` is the ONLY documented
// field, so it is the only one emitted. It is set explicitly rather than
// relying on the documented directory-name default, because the staged
// install directory (~/.gemini/antigravity-cli/plugins/<plugin_name>/) and
// the `agy plugin enable|disable|uninstall <plugin_name>` argument are both
// taken from it — leaving it implicit would make the CLI argument depend on
// whatever the user named the folder they copied.
type antigravityPluginManifest struct {
	Name string `json:"name"`
}

func renderAntigravityPluginJSON() ([]byte, error) {
	return marshalJSON(antigravityPluginManifest{Name: pluginName})
}

// renderAntigravityMCPConfigJSON emits the plugin-root `mcp_config.json`.
// The stdio keys Antigravity documents (command, args, env, cwd) are a
// superset of what the mcpServer struct carries, so the canonical entry
// transposes without loss.
func renderAntigravityMCPConfigJSON(entry mcpServer) ([]byte, error) {
	return marshalJSON(struct {
		MCPServers map[string]mcpServer `json:"mcpServers"`
	}{MCPServers: map[string]mcpServer{mcp.ServerName: entry}})
}

func antigravityReadme(entry mcpServer) string {
	var b strings.Builder
	b.WriteString(generatedBanner)
	b.WriteString(`
# SuperBased Observer — Antigravity plugin

Local-first token, cost and cache observability for Google Antigravity
(the IDE and the ` + "`agy`" + ` CLI, which share one plugin format).

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
├── plugin.json        ← the required marker file (name only)
└── mcp_config.json    ← the MCP server it declares
` + "```" + `

## Install

Clone or download this repository, then either let the CLI stage it:

` + "```bash" + `
agy plugin install ./` + antigravitySurfaceDir + `/` + pluginDir + `
` + "```" + `

— which copies it into ` + "`~/.gemini/antigravity-cli/plugins/" + pluginName + "/`" + ` —
or copy the folder into one of the two documented plugin directories yourself
and restart:

` + "```bash" + `
# active in every workspace
cp -r ` + antigravitySurfaceDir + `/` + pluginDir + ` ~/.gemini/config/plugins/

# active in one workspace only
cp -r ` + antigravitySurfaceDir + `/` + pluginDir + ` <your-project>/.agents/plugins/
` + "```" + `

Then ` + "`agy plugin list`" + ` to confirm, and
` + "`agy plugin disable|enable|uninstall " + pluginName + "`" + ` to manage it. The
argument is the manifest's ` + "`name`" + `, which is why this plugin sets it
explicitly instead of inheriting the folder name.

## Why this plugin is NOT shipped at the repository root

Antigravity's workspace plugin directory is ` + "`.agents/plugins/`" + ` — the same
directory Codex reads its ` + "`marketplace.json`" + ` catalog from, and that catalog
is at the root of the published plugins repository. This surface stays out of
that directory on purpose.

It can, because Antigravity — unlike Claude Code, Codex and the Gemini CLI —
resolves nothing from a repository root. Its documented install takes a
**local directory** and stages it under your own home, so there is no reason
for this repository to imitate a user's workspace. Codex only ever reads the
one file it names, so it is unaffected either way; what is **not** documented
anywhere is what Antigravity does with a loose file sitting in a plugins
directory, and we would rather not find out on your machine. Keeping the two
apart costs nothing and needs no assumption.

## What it wires

| Component | What it declares |
|---|---|
| ` + "`plugin.json`" + ` | ` + "`name`" + `: ` + "`" + pluginName + "`" + ` — the required marker file. It is the only documented field, so it is the only one written. |
| ` + "`mcp_config.json`" + ` | The ` + "`" + mcp.ServerName + "`" + ` MCP server: ` + "`" + commandLine(entry) + "`" + ` — on-demand project/session/cost queries from inside Antigravity. |

Note for anyone hand-editing this file later: Antigravity's remote MCP entries
use ` + "`serverUrl`" + `, and its docs warn that ` + "`url`" + ` and ` + "`httpUrl`" + `
"are not supported". Ours is a stdio entry, so the trap does not apply here —
but it is the one place this format differs from the others in this repository.

**No ` + "`hooks.json`" + ` is declared.** Antigravity documents plugin-root hooks,
but observer has no Antigravity hook receiver — ` + "`observer hook`" + ` accepts
claude-code, cursor, codex and hermes only — so a hooks file would declare a
command that does not exist. Antigravity capture works without it: the
` + "`agy`" + ` CLI writes a plaintext-protobuf SQLite database that observer's
watcher parses directly.

There is also no version number in this plugin, and that is the format, not an
omission: Antigravity's ` + "`plugin.json`" + ` documents exactly one field.

Captured data lands in the local ` + "`~/.observer/observer.db`" + `. The MCP server
makes no network calls of its own.

## This is not the Gemini CLI extension

They are different artifacts for different tools that happen to share a vendor
and a config directory. The Gemini CLI extension lives in ` + "`../gemini/`" + ` as a
` + "`gemini-extension.json`" + ` installed into ` + "`~/.gemini/extensions/`" + `;
this plugin is Antigravity's own format, installed into
` + "`~/.gemini/config/plugins/`" + ` or ` + "`~/.gemini/antigravity-cli/plugins/`" + `.
Installing one does not install the other, and installing both is fine — they
wire different tools.

## Double-wiring

` + "`observer init`" + ` writes **no** Antigravity config today —
` + "`internal/mcp`" + ` has no Antigravity client row — so this plugin cannot
duplicate anything observer wrote. If you have separately added the server to
` + "`~/.gemini/config/mcp_config.json`" + ` (global) or ` + "`.agents/mcp_config.json`" + `
(workspace), remove one of the two: Antigravity would otherwise load the same
tool schema twice per turn.

The automatic detect-and-skip ` + "`observer init`" + ` performs for the Claude Code
plugin is **claude-code-only** (` + "`internal/claudeplugin`" + `); no equivalent probe
exists for Antigravity. Documented, not built.

## Links

- Docs: ` + homepage + `
- Source: https://github.com/superbasedapp/observer
- License: ` + license + `
`)
	return b.String()
}
