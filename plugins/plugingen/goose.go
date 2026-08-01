package main

import (
	"fmt"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/mcp"
)

// ---------------------------------------------------------------------
// Goose directory listing (plan row 4).
//
// Goose extensions ARE MCP servers, and `observer serve` already speaks
// stdio MCP — so there is no manifest to invent here. The deliverable is
// the exact config block plus the install walkthrough.
//
// Format grounding: the Goose docs' "Configuration Files" page (the
// ~/.config/goose/config.yaml `extensions:` map and its stdio entry keys —
// type/name/enabled/cmd/args/env_keys/envs/timeout, with `bundled`,
// `display_name` and `available_tools` as the remaining documented keys)
// and the "Using Extensions" page (the `goose configure` → Add Extension →
// Command-line Extension prompt sequence, the
// `goose session --with-extension "<command>"` form, and the goose://
// deeplink parameter table).
//
// The block below is GENERATED from canonicalStdio, not hand-typed: the
// whole point of the §3 one-owner rule is that a changed launch argument
// reaches every published snippet. Goose has no MCP registrar in
// internal/mcp (locate.Locations has no goose row; internal/integration
// records MCP: nil with "no guarded write path into config.yaml is
// grounded"), so this README documents a hand-edit the USER makes — it is
// not something `observer init` writes.
// ---------------------------------------------------------------------

// gooseTimeoutSeconds is the timeout the Goose docs use in every stdio
// example and describe as the default.
const gooseTimeoutSeconds = 300

// yamlFlowStrings renders a YAML flow sequence with double-quoted scalars,
// matching the style of the Goose docs' stdio example (`args: ["-y", …]`).
func yamlFlowStrings(items []string) string {
	quoted := make([]string, 0, len(items))
	for _, it := range items {
		quoted = append(quoted, `"`+it+`"`)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// gooseExtensionYAML renders the config.yaml entry for observer.
func gooseExtensionYAML(entry mcpServer) string {
	var b strings.Builder
	b.WriteString("extensions:\n")
	b.WriteString("  " + mcp.ServerName + ":\n")
	b.WriteString("    type: stdio\n")
	b.WriteString("    name: " + mcp.ServerName + "\n")
	b.WriteString("    enabled: true\n")
	b.WriteString("    cmd: " + entry.Command + "\n")
	b.WriteString("    args: " + yamlFlowStrings(entry.Args) + "\n")
	b.WriteString("    env_keys: []\n")
	b.WriteString("    envs: {}\n")
	b.WriteString(fmt.Sprintf("    timeout: %d\n", gooseTimeoutSeconds))
	return b.String()
}

func gooseReadme(entry mcpServer) string {
	argv := strings.TrimSpace(entry.Command + " " + strings.Join(entry.Args, " "))
	var b strings.Builder
	b.WriteString(generatedBanner)
	b.WriteString(`
# SuperBased Observer — Goose extension

Goose extensions **are** MCP servers, and ` + "`observer serve`" + ` already speaks
stdio MCP. So there is no extension package to install: you point Goose at
the ` + "`observer`" + ` binary you already have.

**` + binaryPrereqSentence + `.** This is
wiring only; nothing here installs or bundles a binary.

` + "```bash" + `
npm i -g @superbased/observer     # or: pipx install superbased-observer
observer start                    # run the local daemon
` + "```" + `

## Install — the interactive way

` + "```bash" + `
goose configure
` + "```" + `

Then answer the prompts:

| Prompt | Answer |
|---|---|
| What would you like to configure? | **Add Extension** |
| What type of extension would you like to add? | **Command-line Extension** |
| What would you like to call this extension? | ` + "`" + mcp.ServerName + "`" + ` |
| What command should be run? | ` + "`" + argv + "`" + ` |
| Please set the timeout for this tool (in secs): | ` + fmt.Sprintf("`%d`", gooseTimeoutSeconds) + ` |
| Would you like to add environment variables? | **No** |

## Install — the config-file way

Add this to ` + "`~/.config/goose/config.yaml`" + ` (merge the ` + "`extensions:`" + ` key
if you already have one):

` + "```yaml" + `
` + gooseExtensionYAML(entry) + "```" + `

Restart Goose (edits to the config file do not reach an already-running
session), then check it with ` + "`goose info -v`" + `.

## Try it for one session only

` + "```bash" + `
goose session --with-extension "` + argv + `"
` + "```" + `

Per the Goose docs this does **not** install the extension — it is enabled
for that session only.

## Why there is no goose:// deeplink

Goose's ` + "`goose://extension?…`" + ` deeplink documents ` + "`cmd`" + ` as "the base
command to run, **one of** ` + "`jbang`" + `, ` + "`npx`" + `, ` + "`uvx`" + `, ` + "`goosed`" + `
or ` + "`docker`" + `". ` + "`observer`" + ` is not on that list, so a deeplink for it
would be an unsupported ` + "`cmd`" + `. We do not ship one rather than publish a
link that may silently not install. (Cursor's deeplink, in ` + "`../cursor/`" + `,
has no such restriction — that one we do ship.)

## What you get

The ` + "`" + mcp.ServerName + "`" + ` MCP server exposes observer's project,
session, cost and cache queries to Goose as tools. They read the local
database; the one exception is ` + "`continue_session`" + `, which writes a
handover file only when you pass ` + "`write_file=true`" + `. Captured data lands in
the local ` + "`~/.observer/observer.db`" + `; the server makes no network calls of
its own.

Goose's own session capture is unaffected: observer's watcher reads Goose's
` + "`sessions.db`" + ` regardless of whether this extension is configured.

## Double-wiring

` + "`observer init`" + ` writes **no** Goose config today — ` + "`internal/mcp`" + ` has
no Goose client row (no guarded write path into ` + "`config.yaml`" + ` is
grounded), so this hand-added entry cannot duplicate anything observer
wrote. If you have added the server twice under different extension names,
Goose will load two copies of the same tool schema; remove one with
` + "`goose configure`" + ` → Toggle/Remove Extensions.

The automatic detect-and-skip ` + "`observer init`" + ` performs for the Claude Code
plugin is **claude-code-only** (` + "`internal/claudeplugin`" + `); no equivalent
probe exists for Goose. Documented, not built.

## Links

- Docs: ` + homepage + `
- Source: https://github.com/superbasedapp/observer
- License: ` + license + `
`)
	return b.String()
}
