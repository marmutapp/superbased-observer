package main

import (
	"fmt"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/mcp"
)

// ---------------------------------------------------------------------
// Gemini CLI extension (plan row 3).
//
// Format grounding: the Gemini CLI extensions reference at
// google-gemini.github.io/gemini-cli/docs/extensions/. It documents an
// extension as "a directory that contains a gemini-extension.json file"
// installed with `gemini extensions install <github-url-or-local-path>`
// into <home>/.gemini/extensions/<name>/, and enumerates exactly five
// manifest fields: name, version, mcpServers, contextFileName and
// excludeTools. `name` "should be lowercase or numbers and use dashes",
// and is expected to match the extension directory name. mcpServers
// "supports all standard MCP options except for trust", and a same-named
// server in settings.json wins over the extension's.
//
// Two things that page does NOT document, and which are therefore not
// emitted: a `description` key (the honesty sentence lives in the README
// instead — there is nowhere in the manifest to put it), and hooks. Custom
// commands are TOML files under a commands/ subdirectory, not a manifest
// key; we ship none because no registrar declares any.
//
// One-owner note: internal/mcp has NO gemini row (locate.Locations carries
// none, and internal/integration records MCP: nil for gemini-cli), so this
// surface transposes canonicalStdio — the launch every MCP registrar
// agrees on — rather than a gemini-specific registrar output. See
// canonicalStdio for why that assertion is the honest substitute.
// ---------------------------------------------------------------------

// geminiExtensionName is the manifest `name` and therefore the directory
// the CLI installs into (~/.gemini/extensions/<name>/).
const geminiExtensionName = pluginName

// geminiExtension is gemini-extension.json. Only documented fields are
// modelled: adding a key the reference does not list is exactly the kind
// of guess this generator exists to prevent.
type geminiExtension struct {
	Name       string               `json:"name"`
	Version    string               `json:"version"`
	MCPServers map[string]mcpServer `json:"mcpServers"`
}

func renderGeminiExtension(entry mcpServer, version string) ([]byte, error) {
	return marshalJSON(geminiExtension{
		Name:       geminiExtensionName,
		Version:    version,
		MCPServers: map[string]mcpServer{mcp.ServerName: entry},
	})
}

func geminiReadme(entry mcpServer) string {
	var b strings.Builder
	b.WriteString(generatedBanner)
	b.WriteString(`
# SuperBased Observer — Gemini CLI extension

Local-first token, cost and cache observability for the Gemini CLI.

**` + binaryPrereqSentence + `.** This extension is
wiring only — it declares an MCP server that runs ` + "`observer`" + `; it does not
download or bundle it. ` + "`gemini-extension.json`" + ` has no ` + "`description`" + ` field
to carry that sentence, so it is stated here and in the listing copy.

` + "```bash" + `
npm i -g @superbased/observer     # or: pipx install superbased-observer
observer start                    # run the local daemon
` + "```" + `

## Install

` + "```bash" + `
gemini extensions install https://github.com/superbasedapp/plugins
` + "```" + `

Installing from GitHub requires ` + "`git`" + `, and the CLI **copies** the extension —
run ` + "`gemini extensions update " + geminiExtensionName + "`" + ` to pick up a new release.
Uninstall with ` + "`gemini extensions uninstall " + geminiExtensionName + "`" + `; disable
without removing via ` + "`gemini extensions disable " + geminiExtensionName + "`" + `
(add ` + "`--scope=workspace`" + ` for just the current workspace). Inside the CLI,
` + "`/extensions list`" + ` shows what is loaded.

⚠️ **The manifest has to be at the root of whatever you install.** Gemini
documents a GitHub URL or a local path, and nothing else — there is no
documented way to install from a subdirectory of a repo. So publishing this
directory means either its own repo, or a repo root that also carries the
other surfaces (their root-level names — ` + "`.claude-plugin/`" + `,
` + "`.agents/`" + ` — do not collide with ` + "`gemini-extension.json`" + `). That choice
is an operator step; nothing here has been published or tested against a
live install.

## What it wires

| Field | Value |
|---|---|
| ` + "`name`" + ` | ` + "`" + geminiExtensionName + "`" + ` — also the install directory name (` + "`~/.gemini/extensions/" + geminiExtensionName + "/`" + `). |
| ` + "`version`" + ` | Stamped from the observer release tag. |
| ` + "`mcpServers." + mcp.ServerName + "`" + ` | ` + "`" + commandLine(entry) + "`" + ` — on-demand project/session/cost queries from inside Gemini. |

No hooks and no custom commands are declared. Observer's hook registry has
no Gemini target (` + "`internal/hook`" + ` registers claude-code, cursor and codex
only), so there is nothing to transpose — and the extensions reference
documents no ` + "`hooks`" + ` key anyway. Gemini capture continues to work without
hooks: observer's watcher reads ` + "`~/.gemini`" + ` session logs, and
` + "`observer gemini -- …`" + ` routes turns through the local proxy for exact
token counts.

Captured data lands in the local ` + "`~/.observer/observer.db`" + `. The MCP server
makes no network calls of its own.

## Double-wiring

` + "`observer init`" + ` writes **no** Gemini MCP entry today — ` + "`internal/mcp`" + `
has no Gemini client row — so installing this extension cannot duplicate
anything observer wrote. If a Gemini MCP registrar is added later, the two
would both declare the ` + "`" + mcp.ServerName + "`" + ` server; Gemini resolves that in
the user's favour (a same-named server in ` + "`settings.json`" + ` takes precedence
over an extension's), but the tool schema would still load twice.

The automatic detect-and-skip that ` + "`observer init`" + ` performs for the Claude
Code plugin is **claude-code-only** (` + "`internal/claudeplugin`" + `); no equivalent
probe exists for Gemini. This is documented, not built.

## Links

- Docs: ` + homepage + `
- Source: https://github.com/superbasedapp/observer
- License: ` + license + `
`)
	return b.String()
}

// geminiSnapshotLine is the top-level README's one-line summary of this
// surface, so the layout table and the surface agree by construction.
func geminiSnapshotLine(entry mcpServer) string {
	return fmt.Sprintf("`%s` extension declaring the `%s` MCP server (`%s`)",
		geminiExtensionName, mcp.ServerName, commandLine(entry))
}
