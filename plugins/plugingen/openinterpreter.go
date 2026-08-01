package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/mcp"
)

// ---------------------------------------------------------------------
// Open Interpreter config listing (coverage wave B — plan §7 row
// "open-interpreter", verdict BUILD (config)).
//
// Format grounding, first-party (openinterpreter.com/docs/terminal/mcp,
// as recorded in docs/plans/plugin-coverage-research-2026-07-31.md
// §"Open Interpreter"):
//
//   - Config file `~/.openinterpreter/config.toml` — its OWN home, not
//     `~/.codex`, despite the binary being a rebadged Codex CLI Rust
//     build. The research pass confirmed this from the project's README.
//   - MCP servers are `[mcp_servers.<name>]` TOML tables carrying
//     `command`, `args`, `env`, plus per-server `enabled_tools` /
//     `disabled_tools` / `startup_timeout_sec` / `tool_timeout_sec` /
//     `required` / `enabled`.
//   - CLI: `interpreter mcp list|get|remove|login|logout`; TUI `/mcp`.
//   - NO plugin or marketplace surface exists: the research pass walked
//     the whole first-party docs sidebar and `/docs/terminal/extensions`
//     and `/docs/terminal/plugins` both 404. MCP config is the entire
//     extension surface, which is why this is a listing.
//
// ── TRANSPOSED FROM THE CODEX REGISTRAR, LITERALLY ───────────────────
//
// The format is structurally identical to Codex's own `[mcp_servers.*]`
// table (shared lineage), and observer HAS a real Codex MCP registrar
// (internal/mcp's registerCodexTOML). internal/mcp/locate has no
// `open-interpreter` row — internal/integration records `MCP: nil` for it
// with the note that no writer exists yet — so there is no registrar of
// its OWN to drive.
//
// What this surface publishes is therefore not a hand-typed table: it is
// the EXACT TEXT the Codex registrar wrote into the sandbox
// `~/.codex/config.toml`, lifted verbatim (header included, since the
// table name is the same) and documented as belonging in
// `~/.openinterpreter/config.toml` instead. A changed argument or a new
// key in the registrar's TOML writer reaches this page in the same run,
// and TestOpenInterpreterBlockParsesToTheCodexEntry pins that the lifted
// text still decodes to the entry the registrar wrote.
//
// Deliberate omissions: every optional per-server key. observer's server
// takes no environment, needs no startup-timeout override, and
// pre-disabling one of its own tools on a user's behalf is not a
// listing's decision.
// ---------------------------------------------------------------------

// openInterpreterSurfaceDir is this surface's directory in the in-tree
// layout.
const openInterpreterSurfaceDir = "open-interpreter"

// codexServerTableHeader is the TOML table both Codex and Open Interpreter
// read the observer server from.
var codexServerTableHeader = "[mcp_servers." + mcp.ServerName + "]"

// codexServerTablePath is the observer entry's dotted key path — the
// table header without its brackets.
var codexServerTablePath = "mcp_servers." + mcp.ServerName

// isCodexServerSubTableHeader reports whether a trimmed line is the header
// of a table NESTED under [mcp_servers.observer] — `[mcp_servers.observer.env]`
// for a map-valued key, `[[mcp_servers.observer.x]]` for an array of
// tables. Those belong to the entry and must be lifted WITH it; any other
// header ends the block.
func isCodexServerSubTableHeader(trimmed string) bool {
	if !strings.HasPrefix(trimmed, "[") {
		return false
	}
	closing := strings.Index(trimmed, "]")
	if closing < 0 {
		return false
	}
	inner := strings.TrimSpace(strings.TrimLeft(trimmed[:closing], "["))
	return strings.HasPrefix(inner, codexServerTablePath+".")
}

// extractCodexServerTable lifts the [mcp_servers.observer] table out of the
// config.toml the Codex MCP registrar just wrote in the sandbox HOME. The
// text is returned verbatim (one trailing newline) so the published snippet
// is the registrar's own output rather than a re-rendering of it.
//
// NESTED TABLES COUNT AS PART OF THE ENTRY. A TOML table ends at the next
// header, but a map-valued key on the entry (an `env` table, say) is
// EMITTED as its own header — `[mcp_servers.observer.env]`, indented,
// after the entry's scalar keys. Stopping at the first `[` would publish
// a truncated entry that silently dropped it. The registrar writes only
// command + args today, so this is future-proofing rather than a live
// fix; TestOpenInterpreterExtractorCarriesNestedTables drives it with a
// synthetic env table so the behaviour is pinned before it matters.
func extractCodexServerTable(home string) (string, error) {
	path := filepath.Join(home, ".codex", "config.toml")
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path is inside the generator's own sandbox HOME.
	if err != nil {
		return "", fmt.Errorf("plugingen: read codex config for the open-interpreter listing: %w", err)
	}
	block, err := liftCodexServerTable(string(raw))
	if err != nil {
		return "", err
	}
	if !strings.Contains(block, `"`+pathBinary+`"`) {
		return "", fmt.Errorf("plugingen: the lifted %s table does not name the %q binary:\n%s",
			codexServerTableHeader, pathBinary, block)
	}
	return block, nil
}

// liftCodexServerTable is extractCodexServerTable's pure half: given a
// whole config.toml it returns the [mcp_servers.observer] table text plus
// every table nested beneath it, verbatim, with one trailing newline.
func liftCodexServerTable(body string) (string, error) {
	lines := strings.Split(body, "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == codexServerTableHeader {
			start = i
			break
		}
	}
	if start < 0 {
		return "", fmt.Errorf("plugingen: codex config has no %s table to transpose", codexServerTableHeader)
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(trimmed, "[") {
			continue
		}
		if isCodexServerSubTableHeader(trimmed) {
			continue
		}
		end = i
		break
	}
	return strings.TrimRight(strings.Join(lines[start:end], "\n"), "\n") + "\n", nil
}

func openInterpreterReadme(entry mcpServer, block string) string {
	var b strings.Builder
	b.WriteString(generatedBanner)
	b.WriteString(`
# SuperBased Observer — Open Interpreter

Local-first token, cost and cache observability for
[Open Interpreter](https://www.openinterpreter.com/docs/terminal)
(` + "`interpreter`" + `).

Open Interpreter documents no plugin or extension mechanism at all — MCP
config is its entire extension surface. So this is a **config listing**: the
exact TOML table to paste.

**` + binaryPrereqSentence + `.** This is
wiring only — the table below declares an MCP server that runs
` + "`observer`" + `; it does not download or bundle it.

` + "```bash" + `
npm i -g @superbased/observer     # or: pipx install superbased-observer
observer start                    # run the local daemon
` + "```" + `

## Install

Append this to ` + "`~/.openinterpreter/config.toml`" + `:

` + "```toml" + `
` + block + "```" + `

Then check it with ` + "`interpreter mcp list`" + `, or ` + "`/mcp`" + ` inside the
TUI.

(The indentation is the TOML writer's, not a typo — leading whitespace before
a table header and its keys is legal TOML, and this block is reproduced
exactly as observer's own writer emits it. See "Where this block comes from".)

## What it wires

| Key | Value |
|---|---|
| ` + "`command`" + ` + ` + "`args`" + ` | ` + "`" + commandLine(entry) + "`" + ` — the same launch ` + "`observer init`" + ` writes for every other client, resolved from ` + "`PATH`" + `. |

Open Interpreter also documents ` + "`env`" + `, ` + "`enabled`" + `,
` + "`required`" + `, ` + "`enabled_tools`" + `, ` + "`disabled_tools`" + `,
` + "`startup_timeout_sec`" + ` and ` + "`tool_timeout_sec`" + ` on a server table.
None is emitted: observer's server takes no environment, starts locally in
milliseconds, and pre-disabling one of its own tools on your behalf is not
this listing's decision to make.

The ` + "`" + mcp.ServerName + "`" + ` server exposes observer's project, session,
cost and cache queries as tools, reading the local
` + "`~/.observer/observer.db`" + `. It makes no network calls of its own.

Open Interpreter capture is unaffected either way: observer's watcher reads
its rollout JSONL under ` + "`~/.openinterpreter/sessions`" + ` whether or not
this table exists.

## Where this block comes from

It is **not hand-typed**. Open Interpreter is a rebadged Codex CLI build and
shares Codex's ` + "`[mcp_servers.<name>]`" + ` config format, so the text above
is the exact table observer's real Codex MCP registrar writes — lifted
verbatim out of a throwaway sandbox home and documented against
` + "`~/.openinterpreter/config.toml`" + ` instead of ` + "`~/.codex/config.toml`" + `.
A changed launch argument reaches this page automatically.

The one difference from what ` + "`observer init --codex`" + ` writes is the file
it goes in: Open Interpreter keeps "product-only config and session state
local under ` + "`~/.openinterpreter`" + `", its own home. ` + "`observer init`" + `
does not write that file — there is no Open Interpreter client row in
` + "`internal/mcp`" + ` — which is why this is a paste rather than a command.

## Double-wiring

` + "`observer init`" + ` writes **no** Open Interpreter config, so this
hand-added table cannot duplicate anything observer wrote. Note that
` + "`observer init --codex`" + ` writes the SAME table into
` + "`~/.codex/config.toml`" + ` for the Codex CLI — a different tool reading a
different file, not a duplicate.

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
