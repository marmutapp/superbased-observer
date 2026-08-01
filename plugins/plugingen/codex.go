package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/marmutapp/superbased-observer/internal/mcp"
)

// ---------------------------------------------------------------------
// Codex plugin + local marketplace catalog (plan row 5).
//
// Format grounding, all first-party:
//
//   - developers.openai.com/plugins/build/plugins ("Package your plugin"):
//     the plugin layout ("Only plugin.json belongs in .codex-plugin/",
//     with skills/, hooks/, assets/, .app.json and .mcp.json at the plugin
//     ROOT), the full plugin.json example (name, version, description,
//     author{name,email,url}, homepage, repository, license, keywords,
//     skills, mcpServers, apps, hooks, interface{…}), the "keep manifest
//     paths relative to the plugin root and start them with ./" rule, the
//     marketplace.json locations ($REPO_ROOT/.agents/plugins/marketplace.json,
//     ~/.agents/plugins/marketplace.json, plus a legacy-compatible
//     $REPO_ROOT/.claude-plugin/marketplace.json), the marketplace schema
//     (name, interface.displayName, plugins[] with name/source/policy/
//     category, "Always include policy.installation, policy.authentication
//     and category"), and the `codex plugin marketplace add …` CLI.
//   - github.com/openai/codex … plugin-creator/references/plugin-json-spec.md:
//     name is kebab-case, version is strict semver, the validator requires
//     real name/version/description/author.name + interface fields, and
//     mcpServers may be either a "./.mcp.json" path or an inline object.
//   - github.com/openai/plugins (the official catalog): the live
//     .agents/plugins/marketplace.json (180 entries — the observed
//     vocabulary is installation AVAILABLE, authentication ON_INSTALL |
//     ON_USE, products ["CODEX"], and a fixed category list including
//     "Developer Tools"), and real .mcp.json files. plugins/build-ios-apps
//     and plugins/openai-developers pin the STDIO shape we need:
//     {"mcpServers":{"<id>":{"command":…,"args":[…],"env":{…}}}} — the
//     same Claude-compatible object, not a codex-only spelling.
//
// Deliberate omissions, each because a guess would be worse:
//
//   - `hooks`. Our codex hook registrar writes TOML into
//     ~/.codex/config.toml; a plugin's hooks live in hooks/hooks.json,
//     whose schema is not documented on any page above, and the
//     plugin-json-spec explicitly notes ingestion "rejects unsupported
//     manifest fields such as hooks". Codex hooks therefore stay with
//     `observer init --codex`, and the README says so.
//   - assets/screenshots/logo and the policy URLs. The spec requires those
//     paths to point at real files and absolute https:// URLs; shipping
//     placeholders would fail validation loudly at best and mislead at
//     worst. An operator preparing a directory submission adds them.
//   - `codex plugin add <plugin>@<marketplace>`. The plan sketched that
//     verb, but the official packaging page documents only
//     `codex plugin marketplace add|list|upgrade|remove` and states it
//     "defines no standalone codex plugin add command"; the per-plugin
//     install is the CLI's `/plugins` browser. The README uses only what
//     is documented.
// ---------------------------------------------------------------------

// codexPluginPath is the plugin's directory INSIDE the marketplace root.
// The official catalog puts plugins under ./plugins/<name>, and the
// marketplace entry's source.path is that same "./"-prefixed relative
// path — resolved against the marketplace root, the directory holding
// .agents/.
var codexPluginPath = "plugins/" + pluginName

// codexCategory and the policy values below are taken from the live
// openai/plugins catalog. codex-security — a local, stdio, no-network
// plugin, the closest shape to ours — declares exactly
// {installation: AVAILABLE, authentication: ON_USE, products: [CODEX]}.
// There is no documented "no authentication" value, so we mirror the
// first-party local-stdio entry rather than invent one.
const (
	codexCategory       = "Developer Tools"
	codexInstallation   = "AVAILABLE"
	codexAuthentication = "ON_USE"
	codexProduct        = "CODEX"
)

// readCodexEntry pulls [mcp_servers.observer] out of the config.toml the
// codex MCP registrar just wrote and converts it to the canonical stdio
// shape. An unmodelled key is a hard error, exactly as in readMCPEntry.
func readCodexEntry(home string) (mcpServer, error) {
	path := filepath.Join(home, ".codex", "config.toml")
	raw, err := os.ReadFile(path)
	if err != nil {
		return mcpServer{}, fmt.Errorf("plugingen: read codex config: %w", err)
	}
	var root map[string]any
	if err := toml.Unmarshal(raw, &root); err != nil {
		return mcpServer{}, fmt.Errorf("plugingen: parse codex config: %w", err)
	}
	servers, _ := root["mcp_servers"].(map[string]any)
	if servers == nil {
		return mcpServer{}, fmt.Errorf("plugingen: codex config has no [mcp_servers] table")
	}
	table, ok := servers[mcp.ServerName].(map[string]any)
	if !ok {
		return mcpServer{}, fmt.Errorf("plugingen: codex config has no [mcp_servers.%s] table", mcp.ServerName)
	}

	var entry mcpServer
	for k, v := range table {
		switch k {
		case "command":
			s, ok := v.(string)
			if !ok {
				return mcpServer{}, fmt.Errorf("plugingen: codex mcp command is %T, want string", v)
			}
			entry.Command = s
		case "args":
			items, ok := v.([]any)
			if !ok {
				return mcpServer{}, fmt.Errorf("plugingen: codex mcp args is %T, want an array", v)
			}
			for _, it := range items {
				s, ok := it.(string)
				if !ok {
					return mcpServer{}, fmt.Errorf("plugingen: codex mcp arg is %T, want string", it)
				}
				entry.Args = append(entry.Args, s)
			}
		case "env":
			table, ok := v.(map[string]any)
			if !ok {
				return mcpServer{}, fmt.Errorf("plugingen: codex mcp env is %T, want a table", v)
			}
			entry.Env = map[string]string{}
			for ek, ev := range table {
				s, ok := ev.(string)
				if !ok {
					return mcpServer{}, fmt.Errorf("plugingen: codex mcp env %q is %T, want string", ek, ev)
				}
				entry.Env[ek] = s
			}
		default:
			return mcpServer{}, fmt.Errorf(
				"plugingen: codex MCP entry carries unmodelled key %q — teach plugingen's readCodexEntry about it before regenerating", k,
			)
		}
	}
	if entry.Command != pathBinary {
		return mcpServer{}, fmt.Errorf("plugingen: codex MCP command is %q, want %q", entry.Command, pathBinary)
	}
	return entry, nil
}

// codexInterface is plugin.json's presentation block. Only fields we can
// fill with real values are modelled — see the omissions note above.
type codexInterface struct {
	DisplayName      string   `json:"displayName"`
	ShortDescription string   `json:"shortDescription"`
	LongDescription  string   `json:"longDescription"`
	DeveloperName    string   `json:"developerName"`
	Category         string   `json:"category"`
	Capabilities     []string `json:"capabilities"`
	WebsiteURL       string   `json:"websiteURL"`
	DefaultPrompt    []string `json:"defaultPrompt"`
}

// codexDefaultPrompt is interface.defaultPrompt — "starter prompts shown
// in composer/UX context" per plugin-json-spec.md, which caps the array at
// 3 entries (later ones are dropped) and each string at 128 characters,
// preferring ~50 so they scan in the UI.
//
// REQUIRED, on evidence: the plugin-creator skill's own validate_plugin.py
// (codex-rs/skills/src/assets/samples/plugin-creator/scripts/) errors with
// "plugin.json field `interface.defaultPrompt` or `interface.default_prompt`
// is required" when it is absent. Running that validator against the
// assembled tree on 2026-07-31 is what found this; the field's optional-
// looking prose in the spec's field list is not the contract ingestion
// enforces.
//
// Each prompt must be answerable by a tool this plugin actually declares:
// they map to the MCP server's get_cost_summary, get_session_summary and
// get_project_patterns. A starter prompt the plugin cannot serve would be
// a listing that oversells itself.
func codexDefaultPrompt() []string {
	return []string{
		"What did my coding agents cost this week?",
		"Summarise my last session's token usage.",
		"What are the hot files in this project?",
	}
}

// codexPluginManifest is .codex-plugin/plugin.json.
type codexPluginManifest struct {
	Name        string         `json:"name"`
	Version     string         `json:"version"`
	Description string         `json:"description"`
	Author      author         `json:"author"`
	Homepage    string         `json:"homepage"`
	Repository  string         `json:"repository"`
	License     string         `json:"license"`
	Keywords    []string       `json:"keywords"`
	MCPServers  string         `json:"mcpServers"`
	Interface   codexInterface `json:"interface"`
}

// codexPluginDescription states the binary prerequisite plainly (§0).
const codexPluginDescription = "Token, cost and cache observability for Codex. " +
	"Wires the SuperBased Observer MCP server. " +
	binaryPrereqSentence + " " + installHint + " — this plugin is wiring only and installs no binary."

func renderCodexPluginJSON(version string) ([]byte, error) {
	return marshalJSON(codexPluginManifest{
		Name:        pluginName,
		Version:     version,
		Description: codexPluginDescription,
		Author:      author{Name: authorName, Email: authorMail, URL: authorURL},
		Homepage:    homepage,
		Repository:  repository,
		License:     license,
		Keywords:    pluginKeywords(),
		// The documented alternative is an inline object; the companion
		// file keeps ONE spelling of the server entry per surface and
		// matches how the first-party catalog ships it.
		MCPServers: "./.mcp.json",
		Interface: codexInterface{
			DisplayName:      "SuperBased Observer",
			ShortDescription: "Local-first token, cost and cache observability",
			LongDescription: "Query your own coding-agent history from inside Codex: per-session token " +
				"and cost totals, cache behaviour, project patterns and past tool output. All data stays " +
				"in a local SQLite database. " + binaryPrereqSentence + ".",
			DeveloperName: "SuperBased",
			Category:      codexCategory,
			// The MCP server's tools are queries over the local database.
			// "Write" is declared because ONE tool can write:
			// continue_session's opt-in write_file=true drops a
			// HANDOFF-<id>.md into the source project root. Everything
			// else only reads.
			Capabilities:  []string{"Read", "Write"},
			WebsiteURL:    homepage,
			DefaultPrompt: codexDefaultPrompt(),
		},
	})
}

// renderCodexMCPJSON emits the plugin-root .mcp.json. The shape is the
// stdio one the first-party catalog uses: a top-level "mcpServers" object
// whose entries carry command/args/env.
func renderCodexMCPJSON(entry mcpServer) ([]byte, error) {
	return marshalJSON(struct {
		MCPServers map[string]mcpServer `json:"mcpServers"`
	}{MCPServers: map[string]mcpServer{mcp.ServerName: entry}})
}

// codexMarketplaceSource is a plugins[] entry's source object. Only the
// "local" form is emitted: the plugin ships in the same repo as the
// catalog.
type codexMarketplaceSource struct {
	Source string `json:"source"`
	Path   string `json:"path"`
}

type codexMarketplacePolicy struct {
	Installation   string   `json:"installation"`
	Authentication string   `json:"authentication"`
	Products       []string `json:"products"`
}

type codexMarketplaceEntry struct {
	Name     string                 `json:"name"`
	Source   codexMarketplaceSource `json:"source"`
	Policy   codexMarketplacePolicy `json:"policy"`
	Category string                 `json:"category"`
}

type codexMarketplaceManifest struct {
	Name      string                  `json:"name"`
	Interface codexMarketplaceIface   `json:"interface"`
	Plugins   []codexMarketplaceEntry `json:"plugins"`
}

// codexMarketplaceIface mirrors the catalog's interface object, which the
// official example uses for the picker label only.
type codexMarketplaceIface struct {
	DisplayName string `json:"displayName"`
}

func renderCodexMarketplaceJSON() ([]byte, error) {
	return marshalJSON(codexMarketplaceManifest{
		Name:      marketplaceName,
		Interface: codexMarketplaceIface{DisplayName: "SuperBased"},
		Plugins: []codexMarketplaceEntry{{
			Name:   pluginName,
			Source: codexMarketplaceSource{Source: "local", Path: "./" + codexPluginPath},
			Policy: codexMarketplacePolicy{
				Installation:   codexInstallation,
				Authentication: codexAuthentication,
				Products:       []string{codexProduct},
			},
			Category: codexCategory,
		}},
	})
}

func codexReadme(entry mcpServer) string {
	var b strings.Builder
	b.WriteString(generatedBanner)
	b.WriteString(`
# SuperBased Observer — Codex plugin

Local-first token, cost and cache observability for Codex.

**` + binaryPrereqSentence + `.** This plugin is
wiring only — it declares an MCP server that runs ` + "`observer`" + `; it does not
download or bundle it.

` + "```bash" + `
npm i -g @superbased/observer     # or: pipx install superbased-observer
observer start                    # run the local daemon
` + "```" + `

## Layout

` + "```" + `
codex/                                  ← marketplace root
├── .agents/plugins/marketplace.json     ← the catalog
└── ` + codexPluginPath + `/
    ├── .codex-plugin/plugin.json        ← the plugin manifest
    └── .mcp.json                        ← the MCP server it bundles
` + "```" + `

Codex looks for a repo-scoped catalog at
` + "`$REPO_ROOT/.agents/plugins/marketplace.json`" + ` (and a personal one at
` + "`~/.agents/plugins/marketplace.json`" + `), so the marketplace root is the
directory that holds ` + "`.agents/`" + `. The entry's ` + "`source.path`" + ` is
` + "`./" + codexPluginPath + "`" + ` — relative to that root, ` + "`./`" + `-prefixed,
never ` + "`../`" + `.

## Install

` + "```bash" + `
codex plugin marketplace add superbasedapp/plugins
` + "```" + `

Then, inside Codex, open the plugin browser with ` + "`/plugins`" + `, find
**SuperBased Observer** under the ` + "`" + marketplaceName + "`" + ` marketplace, and
install it. Start a **new** thread afterwards — sessions already open do not
pick up newly installed plugin files.

Other documented marketplace verbs: ` + "`codex plugin marketplace list`" + `,
` + "`… upgrade [name]`" + `, ` + "`… remove <name>`" + `. Installs are cached under
` + "`~/.codex/plugins/cache/<marketplace>/<plugin>/<version>/`" + `.

## What it wires

| Component | What it declares |
|---|---|
| ` + "`.mcp.json`" + ` | The ` + "`" + mcp.ServerName + "`" + ` MCP server: ` + "`" + commandLine(entry) + "`" + ` — the same launch ` + "`observer init --codex`" + ` writes into ` + "`~/.codex/config.toml`" + `'s ` + "`[mcp_servers." + mcp.ServerName + "]`" + `, transposed into the plugin's JSON shape. |

**Hooks are NOT in this plugin.** Observer's Codex hook registrar writes
TOML into ` + "`~/.codex/config.toml`" + `, and a Codex plugin's hooks live in a
` + "`hooks/hooks.json`" + ` whose schema is not documented on any first-party page
— and the plugin-manifest spec notes that marketplace ingestion rejects an
unsupported ` + "`hooks`" + ` manifest field. Rather than guess a schema, this
plugin ships the MCP server only:

` + "```bash" + `
observer init --codex --skip-mcp    # hooks + proxy route, no duplicate MCP entry
` + "```" + `

## Don't double-wire

` + "`observer init --codex`" + ` writes the same ` + "`" + mcp.ServerName + "`" + ` server into
` + "`~/.codex/config.toml`" + `. Carrying both that entry and this plugin loads
observer's MCP tool schema twice per turn (wasted context, no data
corruption — observer's rows are keyed and upsert).

**Codex is not auto-detected.** The detect-and-skip ` + "`observer init`" + `
performs is ` + "`internal/claudeplugin`" + ` — claude-code-only, by design. There
is no Codex equivalent, so if you install this plugin, pass
` + "`--skip-mcp`" + ` to ` + "`observer init --codex`" + ` yourself (or run
` + "`observer uninstall --codex`" + ` and re-init with ` + "`--skip-mcp`" + ` if you
already wired it). This is documented, not built.

## Not submitted anywhere

This directory is the local/self-hosted marketplace form. The manifest
deliberately omits ` + "`interface`" + ` assets (logo, composerIcon, screenshots)
and the privacy/terms URLs a public directory submission requires — those
have to point at real files and real https:// pages, so an operator adds
them at submission time rather than shipping placeholders.

## Links

- Docs: ` + homepage + `
- Source: https://github.com/superbasedapp/observer
- License: ` + license + `
`)
	return b.String()
}
