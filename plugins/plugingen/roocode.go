package main

import (
	"strings"

	"github.com/marmutapp/superbased-observer/internal/mcp"
)

// ---------------------------------------------------------------------
// Roo Code config listing (coverage wave B — plan §7 row "roo-code",
// verdict LISTING).
//
// Format grounding, first-party (docs.roocode.com/features/mcp/*, plus
// docs.roocode.com/features/marketplace and RooCodeInc/Roo-Code PR #4538
// / issue #5384, as recorded in
// docs/plans/plugin-coverage-research-2026-07-31.md §"Roo Code"):
//
//   - Roo Code is a Cline fork and kept Cline's MCP config shape:
//     `{"mcpServers": {"<name>": {"command", "args", "env",
//     "alwaysAllow", "disabled"}}}`.
//   - Two scopes: project `.roo/mcp.json` (checked in with the repo) and
//     global `mcp_settings.json` in the extension's settings directory.
//     Project scope takes precedence.
//   - A Marketplace exists in-extension for MCP servers and Modes, and it
//     really does one-click installs.
//
// WHY NO MARKETPLACE ARTIFACT IS GENERATED: Roo's marketplace items are
// fetched from Roo's own API, and SUBMISSION is a GitHub issue on the main
// repository (tracked under #5384) — there is no public submission repo
// and no published schema for the metadata file one closed issue hints at.
// Generating a manifest whose shape we would have to infer is exactly the
// guess this generator exists to prevent. Filing that issue is an
// operator-gated public action, not a build step. Cline's own
// `cline/mcp-marketplace` submission channel (plan §7, verdict LISTING for
// the same reason) is the same shape of action.
//
// Deliberate omissions: `env` (observer's server takes none),
// `alwaysAllow` (an auto-approval list is the user's trust decision to
// make, never a listing's) and `disabled`.
// ---------------------------------------------------------------------

// rooSurfaceDir is this surface's directory in the in-tree layout.
const rooSurfaceDir = "roo-code"

// renderRooMCPJSON emits the copy-paste `.roo/mcp.json` block.
func renderRooMCPJSON(entry mcpServer) ([]byte, error) {
	return marshalJSON(struct {
		MCPServers map[string]mcpServer `json:"mcpServers"`
	}{MCPServers: map[string]mcpServer{mcp.ServerName: entry}})
}

func rooReadme(entry mcpServer, block []byte) string {
	var b strings.Builder
	b.WriteString(generatedBanner)
	b.WriteString(`
# SuperBased Observer — Roo Code

Local-first token, cost and cache observability for
[Roo Code](https://docs.roocode.com/) (VS Code).

Roo Code has an in-extension Marketplace, but its submission channel is a
GitHub issue on the main repository rather than a public schema we could
package against — so this surface is a **config listing**: the exact block
to paste.

**` + binaryPrereqSentence + `.** This is
wiring only — the block below declares an MCP server that runs
` + "`observer`" + `; it does not download or bundle it.

` + "```bash" + `
npm i -g @superbased/observer     # or: pipx install superbased-observer
observer start                    # run the local daemon
` + "```" + `

## Install

Put this in ` + "`.roo/mcp.json`" + ` at the root of a project (project scope,
shareable with the repository), or in the global ` + "`mcp_settings.json`" + `
Roo Code opens from **MCP Servers → Edit Global MCP** (every project). If
either file already has an ` + "`mcpServers`" + ` object, add the
` + "`" + mcp.ServerName + "`" + ` entry to it rather than replacing it:

` + "```json" + `
` + string(block) + "```" + `

Project scope wins over global scope for the same server name.

## What it wires

| Key | Value |
|---|---|
| ` + "`command`" + ` + ` + "`args`" + ` | ` + "`" + commandLine(entry) + "`" + ` — the same launch ` + "`observer init`" + ` writes for every other client, resolved from ` + "`PATH`" + `. |

Roo Code inherited Cline's entry shape, which also documents ` + "`env`" + `,
` + "`alwaysAllow`" + ` and ` + "`disabled`" + `. None is emitted: observer's server
takes no environment, and an auto-approval list is your trust decision to
make, not something a published listing should make for you.

The ` + "`" + mcp.ServerName + "`" + ` server exposes observer's project, session,
cost and cache queries as tools, reading the local
` + "`~/.observer/observer.db`" + `. It makes no network calls of its own.

## Why there is no Marketplace entry

Roo Code's Marketplace is real and installs MCP servers in one click, but
items are served from Roo's own API and submissions are filed as GitHub
issues on ` + "`RooCodeInc/Roo-Code`" + ` — no public submission repository, and
no published schema for the metadata a submission carries. Inventing that
file is the kind of guess this repository's generator exists to prevent.
Filing the issue is an operator decision, not a build artifact.

## Double-wiring

` + "`observer init`" + ` writes **no** Roo Code config today —
` + "`internal/mcp`" + ` has no Roo client row — so this hand-added entry cannot
duplicate anything observer wrote. Adding it at BOTH project and global
scope is fine (project wins), but adding it twice under different names
loads the same tool schema twice.

The automatic detect-and-skip ` + "`observer init`" + ` performs for the Claude Code
plugin is **claude-code-only** (` + "`internal/claudeplugin`" + `); no equivalent
probe exists for Roo Code. Documented, not built.

## Links

- Docs: ` + homepage + `
- Source: https://github.com/superbasedapp/observer
- License: ` + license + `
`)
	return b.String()
}
