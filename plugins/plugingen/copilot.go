package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/mcp"
)

// ---------------------------------------------------------------------
// GitHub Copilot in VS Code — the first-party one-click install URI
// (coverage wave C — plan §7 row "copilot (VS Code)", verdict BUILD).
//
// Format grounding, first-party (code.visualstudio.com/api/
// extension-guides/ai/mcp, re-read 2026-07-31; the Copilot half from
// docs.github.com/copilot/…/extending-copilot-chat-with-mcp):
//
//   - The URI scheme is `vscode:mcp/install?{json-configuration}`, with
//     `vscode-insiders:mcp/install?{json-configuration}` for Insiders.
//   - The payload is the server configuration "in the form
//     {"name":"server-name","command":...}", and the page's own example
//     builds it as
//       const link = `vscode:mcp/install?${encodeURIComponent(JSON.stringify(obj))}`;
//     — i.e. compact JSON, then encodeURIComponent, and the link "can be
//     used in a browser, or opened on the command line".
//   - The config-file shape (`.vscode/mcp.json`, or the user profile for
//     every workspace) uses the top-level key `servers`, and each entry
//     carries `type` (e.g. "stdio"), `command`, `args`.
//
// The doc's install-URI example spells only `name` and `command`; the
// remaining keys of the payload are the documented SERVER-ENTRY keys
// above. That is a transposition, not an invention, and it is stated here
// rather than assumed silently.
//
// ── THE ENCODING, AND WHY IT IS NOT url.QueryEscape ──────────────────
//
// The documented encoder is JavaScript's encodeURIComponent. Go's
// url.QueryEscape differs from it in two ways that matter to a link a
// user clicks: it writes a space as `+` (encodeURIComponent writes
// `%20`), and it percent-encodes `!'()*`, which encodeURIComponent leaves
// alone. Our payload contains none of those characters today, but a
// future argument could — so this file implements encodeURIComponent
// exactly rather than relying on the difference staying invisible.
// TestCopilotInstallURIDecodesToTheSamePayload decodes the emitted link
// back and requires byte-equal JSON.
//
// Not built here: a `.vscode/mcp.json` FILE. The workspace config is a
// user's own repository file, not something this repository ships; the
// README carries the snippet instead, which is also where the
// `servers`-vs-`mcpServers` trap gets named (Copilot CLI, the sibling
// surface in ../copilot-cli/, uses the other key).
// ---------------------------------------------------------------------

// copilotSurfaceDir is this surface's directory in the in-tree layout.
const copilotSurfaceDir = "copilot"

// copilotVSCodeServersKey is VS Code's documented top-level key — the
// opposite of Copilot CLI's `mcpServers` (see copilotcli.go). It is a
// constant so the cross-product trap is pinned by a test on both sides.
const copilotVSCodeServersKey = "servers"

// copilotStdioType is the documented transport discriminator for a
// command-launched server in `.vscode/mcp.json`.
const copilotStdioType = "stdio"

// copilotInstallScheme / copilotInsidersScheme are the documented URI
// prefixes.
const (
	copilotInstallScheme  = "vscode:mcp/install?"
	copilotInsidersScheme = "vscode-insiders:mcp/install?"
)

// copilotInstallPayload is the object the install URI carries: the
// documented `name`, plus the documented server-entry keys.
type copilotInstallPayload struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
}

// encodeURIComponent implements JavaScript's encodeURIComponent: every
// byte outside the unreserved set is percent-encoded with UPPERCASE hex,
// over the UTF-8 bytes. The unreserved set is the one the ECMAScript spec
// names — alphanumerics plus `-_.!~*'()`.
func encodeURIComponent(s string) string {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.!~*'()"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if strings.IndexByte(unreserved, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// jsonMarshalCompact renders v as compact JSON with no trailing newline
// and no HTML escaping — the byte-for-byte equivalent of the JSON.stringify
// the VS Code recipe calls before encoding.
func jsonMarshalCompact(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// copilotInstallPayloadJSON renders the compact JSON the URI carries.
// json.Marshal (not marshalJSON) is deliberate: the documented recipe is
// JSON.stringify, which is compact and has no trailing newline.
func copilotInstallPayloadJSON(entry mcpServer) ([]byte, error) {
	raw, err := jsonMarshalCompact(copilotInstallPayload{
		Name:    mcp.ServerName,
		Type:    copilotStdioType,
		Command: entry.Command,
		Args:    entry.Args,
	})
	if err != nil {
		return nil, fmt.Errorf("plugingen: marshal copilot install payload: %w", err)
	}
	return raw, nil
}

// copilotInstallURI builds the one-click link for stable VS Code.
func copilotInstallURI(entry mcpServer) (string, error) {
	payload, err := copilotInstallPayloadJSON(entry)
	if err != nil {
		return "", err
	}
	return copilotInstallScheme + encodeURIComponent(string(payload)), nil
}

// renderCopilotWorkspaceJSON emits the `.vscode/mcp.json` snippet, under
// VS Code's own `servers` key.
func renderCopilotWorkspaceJSON(entry mcpServer) ([]byte, error) {
	type vsCodeServer struct {
		Type    string   `json:"type"`
		Command string   `json:"command"`
		Args    []string `json:"args,omitempty"`
	}
	servers := map[string]vsCodeServer{mcp.ServerName: {
		Type:    copilotStdioType,
		Command: entry.Command,
		Args:    entry.Args,
	}}
	return marshalJSON(map[string]map[string]vsCodeServer{copilotVSCodeServersKey: servers})
}

func copilotReadme(entry mcpServer, uri string, workspaceBlock []byte) string {
	insiders := copilotInsidersScheme + strings.TrimPrefix(uri, copilotInstallScheme)
	var b strings.Builder
	b.WriteString(generatedBanner)
	b.WriteString(`
# SuperBased Observer — GitHub Copilot (VS Code)

Local-first token, cost and cache observability for GitHub Copilot Chat in
VS Code, installed through VS Code's own **one-click MCP install link**.

**` + binaryPrereqSentence + `.** This is
wiring only — the link declares an MCP server that runs ` + "`observer`" + `;
it does not download or bundle it.

` + "```bash" + `
npm i -g @superbased/observer     # or: pipx install superbased-observer
observer start                    # run the local daemon
` + "```" + `

## Install

Click the link (or paste it into a browser; on Linux,
` + "`xdg-open \"<link>\"`" + ` works from a terminal):

` + "```" + `
` + uri + `
` + "```" + `

VS Code Insiders:

` + "```" + `
` + insiders + `
` + "```" + `

VS Code opens the MCP install prompt with the server pre-filled; confirm it,
and the entry lands in your user profile. ` + "`MCP: Add Server`" + ` from the
Command Palette and ` + "`code --add-mcp '<json>'`" + ` are the equivalent manual
routes.

### As a badge

To put the link in a README of your own:

` + "```markdown" + `
[![Install in VS Code](https://img.shields.io/badge/VS_Code-Install_observer-0098FF?logo=visualstudiocode&logoColor=white)](` + uri + `)
` + "```" + `

(The image comes from shields.io, a third-party badge host; the link itself
is first-party VS Code.)

### Or as a workspace file

To share the server with everyone working in a repository, commit this as
` + "`.vscode/mcp.json`" + `:

` + "```json" + `
` + string(workspaceBlock) + "```" + `

## ⚠️ ` + "`" + copilotVSCodeServersKey + "`" + ` here, ` + "`mcpServers`" + ` in Copilot CLI

VS Code's ` + "`mcp.json`" + ` uses the top-level key
` + "`" + copilotVSCodeServersKey + "`" + `. GitHub Copilot **CLI** — a different
product, documented in ` + "`../" + copilotCLISurfaceDir + "/`" + ` — uses
` + "`mcpServers`" + ` and states outright that it will not read
` + "`.vscode/mcp.json`" + ` *because* of the ` + "`" + copilotVSCodeServersKey + "`" + `
key. Copy the block from the page that matches the product you are
configuring; the wrong key fails silently.

## What the link encodes

The payload is compact JSON, ` + "`encodeURIComponent`" + `-escaped, exactly as
VS Code's own developer guide describes:

` + "```json" + `
` + string(mustCopilotPayloadPretty(entry)) + "```" + `

| Key | Value | Where it comes from |
|---|---|---|
| ` + "`name`" + ` | ` + "`" + mcp.ServerName + "`" + ` | The stable MCP server id observer registers under everywhere. |
| ` + "`type`" + ` | ` + "`" + copilotStdioType + "`" + ` | VS Code's discriminator for a command-launched server. |
| ` + "`command`" + ` + ` + "`args`" + ` | ` + "`" + commandLine(entry) + "`" + ` | The same launch ` + "`observer init`" + ` writes for every other client, resolved from ` + "`PATH`" + `. |

The link is **static and exact**: it encodes the one configuration observer
would write itself, and nothing is composed from runtime or user input. That
is the same rule the Cursor deeplink in ` + "`../cursor/`" + ` follows, and for
the same reason — a one-click install URI that accepts arbitrary payloads is
a command-execution surface.

The ` + "`" + mcp.ServerName + "`" + ` server exposes observer's project, session,
cost and cache queries as tools, reading the local
` + "`~/.observer/observer.db`" + `. It makes no network calls of its own.

## The VS Code extension is a different thing

SuperBased Observer also ships a VS Code **extension** (cost/status surfaces
in the editor), published on the Marketplace and Open VSX. It is unrelated to
this link: this wires observer's MCP server into Copilot Chat, the extension
adds observer's own UI. Installing one does not install the other, and
installing both is fine.

## Double-wiring

` + "`observer init`" + ` writes **no** VS Code MCP config today —
` + "`internal/mcp`" + ` has no VS Code client row (the Cline row is the VS Code
Cline extension's own settings file, a different target) — so this link
cannot duplicate anything observer wrote. Installing it AND committing the
` + "`.vscode/mcp.json`" + ` above declares the same server twice; pick one.

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

// mustCopilotPayloadPretty renders the payload indented for the README.
// The URI itself carries the COMPACT form; this is the same object, shown
// readably. A failure here is a programming error in this file, not a
// runtime condition, and generate() has already marshalled the same struct
// successfully by the time the README is rendered.
func mustCopilotPayloadPretty(entry mcpServer) []byte {
	raw, err := marshalJSON(copilotInstallPayload{
		Name:    mcp.ServerName,
		Type:    copilotStdioType,
		Command: entry.Command,
		Args:    entry.Args,
	})
	if err != nil {
		return []byte("{}\n")
	}
	return raw
}
