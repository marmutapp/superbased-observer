package main

import (
	"strings"

	"github.com/marmutapp/superbased-observer/internal/mcp"
)

// ---------------------------------------------------------------------
// Kiro CLI config listing (coverage wave B — plan §7 row "kiro-cli",
// verdict LISTING).
//
// Format grounding, first-party (kiro.dev/docs/cli/mcp/ and
// /docs/cli/mcp/configuration/, as recorded in
// docs/plans/plugin-coverage-research-2026-07-31.md §"Kiro CLI"):
//
//   - Config file `mcp.json`, at user level `~/.kiro/settings/mcp.json`
//     or workspace level `<project-root>/.kiro/settings/mcp.json`.
//   - Shape: an `mcpServers` object; a local server carries `command`,
//     `args`, `env`; a remote one `url`, `headers`. Optional per entry:
//     `disabled`, `disabledTools`.
//   - `kiro-cli mcp add --name <name> --scope global --command <cmd>`
//     writes the same entry from the shell.
//
// Why this is a LISTING and not a plugin: Kiro's plugin-shaped concept is
// "Powers" (MCP + steering + hooks bundled), and kiro.dev's own FAQ states
// Powers are "currently available in the IDE" with CLI support planned —
// no manifest schema is published and no install verb exists from
// `kiro-cli`. There is nothing to build against yet, so the honest
// artifact is the config block.
//
// Kiro CLI also has lifecycle hooks (PreToolUse etc.), but they are
// per-workspace hook SCRIPTS, and observer has no Kiro hook receiver
// (`observer hook` accepts claude-code, cursor, codex and hermes), so
// nothing hook-shaped is listed.
// ---------------------------------------------------------------------

// kiroSurfaceDir is this surface's directory in the in-tree layout.
const kiroSurfaceDir = "kiro-cli"

// renderKiroMCPJSON emits the copy-paste `mcp.json` block, generated from
// the canonical registrar launch.
func renderKiroMCPJSON(entry mcpServer) ([]byte, error) {
	return marshalJSON(struct {
		MCPServers map[string]mcpServer `json:"mcpServers"`
	}{MCPServers: map[string]mcpServer{mcp.ServerName: entry}})
}

func kiroReadme(entry mcpServer, block []byte) string {
	var b strings.Builder
	b.WriteString(generatedBanner)
	b.WriteString(`
# SuperBased Observer — Kiro CLI

Local-first token, cost and cache observability for AWS
[Kiro](https://kiro.dev/docs/cli/) (` + "`kiro-cli`" + `).

Kiro's plugin-shaped concept — **Powers** — is IDE-only today (its own FAQ
says CLI support is planned) and publishes no manifest schema, so there is
nothing to package against. Kiro CLI's one documented extension surface is
its ` + "`mcp.json`" + `, which makes this a **config listing**: the exact block
to paste.

**` + binaryPrereqSentence + `.** This is
wiring only — the block below declares an MCP server that runs
` + "`observer`" + `; it does not download or bundle it.

` + "```bash" + `
npm i -g @superbased/observer     # or: pipx install superbased-observer
observer start                    # run the local daemon
` + "```" + `

## Install

Merge this into ` + "`~/.kiro/settings/mcp.json`" + ` (every workspace) or
` + "`<project>/.kiro/settings/mcp.json`" + ` (one workspace). If the file already
has an ` + "`mcpServers`" + ` object, add the ` + "`" + mcp.ServerName + "`" + ` entry to
it rather than replacing it:

` + "```json" + `
` + string(block) + "```" + `

Kiro also documents a shell verb, ` + "`kiro-cli mcp add --name … --scope global --command …`" + `,
which writes the same entry. It is not spelled out here as a one-liner: the
pages we grounded show ` + "`--command`" + ` taking a single command value and do
not document how the ` + "`" + strings.Join(entry.Args, " ") + "`" + ` argument is
passed, and guessing a flag would be worse than pasting the JSON above.

## What it wires

| Key | Value |
|---|---|
| ` + "`command`" + ` + ` + "`args`" + ` | ` + "`" + commandLine(entry) + "`" + ` — the same launch ` + "`observer init`" + ` writes for every other client, resolved from ` + "`PATH`" + `. |

Kiro documents ` + "`env`" + `, ` + "`disabled`" + ` and ` + "`disabledTools`" + ` on an
entry as well. None is emitted: observer's server takes no environment, is
not disabled, and pre-disabling one of its own tools on your behalf would be
a decision this listing has no business making.

The ` + "`" + mcp.ServerName + "`" + ` server exposes observer's project, session,
cost and cache queries as tools, reading the local
` + "`~/.observer/observer.db`" + `. It makes no network calls of its own.

Kiro capture is unaffected either way: observer's watcher reads Kiro's own
session store whether or not this entry exists.

## Double-wiring

` + "`observer init`" + ` writes **no** Kiro config today — ` + "`internal/mcp`" + `
has no Kiro client row — so this hand-added entry cannot duplicate anything
observer wrote. Adding it at BOTH user and workspace scope loads the same
tool schema twice; pick one.

The automatic detect-and-skip ` + "`observer init`" + ` performs for the Claude Code
plugin is **claude-code-only** (` + "`internal/claudeplugin`" + `); no equivalent
probe exists for Kiro. Documented, not built.

## Links

- Docs: ` + homepage + `
- Source: https://github.com/superbasedapp/observer
- License: ` + license + `
`)
	return b.String()
}
