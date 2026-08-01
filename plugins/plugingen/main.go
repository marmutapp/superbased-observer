// Command plugingen generates the in-tool plugin/extension manifests that
// let users wire SuperBased Observer from INSIDE their AI tool's own
// packaging surface, instead of only through `observer init`.
//
// Phase 1 (docs/plans/adapter-plugins-distribution-plan-2026-07-31.md §4)
// covers two surfaces:
//
//   - a Claude Code plugin + single-plugin marketplace catalog
//     (plugins/claude-code/), installed with `/plugin marketplace add …`
//     followed by `/plugin install …`;
//   - a Cursor one-click MCP install deeplink (plugins/cursor/).
//
// Phases 2 and 3 add four more:
//
//   - a Gemini CLI extension (plugins/gemini/), installed with
//     `gemini extensions install <repo-url>`;
//   - a Goose directory listing (plugins/goose/) — Goose extensions ARE
//     MCP servers, so this is the exact config.yaml block plus the
//     `goose configure` walkthrough, no manifest of its own;
//   - a Codex plugin + local marketplace catalog (plugins/codex/),
//     installed with `codex plugin marketplace add …`;
//   - an OpenCode npm plugin (plugins/opencode/) whose `config` hook
//     injects the MCP server, the only hand-written glue in the tree
//     besides plugingen itself.
//
// Grok Build ships NO artifact: docs.x.ai grounds that it reads Claude
// Code marketplaces/plugins/MCPs directly, and its own plugin.json schema
// is not publicly documented, so plugins/claude-code IS the Grok surface.
// See the plan's Phase 2/3 section.
//
// # Why this is generated, not hand-authored (§3, the one-owner rule)
//
// `observer init`'s registrars are the source of truth for WHAT observer
// declares into an AI tool: internal/mcp's Registrar owns the MCP server
// entry, internal/hook's Registry owns the Claude Code hook commands. A
// hand-written plugin manifest is how that wiring forks silently — the
// plugin keeps declaring last year's hook events while init declares this
// year's, and nothing fails loudly.
//
// So plugingen does not RE-DESCRIBE the wiring; it EXECUTES the real
// registrars against a throwaway sandbox HOME, reads back the files they
// wrote, and transposes those exact entries into each surface's manifest
// format. A new hook event or a changed MCP argument therefore propagates
// to the plugin manifests the moment the registrar changes, and CI's
// `make verify-plugins-build` fails until the regenerated manifests are
// committed.
//
// # The one deliberate deviation: binary resolution
//
// `observer init` registers the ABSOLUTE path of the running binary,
// because init knows which binary is running. A published plugin cannot:
// it is cache-copied onto machines whose install path we don't know, and
// `${CLAUDE_PLUGIN_ROOT}` points at the plugin copy, not at observer. So
// plugingen drives the registrars with BinaryPath = "observer" — the name
// npm/PyPI/brew put on PATH. Everything else (args, event roster, event
// argument spelling, command shape) is whatever the registrar produced.
//
// # Determinism
//
// The gate diffs a fresh run against the committed tree, so the output has
// to be byte-stable: no timestamps, no map-iteration order, no absolute
// paths (the sandbox HOME never appears in any output), 2-space JSON
// indent, single trailing newline. TestGenerateIsDeterministic pins it.
//
// Usage:
//
//	go run ./plugins/plugingen              # rewrite ./plugins in place
//	go run ./plugins/plugingen -out /tmp/x  # generate elsewhere (the gate)
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/claudeplugin"
	"github.com/marmutapp/superbased-observer/internal/hook"
	"github.com/marmutapp/superbased-observer/internal/mcp"
	"github.com/marmutapp/superbased-observer/internal/mcp/locate"
)

// pathBinary is the command a published plugin must use to reach observer.
// See the "one deliberate deviation" note in the package comment.
const pathBinary = "observer"

// Catalog identity comes from internal/claudeplugin — the ONE owner of
// the plugin's name, its marketplace name and its directory name. The
// init registrars and `observer doctor` detect an installed plugin by
// matching those same strings on disk, so a rename here that didn't reach
// them would silently break the double-wiring guard.
//
// marketplaceName is what users type after the `@` in
// `/plugin install <plugin>@<marketplace>`; it must be kebab-case and must
// not collide with Anthropic's reserved marketplace names. pluginDir is
// the plugin's directory name INSIDE the marketplace root: a marketplace
// entry's relative source resolves against that root (the dir holding
// .claude-plugin/) and `../` is forbidden, so the plugin lives under it.
//
// All three are the single word `superbased` as of the 2026-07-31 rename
// (one brand, one catalog, one plugin): the install line reads
// `/plugin install superbased@superbased`, the plugin lives at
// <root>/superbased/, and the dedupe key is `superbased@superbased`. The
// rename was free because nothing had been published — see
// internal/claudeplugin for why that window is now closed.
const (
	marketplaceName = claudeplugin.Marketplace
	pluginName      = claudeplugin.Name
	pluginDir       = claudeplugin.Dir

	homepage   = "https://superbased.app/"
	repository = "https://github.com/superbasedapp/plugins"
	license    = "Apache-2.0"
	authorName = "Santosh Kathira"
	authorMail = "contact@superbased.app"
	authorURL  = "https://superbased.app/"
)

func main() {
	out := flag.String("out", "plugins", "output directory (the committed plugins/ tree)")
	versionFile := flag.String("version-file", filepath.Join("npm", "observer", "package.json"),
		"package.json whose \"version\" field stamps the generated manifests")
	// -honesty-check is the SHARED half of the listing-honesty gate: it
	// generates nothing and instead applies honesty.go's rule (literal
	// banned phrases + the %-near-efficiency-vocabulary pattern class +
	// the verbatim prerequisite sentence in every README) to every file of
	// an already-assembled tree. scripts/assemble-plugins-repo.sh shells
	// out to it so the public tree — including the landing README that
	// script authors itself — is gated by the same rule as the generated
	// files, from one owner in Go.
	honestyCheck := flag.String("honesty-check", "",
		"scan an already-assembled tree for savings claims and missing prerequisite sentences, then exit (generates nothing)")
	flag.Parse()

	if *honestyCheck != "" {
		if err := honestyCheckTree(*honestyCheck); err != nil {
			log.Fatalf("%v", err)
		}
		return
	}

	if err := run(*out, *versionFile); err != nil {
		log.Fatalf("plugingen: %v", err)
	}
}

func run(outDir, versionFile string) error {
	version, err := readVersion(versionFile)
	if err != nil {
		return err
	}
	files, err := generate(version)
	if err != nil {
		return err
	}
	for _, f := range files {
		dest := filepath.Join(outDir, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return fmt.Errorf("plugingen: mkdir %s: %w", filepath.Dir(dest), err)
		}
		if err := os.WriteFile(dest, f.Data, 0o644); err != nil { //nolint:gosec // G306: build-time generator emitting committed, publicly-distributed plugin manifests; world-readable is intended.
			return fmt.Errorf("plugingen: write %s: %w", dest, err)
		}
	}
	return nil
}

// outFile is one generated artifact. Path is always slash-separated and
// relative to the output dir.
type outFile struct {
	Path string
	Data []byte
}

// generate derives the live wiring from the init registrars and renders
// every Phase-1 artifact. It is pure with respect to the repo: the only
// filesystem it touches is a throwaway sandbox HOME it creates and removes.
func generate(version string) ([]outFile, error) {
	w, err := deriveWiring()
	if err != nil {
		return nil, err
	}

	pluginJSON, err := renderPluginJSON(version)
	if err != nil {
		return nil, err
	}
	mcpJSON, err := renderMCPJSON(w.claudeCodeMCP)
	if err != nil {
		return nil, err
	}
	hooksJSON, err := renderHooksJSON(w.claudeCodeHooks)
	if err != nil {
		return nil, err
	}
	marketplaceJSON, err := renderMarketplaceJSON(version)
	if err != nil {
		return nil, err
	}
	deeplink, err := cursorDeeplink(w.cursorMCP)
	if err != nil {
		return nil, err
	}

	// Gemini and Goose have no registrar of their own; they carry the
	// launch every registrar agrees on. See canonicalStdio.
	stdio, err := canonicalStdio(w)
	if err != nil {
		return nil, err
	}
	geminiJSON, err := renderGeminiExtension(stdio, version)
	if err != nil {
		return nil, err
	}
	codexPluginJSON, err := renderCodexPluginJSON(version)
	if err != nil {
		return nil, err
	}
	codexMCPJSON, err := renderCodexMCPJSON(w.codexMCP)
	if err != nil {
		return nil, err
	}
	codexMarketplaceJSON, err := renderCodexMarketplaceJSON()
	if err != nil {
		return nil, err
	}

	// Coverage wave A (plan §7). Five of the six are registrar-less and
	// carry canonicalStdio, exactly like Gemini and Goose; droid is the
	// exception and transposes its own registrar's ~/.factory/mcp.json
	// entry. None declares a hook: `observer hook <tool>` accepts
	// claude-code, cursor, codex and hermes only, so a hook on any of
	// these surfaces would name a command that does not exist.
	kimiPluginJSON, err := renderKimiPluginJSON(stdio, version)
	if err != nil {
		return nil, err
	}
	qoderPluginJSON, err := renderQoderPluginJSON(version)
	if err != nil {
		return nil, err
	}
	qoderMCPJSON, err := renderQoderMCPJSON(stdio)
	if err != nil {
		return nil, err
	}
	qoderMarketplaceJSON, err := renderQoderMarketplaceJSON()
	if err != nil {
		return nil, err
	}
	devinPluginJSON, err := renderDevinPluginJSON(version)
	if err != nil {
		return nil, err
	}
	devinMCPConfigJSON, err := renderDevinMCPConfigJSON(stdio)
	if err != nil {
		return nil, err
	}
	droidPluginJSON, err := renderDroidPluginJSON(version)
	if err != nil {
		return nil, err
	}
	droidMCPJSON, err := renderDroidMCPJSON(w.droidMCP)
	if err != nil {
		return nil, err
	}
	droidMarketplaceJSON, err := renderDroidMarketplaceJSON()
	if err != nil {
		return nil, err
	}
	openClawManifestJSON, err := renderOpenClawManifest(stdio, version)
	if err != nil {
		return nil, err
	}
	antigravityPluginJSON, err := renderAntigravityPluginJSON()
	if err != nil {
		return nil, err
	}
	antigravityMCPConfigJSON, err := renderAntigravityMCPConfigJSON(stdio)
	if err != nil {
		return nil, err
	}

	coverage, err := coverageListingFiles(w, stdio, version)
	if err != nil {
		return nil, err
	}

	files := []outFile{
		{Path: "claude-code/.claude-plugin/marketplace.json", Data: marketplaceJSON},
		{Path: "claude-code/" + pluginDir + "/.claude-plugin/plugin.json", Data: pluginJSON},
		{Path: "claude-code/" + pluginDir + "/.mcp.json", Data: mcpJSON},
		{Path: "claude-code/" + pluginDir + "/hooks/hooks.json", Data: hooksJSON},
		{Path: "claude-code/" + pluginDir + "/README.md", Data: []byte(pluginReadme(w))},
		{Path: "cursor/deeplink.txt", Data: []byte(deeplink + "\n")},
		{Path: "cursor/README.md", Data: []byte(cursorReadme(deeplink, w.cursorMCP))},
		{Path: "gemini/gemini-extension.json", Data: geminiJSON},
		{Path: "gemini/README.md", Data: []byte(geminiReadme(stdio))},
		{Path: "goose/README.md", Data: []byte(gooseReadme(stdio))},
		{Path: "codex/.agents/plugins/marketplace.json", Data: codexMarketplaceJSON},
		{Path: "codex/" + codexPluginPath + "/.codex-plugin/plugin.json", Data: codexPluginJSON},
		{Path: "codex/" + codexPluginPath + "/.mcp.json", Data: codexMCPJSON},
		{Path: "codex/README.md", Data: []byte(codexReadme(w.codexMCP))},
		{Path: "opencode/src/wiring.generated.ts", Data: []byte(openCodeWiringTS(w.openCodeMCP))},
		{Path: "opencode/README.md", Data: []byte(openCodeReadme(w.openCodeMCP))},

		// -- coverage wave A ------------------------------------------
		{Path: kimiSurfaceDir + "/" + pluginDir + "/kimi.plugin.json", Data: kimiPluginJSON},
		{Path: kimiSurfaceDir + "/README.md", Data: []byte(kimiReadme(stdio))},
		{Path: qoderSurfaceDir + "/.qoder-plugin/marketplace.json", Data: qoderMarketplaceJSON},
		{Path: qoderSurfaceDir + "/" + pluginDir + "/.qoder-plugin/plugin.json", Data: qoderPluginJSON},
		{Path: qoderSurfaceDir + "/" + pluginDir + "/.mcp.json", Data: qoderMCPJSON},
		{Path: qoderSurfaceDir + "/README.md", Data: []byte(qoderReadme(stdio))},
		{Path: devinSurfaceDir + "/" + pluginDir + "/.devin-plugin/plugin.json", Data: devinPluginJSON},
		{Path: devinSurfaceDir + "/" + pluginDir + "/mcp_config.json", Data: devinMCPConfigJSON},
		{Path: devinSurfaceDir + "/README.md", Data: []byte(devinReadme(stdio))},
		{Path: droidSurfaceDir + "/.factory-plugin/marketplace.json", Data: droidMarketplaceJSON},
		{Path: droidSurfaceDir + "/" + droidPluginDir + "/.factory-plugin/plugin.json", Data: droidPluginJSON},
		{Path: droidSurfaceDir + "/" + droidPluginDir + "/mcp.json", Data: droidMCPJSON},
		{Path: droidSurfaceDir + "/README.md", Data: []byte(droidReadme(w.droidMCP))},
		{Path: openClawSurfaceDir + "/" + pluginDir + "/openclaw.plugin.json", Data: openClawManifestJSON},
		{Path: openClawSurfaceDir + "/README.md", Data: []byte(openClawReadme(stdio))},
		{Path: antigravitySurfaceDir + "/" + pluginDir + "/plugin.json", Data: antigravityPluginJSON},
		{Path: antigravitySurfaceDir + "/" + pluginDir + "/mcp_config.json", Data: antigravityMCPConfigJSON},
		{Path: antigravitySurfaceDir + "/README.md", Data: []byte(antigravityReadme(stdio))},
	}
	files = append(files, coverage...)

	// The index README's "Version stamping" section enumerates the
	// version-bearing manifests. That list is DERIVED from the files just
	// rendered rather than re-typed in prose — the prose went stale once
	// already (it claimed nine files and a four-file stamper while the
	// generator emitted ten and the stamper covered all of them). So the
	// README is rendered LAST, over the tree it is describing.
	files = append(files, outFile{
		Path: "README.md",
		Data: []byte(topReadme(w, stdio, versionBearingPaths(files, version))),
	})
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// handWrittenVersionBearingPaths are the files under plugins/ that carry
// the release version but are NOT generated — plugingen owns the OpenCode
// package's wiring constants and README, not its npm plumbing (see
// opencode.go). Declared rather than scanned because generate() never
// reads the repository tree; TestVersionBearingInventoryIsComplete checks
// the declaration against what is actually on disk.
func handWrittenVersionBearingPaths() []string {
	return []string{"opencode/package.json"}
}

// versionBearingPaths returns the slash-separated paths of the generated
// JSON artifacts that carry version somewhere in them, sorted. "Carries
// it" is decided STRUCTURALLY — any string-valued key named "version", at
// any depth, whose value is the release version — which is the same rule
// scripts/assemble-plugins-repo.sh's assert_every_version_field walks the
// assembled tree with. A key merely ENDING in "version" (`manifest_version`
// in the .mcpb bundle manifest, whose value is the bundle-spec version and
// not ours) is a different key and is not counted.
func versionBearingPaths(files []outFile, version string) []string {
	var out []string
	for _, f := range files {
		if !strings.HasSuffix(f.Path, ".json") {
			continue
		}
		var doc any
		if err := json.Unmarshal(f.Data, &doc); err != nil {
			// A generated file that is not valid JSON is caught loudly by
			// the shape tests; it simply carries no version field here.
			continue
		}
		if jsonCarriesVersion(doc, version) {
			out = append(out, f.Path)
		}
	}
	sort.Strings(out)
	return out
}

// jsonCarriesVersion reports whether node has a string-valued "version"
// key equal to want, at any depth.
func jsonCarriesVersion(node any, want string) bool {
	switch v := node.(type) {
	case map[string]any:
		for key, val := range v {
			if key == "version" {
				if s, ok := val.(string); ok && s == want {
					return true
				}
				continue
			}
			if jsonCarriesVersion(val, want) {
				return true
			}
		}
	case []any:
		for _, item := range v {
			if jsonCarriesVersion(item, want) {
				return true
			}
		}
	}
	return false
}

// coverageListingFiles renders the coverage waves B and C — the surfaces
// whose artifact is a config block or a special format rather than a
// plugin manifest. It is split out of generate() deliberately: every one
// of these is a render-then-append with the same error shape, and folding
// them inline pushed generate() past the repository's cyclomatic-
// complexity gate for no readability gain.
func coverageListingFiles(w wiring, stdio mcpServer, version string) ([]outFile, error) {
	// Coverage wave B (plan §7) — config LISTINGS. None of these tools has
	// a package format to build against, so the artifact is the exact
	// config block a user pastes, rendered from the registrar-derived
	// launch rather than hand-typed. Five carry canonicalStdio; kilo-code
	// carries the OpenCode registrar's own typed-local entry (its config
	// IS OpenCode's), and open-interpreter carries the Codex registrar's
	// own TOML table text (its config IS Codex's, at a different home).
	crushBlock, err := renderCrushConfigJSON(stdio)
	if err != nil {
		return nil, err
	}
	kiroBlock, err := renderKiroMCPJSON(stdio)
	if err != nil {
		return nil, err
	}
	copilotCLIBlock, err := renderCopilotCLIConfigJSON(stdio)
	if err != nil {
		return nil, err
	}
	kiloCodeBlock, err := renderKiloCodeConfigJSON(w.openCodeMCP)
	if err != nil {
		return nil, err
	}
	rooBlock, err := renderRooMCPJSON(stdio)
	if err != nil {
		return nil, err
	}

	// Coverage wave C (plan §7) — special formats. copilot is a one-click
	// install URI; cowork is an .mcpb manifest (LIVE-VERIFY GATED — see
	// cowork.go); pi and command-code are decisions NOT to build a package,
	// each documented with its evidence (pi.go, commandcode.go).
	copilotURI, err := copilotInstallURI(stdio)
	if err != nil {
		return nil, err
	}
	copilotWorkspaceBlock, err := renderCopilotWorkspaceJSON(stdio)
	if err != nil {
		return nil, err
	}
	coworkManifestJSON, err := renderCoworkManifest(stdio, version)
	if err != nil {
		return nil, err
	}
	coworkDesktopBlock, err := renderCoworkDesktopConfigJSON(stdio)
	if err != nil {
		return nil, err
	}
	commandCodeBlock, err := renderCommandCodeMCPJSON(w.commandCodeMCP)
	if err != nil {
		return nil, err
	}

	return []outFile{
		// -- coverage wave B (config listings) -------------------------
		{Path: crushSurfaceDir + "/README.md", Data: []byte(crushReadme(stdio, crushBlock))},
		{Path: kiroSurfaceDir + "/README.md", Data: []byte(kiroReadme(stdio, kiroBlock))},
		{Path: copilotCLISurfaceDir + "/README.md", Data: []byte(copilotCLIReadme(stdio, copilotCLIBlock))},
		{Path: kiloCodeSurfaceDir + "/README.md", Data: []byte(kiloCodeReadme(w.openCodeMCP, kiloCodeBlock))},
		{Path: rooSurfaceDir + "/README.md", Data: []byte(rooReadme(stdio, rooBlock))},
		{Path: openInterpreterSurfaceDir + "/README.md", Data: []byte(openInterpreterReadme(stdio, w.codexTOMLBlock))},

		// -- coverage wave C (special formats + two no-build decisions) -
		{Path: copilotSurfaceDir + "/install-uri.txt", Data: []byte(copilotURI + "\n")},
		{Path: copilotSurfaceDir + "/README.md", Data: []byte(copilotReadme(stdio, copilotURI, copilotWorkspaceBlock))},
		{Path: coworkSurfaceDir + "/" + pluginDir + "/manifest.json", Data: coworkManifestJSON},
		{Path: coworkSurfaceDir + "/README.md", Data: []byte(coworkReadme(stdio, coworkDesktopBlock))},
		{Path: piSurfaceDir + "/README.md", Data: []byte(piReadme())},
		{Path: commandCodeSurfaceDir + "/README.md", Data: []byte(commandCodeReadme(w.commandCodeMCP, commandCodeBlock))},
	}, nil
}

// -------------------------------------------------------------------
// Derivation: run the real registrars, read back what they wrote.
// -------------------------------------------------------------------

// mcpServer is the documented stdio MCP server shape shared by the Claude
// Code plugin `.mcp.json` and Cursor's mcp.json / deeplink config. Field
// order here is the emitted JSON key order.
type mcpServer struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// wiring is the live init-registrar output, in the tools' own vocabulary.
type wiring struct {
	claudeCodeMCP mcpServer
	cursorMCP     mcpServer
	// codexMCP is ~/.codex/config.toml's [mcp_servers.observer] table,
	// converted to the canonical stdio shape.
	codexMCP mcpServer
	// droidMCP is ~/.factory/mcp.json's observer entry — the one wave-A
	// coverage surface with a real registrar behind it (internal/mcp/locate
	// carries a `droid` row). See droid.go.
	droidMCP mcpServer
	// commandCodeMCP is ~/.commandcode/mcp.json's observer entry — the
	// wave-C coverage surface with a real registrar behind it
	// (internal/mcp/locate carries a `command-code` row). See
	// commandcode.go.
	commandCodeMCP mcpServer
	// codexTOMLBlock is the [mcp_servers.observer] TABLE TEXT the codex
	// registrar wrote, lifted verbatim. Open Interpreter reads the same
	// TOML shape from its own home, so its listing publishes the
	// registrar's own bytes rather than a re-rendering. See
	// openinterpreter.go.
	codexTOMLBlock string
	// openCodeMCP is opencode.json's mcp."observer" entry — OpenCode's own
	// typed-local shape ({"type":"local","command":[…],"enabled":true}),
	// NOT the {command,args} one, so it keeps its own struct.
	openCodeMCP openCodeServer
	// claudeCodeHooks is the settings.json "hooks" object verbatim:
	// event name -> array of {matcher, hooks:[{type, command}]} groups.
	// Kept as raw JSON so a future registrar field survives the transpose
	// instead of being silently dropped by an out-of-date struct here.
	claudeCodeHooks map[string]json.RawMessage
}

// deriveWiring registers observer into a sandbox HOME with the real
// registrars and reads the resulting config files back.
func deriveWiring() (wiring, error) {
	var w wiring

	home, err := os.MkdirTemp("", "plugingen-home-")
	if err != nil {
		return w, fmt.Errorf("plugingen: sandbox home: %w", err)
	}
	defer func() { _ = os.RemoveAll(home) }()

	reg, err := mcp.NewRegistrar(mcp.RegisterOptions{BinaryPath: pathBinary, HomeDir: home})
	if err != nil {
		return w, fmt.Errorf("plugingen: mcp.NewRegistrar: %w", err)
	}
	// Every client the MCP registrar supports is registered, then read
	// back through that client's own parser. claude-code and cursor share
	// the {"mcpServers":{…}} JSON shape; codex writes TOML; opencode writes
	// its own typed-local JSON. Registering ALL of them (rather than only
	// the ones a manifest quotes) is what lets canonicalStdio assert they
	// still agree — the guard behind the Gemini and Goose surfaces, which
	// have no registrar of their own.
	for _, tc := range []struct {
		tool string
		read func(home string) (mcpServer, error)
		dst  *mcpServer
	}{
		{"claude-code", func(h string) (mcpServer, error) { return readMCPEntry("claude-code", h) }, &w.claudeCodeMCP},
		{"cursor", func(h string) (mcpServer, error) { return readMCPEntry("cursor", h) }, &w.cursorMCP},
		{"codex", readCodexEntry, &w.codexMCP},
		{"droid", readDroidEntry, &w.droidMCP},
		{"command-code", readCommandCodeEntry, &w.commandCodeMCP},
	} {
		res := reg.Register(tc.tool)
		if res.Error != nil {
			return w, fmt.Errorf("plugingen: mcp register %s: %w", tc.tool, res.Error)
		}
		if !res.Added {
			return w, fmt.Errorf("plugingen: mcp register %s: registrar reported no write", tc.tool)
		}
		entry, err := tc.read(home)
		if err != nil {
			return w, err
		}
		*tc.dst = entry
	}

	// The Open Interpreter listing publishes the codex registrar's own
	// TOML bytes (same [mcp_servers.<name>] format, different home), so
	// the table is lifted here while the sandbox still exists.
	block, err := extractCodexServerTable(home)
	if err != nil {
		return w, err
	}
	w.codexTOMLBlock = block

	if res := reg.Register("opencode"); res.Error != nil {
		return w, fmt.Errorf("plugingen: mcp register opencode: %w", res.Error)
	} else if !res.Added {
		return w, errors.New("plugingen: mcp register opencode: registrar reported no write")
	}
	openCode, err := readOpenCodeEntry(home)
	if err != nil {
		return w, err
	}
	w.openCodeMCP = openCode

	hooks, err := deriveClaudeCodeHooks(home)
	if err != nil {
		return w, err
	}
	w.claudeCodeHooks = hooks

	return w, nil
}

// readMCPEntry pulls the "observer" entry out of the config file the MCP
// registrar just wrote for tool, and converts it to the canonical stdio
// shape. An entry key we don't model is a hard error rather than a silent
// drop — that is the whole point of generating from the registrar.
func readMCPEntry(tool, home string) (mcpServer, error) {
	loc, ok := locate.ForClient(tool, home)
	if !ok {
		return mcpServer{}, fmt.Errorf("plugingen: locate.ForClient(%q): not supported", tool)
	}
	raw, err := os.ReadFile(loc.Path)
	if err != nil {
		return mcpServer{}, fmt.Errorf("plugingen: read %s config: %w", tool, err)
	}
	var doc struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return mcpServer{}, fmt.Errorf("plugingen: parse %s config: %w", tool, err)
	}
	entryRaw, ok := doc.MCPServers[mcp.ServerName]
	if !ok {
		return mcpServer{}, fmt.Errorf("plugingen: %s config has no %q server", tool, mcp.ServerName)
	}

	var keys map[string]json.RawMessage
	if err := json.Unmarshal(entryRaw, &keys); err != nil {
		return mcpServer{}, fmt.Errorf("plugingen: parse %s entry: %w", tool, err)
	}
	for k := range keys {
		switch k {
		case "command", "args", "env":
		default:
			return mcpServer{}, fmt.Errorf(
				"plugingen: %s MCP entry carries unmodelled key %q — teach plugingen's mcpServer struct about it before regenerating", tool, k,
			)
		}
	}

	var entry mcpServer
	if err := json.Unmarshal(entryRaw, &entry); err != nil {
		return mcpServer{}, fmt.Errorf("plugingen: decode %s entry: %w", tool, err)
	}
	if entry.Command != pathBinary {
		return mcpServer{}, fmt.Errorf("plugingen: %s MCP command is %q, want %q", tool, entry.Command, pathBinary)
	}
	return entry, nil
}

// canonicalStdio returns THE stdio launch every MCP registrar agrees on,
// and errors if they disagree.
//
// Gemini CLI, Goose, Kimi Code, Qoder, Devin, OpenClaw and Antigravity
// have no registrar in internal/mcp — none of those clients has a guarded
// write path into its config (locate.Locations carries no row for them,
// and internal/integration records MCP: nil for each). Their surfaces
// therefore cannot transpose "the entry the registrar wrote for this
// tool"; there isn't one. (Claude Code, Cursor, Codex, OpenCode and Droid
// do have one, and their surfaces transpose it directly.)
//
// What they CAN transpose honestly is the entry every registrar writes:
// claude-code, cursor and codex each declare the same command and the same
// argument vector, in three different file formats, and opencode declares
// the same words joined into one argv. This function asserts that
// agreement and hands back the single entry. If a future registrar starts
// writing a per-tool argument, the assertion fails and the generator
// refuses to emit rather than guessing which variant Gemini should get.
func canonicalStdio(w wiring) (mcpServer, error) {
	base := w.claudeCodeMCP
	for _, other := range []struct {
		tool  string
		entry mcpServer
	}{
		{"cursor", w.cursorMCP},
		{"codex", w.codexMCP},
		// droid and command-code register through the same generic JSON
		// writer as claude-code and cursor, into their own files. They
		// join the agreement set for the same reason the others are in
		// it: the registrar-less surfaces (gemini, goose, kimi-code,
		// qoder, devin, openclaw, antigravity and every wave-B/C config
		// listing) can only honestly carry a launch that EVERY registrar
		// writes, so every registrar has to be checked.
		{"droid", w.droidMCP},
		{"command-code", w.commandCodeMCP},
	} {
		if !sameMCPServer(base, other.entry) {
			return mcpServer{}, fmt.Errorf(
				"plugingen: registrars disagree on the stdio launch — claude-code writes %s, %s writes %s; "+
					"the Gemini/Goose surfaces have no registrar of their own and can only carry an entry every registrar agrees on",
				commandLine(base), other.tool, commandLine(other.entry),
			)
		}
	}
	// OpenCode's entry is compared in FULL, not just on the flattened
	// argv: its shape also carries `enabled` and `environment`
	// (opencodeMCPEntry in internal/mcp/register.go), and a registrar that
	// started setting either would otherwise leave the Gemini and Goose
	// snippets — which carry neither — silently divergent from what
	// observer actually declares.
	argv := append([]string{base.Command}, base.Args...)
	if strings.Join(w.openCodeMCP.Command, "\x00") != strings.Join(argv, "\x00") {
		return mcpServer{}, fmt.Errorf(
			"plugingen: opencode's argv %v does not match the canonical launch %v", w.openCodeMCP.Command, argv,
		)
	}
	if !sameStringMap(w.openCodeMCP.Environment, base.Env) {
		return mcpServer{}, fmt.Errorf(
			"plugingen: opencode declares environment %v but the canonical entry declares env %v — "+
				"the Gemini and Goose surfaces carry no environment and would silently diverge",
			w.openCodeMCP.Environment, base.Env,
		)
	}
	if !w.openCodeMCP.Enabled {
		return mcpServer{}, errors.New(
			"plugingen: the opencode registrar wrote enabled=false — every other surface declares an " +
				"unconditionally active server, so the agreement the registrar-less surfaces rely on no longer holds",
		)
	}
	return base, nil
}

// sameStringMap compares two string maps, treating nil and empty as equal
// (the registrars omit an absent env rather than writing an empty object).
func sameStringMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// sameMCPServer compares two stdio entries field by field, including env.
func sameMCPServer(a, b mcpServer) bool {
	if a.Command != b.Command || len(a.Args) != len(b.Args) || len(a.Env) != len(b.Env) {
		return false
	}
	for i := range a.Args {
		if a.Args[i] != b.Args[i] {
			return false
		}
	}
	for k, v := range a.Env {
		if b.Env[k] != v {
			return false
		}
	}
	return true
}

// deriveClaudeCodeHooks registers the Claude Code hooks into the sandbox
// and returns settings.json's "hooks" object verbatim.
func deriveClaudeCodeHooks(home string) (map[string]json.RawMessage, error) {
	reg, err := hook.NewRegistry(hook.Options{
		BinaryPath:    pathBinary,
		HomeDir:       home,
		ChecksumsPath: filepath.Join(home, ".observer", "hook_checksums.json"),
	})
	if err != nil {
		return nil, fmt.Errorf("plugingen: hook.NewRegistry: %w", err)
	}
	res := reg.Register("claude-code")
	if res.Error != nil {
		return nil, fmt.Errorf("plugingen: hook register claude-code: %w", res.Error)
	}
	if len(res.HooksAdded) == 0 {
		return nil, errors.New("plugingen: hook register claude-code: registrar added no events")
	}

	raw, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		return nil, fmt.Errorf("plugingen: read claude-code settings: %w", err)
	}
	var doc struct {
		Hooks map[string]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("plugingen: parse claude-code settings: %w", err)
	}
	if len(doc.Hooks) == 0 {
		return nil, errors.New("plugingen: claude-code settings.json has no hooks object")
	}
	// The sandbox path must never leak into a published manifest.
	if bytes.Contains(raw, []byte(home)) {
		return nil, fmt.Errorf("plugingen: sandbox home %q leaked into the hook commands", home)
	}
	return doc.Hooks, nil
}

// -------------------------------------------------------------------
// Rendering
// -------------------------------------------------------------------

// author is the manifest author/owner object shape shared by plugin.json
// (author) and marketplace.json (owner).
type author struct {
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
	URL   string `json:"url,omitempty"`
}

// pluginManifest is .claude-plugin/plugin.json. `name` is the only field
// Claude Code requires; the rest is metadata. Component paths are left at
// their defaults (.mcp.json and hooks/hooks.json at the plugin root), so
// no path-override keys are emitted.
type pluginManifest struct {
	Name        string   `json:"name"`
	DisplayName string   `json:"displayName,omitempty"`
	Version     string   `json:"version,omitempty"`
	Description string   `json:"description,omitempty"`
	Author      author   `json:"author"`
	Homepage    string   `json:"homepage,omitempty"`
	Repository  string   `json:"repository,omitempty"`
	License     string   `json:"license,omitempty"`
	Keywords    []string `json:"keywords,omitempty"`
}

// binaryPrereqSentence is THE prerequisite sentence. Every listing this
// tree publishes — each manifest description AND each README — carries it
// verbatim, so the plan's honesty rule (§0: "each plugin's description must
// state the binary prerequisite plainly") has ONE source of truth rather
// than a family of near-misses that a substring test would each accept.
// TestListingsStateThePrerequisiteAndClaimNoSavings asserts the verbatim
// presence, including in the hand-written OpenCode package.json.
const binaryPrereqSentence = "Requires the `observer` binary already on your PATH"

// installHint names the two channels that actually put the binary on PATH.
const installHint = "(npm i -g @superbased/observer, or pipx install superbased-observer)"

// pluginDescription states the binary prerequisite plainly — the plan's
// honesty rule for every published listing (§0).
const pluginDescription = "Token, cost and cache observability for Claude Code. " +
	"Wires the SuperBased Observer MCP server plus its session/tool lifecycle hooks. " +
	binaryPrereqSentence + " " + installHint + " — this plugin is wiring only and installs no binary."

func pluginKeywords() []string {
	return []string{"cost", "cost-tracking", "mcp", "observability", "tokens", "usage"}
}

func renderPluginJSON(version string) ([]byte, error) {
	return marshalJSON(pluginManifest{
		Name:        pluginName,
		DisplayName: "SuperBased Observer",
		Version:     version,
		Description: pluginDescription,
		Author:      author{Name: authorName, Email: authorMail, URL: authorURL},
		Homepage:    homepage,
		Repository:  repository,
		License:     license,
		Keywords:    pluginKeywords(),
	})
}

func renderMCPJSON(entry mcpServer) ([]byte, error) {
	return marshalJSON(struct {
		MCPServers map[string]mcpServer `json:"mcpServers"`
	}{MCPServers: map[string]mcpServer{mcp.ServerName: entry}})
}

func renderHooksJSON(hooks map[string]json.RawMessage) ([]byte, error) {
	return marshalJSON(struct {
		Hooks map[string]json.RawMessage `json:"hooks"`
	}{Hooks: hooks})
}

// marketplaceEntry is one row of marketplace.json's "plugins" array.
type marketplaceEntry struct {
	Name        string   `json:"name"`
	Source      string   `json:"source"`
	DisplayName string   `json:"displayName,omitempty"`
	Description string   `json:"description,omitempty"`
	Version     string   `json:"version,omitempty"`
	Author      author   `json:"author"`
	Homepage    string   `json:"homepage,omitempty"`
	Repository  string   `json:"repository,omitempty"`
	License     string   `json:"license,omitempty"`
	Keywords    []string `json:"keywords,omitempty"`
	Category    string   `json:"category,omitempty"`
}

// marketplaceManifest is .claude-plugin/marketplace.json. name, owner and
// plugins are the required fields.
type marketplaceManifest struct {
	Name        string             `json:"name"`
	Owner       author             `json:"owner"`
	Description string             `json:"description,omitempty"`
	Plugins     []marketplaceEntry `json:"plugins"`
}

func renderMarketplaceJSON(version string) ([]byte, error) {
	return marshalJSON(marketplaceManifest{
		Name:        marketplaceName,
		Owner:       author{Name: "SuperBased", Email: authorMail, URL: authorURL},
		Description: "SuperBased plugins for AI coding agents — local-first token, cost and cache observability.",
		Plugins: []marketplaceEntry{{
			Name:        pluginName,
			Source:      "./" + pluginDir,
			DisplayName: "SuperBased Observer",
			Description: pluginDescription,
			Version:     version,
			Author:      author{Name: authorName, Email: authorMail, URL: authorURL},
			Homepage:    homepage,
			Repository:  repository,
			License:     license,
			Keywords:    pluginKeywords(),
			Category:    "observability",
		}},
	})
}

// cursorDeeplink builds the one-click Cursor MCP install URL. The `config`
// query parameter carries base64 (standard alphabet, padded) of the bare
// server object — exactly the JSON value `observer init` writes under
// ~/.cursor/mcp.json's mcpServers."observer".
//
// The deeplink surface has a documented RCE-class abuse history
// (CursorJack), so this is a STATIC, exact-config-only link: never compose
// a deeplink from user-supplied or runtime-derived input.
func cursorDeeplink(entry mcpServer) (string, error) {
	cfg, err := json.Marshal(entry)
	if err != nil {
		return "", fmt.Errorf("plugingen: marshal cursor config: %w", err)
	}
	// Query parameters are written in the documented order (name, then
	// config) rather than url.Values.Encode's alphabetical order, so the
	// emitted link matches the shape Cursor documents. Each value is still
	// percent-escaped: the standard base64 alphabet includes '+', '/' and
	// '=', all of which must not reach a query parser raw.
	return "cursor://anysphere.cursor-deeplink/mcp/install" +
		"?name=" + url.QueryEscape(mcp.ServerName) +
		"&config=" + url.QueryEscape(base64.StdEncoding.EncodeToString(cfg)), nil
}

// marshalJSON renders v with a 2-space indent and exactly one trailing
// newline. HTML escaping is off so a URL's `&` survives verbatim.
func marshalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("plugingen: marshal: %w", err)
	}
	// Encode already appends a newline; guarantee exactly one.
	return append(bytes.TrimRight(buf.Bytes(), "\n"), '\n'), nil
}

// readVersion reads the "version" field of a package.json. npm/observer is
// the canonical current release version and is stamped by
// scripts/sync-npm-version.sh, so the plugin manifests inherit the same
// number instead of carrying a second hand-maintained one.
func readVersion(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("plugingen: read version file: %w", err)
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return "", fmt.Errorf("plugingen: parse %s: %w", path, err)
	}
	if pkg.Version == "" {
		return "", fmt.Errorf("plugingen: %s has no version field", path)
	}
	return pkg.Version, nil
}

// -------------------------------------------------------------------
// Generated prose
// -------------------------------------------------------------------

const generatedBanner = "<!-- GENERATED by plugins/plugingen — do not edit by hand.\n" +
	"     Regenerate with `make plugins-build`; CI gate: `make verify-plugins-build`. -->\n"

// hookEventList renders the registered Claude Code hook events, sorted, for
// the generated prose. Derived from the same map the manifest carries.
func hookEventList(hooks map[string]json.RawMessage) []string {
	events := make([]string, 0, len(hooks))
	for e := range hooks {
		events = append(events, e)
	}
	sort.Strings(events)
	return events
}

func commandLine(entry mcpServer) string {
	return strings.TrimSpace(entry.Command + " " + strings.Join(entry.Args, " "))
}

// The release version is deliberately absent from the generated PROSE and
// lives ONLY in the generated JSON manifests. scripts/sync-npm-version.sh
// runs in a Node-only release job with no Go toolchain, so it stamps those
// manifests directly; a version string in prose would need a second, prose-
// aware stamper and would drift the moment one of them missed a spot.
//
// stampedGenerated is versionBearingPaths' output for this run — the
// "Version stamping" section is written FROM it, counts included, so the
// section cannot drift from the tree the way a hand-maintained list did.
func topReadme(w wiring, stdio mcpServer, stampedGenerated []string) string {
	var b strings.Builder
	b.WriteString(generatedBanner)
	b.WriteString(`
# plugins/ — SuperBased Observer as an in-tool plugin

Observer normally wires itself into an AI tool with ` + "`observer init`" + `, which
edits that tool's own config files. These directories carry the same wiring
expressed in each tool's **native packaging format**, so a user can install it
from inside the tool instead.

Plan of record: ` + "`docs/plans/adapter-plugins-distribution-plan-2026-07-31.md`" + `
(Phase 1 = Claude Code plugin + Cursor deeplink; Phase 2 = Gemini CLI
extension + Goose listing; Phase 3 = Codex plugin + OpenCode npm plugin,
with Grok Build verified and deliberately shipping no artifact; §7's
coverage **wave A** = Kimi Code, Qoder, Devin, Droid, OpenClaw and
Antigravity, each built against that vendor's own documented manifest
schema; **wave B** = the config listings — Crush, Kiro CLI, Copilot CLI,
Kilo Code, Roo Code and Open Interpreter — for tools whose only
first-party extension surface is their own config file; **wave C** = the
special formats — Copilot's one-click ` + "`vscode:mcp/install`" + ` URI and
the Claude Desktop ` + "`.mcpb`" + ` bundle — plus the two surfaces where
the honest artifact was a documented decision NOT to ship a package, Pi
and Command Code).

## Honesty note — this is a wiring layer, not a binary channel

Every artifact here declares an MCP server and hooks that invoke the
` + "`observer`" + ` binary **already on the user's PATH**. Nothing here downloads,
bundles or installs that binary. Every listing in this tree says so in those
words: **` + binaryPrereqSentence + `.** The binary still arrives through npm
(` + "`npm i -g @superbased/observer`" + `), PyPI (` + "`pipx install superbased-observer`" + `),
or the VS Code extension. Every published description states this.

## Layout

| Path | What it is |
|---|---|
| ` + "`claude-code/`" + ` | The Claude Code **marketplace root** — holds ` + "`.claude-plugin/marketplace.json`" + ` (a single-plugin catalog) and the plugin directory it points at. Its CONTENTS have to land at a repo root — see "Remaining steps". |
| ` + "`claude-code/" + pluginDir + "/`" + ` | The plugin itself: ` + "`.claude-plugin/plugin.json`" + `, ` + "`.mcp.json`" + ` (MCP server), ` + "`hooks/hooks.json`" + ` (lifecycle hooks). |
| ` + "`cursor/`" + ` | The static one-click Cursor MCP install deeplink (` + "`deeplink.txt`" + `) plus its README. |
| ` + "`gemini/`" + ` | The Gemini CLI extension: ` + "`gemini-extension.json`" + ` (MCP server) plus its README. Installed with ` + "`gemini extensions install <repo-url>`" + `. |
| ` + "`goose/`" + ` | A README only. Goose extensions ARE MCP servers, so the deliverable is the exact ` + "`config.yaml`" + ` block + the ` + "`goose configure`" + ` walkthrough — the block itself is generated. |
| ` + "`codex/`" + ` | The Codex **marketplace root**: ` + "`.agents/plugins/marketplace.json`" + ` plus ` + "`" + codexPluginPath + "/`" + ` (` + "`.codex-plugin/plugin.json`" + ` + ` + "`.mcp.json`" + `). |
| ` + "`opencode/`" + ` | The ` + "`" + openCodePackageName + "`" + ` npm package. The only surface with hand-written glue: ` + "`package.json`" + `, ` + "`tsconfig.json`" + ` and ` + "`src/index.ts`" + ` are hand-written; ` + "`src/wiring.generated.ts`" + ` and ` + "`README.md`" + ` are generated. |
| ` + "`" + kimiSurfaceDir + "/`" + ` | Kimi Code plugin: ` + "`" + pluginDir + "/kimi.plugin.json`" + ` (manifest-embedded MCP server). Installed with ` + "`/plugins install <repo-url>`" + `, so the manifest belongs at a repo root. |
| ` + "`" + qoderSurfaceDir + "/`" + ` | Qoder CLI plugin: ` + "`" + pluginDir + "/.qoder-plugin/plugin.json`" + ` + ` + "`" + pluginDir + "/.mcp.json`" + ` (MCP is a FILE here, not a manifest field). |
| ` + "`" + devinSurfaceDir + "/`" + ` | Devin CLI plugin: ` + "`" + pluginDir + "/.devin-plugin/plugin.json`" + ` + ` + "`" + pluginDir + "/mcp_config.json`" + `. Git-URL install only — Devin documents no CLI plugin catalog. |
| ` + "`" + droidSurfaceDir + "/`" + ` | Droid **marketplace root**: ` + "`.factory-plugin/marketplace.json`" + ` + ` + "`" + droidPluginDir + "/`" + ` (` + "`.factory-plugin/plugin.json`" + ` + ` + "`mcp.json`" + ` — undotted, Factory's native spelling). The one wave-A surface with a real observer registrar behind it. |
| ` + "`" + openClawSurfaceDir + "/`" + ` | OpenClaw native plugin: ` + "`" + pluginDir + "/openclaw.plugin.json`" + ` (its own manifest schema — ` + "`id`" + ` + ` + "`configSchema`" + ` required, ` + "`mcpServers`" + ` entries carry a ` + "`transport`" + ` key). |
| ` + "`" + antigravitySurfaceDir + "/`" + ` | Antigravity plugin: ` + "`" + pluginDir + "/plugin.json`" + ` + ` + "`" + pluginDir + "/mcp_config.json`" + `. A plain directory the user copies or ` + "`agy plugin install`" + `s — deliberately NOT at ` + "`.agents/plugins/`" + `; see that README. |
| ` + "`" + crushSurfaceDir + "/`" + ` | Crush config listing: the ` + "`crush.json`" + ` ` + "`mcp`" + ` block. README only — Crush has no package format. |
| ` + "`" + kiroSurfaceDir + "/`" + ` | Kiro CLI config listing: ` + "`~/.kiro/settings/mcp.json`" + `. README only — "Powers" is IDE-only and publishes no schema. |
| ` + "`" + copilotCLISurfaceDir + "/`" + ` | GitHub Copilot CLI config listing: ` + "`~/.copilot/mcp-config.json`" + `, top-level key ` + "`" + copilotCLIServersKey + "`" + ` — **not** VS Code's ` + "`" + copilotCLIRejectedKey + "`" + `. |
| ` + "`" + kiloCodeSurfaceDir + "/`" + ` | Kilo Code config listing: ` + "`kilo.jsonc`" + `'s ` + "`mcp`" + ` key. The entry is OpenCode-shaped, so it comes from observer's OpenCode registrar. |
| ` + "`" + rooSurfaceDir + "/`" + ` | Roo Code config listing: ` + "`.roo/mcp.json`" + ` (Cline-shaped). Its marketplace takes GitHub-issue submissions and publishes no schema. |
| ` + "`" + openInterpreterSurfaceDir + "/`" + ` | Open Interpreter config listing: the ` + "`[mcp_servers." + mcp.ServerName + "]`" + ` table for ` + "`~/.openinterpreter/config.toml`" + ` — the Codex registrar's own TOML text, at a different home. |
| ` + "`" + copilotSurfaceDir + "/`" + ` | GitHub Copilot in VS Code: the first-party one-click ` + "`vscode:mcp/install?<json>`" + ` link (` + "`install-uri.txt`" + `) plus a badge snippet and the ` + "`.vscode/mcp.json`" + ` block. |
| ` + "`" + coworkSurfaceDir + "/`" + ` | Claude Desktop MCP Bundle: ` + "`" + pluginDir + "/manifest.json`" + `. **UNVERIFIED** — no ` + "`.mcpb`" + ` has been packed or installed, and Cowork's support for bundles is inferred, not documented. |
| ` + "`" + piSurfaceDir + "/`" + ` | Pi: a README explaining why **no plugin is needed** (no MCP client by design, no hook receiver to call, capture already complete). |
| ` + "`" + commandCodeSurfaceDir + "/`" + ` | Command Code: the registrar-written ` + "`~/.commandcode/mcp.json`" + ` entry, plus why no "Mod" package ships. |
| ` + "`plugingen/`" + ` | The generator. The ONLY hand-written Go in this tree. |

Everything is generated except ` + "`plugingen/`" + ` and the three OpenCode
package files named above.

## Where each surface lands in the PUBLIC repository

` + "`plugins/`" + ` is one directory per surface, which is right in this repo and
wrong publicly: several formats resolve their paths from a REPOSITORY ROOT.
` + "`scripts/assemble-plugins-repo.sh`" + ` performs that transpose. The rule is
each vendor's own documented INSTALL MECHANISM — a surface goes to the root
if and only if some documented install resolves it FROM a repository root —
and the decision per surface is:

| Public location | Surfaces | Why |
|---|---|---|
| Repository ROOT | ` + "`.claude-plugin/marketplace.json`" + ` + ` + "`" + pluginDir + "/`" + `, ` + "`.agents/plugins/marketplace.json`" + ` + ` + "`" + codexPluginPath + "/`" + `, ` + "`gemini-extension.json`" + `, ` + "`kimi.plugin.json`" + `, ` + "`openclaw.plugin.json`" + `, ` + "`.devin-plugin/plugin.json`" + ` + ` + "`mcp_config.json`" + `, ` + "`.factory-plugin/marketplace.json`" + ` + ` + "`" + droidPluginDir + "/`" + ` | A catalog whose ` + "`./`" + `-relative source resolves against the root (Claude Code, Codex, Droid), or an install that takes a repository URL / ` + "`owner/repo`" + ` and therefore makes the repository root the plugin root (Gemini CLI, Kimi Code, OpenClaw, Devin). |
| Subdirectory | ` + "`" + qoderSurfaceDir + "/`" + `, ` + "`" + antigravitySurfaceDir + "/`" + `, ` + "`" + coworkSurfaceDir + "/`" + `, ` + "`" + copilotSurfaceDir + "/`" + `, and every wave-B listing plus ` + "`" + piSurfaceDir + "/`" + ` and ` + "`" + commandCodeSurfaceDir + "/`" + ` | Nothing about them resolves from a root: Qoder installs from a local path, Antigravity stages a local directory into the user's own home, the Claude Desktop bundle is packed rather than read in place, the Copilot artifact is a URI, and the listings are pages to read. |

Two of those decisions are load-bearing rather than cosmetic:

- **Antigravity stays OUT of ` + "`.agents/plugins/`" + `** — that directory is
  Codex's catalog location, and what Antigravity does with a foreign file
  sitting in a plugins directory is undocumented. Its README explains it.
- **Droid's plugin is nested at ` + "`" + droidPluginDir + "/`" + `**, not at
  ` + "`" + pluginDir + "/`" + `. A bare source would put it in the SAME root
  directory as the Claude Code plugin — which carries a
  ` + "`hooks/hooks.json`" + ` full of ` + "`observer hook claude-code`" + `
  commands, and Droid reads plugin hooks from exactly that path. Droid's
  catalog is nonetheless at the root, because Factory documents
  ` + "`.claude-plugin/marketplace.json`" + ` as a FALLBACK: without our own
  catalog there, ` + "`droid plugin marketplace add`" + ` would answer with the
  Claude Code plugin, hooks and all.

**Grok Build ships no artifact, on purpose.** ` + "`docs.x.ai`" + ` states that Grok
"automatically reads Claude Code marketplaces, plugins, skills, MCPs, agents,
hooks, and instruction files" with "zero configuration needed", and its own
` + "`plugin.json`" + ` schema is not publicly documented. So ` + "`claude-code/`" + ` IS the
Grok surface, and inventing a second manifest from an undocumented schema
would be the exact guess this generator exists to prevent. See the plan doc's
Phase 2/3 section for the verification record.

## Generation

` + "```bash" + `
make plugins-build          # regenerate plugins/ in place
make verify-plugins-build   # CI drift gate: rebuild into a temp dir, diff
` + "```" + `

` + "`make verify-plugins-build`" + ` runs in CI as the ` + "`plugins-build-drift`" + ` job.
It never mutates the working tree. It has two halves:

1. **Generated files** — regenerate into a scratch copy and byte-diff.
2. **The hand-written OpenCode TypeScript** — parse ` + "`opencode/src/*.ts`" + ` with
   the esbuild vendored in ` + "`web/node_modules`" + ` (the
   ` + "`verify-taxonomy-ts`" + ` lever) and assert the transpiled entry still
   imports the generated wiring and still exports something. This is a
   **parse gate, not a type check**: type-checking against
   ` + "`@opencode-ai/plugin`" + ` needs declarations that are not vendored here,
   so a type error is caught by ` + "`npm run build`" + ` in ` + "`opencode/`" + ` at
   publish time instead. Without this half, a syntax error in glue the Go
   tests can only substring-check would reach an operator's ` + "`npm publish`" + `.

## Why generated (the §3 one-owner rule)

` + "`observer init`" + `'s registrars are the source of truth for what observer
declares into a tool:

- ` + "`internal/mcp`" + `'s ` + "`Registrar`" + ` owns the MCP server entry;
- ` + "`internal/hook`" + `'s ` + "`Registry`" + ` owns the Claude Code hook commands and
  the event roster.

` + "`plugingen`" + ` does not re-describe that wiring. It **runs the real registrars**
against a throwaway sandbox ` + "`HOME`" + `, reads back the files they wrote, and
transposes those exact entries into the plugin formats. A new hook event or a
changed MCP argument reaches the manifests the moment the registrar changes,
and the CI gate fails until the regenerated manifests are committed. A
hand-authored manifest is how plugin wiring forks from init wiring silently.

The one deliberate deviation: ` + "`observer init`" + ` registers the **absolute path**
of the running binary, which a cache-copied plugin cannot know. The generator
therefore drives the registrars with ` + "`BinaryPath = \"observer\"`" + `, resolved from
PATH. Everything else — argument vector, event roster, event-argument
spelling, command shape — is whatever the registrar produced.

### Surfaces without a registrar

Gemini CLI and Goose have **no MCP registrar** in ` + "`internal/mcp`" + ` —
` + "`locate.Locations`" + ` carries no row for either, and
` + "`internal/integration`" + ` records ` + "`MCP: nil`" + ` for both. Neither surface
can therefore transpose "the entry the registrar wrote for this tool"; there
isn't one. What they carry instead is the entry **every** registrar agrees on:
claude-code, cursor and codex write the same command and argument vector in
three different file formats, and opencode writes the same words as one argv.
` + "`canonicalStdio`" + ` asserts that agreement and refuses to emit if a future
registrar starts writing a per-tool argument — so the Gemini and Goose
snippets can never quietly become a fourth, hand-maintained spelling.

The same applies to every coverage-wave surface without a registrar, which is
most of them. Three of the listings do better than canonicalStdio because
their tool's config format IS another tool's: Kilo Code reads OpenCode's
` + "`mcp`" + ` entry shape, so its block is the OpenCode registrar's own entry;
Open Interpreter reads Codex's ` + "`[mcp_servers.<name>]`" + ` TOML from its own
home, so its block is the Codex registrar's own TABLE TEXT, lifted verbatim;
and Command Code has a registrar of its very own
(` + "`internal/mcp/locate`" + ` carries the row), so its listing is simply what
` + "`observer init`" + ` writes.

## Current wiring snapshot

- MCP server id: ` + "`" + mcp.ServerName + "`" + `
- MCP launch (canonical, all registrars agree): ` + "`" + commandLine(stdio) + "`" + `
- Claude Code hook events (` + fmt.Sprintf("%d", len(w.claudeCodeHooks)) + `): ` + strings.Join(hookEventList(w.claudeCodeHooks), ", ") + `
- Codex MCP entry (from ` + "`~/.codex/config.toml`" + `'s ` + "`[mcp_servers." + mcp.ServerName + "]`" + `): ` + "`" + commandLine(w.codexMCP) + "`" + `
- Droid MCP entry (from ` + "`~/.factory/mcp.json`" + `): ` + "`" + commandLine(w.droidMCP) + "`" + `
- Command Code MCP entry (from ` + "`~/.commandcode/mcp.json`" + `): ` + "`" + commandLine(w.commandCodeMCP) + "`" + `
- OpenCode MCP entry (from ` + "`opencode.json`" + `'s ` + "`mcp`" + `): ` + "`" + strings.Join(w.openCodeMCP.Command, " ") + "`" + ` (` + "`type: " + w.openCodeMCP.Type + "`" + `)
- Gemini extension: ` + geminiSnapshotLine(stdio) + `

## Double-wiring, per surface

| Surface | Does ` + "`observer init`" + ` write the same thing? | Guard |
|---|---|---|
| Claude Code | Yes (hooks + MCP) | **Detected and skipped** by ` + "`internal/claudeplugin`" + `; ` + "`observer doctor claude-code`" + ` warns. |
| Cursor | Yes (MCP) | The deeplink IS init's entry, written to the same file under the same key — installing both is idempotent, not duplicative. |
| Gemini CLI | No — no Gemini registrar exists | Nothing to duplicate. |
| Goose | No — no Goose registrar exists | Nothing to duplicate. |
| Codex | **Yes (MCP)** | **None.** Use ` + "`observer init --codex --skip-mcp`" + ` when the plugin is installed. Documented in ` + "`codex/README.md`" + `. |
| OpenCode | **Yes (MCP)** | The plugin's ` + "`config`" + ` hook skips itself when the ` + "`" + mcp.ServerName + "`" + ` key is already present, so both together declare the server once. |
| Droid | **Yes (MCP)** — via auto-detection; there is no ` + "`--droid`" + ` flag | **None.** Use ` + "`observer init --skip-mcp`" + `, or drop the ` + "`" + mcp.ServerName + "`" + ` key from ` + "`~/.factory/mcp.json`" + `. Documented in ` + "`" + droidSurfaceDir + "/README.md`" + `. |
| Kimi Code | No — Kimi's only MCP surface is a never-read config file | Nothing to duplicate. |
| Qoder | No — no Qoder registrar exists | Nothing to duplicate. |
| Devin | No — no Devin registrar exists | Nothing to duplicate. |
| OpenClaw | No — no OpenClaw registrar exists | Nothing to duplicate; and OpenClaw's own ` + "`mcp.servers.<name>`" + ` replaces a plugin default rather than stacking with it. |
| Antigravity | No — no Antigravity registrar exists | Nothing to duplicate. |
| Crush / Kiro CLI / Roo Code | No — no registrar exists for any of them | Nothing to duplicate; each listing is a hand-edit the user makes. |
| Copilot CLI | No — no Copilot CLI registrar exists | Nothing to duplicate. Adding the entry at BOTH user and repo level does duplicate it. |
| Kilo Code | No — no Kilo registrar exists | Nothing observer wrote. It CAN duplicate the OpenCode npm plugin, which declares the same key; use one. |
| Open Interpreter | No — no Open Interpreter registrar exists | Nothing to duplicate. ` + "`observer init --codex`" + ` writes the same TABLE, but into ` + "`~/.codex/config.toml`" + ` — a different tool's file. |
| Copilot (VS Code) | No — no VS Code registrar exists | Nothing to duplicate. The link and a committed ` + "`.vscode/mcp.json`" + ` would duplicate each other. |
| Claude Desktop / Cowork | No — no Claude Desktop registrar exists | Nothing to duplicate. The bundle and the config-file route would duplicate each other. |
| Pi | No — and no artifact ships | Not possible: there is nothing to install. |
| Command Code | **Yes (MCP)** — via auto-detection (` + "`~/.commandcode/`" + `); there is no ` + "`--command-code`" + ` flag | **None.** The listing IS what init writes, so pasting it as well is the duplicate; ` + "`" + commandCodeSurfaceDir + "/README.md`" + ` says so. |

Detection like ` + "`internal/claudeplugin`" + `'s is **claude-code-only** and none was
built for the other tools this round. Where a duplicate is possible, the
surface's README says so plainly instead.

## Hooks: exactly one surface declares them

Only the Claude Code plugin carries hooks, because ` + "`observer hook <tool>`" + `
accepts **claude-code, cursor, codex and hermes** and nothing else. Several of
the coverage-wave formats document a hooks vocabulary of their own — Kimi
Code's manifest ` + "`hooks`" + ` array, Qoder's and Droid's ` + "`hooks/hooks.json`" + `,
Devin's ` + "`hooks.json`" + `, Antigravity's plugin-root ` + "`hooks.json`" + `,
Crush's and Kiro CLI's Claude-shaped lifecycle hooks, Command Code's ModApi
handlers, Pi's ` + "`pi.on(…)`" + ` events — and every one of them is
deliberately left unwritten. Declaring a hook we cannot serve would point the
tool at a command that does not exist. Those tools are captured by the watcher
instead, which needs no hook.

That same rule is why two of the wave-C surfaces ship **no package at all**:
Pi's only extension shape is a hook receiver observer cannot back (and Pi has
no MCP client by design), and a Command Code "Mod" would carry either the MCP
entry a real registrar already writes or hook handlers with nothing to call.
Their READMEs give the evidence instead of shipping a shell.

## Double-wiring: detected, skipped, and characterised

Claude Code merges hook configuration from every source it loads, and
namespaces a plugin's MCP server (` + "`plugin:<plugin>:<server>`" + `) separately from
a user-config one (` + "`<server>`" + `). So a user carrying BOTH the plugin and
` + "`observer init --claude-code`" + `'s writes fires every hook event twice and loads
the observer MCP tool schema twice per turn.

**` + "`observer init`" + ` now detects the plugin and skips the steps it covers.** The
hook step and the MCP step both consult ` + "`internal/claudeplugin`" + `, which looks
for the EXACT ` + "`enabledPlugins`" + ` key ` + "`" + claudeplugin.EnabledKey + "`" + ` in
` + "`~/.claude/settings.json`" + `, or a VERIFIED plugin cache directory at
` + "`~/.claude/plugins/cache/" + marketplaceName + "/" + pluginName + "/<version>/`" + `
whose own ` + "`.claude-plugin/plugin.json`" + ` names this plugin. On a hit, both
steps write nothing and say why. The proxy-route step still runs — the plugin
declares no proxy route, so it is not a duplicate. ` + "`--force`" + ` overrides.
` + "`observer doctor claude-code`" + ` warns when a pre-existing install already has
both, on the native side AND on the Windows side of a WSL install.

Detection is deliberately narrow, and errs toward wiring:

- a same-named plugin from a DIFFERENT marketplace
  (` + "`" + pluginName + "@acme-internal`" + `) is not ours and does not suppress
  anything — the cost of being wrong there is a user with no capture at all;
- a bare or orphaned cache directory is not an install (Claude Code keeps
  orphaned version directories for ~14 days after an update or uninstall);
- if ` + "`settings.json`" + ` cannot be read or parsed, "not enabled" is unprovable,
  so init REGISTERS and reports the failure rather than silently skipping.

**What a double fire actually costs, measured.** Every claude-code hook
builder derives a deterministic ` + "`SourceEventID`" + ` from the payload and writes
under the constant ` + "`SourceFile`" + ` ` + "`\"claude-code:hook\"`" + `, and the ` + "`actions`" + `
table is ` + "`UNIQUE(source_file, source_event_id)`" + ` — so the second fire takes the
store's ` + "`ON CONFLICT DO UPDATE`" + ` path. **No duplicate action rows.** Pinned by
` + "`cmd/observer`" + `'s ` + "`TestClaudeCodeHookDoubleFireIsIdempotent`" + `, which ingests all
17 builder-backed event shapes twice and asserts the row count is unchanged.
The ` + "`claudecode_effort`" + ` sidecar upserts on ` + "`(session_id, tool_use_id)`" + `, and
` + "`session_pid_bridge`" + ` is keyed by ` + "`pid`" + `, so both are idempotent too.

What does double: one extra hook process spawn and DB open per event; the
MCP tool schema in every turn's context; ` + "`~/.observer/hook-events.jsonl`" + ` lines;
rows in the append-only ` + "`guard_events`" + ` ledger when ` + "`[guard]`" + ` is enabled
(off by default, record-worthy verdicts only); and **one duplicate
` + "`compaction_events`" + ` row per ` + "`/compact`" + `**.

**The ` + "`compaction_events`" + ` duplicate is documented residue, not a fixed bug.**
` + "`PreCompact`" + ` takes its own handler path and the table has no uniqueness
constraint. Verified against the shipped Claude Code v2.1.220 binary: the
PreCompact payload is
` + "`{session_id, transcript_path, cwd, prompt_id?, agent_type?, hook_event_name, trigger, custom_instructions}`" + `
and no compaction/occurrence identifier exists anywhere in that binary.
` + "`prompt_id`" + ` is prompt-grain by its own definition, optional, and absent until
the first user input. Claude Code can legitimately fire ` + "`PreCompact`" + ` more than
once inside one prompt (a speculative "precomputed" compaction with its own
retry counter, then a reactive one), and those fires carry BYTE-IDENTICAL
payloads — so any key we could synthesise would silently collapse two REAL
compactions. Eating a genuine event is worse than keeping a duplicate, so the
duplicate stays and is disclosed here. Pinned by
` + "`TestClaudeCodePreCompactDoubleFireDuplicates`" + `, which drives the real hook
dispatcher.

No ingest-side dedupe was added anywhere: the store's existing key already
does the job for the rows that matter, ` + "`guard_events`" + ` is a tamper-evident
chain where two evaluations genuinely happened, and ` + "`compaction_events`" + ` has no
trustworthy key to dedupe on.

## Remaining steps (OPERATOR-GATED, not done here)

Nothing in this tree is published or submitted anywhere. To ship publicly:

0. **The assembler carries every surface.**
   ` + "`scripts/assemble-plugins-repo.sh`" + ` enumerates the exact source paths
   it copies, and that list now names all three waves as well as the
   Phase 1/2/3 surfaces. Its ` + "`--self-check`" + ` asserts the enumeration
   is COMPLETE — every file under ` + "`plugins/`" + ` is either copied or
   named on the script's intentionally-unshipped list, so a new surface
   cannot be added here and silently left out of the public tree. Root
   placement is decided per vendor by the vendor's own documented install
   mechanism (` + "`" + kimiSurfaceDir + "/`" + `,
   ` + "`" + openClawSurfaceDir + "/`" + `, ` + "`" + devinSurfaceDir + "/`" + ` and
   ` + "`" + droidSurfaceDir + "/`" + `'s catalog go to the root; Antigravity
   deliberately stays OUT of the root's ` + "`.agents/plugins/`" + `, see
   ` + "`" + antigravitySurfaceDir + "/README.md`" + `; ` + "`" + qoderSurfaceDir + "/`" + `
   keeps its plugin in a subdirectory but ships its CATALOG at the root,
   because Qoder falls back to ` + "`.claude-plugin/marketplace.json`" + `
   otherwise).

1. **The public repo tree is already assembled for you.** Run
   ` + "```bash" + `
   scripts/assemble-plugins-repo.sh <output-dir> <version>   # version REQUIRED: the release tag you are publishing
   scripts/assemble-plugins-repo.sh --self-check             # CI gate; assembles twice at a fixture version
   ` + "```" + `
   The version has **no default and is never derived**. A default goes stale
   the moment the next tag ships, and deriving it from
   ` + "`npm/observer/package.json`" + ` would be worse — that file carries the
   UNRELEASED next version, so a derived run would publish manifests pinned
   to a binary that does not exist yet. ` + "`--self-check`" + ` publishes nothing
   and says so, stamping the explicitly-labelled fixture
   ` + "`0.0.0-selfcheck`" + `.
   It performs the directory-per-surface → repository-root transpose that
   every one of these formats requires, because each resolves relative paths
   from a **repo root**:
   - Claude Code wants ` + "`.claude-plugin/marketplace.json`" + ` at the root
     (so ` + "`./" + pluginDir + "`" + ` resolves) → ` + "`" + pluginDir + "/`" + `.
   - Codex wants ` + "`.agents/plugins/marketplace.json`" + ` at the root (so
     ` + "`./" + codexPluginPath + "`" + ` resolves) → ` + "`" + codexPluginPath + "/`" + `.
   - Gemini wants ` + "`gemini-extension.json`" + ` at the root; installing from a
     repo SUBDIRECTORY is not documented. Root placement also serves
     qwen-code, whose docs say ` + "`qwen extensions install`" + ` converts a
     ` + "`gemini-extension.json`" + `.

   Those three root-level names do not collide, so **ONE repo carries all
   three** — that decision is now made, and it is why the in-tree layout
   keeps them in separate directories with disjoint file names. The script
   never mutates the working tree, restamps the assembled manifests to the
   release version, and re-runs the honesty gate on what it produced;
   ` + "`--self-check`" + ` proves two runs are byte-identical (CI runs it).

   What is still operator-gated: creating ` + "`superbasedapp/plugins`" + ` and
   pushing that tree into it.
2. Validation status — run against an assembled tree, not a live install:
   ` + "`claude plugin validate . --strict`" + ` and
   ` + "`claude plugin validate " + pluginDir + " --strict`" + ` both PASS, and Codex's
   ` + "`validate_plugin.py`" + ` (bundled with the ` + "`plugin-creator`" + ` skill in
   ` + "`openai/codex`" + `) PASSES against the Codex plugin. **No install has
   been performed by anyone yet** — a passing manifest validator is not a
   live install.
3. Publish the install snippets on the website + READMEs:
   ` + "`/plugin marketplace add superbasedapp/plugins`" + ` +
   ` + "`/plugin install " + pluginName + "@" + marketplaceName + "`" + ` (Claude Code),
   ` + "`codex plugin marketplace add superbasedapp/plugins`" + ` (Codex),
   ` + "`gemini extensions install <repo-url>`" + ` (Gemini), the ` + "`config.yaml`" + `
   block in ` + "`goose/README.md`" + ` (Goose), and the Cursor deeplink from
   ` + "`cursor/deeplink.txt`" + ` (see ` + "`cursor/README.md`" + ` for the security
   constraint).
4. Publish ` + "`" + openCodePackageName + "`" + ` to npm (` + "`npm run build`" + ` first;
   ` + "`dist/`" + ` is not committed) and list the package name in the docs.
5. Gallery/directory submissions — the Gemini Extensions Gallery, the Goose
   extension directory, the Codex plugin directory — are separate
   operator-gated public actions. Codex self-serve publishing to the official
   directory was still unavailable as of the last check, which is why
   ` + "`codex/`" + ` is a self-hosted marketplace.
6. Wire the release pipeline to refresh the public plugins repo, the way
   ` + "`scripts/release.sh`" + ` refreshes the public observer repo.

## Version stamping

⚠️ **This section is generated FROM the tree it describes.** The file list and
the counts below are derived by walking every generated JSON artifact for a
` + "`version`" + ` field, not typed by hand — an earlier hand-maintained version
of this list drifted (it claimed nine files and a four-file stamper) and that
is exactly what the derivation prevents.

The release version appears in ` + versionCountWords(len(stampedGenerated)) + ` generated files —

` + versionBearingBullets(stampedGenerated) + `
— plus the hand-written ` + versionBearingHandWrittenPhrase() + `, and nowhere in the
prose. That is ` + fmt.Sprintf("%d", len(stampedGenerated)+len(handWrittenVersionBearingPaths())) + ` files under ` + "`plugins/`" + ` in total.

` + "`" + antigravitySurfaceDir + "/`" + ` carries none: Antigravity's ` + "`plugin.json`" + `
documents exactly one field (` + "`name`" + `), so there is no version key to stamp.
The Codex, Droid and Qoder marketplace ENTRIES carry no ` + "`version`" + ` either
— each of those catalog schemas resolves the pin from the plugin's own
manifest — and ` + "`goose/README.md`" + ` carries none because a Goose extension is
just a command.

` + "`scripts/sync-npm-version.sh <tag>`" + ` stamps **exactly this set**, alongside
the 6 npm manifests, ` + "`vscode/package.json`" + ` and
` + "`browser-extension/manifest.json`" + `. That is not a claim you have to take
on trust: ` + "`plugingen`" + `'s
` + "`TestSyncNpmVersionStampsEveryVersionBearingManifest`" + ` reads the script,
extracts the ` + "`plugins/…`" + ` paths it stamps, and fails on set inequality
in either direction — a manifest the script forgot, or a path it stamps that
the generator no longer emits.

It stamps them directly with Node rather than re-running ` + "`plugingen`" + `, because
the release job that calls it has Node but no Go toolchain; the result is
byte-identical to a regenerated tree because ` + "`plugingen`" + ` sources the same
number from ` + "`npm/observer/package.json`" + `, the canonical current release
version, so there is no second number to maintain. Unlike the MV3 manifest,
every plugin manifest takes the FULL semver
string: Claude Code treats ` + "`version`" + ` as an opaque pin (not a numeric
tuple), Codex's plugin spec requires strict semver — which a ` + "`-rc.1`" + `
pre-release satisfies — and Gemini only compares it to detect a newer
release. So ` + "`1.29.0-rc.1`" + ` stamps as-is everywhere here.

Two later gates re-check the same thing on the way out:
` + "`make verify-plugins-build`" + ` fails if the committed tree drifts from a
fresh generator run, and ` + "`scripts/assemble-plugins-repo.sh`" + `'s post-stamp
guard enumerates every version-bearing field in the ASSEMBLED tree and
requires each to equal the release tag.

Regenerate and commit ` + "`plugins/`" + ` (` + "`make plugins-build`" + `) as part of the
release stamp, the same way ` + "`vscode/package.json`" + ` is checked pre-flight.
`)
	return b.String()
}

// versionCountWords spells a small count so the prose reads naturally
// while still being derived. Anything past the spelled range falls back to
// digits rather than inventing a word.
func versionCountWords(n int) string {
	words := []string{
		"zero", "exactly one", "exactly two", "exactly three", "exactly four",
		"exactly five", "exactly six", "exactly seven", "exactly eight",
		"exactly nine", "exactly ten", "exactly eleven", "exactly twelve",
	}
	if n >= 0 && n < len(words) {
		return words[n]
	}
	return fmt.Sprintf("exactly %d", n)
}

// versionBearingBullets renders the derived path list as a markdown bullet
// list, annotating the one entry whose version lives on a catalog ENTRY
// rather than at the top level.
func versionBearingBullets(paths []string) string {
	var b strings.Builder
	for _, p := range paths {
		b.WriteString("- `" + p + "`")
		if strings.HasSuffix(p, "marketplace.json") {
			b.WriteString(" (the catalog entry's `version`)")
		}
		b.WriteString(",\n")
	}
	return b.String()
}

// versionBearingHandWrittenPhrase renders handWrittenVersionBearingPaths
// as an inline prose phrase.
func versionBearingHandWrittenPhrase() string {
	paths := handWrittenVersionBearingPaths()
	quoted := make([]string, 0, len(paths))
	for _, p := range paths {
		quoted = append(quoted, "`"+p+"`")
	}
	return strings.Join(quoted, ", ")
}

func pluginReadme(w wiring) string {
	var b strings.Builder
	b.WriteString(generatedBanner)
	b.WriteString(`
# SuperBased Observer — Claude Code plugin

Local-first token, cost and cache observability for Claude Code.

**` + binaryPrereqSentence + `.** This plugin is
wiring only — it declares an MCP server and lifecycle hooks that call
` + "`observer`" + `; it does not download or bundle it.

` + "```bash" + `
npm i -g @superbased/observer     # or: pipx install superbased-observer
observer start                    # run the local daemon
` + "```" + `

## Install

` + "```" + `
/plugin marketplace add superbasedapp/plugins
/plugin install ` + pluginName + `@` + marketplaceName + `
` + "```" + `

## What it wires

| Component | What it declares |
|---|---|
| ` + "`.mcp.json`" + ` | The ` + "`" + mcp.ServerName + "`" + ` MCP server: ` + "`" + commandLine(w.claudeCodeMCP) + "`" + ` — on-demand project/session/cost queries from inside Claude Code. |
| ` + "`hooks/hooks.json`" + ` | ` + fmt.Sprintf("%d", len(w.claudeCodeHooks)) + ` lifecycle hooks: ` + strings.Join(hookEventList(w.claudeCodeHooks), ", ") + `. Each runs ` + "`observer hook claude-code <event>`" + ` and records the event locally. |

Captured events are written to the local ` + "`~/.observer/observer.db`" + `; the hook
commands themselves make no network calls. (Shipping anything off the machine
is a separate, opt-in Teams/org configuration this plugin does not touch.)

## Don't double-wire

` + "`observer init --claude-code`" + ` writes the same hooks into
` + "`~/.claude/settings.json`" + ` and the same MCP server into ` + "`~/.claude.json`" + `.
Claude Code loads both sources, so carrying both fires every hook event twice
and loads observer's MCP tool schema twice per turn.

**With this plugin installed, ` + "`observer init`" + ` skips those two steps by
itself** and tells you it did. Its proxy-route step still runs, which is what
you want — the plugin does not declare a proxy route, and routing Claude Code
through the local proxy is what makes token counts exact:

` + "```bash" + `
observer init --claude-code    # writes the proxy route; skips hooks + MCP
observer doctor claude-code    # warns if both wirings are somehow present
` + "```" + `

If you installed the plugin on an older observer that already wrote the hooks,
run ` + "`observer uninstall --claude-code`" + ` to remove init's copy, then
` + "`observer init --claude-code`" + ` to put the proxy route back.

Duplicate fires do NOT duplicate your captured history — the hook rows carry a
deterministic event id and the store upserts on it. They do cost an extra
process spawn per event, a doubled tool schema per turn, and one extra
` + "`compaction_events`" + ` row per ` + "`/compact`" + ` (Claude Code's PreCompact payload
carries no identifier for a single compaction, so that one cannot be
deduplicated without risking the loss of a real compaction).

## Version

The plugin version is stamped from the observer release tag and kept in
lockstep with the binary — see ` + "`.claude-plugin/plugin.json`" + `.

## Links

- Docs: ` + homepage + `
- Source: https://github.com/superbasedapp/observer
- License: ` + license + `
`)
	return b.String()
}

func cursorReadme(deeplink string, entry mcpServer) string {
	cfg, _ := json.Marshal(entry)
	var b strings.Builder
	b.WriteString(generatedBanner)
	b.WriteString(`
# Cursor — one-click MCP install deeplink

Cursor installs an MCP server from a ` + "`cursor://`" + ` deeplink. Clicking the link
below opens Cursor and offers to add the ` + "`" + mcp.ServerName + "`" + ` MCP server — the
same entry ` + "`observer init --cursor`" + ` would write into ` + "`~/.cursor/mcp.json`" + `.

**` + binaryPrereqSentence + `** (` + "`npm i -g @superbased/observer`" + `
or ` + "`pipx install superbased-observer`" + `). The deeplink is wiring only.

## The link

` + "```" + `
` + deeplink + `
` + "```" + `

Also in ` + "`deeplink.txt`" + ` (single line, newline-terminated) for scripting.

Markdown form for a README or a web page:

` + "```markdown" + `
[![Add to Cursor](https://cursor.com/deeplink/mcp-install-dark.svg)](` + deeplink + `)
` + "```" + `

## What the link encodes

| Part | Value |
|---|---|
| scheme + handler | ` + "`cursor://anysphere.cursor-deeplink/mcp/install`" + ` |
| ` + "`name`" + ` | ` + "`" + mcp.ServerName + "`" + ` — the stable server id ` + "`observer init`" + ` uses. |
| ` + "`config`" + ` | base64 (standard alphabet, padded) of ` + "`" + string(cfg) + "`" + ` |

Decode it yourself to check:

` + "```bash" + `
printf '%s' "$(grep -o 'config=[^&]*' deeplink.txt | cut -d= -f2-)" | base64 -d
` + "```" + `

## Security constraint — static links only

Cursor deeplinks have a documented abuse history (the "CursorJack"
proof-of-concept: a crafted ` + "`cursor://`" + ` link plus one user approval can install
an arbitrary MCP server and run arbitrary commands). So this link is
**generated once, from the exact config our own registrar writes, and never
composed at runtime**. Do not build a deeplink from user input, a query
string, or any value not visible in ` + "`plugins/plugingen`" + `.

## The extension side needs no work

Cursor consumes the Open VSX registry, and the SuperBased Observer VS Code
extension is already published there — so the editor extension reaches Cursor
users through the existing release pipeline. This deeplink covers only the MCP
server wiring.
`)
	return b.String()
}
