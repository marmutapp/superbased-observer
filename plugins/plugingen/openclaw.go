package main

import (
	"strings"

	"github.com/marmutapp/superbased-observer/internal/mcp"
)

// ---------------------------------------------------------------------
// OpenClaw native plugin (coverage wave A — plan §7 row "openclaw").
//
// Format grounding, first-party (docs.openclaw.ai/plugins/manifest, read
// 2026-07-31, which supplies the field-level detail the research note in
// docs/plans/plugin-coverage-research-2026-07-31.md summarised):
//
//   - `openclaw.plugin.json` at the plugin root. EXACTLY TWO required
//     fields: `id` ("Canonical plugin id. This is the id used in
//     plugins.entries.<id>") and `configSchema` ("Inline JSON Schema for
//     this plugin's config"). A missing or invalid manifest "blocks config
//     validation and is treated as a plugin error", so both must be real.
//   - The docs' own MINIMAL example is
//     {"id": "...", "configSchema": {"type":"object",
//      "additionalProperties": false, "properties": {}}} — which is
//     exactly the schema a plugin with no configuration of its own wants,
//     so that is what is emitted rather than an invented one.
//   - Optional fields we can honestly fill: `name` ("Human-readable plugin
//     name" — note there is NO displayName in this manifest),
//     `description` ("Short summary shown in plugin surfaces"), `version`
//     ("Informational plugin version"), `mcpServers`
//     (Record<string, object>; entry keys include `transport` — e.g.
//     "stdio" — `command` and `args`; `cwd`/`workingDirectory` resolve
//     from the plugin root; NO `env` key is documented), and `activation`
//     (planner metadata, whose `onStartup` boolean "every plugin is
//     expected to set deliberately").
//   - Operator config wins over a plugin default: "mcp.servers.<name> can
//     replace a plugin default or set enabled: false to omit it".
//   - Install: `openclaw plugins install <spec>` — an npm spec, a local
//     path, a tarball, a zip or git; `-l` link mode and `--pin` exist.
//     Also list / info / update / enable / disable / doctor / publish.
//
// Deliberate omissions:
//
//   - `homepage`, `repository`, `license`, `keywords`. These are NOT part
//     of openclaw.plugin.json; the page says the manifest should not hold
//     "npm install metadata" — that belongs in package.json. Emitting them
//     would be inventing four fields into a schema'd manifest.
//   - `package.json`. Publishing an npm package for this surface is a
//     second npm artifact and a separate operator decision; the git /
//     local-path install channels are first-party documented and need no
//     package metadata. The README documents those.
//   - `icon`. Documented as an HTTPS image URL for catalog cards; we have
//     no published image URL to point at, and a broken one is worse than
//     the documented default-icon fallback.
//   - `catalog` ({featured, order}). Those are ClawHub display hints for
//     curated plugins; setting them on an unlisted third-party plugin
//     would be asking for placement we have not been given.
//   - `skills`, `channels`, `providers`, `commandAliases`, `dashboard`,
//     `setup`. Observer ships none of those components for OpenClaw.
//   - hooks of any kind. `observer hook <tool>` accepts claude-code,
//     cursor, codex and hermes; internal/integration records
//     HookMechanism None for openclaw. A declared hook would name a
//     command that does not exist.
// ---------------------------------------------------------------------

// openClawSurfaceDir is this surface's directory in the in-tree layout.
const openClawSurfaceDir = "openclaw"

// openClawStdioTransport is the documented transport discriminator for a
// command/args MCP entry in this manifest. OpenClaw's entry shape is its
// own — it is NOT the {command,args} object the Claude/Cursor convention
// uses, which is why this surface has its own struct instead of reusing
// mcpServer.
const openClawStdioTransport = "stdio"

// openClawMCPServer is one entry of openclaw.plugin.json's `mcpServers`
// map. Only documented keys are modelled; `env` is absent because the
// manifest reference does not document one (and canonicalStdio carries no
// environment — a divergence there is a hard error, see canonicalStdio).
type openClawMCPServer struct {
	Transport string   `json:"transport"`
	Command   string   `json:"command"`
	Args      []string `json:"args,omitempty"`
}

// openClawActivation is the manifest's planner-metadata block. Only
// `onStartup` is set, and deliberately: this plugin's whole contribution
// is a statically-declared MCP server, which has to be present from the
// start of a session rather than activated by some later signal.
type openClawActivation struct {
	OnStartup bool `json:"onStartup"`
}

// openClawManifest is openclaw.plugin.json. Field order here is the
// emitted key order: the two REQUIRED fields lead, so a reader sees the
// contract first.
type openClawManifest struct {
	ID           string                       `json:"id"`
	ConfigSchema openClawConfigSchema         `json:"configSchema"`
	Name         string                       `json:"name"`
	Description  string                       `json:"description"`
	Version      string                       `json:"version"`
	Activation   openClawActivation           `json:"activation"`
	MCPServers   map[string]openClawMCPServer `json:"mcpServers"`
}

// openClawConfigSchema is the inline JSON Schema the manifest requires.
// Observer's OpenClaw plugin takes no configuration of its own — every
// knob lives in ~/.observer/config.toml, which the binary reads — so the
// honest schema is the docs' own minimal one: an object that accepts
// nothing. `properties` is a non-nil empty map so it marshals as `{}`
// rather than `null`, matching the documented example byte for byte.
type openClawConfigSchema struct {
	Type                 string         `json:"type"`
	AdditionalProperties bool           `json:"additionalProperties"`
	Properties           map[string]any `json:"properties"`
}

// openClawPluginDescription states the binary prerequisite plainly (§0).
const openClawPluginDescription = "Token, cost and cache observability for OpenClaw. " +
	"Wires the SuperBased Observer MCP server. " +
	binaryPrereqSentence + " " + installHint + " — this plugin is wiring only and installs no binary."

func renderOpenClawManifest(entry mcpServer, version string) ([]byte, error) {
	return marshalJSON(openClawManifest{
		ID: pluginName,
		ConfigSchema: openClawConfigSchema{
			Type:                 "object",
			AdditionalProperties: false,
			Properties:           map[string]any{},
		},
		Name:        "SuperBased Observer",
		Description: openClawPluginDescription,
		Version:     version,
		Activation:  openClawActivation{OnStartup: true},
		MCPServers: map[string]openClawMCPServer{
			mcp.ServerName: {
				Transport: openClawStdioTransport,
				Command:   entry.Command,
				Args:      entry.Args,
			},
		},
	})
}

func openClawReadme(entry mcpServer) string {
	var b strings.Builder
	b.WriteString(generatedBanner)
	b.WriteString(`
# SuperBased Observer — OpenClaw plugin

Local-first token, cost and cache observability for OpenClaw.

**` + binaryPrereqSentence + `.** This plugin is
wiring only — it declares an MCP server that runs ` + "`observer`" + `; it does not
download or bundle it.

` + "```bash" + `
npm i -g @superbased/observer     # or: pipx install superbased-observer
observer start                    # run the local daemon
` + "```" + `

## Install

` + "```bash" + `
openclaw plugins install https://github.com/superbasedapp/plugins
` + "```" + `

` + "`openclaw plugins install`" + ` accepts an npm spec, a git URL, a local path,
a tarball or a zip. From a local checkout:

` + "```bash" + `
openclaw plugins install ./` + openClawSurfaceDir + `/` + pluginDir + `
` + "```" + `

Installed plugins are extracted into ` + "`~/.openclaw/extensions/<id>/`" + ` and
enabled in config. Manage them with
` + "`openclaw plugins list|info|update|enable|disable|doctor`" + `. The plugin id is
` + "`" + pluginName + "`" + `, which is also the key under ` + "`plugins.entries`" + ` in
your OpenClaw config.

## What it wires

| Manifest field | Value |
|---|---|
| ` + "`id`" + ` (required) | ` + "`" + pluginName + "`" + ` — the canonical plugin id, and the ` + "`plugins.entries.<id>`" + ` key. |
| ` + "`configSchema`" + ` (required) | An object that accepts no properties. This plugin has no configuration of its own; observer's own knobs live in ` + "`~/.observer/config.toml`" + `, which the binary reads. |
| ` + "`name`" + ` / ` + "`description`" + ` / ` + "`version`" + ` | Display metadata; the version is stamped from the observer release tag. |
| ` + "`activation.onStartup`" + ` | ` + "`true`" + ` — the server is a static declaration and has to be present from the start of a session. |
| ` + "`mcpServers." + mcp.ServerName + "`" + ` | ` + "`transport: " + openClawStdioTransport + "`" + `, ` + "`" + commandLine(entry) + "`" + ` — on-demand project/session/cost queries from inside OpenClaw. |

Nothing else is declared. In particular there are **no ` + "`homepage`" + `,
` + "`repository`" + `, ` + "`license`" + ` or ` + "`keywords`" + ` fields** — OpenClaw's
manifest reference does not have them, and states that npm install metadata
belongs in ` + "`package.json`" + ` instead. There is no ` + "`icon`" + ` (we have no
published image URL to point at, and OpenClaw falls back to a default) and no
` + "`catalog`" + ` block (those are ClawHub curation hints, not something an
unlisted plugin should claim for itself).

**No hooks are declared.** Observer has no OpenClaw hook receiver —
` + "`observer hook`" + ` accepts claude-code, cursor, codex and hermes only — so a
hook here would name a command that does not exist. OpenClaw capture works
without hooks: observer's watcher reads both the ` + "`<id>.jsonl`" + ` message log
and the ` + "`<id>.trajectory.jsonl`" + ` trace, which between them carry real
per-call token counts.

Captured data lands in the local ` + "`~/.observer/observer.db`" + `. The MCP server
makes no network calls of its own.

## Your config always wins

OpenClaw documents that ` + "`mcp.servers.<name>`" + ` in your own config "can
replace a plugin default or set ` + "`enabled: false`" + ` to omit it" — so an
entry you wrote by hand overrides this plugin's, and you can switch the server
off without uninstalling anything.

## Double-wiring

` + "`observer init`" + ` writes **no** OpenClaw config today — ` + "`internal/mcp`" + `
has no OpenClaw client row — so this plugin cannot duplicate anything observer
wrote. If you have separately run
` + "`openclaw mcp set " + mcp.ServerName + " …`" + `, that hand-written entry replaces
this plugin's per the rule above rather than stacking with it.

The automatic detect-and-skip ` + "`observer init`" + ` performs for the Claude Code
plugin is **claude-code-only** (` + "`internal/claudeplugin`" + `); no equivalent probe
exists for OpenClaw. Documented, not built.

## Links

- Docs: ` + homepage + `
- Source: https://github.com/superbasedapp/observer
- License: ` + license + `
`)
	return b.String()
}
