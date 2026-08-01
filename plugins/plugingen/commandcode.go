package main

import (
	"fmt"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/mcp"
	"github.com/marmutapp/superbased-observer/internal/mcp/locate"
)

// ---------------------------------------------------------------------
// Command Code (commandcode.ai) — coverage wave C, plan §7 row
// "command-code", verdict BUILD (secondary).
//
// THE DECISION: no "Mod" package is built. This surface ships the
// registrar-grounded MCP listing instead, because the Mod would add
// nothing observer can serve today.
//
// The evidence, checked here rather than assumed:
//
//  1. **MCP is already wired by a REAL registrar.** internal/mcp/locate
//     carries a `command-code` row (~/.commandcode/mcp.json,
//     FormatMCPServersJSON) and internal/mcp's Registrar auto-detects the
//     tool by the presence of ~/.commandcode. So `observer init` writes
//     this entry itself — this page documents what it writes (transposed
//     from a sandbox run of that same registrar) for anyone who would
//     rather paste it, or who is looking at the repository instead of
//     running init.
//  2. **A Mod's hooks have no receiver.** Command Code's ModApi exposes
//     cmd.hooks.beforeToolCall / transformInput / onStop and cmd.on;
//     `observer hook <tool>` accepts claude-code, cursor, codex and
//     hermes only, so a Mod's handlers would have nothing to call.
//     internal/integration records HookMechanism None for command-code.
//  3. **Capture needs neither.** internal/adapter/commandcode reads
//     Command Code's own CC-shaped JSONL under ~/.commandcode/projects,
//     nets GROSS input against cacheRead, and carries the provider's own
//     costUsd. A Mod would re-report what the watcher already has.
//
// Format grounding for the listing itself, first-party
// (commandcode.ai/docs/mcp, corroborated by this repository's own
// grounding of the npm package's `getUserMcpConfigPath` + bundled
// reference/mcp.md on the locate row): `~/.commandcode/mcp.json`,
// `{"mcpServers": {"<name>": {command, args, env}}}`. Project scope is
// `.commandcode/mcp.json`.
//
// Mods grounding, for the record (commandcode.ai/docs/mods): manifest is
// a `package.json` "commandcode" key, {"mods": ["./src/review.ts", …]};
// a mod default-exports (cmd: ModApi) => {…}; install with
// `cmd mods add npm:<pkg>` / `<owner>/<repo>` / `./local-path`. Real, and
// buildable the moment observer has something to put in it.
// ---------------------------------------------------------------------

// commandCodeSurfaceDir is this surface's directory in the in-tree layout.
const commandCodeSurfaceDir = "command-code"

// readCommandCodeEntry pulls the "observer" entry out of
// ~/.commandcode/mcp.json after the real registrar wrote it. It reuses
// readMCPEntry, which rejects any key the mcpServer struct does not model
// — the guard that stops a registrar field being silently dropped on the
// way into a published listing.
func readCommandCodeEntry(home string) (mcpServer, error) {
	if _, ok := locate.ForClient("command-code", home); !ok {
		return mcpServer{}, fmt.Errorf("plugingen: locate has no command-code row — the command-code listing has no registrar to transpose")
	}
	return readMCPEntry("command-code", home)
}

// renderCommandCodeMCPJSON emits the copy-paste `~/.commandcode/mcp.json`
// block from the entry the real registrar wrote into the sandbox HOME.
func renderCommandCodeMCPJSON(entry mcpServer) ([]byte, error) {
	return marshalJSON(struct {
		MCPServers map[string]mcpServer `json:"mcpServers"`
	}{MCPServers: map[string]mcpServer{mcp.ServerName: entry}})
}

func commandCodeReadme(entry mcpServer, block []byte) string {
	var b strings.Builder
	b.WriteString(generatedBanner)
	b.WriteString(`
# SuperBased Observer — Command Code

Local-first token, cost and cache observability for
[Command Code](https://commandcode.ai/) (` + "`cmd`" + `).

Command Code is one of the few tools here that ` + "`observer init`" + `
already wires by itself — this page documents what it writes, and why the
second extension surface (Mods) deliberately ships nothing.

**` + binaryPrereqSentence + `.** This is
wiring only — the block below declares an MCP server that runs
` + "`observer`" + `; it does not download or bundle it.

` + "```bash" + `
npm i -g @superbased/observer     # or: pipx install superbased-observer
observer start                    # run the local daemon
` + "```" + `

## Install

The short version — ` + "`observer init`" + ` does it for you. It detects
Command Code by the presence of ` + "`~/.commandcode/`" + ` and writes the entry
below into ` + "`~/.commandcode/mcp.json`" + ` (leaving any other server in that
file untouched):

` + "```bash" + `
observer init
` + "```" + `

To do it by hand instead — or to add it per project as
` + "`.commandcode/mcp.json`" + ` — merge this:

` + "```json" + `
` + string(block) + "```" + `

## What it wires

| Key | Value |
|---|---|
| ` + "`command`" + ` + ` + "`args`" + ` | ` + "`" + commandLine(entry) + "`" + ` — resolved from ` + "`PATH`" + `. |

This block is **not hand-typed**: it is the entry observer's own MCP
registrar writes, read back out of a throwaway sandbox home. The one
difference from what ` + "`observer init`" + ` puts on your machine is the
binary path — init writes the absolute path of the binary that ran it, and a
published listing has to resolve ` + "`observer`" + ` from ` + "`PATH`" + `
instead.

The ` + "`" + mcp.ServerName + "`" + ` server exposes observer's project, session,
cost and cache queries as tools, reading the local
` + "`~/.observer/observer.db`" + `. It makes no network calls of its own.
Command Code capture is unaffected either way: observer's watcher reads its
JSONL sessions under ` + "`~/.commandcode/projects`" + ` regardless.

## Why there is no Mod

Command Code's other extension surface is **Mods** — single-file TypeScript
modules declared by a ` + "`commandcode`" + ` key in a ` + "`package.json`" + ` and
installed with ` + "`cmd mods add`" + `. It is a real, first-party API (Command
Code builds its own providers on it), and observer ships no Mod for it, for
one reason: there is nothing to put inside.

- A Mod's job would be to declare the MCP server — which the config file
  above already does, through a registrar observer actually owns.
- The other half of the ModApi is lifecycle hooks
  (` + "`beforeToolCall`" + `, ` + "`transformInput`" + `, ` + "`onStop`" + `). Observer
  has no Command Code hook receiver: ` + "`observer hook`" + ` accepts
  claude-code, cursor, codex and hermes. A Mod's handlers would have nothing
  to call.

So a Mod would be a package whose only content is a second spelling of an
entry that already exists. If observer grows a Command Code hook receiver,
that changes, and the Mod becomes worth publishing.

## Double-wiring

` + "`observer init`" + ` writes this exact server into
` + "`~/.commandcode/mcp.json`" + `. If you also paste the block above into the
project-scoped ` + "`.commandcode/mcp.json`" + `, Command Code loads the same
tool schema twice — harmless to your data, wasteful of context. Keep one.

The automatic detect-and-skip ` + "`observer init`" + ` performs for the Claude Code
plugin is **claude-code-only** (` + "`internal/claudeplugin`" + `); no equivalent
probe exists here. Documented, not built.

## Links

- Docs: ` + homepage + `
- Source: https://github.com/superbasedapp/observer
- License: ` + license + `
`)
	return b.String()
}
