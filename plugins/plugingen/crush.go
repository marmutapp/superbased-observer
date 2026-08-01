package main

import (
	"strings"

	"github.com/marmutapp/superbased-observer/internal/mcp"
)

// ---------------------------------------------------------------------
// Crush config listing (coverage wave B — plan §7 row "crush", verdict
// LISTING).
//
// Format grounding, first-party (github.com/charmbracelet/crush README,
// re-read 2026-07-31, plus the bundled crush-config SKILL.md the research
// note in docs/plans/plugin-coverage-research-2026-07-31.md cites):
//
//   - Config file, in the documented priority order: `.crush.json`,
//     `crush.json`, `$HOME/.config/crush/crush.json`. (The
//     `$HOME/.local/share/crush/crush.json` path is app STATE, not
//     config, and is deliberately not named as an install target here.)
//     `CRUSH_GLOBAL_CONFIG` overrides the global location.
//   - MCP servers live under the top-level `mcp` key. The README's stdio
//     example carries exactly: `type` ("stdio", required), `command`,
//     `args`, `timeout`, `disabled`, `disabled_tools`, `env`.
//   - Every config sample in the README opens with
//     `"$schema": "https://charm.land/crush.json"`, which is what makes
//     an editor autocomplete the file — so the emitted block carries it.
//   - There is no plugin/extension marketplace and no `crush mcp add`
//     verb on any first-party page: the install IS the hand-edit, which
//     is why this surface is a README and not a manifest.
//
// Deliberate omissions: `timeout`, `disabled`, `disabled_tools` and `env`
// are documented but carry no value we can honestly set — observer's
// server takes no environment, is not disabled, and has no tool we would
// pre-disable on a user's behalf. Crush also documents Claude-Code-shaped
// lifecycle hooks; observer ships no Crush hook receiver (`observer hook`
// accepts claude-code, cursor, codex and hermes), so nothing hook-shaped
// appears here.
// ---------------------------------------------------------------------

// crushSurfaceDir is this surface's directory in the in-tree layout.
const crushSurfaceDir = "crush"

// crushSchemaURL is the $schema every first-party Crush config sample
// carries.
const crushSchemaURL = "https://charm.land/crush.json"

// crushStdioType is the documented required transport discriminator.
const crushStdioType = "stdio"

// crushMCPEntry is one `mcp.<name>` entry, in the README's key order.
// Only the keys we can fill honestly are modelled — see the omissions
// note above.
type crushMCPEntry struct {
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
}

// renderCrushConfigJSON emits the copy-paste `crush.json` block. It is
// GENERATED from the canonical registrar launch, not hand-typed, so a
// changed argument reaches this snippet the moment the registrar changes.
func renderCrushConfigJSON(entry mcpServer) ([]byte, error) {
	return marshalJSON(struct {
		Schema string                   `json:"$schema"`
		MCP    map[string]crushMCPEntry `json:"mcp"`
	}{
		Schema: crushSchemaURL,
		MCP: map[string]crushMCPEntry{mcp.ServerName: {
			Type:    crushStdioType,
			Command: entry.Command,
			Args:    entry.Args,
		}},
	})
}

func crushReadme(entry mcpServer, block []byte) string {
	var b strings.Builder
	b.WriteString(generatedBanner)
	b.WriteString(`
# SuperBased Observer — Crush

Local-first token, cost and cache observability for
[Crush](https://github.com/charmbracelet/crush).

Crush has no plugin or extension marketplace: its one first-party
extension surface is the ` + "`mcp`" + ` block of its own config file. So this
surface is a **config listing** — the exact block to paste — rather than a
package to install.

**` + binaryPrereqSentence + `.** This is
wiring only — the block below declares an MCP server that runs
` + "`observer`" + `; it does not download or bundle it.

` + "```bash" + `
npm i -g @superbased/observer     # or: pipx install superbased-observer
observer start                    # run the local daemon
` + "```" + `

## Install

Merge this into your Crush config — ` + "`.crush.json`" + ` or ` + "`crush.json`" + `
in the project, or ` + "`$HOME/.config/crush/crush.json`" + ` for every project
(that is Crush's own documented lookup order; ` + "`CRUSH_GLOBAL_CONFIG`" + `
overrides the global path). If you already have an ` + "`mcp`" + ` key, add the
` + "`" + mcp.ServerName + "`" + ` entry to it rather than replacing it:

` + "```json" + `
` + string(block) + "```" + `

Restart Crush; the server starts with the session.

## What it wires

| Key | Value | Why |
|---|---|---|
| ` + "`type`" + ` | ` + "`" + crushStdioType + "`" + ` | Crush's required transport discriminator (` + "`stdio`" + ` / ` + "`sse`" + ` / ` + "`http`" + `). |
| ` + "`command`" + ` + ` + "`args`" + ` | ` + "`" + commandLine(entry) + "`" + ` | The same launch ` + "`observer init`" + ` writes for every other client, resolved from ` + "`PATH`" + `. |

Crush also documents ` + "`timeout`" + `, ` + "`disabled`" + `, ` + "`disabled_tools`" + `
and ` + "`env`" + ` on an MCP entry. None is emitted: observer's server takes no
environment, is not disabled, and pre-disabling one of its own tools on your
behalf would be a decision this listing has no business making.

The ` + "`" + mcp.ServerName + "`" + ` server exposes observer's project, session,
cost and cache queries as tools. They read the local
` + "`~/.observer/observer.db`" + `; the one exception is ` + "`continue_session`" + `,
which writes a handover file only when you pass ` + "`write_file=true`" + `. The
server makes no network calls of its own.

Crush capture is unaffected either way: observer's watcher reads Crush's own
project-local ` + "`.crush/crush.db`" + ` whether or not this entry exists.

## No hooks, and no plugin

Crush documents Claude-Code-compatible lifecycle hooks, but observer has no
Crush hook receiver — ` + "`observer hook`" + ` accepts claude-code, cursor, codex
and hermes only — so nothing hook-shaped is listed here. And there is no
Crush plugin format to build against: MCP config is the whole surface.

## Double-wiring

` + "`observer init`" + ` writes **no** Crush config today — ` + "`internal/mcp`" + `
has no Crush client row, because no guarded write path into ` + "`crush.json`" + `
is grounded — so this hand-added entry cannot duplicate anything observer
wrote. If you paste it twice under different names, Crush loads the same tool
schema twice; remove one.

The automatic detect-and-skip ` + "`observer init`" + ` performs for the Claude Code
plugin is **claude-code-only** (` + "`internal/claudeplugin`" + `); no equivalent
probe exists for Crush. Documented, not built.

## Links

- Docs: ` + homepage + `
- Source: https://github.com/superbasedapp/observer
- License: ` + license + `
`)
	return b.String()
}
