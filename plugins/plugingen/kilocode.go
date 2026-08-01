package main

import (
	"strings"

	"github.com/marmutapp/superbased-observer/internal/mcp"
)

// ---------------------------------------------------------------------
// Kilo Code config listing (coverage wave B — plan §7 row
// "kilo-code (VS Code legacy)", verdict LISTING).
//
// Format grounding, first-party (kilo.ai/docs/automate/mcp/using-in-kilo-code
// and .../using-in-cli, read 2026-07-31):
//
//   - Since 7.0.33 the VS Code extension and the CLI read ONE config
//     surface: `kilo.jsonc` — global `~/.config/kilo/kilo.jsonc`, project
//     `kilo.jsonc` or `.kilo/kilo.jsonc`, project taking precedence.
//     (Kilo-Org/kilocode#6481 documents that entries in the older
//     `mcp_settings.json` are NOT migrated on upgrade — if MCP servers
//     vanished after an update, that is why.)
//   - MCP servers live under the top-level `mcp` key. A local entry is
//     `{"type":"local","command":[…],"environment":{…},"enabled":true,
//     "timeout":10000}` — note `command` is an ARRAY (argv, not
//     command+args) and the environment key is `environment`, not `env`.
//     Remote entries use `"type":"remote"` with `url`/`headers`.
//
// WHY THIS ENTRY IS REGISTRAR-DERIVED, not hand-typed: that shape is
// OpenCode's `McpLocalConfig` — Kilo CLI is a documented fork of OpenCode
// and kept its config shape. observer HAS a real OpenCode MCP registrar
// (internal/mcp's registerOpenCodeJSON), so this surface transposes the
// entry that registrar wrote into a sandbox HOME, exactly as the OpenCode
// surface does. That is the §3 one-owner rule reaching a second tool for
// free.
//
// Corroborated live 2026-07-31 (plan §7, kilo 7.3.54): our OpenCode plugin
// declared bare in `kilo.jsonc`'s `plugin` array registered the `observer`
// server through its config hook, and `kilo mcp list` showed the entry —
// i.e. Kilo accepted the OpenCode-shaped entry this listing publishes.
//
// Deliberate omissions: `environment` (observer's server takes none),
// `timeout` (the documented default applies; the server starts locally in
// milliseconds) and anything hook-shaped — observer has no Kilo hook
// receiver (`observer hook` accepts claude-code, cursor, codex, hermes).
// ---------------------------------------------------------------------

// kiloCodeSurfaceDir is this surface's directory in the in-tree layout.
const kiloCodeSurfaceDir = "kilo-code"

// renderKiloCodeConfigJSON emits the copy-paste `kilo.jsonc` block from
// the OpenCode registrar's own entry.
func renderKiloCodeConfigJSON(entry openCodeServer) ([]byte, error) {
	return marshalJSON(struct {
		MCP map[string]openCodeServer `json:"mcp"`
	}{MCP: map[string]openCodeServer{mcp.ServerName: entry}})
}

func kiloCodeReadme(entry openCodeServer, block []byte) string {
	argv := strings.Join(entry.Command, " ")
	var b strings.Builder
	b.WriteString(generatedBanner)
	b.WriteString(`
# SuperBased Observer — Kilo Code

Local-first token, cost and cache observability for
[Kilo Code](https://kilo.ai/) — both the VS Code extension
(` + "`kilocode.kilo-code`" + `) and the ` + "`kilo`" + ` CLI, which have shared one
config surface since 7.0.33.

Kilo has no MCP package format: servers are declared in its own config
file. So this is a **config listing** — the exact block to paste.

**` + binaryPrereqSentence + `.** This is
wiring only — the block below declares an MCP server that runs
` + "`observer`" + `; it does not download or bundle it.

` + "```bash" + `
npm i -g @superbased/observer     # or: pipx install superbased-observer
observer start                    # run the local daemon
` + "```" + `

## Install

Merge this into ` + "`~/.config/kilo/kilo.jsonc`" + ` (every project) or a
project's own ` + "`kilo.jsonc`" + ` / ` + "`.kilo/kilo.jsonc`" + ` (project wins).
If you already have an ` + "`mcp`" + ` key, add the
` + "`" + mcp.ServerName + "`" + ` entry to it rather than replacing it:

` + "```json" + `
` + string(block) + "```" + `

In the extension you can do the same through **Settings → MCP → Add Server**
(local stdio). Restart Kilo afterwards.

⚠️ **Upgrading past 7.0.33?** Entries in the older
` + "`mcp_settings.json`" + ` (VS Code globalStorage) are not migrated into
` + "`kilo.jsonc`" + ` — Kilo-Org/kilocode#6481. If your MCP servers disappeared
after an update, re-add them here.

## What it wires

| Key | Value | Why |
|---|---|---|
| ` + "`type`" + ` | ` + "`" + entry.Type + "`" + ` | Kilo's discriminator for a command-launched server (the other is ` + "`remote`" + `). |
| ` + "`command`" + ` | ` + "`" + argv + "`" + ` | An ARRAY here — argv, not command+args. Note the difference from every other listing in this repository. |
| ` + "`enabled`" + ` | ` + "`true`" + ` | Written verbatim by observer's own OpenCode registrar, which is where this entry comes from. |

Kilo documents ` + "`environment`" + ` (not ` + "`env`" + `) and ` + "`timeout`" + ` too.
Neither is emitted: observer's server takes no environment, and the
documented default timeout is ample for a local process.

This block is **not hand-typed**. Kilo CLI is a documented fork of OpenCode
and kept its config shape, so the entry is the one observer's real OpenCode
MCP registrar writes, transposed. A changed launch argument reaches this page
the moment that registrar changes.

The ` + "`" + mcp.ServerName + "`" + ` server exposes observer's project, session,
cost and cache queries as tools, reading the local
` + "`~/.observer/observer.db`" + `. It makes no network calls of its own.

Kilo capture is unaffected either way: observer's watcher reads Kilo's own
` + "`kilo.db`" + ` and the legacy extension's task files whether or not this entry
exists.

## Using the Kilo CLI? There is a plugin as well

Kilo's plugin API is first-party-documented as identical to OpenCode's, and
observer's OpenCode npm plugin declares this same server from a
` + "`plugin`" + ` array entry — see ` + "`../opencode/`" + `. That is a package
install rather than a config paste. Use one or the other, not both.

## Double-wiring

` + "`observer init`" + ` writes **no** Kilo config today — ` + "`internal/mcp`" + `
has no Kilo client row — so this hand-added entry cannot duplicate anything
observer wrote. It CAN duplicate the OpenCode plugin above if you install
both: that plugin's config hook adds the same key, and a second copy pasted
here loads the same tool schema twice.

The automatic detect-and-skip ` + "`observer init`" + ` performs for the Claude Code
plugin is **claude-code-only** (` + "`internal/claudeplugin`" + `); no equivalent
probe exists for Kilo. Documented, not built.

## Links

- Docs: ` + homepage + `
- Source: https://github.com/superbasedapp/observer
- License: ` + license + `
`)
	return b.String()
}
