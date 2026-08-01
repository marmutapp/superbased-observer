package main

import (
	"strings"

	"github.com/marmutapp/superbased-observer/internal/mcp"
)

// ---------------------------------------------------------------------
// GitHub Copilot CLI config listing (coverage wave B — plan §7 row
// "copilot-cli", verdict LISTING).
//
// Format grounding, first-party (docs.github.com/en/copilot/how-tos/
// copilot-cli/customize-copilot/add-mcp-servers, re-read 2026-07-31):
//
//   - User-level config `~/.copilot/mcp-config.json` (the directory moves
//     with `COPILOT_HOME`). Repo-level `.mcp.json` / `.github/mcp.json`
//     are loaded automatically for a trusted folder.
//   - The top-level key is `mcpServers`.
//   - A local (stdio) entry carries `"type": "local"`, `command`, `args`,
//     `env` and `tools`; the doc's own example is
//     {"playwright": {"type":"local","command":"npx",
//      "args":["@playwright/mcp@latest"],"env":{},"tools":["*"]}}.
//   - Shell verbs: `copilot mcp add SERVER-NAME -- COMMAND [ARGS...]`
//     (local) and `copilot mcp add --transport http SERVER-NAME URL`
//     (remote), plus `copilot mcp list|get|remove` and the interactive
//     `/mcp add`.
//
// ── THE CROSS-PRODUCT TRAP: `mcpServers`, NOT `servers` ──────────────
//
// VS Code's own `.vscode/mcp.json` uses the top-level key `servers`
// (see copilot.go, the VS Code surface). GitHub Copilot CLI is a
// DIFFERENT product that reuses the `mcpServers` key, and its docs say so
// explicitly: the CLI does not read `.vscode/mcp.json` because "It uses
// the unsupported top-level key `servers`."
//
// Two GitHub-branded surfaces, two incompatible top-level keys, and the
// wrong one fails SILENTLY — the server simply never appears. So the key
// is a named constant here, and TestCopilotCLIUsesMCPServersNotServers
// pins BOTH halves of the trap (this listing must say `mcpServers` and
// must never say `servers`; the VS Code listing is pinned the other way
// round). Mutating either constant fails a test.
//
// Deliberate omissions: `env` (observer's server takes none) and every
// hook-shaped key — observer has no Copilot CLI hook receiver
// (`observer hook` accepts claude-code, cursor, codex and hermes).
// ---------------------------------------------------------------------

// copilotCLISurfaceDir is this surface's directory in the in-tree layout.
const copilotCLISurfaceDir = "copilot-cli"

// copilotCLIServersKey is Copilot CLI's documented top-level key, and
// copilotCLIRejectedKey is the VS Code spelling its docs name as
// unsupported. Both are constants so the trap is testable rather than a
// literal buried in a template.
const (
	copilotCLIServersKey  = "mcpServers"
	copilotCLIRejectedKey = "servers"
	// copilotCLILocalType is the documented transport discriminator for a
	// command/args entry.
	copilotCLILocalType = "local"
)

// copilotCLIAllTools is the documented value that enables every tool a
// server exposes. It is emitted because Copilot CLI's own example carries
// `tools` on every local entry; leaving it out would publish a config
// whose tool exposure the docs we grounded do not define.
var copilotCLIAllTools = []string{"*"}

// copilotCLIEntry is one `mcpServers.<name>` entry, in the documented key
// order.
type copilotCLIEntry struct {
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	Tools   []string `json:"tools"`
}

// renderCopilotCLIConfigJSON emits the copy-paste `mcp-config.json` block.
// The top-level key comes from copilotCLIServersKey, so the trap has one
// owner.
func renderCopilotCLIConfigJSON(entry mcpServer) ([]byte, error) {
	servers := map[string]copilotCLIEntry{mcp.ServerName: {
		Type:    copilotCLILocalType,
		Command: entry.Command,
		Args:    entry.Args,
		Tools:   copilotCLIAllTools,
	}}
	return marshalJSON(map[string]map[string]copilotCLIEntry{copilotCLIServersKey: servers})
}

func copilotCLIReadme(entry mcpServer, block []byte) string {
	addArgs := entry.Command
	if len(entry.Args) > 0 {
		addArgs += " " + strings.Join(entry.Args, " ")
	}
	var b strings.Builder
	b.WriteString(generatedBanner)
	b.WriteString(`
# SuperBased Observer — GitHub Copilot CLI

Local-first token, cost and cache observability for GitHub Copilot CLI
(` + "`@github/copilot`" + `).

Copilot CLI has no plugin package format; MCP config is its documented
extension surface. So this is a **config listing** — the exact block to
paste — rather than something to install.

**` + binaryPrereqSentence + `.** This is
wiring only — the block below declares an MCP server that runs
` + "`observer`" + `; it does not download or bundle it.

` + "```bash" + `
npm i -g @superbased/observer     # or: pipx install superbased-observer
observer start                    # run the local daemon
` + "```" + `

## Install

From the shell:

` + "```bash" + `
copilot mcp add ` + mcp.ServerName + ` -- ` + addArgs + `
` + "```" + `

Or merge this into ` + "`~/.copilot/mcp-config.json`" + ` (the directory follows
` + "`COPILOT_HOME`" + `). If the file already has an
` + "`" + copilotCLIServersKey + "`" + ` object, add the
` + "`" + mcp.ServerName + "`" + ` entry to it rather than replacing it:

` + "```json" + `
` + string(block) + "```" + `

You can also commit it as ` + "`.github/mcp.json`" + ` in a repository, which
Copilot CLI loads for that project once you trust the folder.

## ⚠️ The key is ` + "`" + copilotCLIServersKey + "`" + `, not ` + "`" + copilotCLIRejectedKey + "`" + `

This is the one thing to get right, because getting it wrong fails
**silently** — the server just never shows up.

| Product | File | Top-level key |
|---|---|---|
| GitHub Copilot **CLI** (this page) | ` + "`~/.copilot/mcp-config.json`" + ` | ` + "`" + copilotCLIServersKey + "`" + ` |
| GitHub Copilot in **VS Code** (` + "`../" + copilotSurfaceDir + "/`" + `) | ` + "`.vscode/mcp.json`" + ` | ` + "`" + copilotCLIRejectedKey + "`" + ` |

GitHub's own documentation says the CLI does not read ` + "`.vscode/mcp.json`" + `
because *it uses the unsupported top-level key* ` + "`" + copilotCLIRejectedKey + "`" + `.
Two GitHub-branded surfaces, two incompatible spellings. Copy the block from
the page that matches the product you are configuring.

## What it wires

| Key | Value | Why |
|---|---|---|
| ` + "`type`" + ` | ` + "`" + copilotCLILocalType + "`" + ` | Copilot CLI's discriminator for a command-launched server. |
| ` + "`command`" + ` + ` + "`args`" + ` | ` + "`" + commandLine(entry) + "`" + ` | The same launch ` + "`observer init`" + ` writes for every other client, resolved from ` + "`PATH`" + `. |
| ` + "`tools`" + ` | ` + "`[\"*\"]`" + ` | Every local example in GitHub's docs carries this key; it exposes the server's tools. Narrow it by listing tool names if you would rather. |

` + "`env`" + ` is documented too and is deliberately absent — observer's server
takes no environment.

The ` + "`" + mcp.ServerName + "`" + ` server exposes observer's project, session,
cost and cache queries as tools, reading the local
` + "`~/.observer/observer.db`" + `. It makes no network calls of its own.

Copilot CLI capture is unaffected either way: observer's watcher reads its
own session logs whether or not this entry exists.

## Double-wiring

` + "`observer init`" + ` writes **no** Copilot CLI config today —
` + "`internal/mcp`" + ` has no Copilot CLI client row — so this hand-added entry
cannot duplicate anything observer wrote. Adding it at BOTH user level and
repo level loads the same tool schema twice; pick one.

The automatic detect-and-skip ` + "`observer init`" + ` performs for the Claude Code
plugin is **claude-code-only** (` + "`internal/claudeplugin`" + `); no equivalent
probe exists for Copilot CLI. Documented, not built.

## Links

- Docs: ` + homepage + `
- Source: https://github.com/superbasedapp/observer
- License: ` + license + `
`)
	return b.String()
}
