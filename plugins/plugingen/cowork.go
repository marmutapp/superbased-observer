package main

import (
	"strings"

	"github.com/marmutapp/superbased-observer/internal/mcp"
)

// ---------------------------------------------------------------------
// Claude Cowork / Claude Desktop — MCP Bundle (`.mcpb`) packaging
// (coverage wave C — plan §7 row "cowork", verdict BUILD, LIVE-VERIFY
// GATED).
//
// Format grounding, first-party (github.com/anthropics/mcpb — README and
// MANIFEST.md, read 2026-07-31 — plus anthropic.com/engineering/
// desktop-extensions and the Claude Help Center's local-MCP article):
//
//   - A bundle is "a zip archive containing a local MCP server and a
//     manifest.json", with the `.mcpb` extension (formerly `.dxt`).
//     "A manifest.json is the only required file."
//   - REQUIRED manifest fields: `manifest_version` (currently "0.3"),
//     `name`, `version` (semver), `description`, `author` (object, `name`
//     required), `server`.
//   - `server.type` is one of node / python / binary (uv from 0.4);
//     `server.entry_point` is the path to the main server file;
//     `server.mcp_config` carries `command`, `args`, `env` and optional
//     `platform_overrides`. The spec's binary example is
//     {"type":"binary","entry_point":"server/my-tool"} with an
//     mcp_config command of "server/my-server", and notes that apps
//     append `.exe` on Windows automatically.
//   - `${__dirname}` in an mcp_config is "replaced with the absolute path
//     to the extension's directory" at install time.
//   - Optional fields used here: display_name, long_description,
//     homepage, documentation, license, keywords, compatibility.platforms.
//   - Install: drag the `.mcpb` into Settings → Extensions; packaged with
//     `npm install -g @anthropic-ai/mcpb` then `mcpb pack`.
//
// ⚠️ CORRECTION TO THE RESEARCH NOTE: docs/plans/plugin-coverage-research
// -2026-07-31.md names the required first field `mcpb_version`. The
// current spec in anthropics/mcpb spells it **`manifest_version`** (value
// "0.3"); `mcpb_version` appears nowhere in it. The generator emits the
// spelling the spec carries, and TestCoworkManifestShape pins it.
//
// ── TWO HONESTY GATES ON THIS SURFACE ────────────────────────────────
//
//  1. **Cowork is INFERRED, not documented.** Anthropic's own material
//     says Claude Desktop's Extensions panel installs `.mcpb` bundles,
//     and the Help Center frames Cowork as a tab of that same app sharing
//     its configuration — but NO first-party sentence says a `.mcpb`
//     extension's tools are available inside a Cowork session. The
//     research pass graded that link "strong-but-not-explicit". Nothing
//     here may claim Cowork coverage until an operator verifies it
//     against a live Cowork; the README says exactly that.
//  2. **This bundle contains no server.** Every documented server type
//     packages its own runtime artefact, and the binary type is described
//     as self-contained. Observer's binary arrives from npm/PyPI and lives
//     on PATH — the same deviation every surface in this repository
//     carries — so `entry_point` and `mcp_config.command` name the PATH
//     command `observer` rather than a file inside the archive. That is a
//     documented departure from the spec's intent, not a spec feature, and
//     it is why this artifact is packaged but unverified.
//
// No `.mcpb` archive is built by the generator: packing needs Anthropic's
// `mcpb` CLI, which is not present in this environment or in CI. The
// manifest is emitted; the README documents the one-command pack step.
// ---------------------------------------------------------------------

// coworkSurfaceDir is this surface's directory in the in-tree layout.
const coworkSurfaceDir = "cowork"

// coworkManifestVersion is the spec version this manifest conforms to.
const coworkManifestVersion = "0.3"

// coworkServerType is the documented server type closest to a compiled,
// runtime-free executable. See honesty gate 2 above for the deviation.
const coworkServerType = "binary"

// coworkMCPConfig is server.mcp_config.
type coworkMCPConfig struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// coworkServer is the manifest's required `server` object.
type coworkServer struct {
	Type       string          `json:"type"`
	EntryPoint string          `json:"entry_point"`
	MCPConfig  coworkMCPConfig `json:"mcp_config"`
}

// coworkCompatibility is the optional `compatibility` object. Only
// `platforms` is set: those are the three platforms observer publishes a
// binary for, which is a fact we can check rather than a claim about any
// host's version.
type coworkCompatibility struct {
	Platforms []string `json:"platforms"`
}

// coworkManifest is manifest.json, in the spec's own field order.
type coworkManifest struct {
	ManifestVersion string              `json:"manifest_version"`
	Name            string              `json:"name"`
	DisplayName     string              `json:"display_name"`
	Version         string              `json:"version"`
	Description     string              `json:"description"`
	LongDescription string              `json:"long_description"`
	Author          author              `json:"author"`
	Homepage        string              `json:"homepage"`
	Documentation   string              `json:"documentation"`
	License         string              `json:"license"`
	Keywords        []string            `json:"keywords"`
	Server          coworkServer        `json:"server"`
	Compatibility   coworkCompatibility `json:"compatibility"`
}

// coworkPlatforms are the spec's platform identifiers for the three
// platforms observer ships a binary for.
func coworkPlatforms() []string { return []string{"darwin", "win32", "linux"} }

// coworkDescription states the binary prerequisite plainly (§0).
const coworkDescription = "Token, cost and cache observability for Claude Desktop. " +
	"Wires the SuperBased Observer MCP server. " +
	binaryPrereqSentence + " " + installHint + " — this bundle is wiring only and installs no binary."

func renderCoworkManifest(entry mcpServer, version string) ([]byte, error) {
	return marshalJSON(coworkManifest{
		ManifestVersion: coworkManifestVersion,
		Name:            pluginName,
		DisplayName:     "SuperBased Observer",
		Version:         version,
		Description:     coworkDescription,
		LongDescription: "Query your own coding-agent history from inside Claude Desktop: per-session " +
			"token and cost totals, cache behaviour, project patterns and past tool output. All data " +
			"stays in a local SQLite database. " + binaryPrereqSentence + ".",
		Author:        author{Name: authorName, Email: authorMail, URL: authorURL},
		Homepage:      homepage,
		Documentation: homepage,
		License:       license,
		Keywords:      pluginKeywords(),
		Server: coworkServer{
			Type: coworkServerType,
			// NOT a file inside the archive — see honesty gate 2. The
			// bundle ships no binary, so both the entry point and the
			// launch command are the PATH name.
			EntryPoint: entry.Command,
			MCPConfig: coworkMCPConfig{
				Command: entry.Command,
				Args:    entry.Args,
				Env:     entry.Env,
			},
		},
		Compatibility: coworkCompatibility{Platforms: coworkPlatforms()},
	})
}

// renderCoworkDesktopConfigJSON emits the GROUNDED alternative: the
// `claude_desktop_config.json` block, which Claude's own Help Center
// documents and which applies to Claude Desktop as a whole.
func renderCoworkDesktopConfigJSON(entry mcpServer) ([]byte, error) {
	return marshalJSON(struct {
		MCPServers map[string]mcpServer `json:"mcpServers"`
	}{MCPServers: map[string]mcpServer{mcp.ServerName: entry}})
}

func coworkReadme(entry mcpServer, desktopBlock []byte) string {
	var b strings.Builder
	b.WriteString(generatedBanner)
	b.WriteString(`
# SuperBased Observer — Claude Desktop (` + "`.mcpb`" + ` bundle) — UNVERIFIED

Local-first token, cost and cache observability for Claude Desktop,
packaged in Anthropic's MCP Bundle format.

> ## ⚠️ Read this before using it
>
> **This artifact has never been installed into a live Claude Desktop or
> Cowork session.** Two separate things are unverified:
>
> 1. **Cowork.** Anthropic documents that Claude Desktop's Extensions panel
>    installs ` + "`.mcpb`" + ` bundles, and describes Cowork as a tab of that
>    same app. No first-party sentence says a bundle's tools are available
>    **inside a Cowork session**. That link is an inference, so this page
>    does not claim Cowork coverage — it claims a bundle exists and needs
>    checking.
> 2. **The bundle itself.** No ` + "`.mcpb`" + ` archive has been packed or
>    installed anywhere; only the manifest is generated (see "Packing"
>    below), and it deviates from the spec in one stated way.
>
> The ` + "`claude_desktop_config.json`" + ` route further down is the
> **grounded** one — Anthropic's Help Center documents it directly. Prefer
> it until someone verifies the bundle.

**` + binaryPrereqSentence + `.** This is
wiring only — the manifest declares an MCP server that runs
` + "`observer`" + `; it does not download or bundle it.

` + "```bash" + `
npm i -g @superbased/observer     # or: pipx install superbased-observer
observer start                    # run the local daemon
` + "```" + `

## Install

**The documented, grounded route** — merge this into
` + "`claude_desktop_config.json`" + ` and restart Claude Desktop:

| OS | Path |
|---|---|
| macOS | ` + "`~/Library/Application Support/Claude/claude_desktop_config.json`" + ` |
| Windows | ` + "`%APPDATA%\\Claude\\claude_desktop_config.json`" + ` |
| Linux | ` + "`~/.config/Claude/claude_desktop_config.json`" + ` |

` + "```json" + `
` + string(desktopBlock) + "```" + `

**The bundle route** (unverified, see the box above): pack this directory
into a ` + "`.mcpb`" + ` and drag the result into **Settings → Extensions**.

## Packing

The generator emits ` + "`" + pluginDir + "/manifest.json`" + ` and stops there:
building the archive needs Anthropic's own packer, which is not available in
this repository's build environment, and shipping a hand-zipped file that
claims to be a validated bundle would be a claim we cannot make.

` + "```bash" + `
npm install -g @anthropic-ai/mcpb
cd ` + coworkSurfaceDir + `/` + pluginDir + `
mcpb pack                         # produces ` + pluginName + `.mcpb
` + "```" + `

` + "`mcpb validate manifest.json`" + ` checks the manifest on its own if you
only want the schema verdict.

## What it wires

| Field | Value |
|---|---|
| ` + "`manifest_version`" + ` | ` + "`" + coworkManifestVersion + "`" + ` — the current spec version in ` + "`anthropics/mcpb`" + `. |
| ` + "`server.type`" + ` | ` + "`" + coworkServerType + "`" + ` |
| ` + "`server.mcp_config`" + ` | ` + "`" + commandLine(entry) + "`" + ` — the same launch ` + "`observer init`" + ` writes for every other client. |

### The one deviation, stated plainly

Every documented bundle type packages its own server artefact, and
` + "`" + coworkServerType + "`" + ` is described as self-contained. **This bundle
contains no binary.** Observer's binary arrives through npm or PyPI and lives
on your ` + "`PATH`" + `, so ` + "`entry_point`" + ` and
` + "`mcp_config.command`" + ` name the command ` + "`" + entry.Command + "`" + `
rather than a file inside the archive. That is a departure from what the
format intends, and it is the main reason this artifact is marked unverified
rather than shipped as-is.

One consequence worth knowing on Windows: the spec notes that hosts append
` + "`.exe`" + ` to a binary command automatically. A PyPI install
(` + "`pipx install superbased-observer`" + `) puts ` + "`observer.exe`" + ` on
PATH, which matches; an npm global install puts a shim without that
extension. Untested either way.

The ` + "`" + mcp.ServerName + "`" + ` server exposes observer's project, session,
cost and cache queries as tools, reading the local
` + "`~/.observer/observer.db`" + `. It makes no network calls of its own.

## Not submitted anywhere

Anthropic curates a reviewed extensions gallery. Nothing here has been
submitted to it, and submitting is an operator decision — one that should
wait until the two unverified points above are settled.

## Double-wiring

` + "`observer init`" + ` writes **no** Claude Desktop config today —
` + "`internal/mcp`" + ` has no Claude Desktop client row (the claude-code row
is the Claude Code CLI, a different product) — so nothing here duplicates
what observer wrote. Installing the bundle AND pasting the config block
declares the same server twice; pick one.

Cowork capture is unaffected either way: observer's watcher reads Claude
Desktop's own local agent-mode session files whether or not any of this is
configured.

## Links

- Docs: ` + homepage + `
- Source: https://github.com/superbasedapp/observer
- License: ` + license + `
`)
	return b.String()
}
