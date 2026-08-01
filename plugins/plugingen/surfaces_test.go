package main

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/marmutapp/superbased-observer/internal/mcp"
)

// TestCanonicalStdioRefusesDisagreement is the guard behind the two
// registrar-less surfaces (Gemini, Goose). They can only carry an entry
// every registrar agrees on, so a divergence must be a hard error rather
// than a silent pick.
func TestCanonicalStdioRefusesDisagreement(t *testing.T) {
	w, err := deriveWiring()
	if err != nil {
		t.Fatalf("deriveWiring: %v", err)
	}
	if _, err := canonicalStdio(w); err != nil {
		t.Fatalf("live registrars disagree on the stdio launch: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*wiring)
	}{
		{"cursor args drift", func(x *wiring) { x.cursorMCP.Args = append([]string{"--cursor"}, x.cursorMCP.Args...) }},
		{"codex command drift", func(x *wiring) { x.codexMCP.Command = "observer2" }},
		{"codex env drift", func(x *wiring) { x.codexMCP.Env = map[string]string{"OBSERVER_X": "1"} }},
		{"opencode argv drift", func(x *wiring) { x.openCodeMCP.Command = []string{"observer", "serve", "--x"} }},
		// The two shape fields OpenCode carries that no other surface
		// does. Comparing only the flattened argv would let either of
		// these diverge silently.
		{"opencode environment drift", func(x *wiring) {
			x.openCodeMCP.Environment = map[string]string{"OBSERVER_CONFIG": "/etc/observer.toml"}
		}},
		{"opencode disabled", func(x *wiring) { x.openCodeMCP.Enabled = false }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mutated := w
			tc.mutate(&mutated)
			if _, err := canonicalStdio(mutated); err == nil {
				t.Error("want an error when the registrars disagree, got nil")
			}
		})
	}
}

// TestGeminiExtensionShape pins the grounded gemini-extension.json
// contract: the documented fields and nothing else, a kebab-case name that
// doubles as the install directory, the npm version, and the MCP entry the
// registrars agree on.
func TestGeminiExtensionShape(t *testing.T) {
	files := generateOnce(t)
	raw := fileByPath(t, files, "gemini/gemini-extension.json")

	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("gemini-extension.json is not valid JSON: %v", err)
	}
	// The extensions reference documents name, version, mcpServers,
	// contextFileName and excludeTools. We emit only the first three;
	// anything else would be a guess.
	for k := range keys {
		switch k {
		case "name", "version", "mcpServers":
		default:
			t.Errorf("undocumented/unexpected manifest key %q", k)
		}
	}

	var m geminiExtension
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m.Name != geminiExtensionName {
		t.Errorf("name = %q, want %q", m.Name, geminiExtensionName)
	}
	if strings.ToLower(m.Name) != m.Name || strings.ContainsAny(m.Name, " _") {
		t.Errorf("name %q must be lowercase with dashes", m.Name)
	}
	if want := committedVersion(t); m.Version != want {
		t.Errorf("version = %q, want %q", m.Version, want)
	}
	entry, ok := m.MCPServers[mcp.ServerName]
	if !ok {
		t.Fatalf("no %q server (keys: %v)", mcp.ServerName, m.MCPServers)
	}
	w, err := deriveWiring()
	if err != nil {
		t.Fatalf("deriveWiring: %v", err)
	}
	stdio, err := canonicalStdio(w)
	if err != nil {
		t.Fatalf("canonicalStdio: %v", err)
	}
	if !sameMCPServer(entry, stdio) {
		t.Errorf("gemini entry %s != canonical registrar launch %s", commandLine(entry), commandLine(stdio))
	}
}

// TestGooseReadmeCarriesRegistrarLaunch asserts the config block is
// generated from the live launch, and that no goose:// deeplink is
// published (Goose documents `cmd` as one of jbang/npx/uvx/goosed/docker —
// `observer` is not on that list).
func TestGooseReadmeCarriesRegistrarLaunch(t *testing.T) {
	files := generateOnce(t)
	body := string(fileByPath(t, files, "goose/README.md"))

	w, err := deriveWiring()
	if err != nil {
		t.Fatalf("deriveWiring: %v", err)
	}
	stdio, err := canonicalStdio(w)
	if err != nil {
		t.Fatalf("canonicalStdio: %v", err)
	}
	for _, needle := range []string{
		"type: stdio",
		"cmd: " + stdio.Command,
		"args: " + yamlFlowStrings(stdio.Args),
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("goose README is missing the generated config line %q", needle)
		}
	}
	if strings.Contains(body, "goose://extension?cmd=") {
		t.Error("goose README publishes a deeplink, but `observer` is not an allowed deeplink cmd")
	}
}

// TestCodexPluginShape pins the grounded Codex contract: manifest fields,
// the "./"-relative companion-file reference, and the catalog vocabulary
// taken from the first-party openai/plugins marketplace.
func TestCodexPluginShape(t *testing.T) {
	files := generateOnce(t)

	var m codexPluginManifest
	if err := json.Unmarshal(fileByPath(t, files, "codex/"+codexPluginPath+"/.codex-plugin/plugin.json"), &m); err != nil {
		t.Fatalf("plugin.json is not valid JSON: %v", err)
	}
	if m.Name != pluginName {
		t.Errorf("name = %q, want %q", m.Name, pluginName)
	}
	if want := committedVersion(t); m.Version != want {
		t.Errorf("version = %q, want %q", m.Version, want)
	}
	if m.Author.Name == "" || m.Description == "" {
		t.Error("the plugin validator requires real name/version/description/author.name")
	}
	if m.MCPServers != "./.mcp.json" {
		t.Errorf("mcpServers = %q, want the \"./\"-relative companion path", m.MCPServers)
	}
	if m.Interface.DisplayName == "" || m.Interface.Category == "" || len(m.Interface.Capabilities) == 0 {
		t.Errorf("interface is under-filled: %+v", m.Interface)
	}
	// interface.defaultPrompt is REQUIRED by the first-party
	// validate_plugin.py bundled with openai/codex's plugin-creator skill
	// ("field `interface.defaultPrompt` or `interface.default_prompt` is
	// required"), and plugin-json-spec.md caps it at 3 entries of at most
	// 128 characters — entries past the third are silently DROPPED, so
	// emitting four would publish a prompt no user ever sees.
	if len(m.Interface.DefaultPrompt) == 0 {
		t.Error("interface.defaultPrompt is required by Codex's plugin validator")
	}
	if len(m.Interface.DefaultPrompt) > 3 {
		t.Errorf("interface.defaultPrompt has %d entries; entries after the first 3 are ignored",
			len(m.Interface.DefaultPrompt))
	}
	for i, p := range m.Interface.DefaultPrompt {
		if strings.TrimSpace(p) == "" {
			t.Errorf("interface.defaultPrompt[%d] is blank", i)
		}
		if len(p) > 128 {
			t.Errorf("interface.defaultPrompt[%d] is %d chars; longer entries are truncated", i, len(p))
		}
	}
	if !strings.HasPrefix(m.Interface.WebsiteURL, "https://") {
		t.Errorf("websiteURL %q must be an absolute https URL", m.Interface.WebsiteURL)
	}
	// The canonical sentence, verbatim — the same one every README and the
	// npm listing carry (binaryPrereqSentence).
	for _, needle := range []string{binaryPrereqSentence, "installs no binary"} {
		if !strings.Contains(m.Description, needle) {
			t.Errorf("description must state the binary prerequisite; missing %q", needle)
		}
	}

	// .mcp.json is the stdio shape the first-party catalog uses, carrying
	// exactly what the codex registrar wrote into config.toml.
	var doc struct {
		MCPServers map[string]mcpServer `json:"mcpServers"`
	}
	if err := json.Unmarshal(fileByPath(t, files, "codex/"+codexPluginPath+"/.mcp.json"), &doc); err != nil {
		t.Fatalf(".mcp.json is not valid JSON: %v", err)
	}
	entry, ok := doc.MCPServers[mcp.ServerName]
	if !ok {
		t.Fatalf(".mcp.json has no %q server", mcp.ServerName)
	}
	w, err := deriveWiring()
	if err != nil {
		t.Fatalf("deriveWiring: %v", err)
	}
	if !sameMCPServer(entry, w.codexMCP) {
		t.Errorf("codex .mcp.json entry %s != registrar's config.toml entry %s",
			commandLine(entry), commandLine(w.codexMCP))
	}
	if entry.Command != pathBinary {
		t.Errorf("a published plugin must resolve the binary from PATH; command = %q", entry.Command)
	}

	// The catalog entry's source resolves against the marketplace root —
	// the directory holding .agents/ — which in this tree is plugins/codex.
	var mkt codexMarketplaceManifest
	if err := json.Unmarshal(fileByPath(t, files, "codex/.agents/plugins/marketplace.json"), &mkt); err != nil {
		t.Fatalf("marketplace.json is not valid JSON: %v", err)
	}
	if mkt.Name != marketplaceName {
		t.Errorf("marketplace name = %q, want %q", mkt.Name, marketplaceName)
	}
	if len(mkt.Plugins) != 1 {
		t.Fatalf("want exactly one catalog entry, got %d", len(mkt.Plugins))
	}
	e := mkt.Plugins[0]
	if e.Source.Source != "local" || !strings.HasPrefix(e.Source.Path, "./") || strings.Contains(e.Source.Path, "..") {
		t.Errorf("source %+v must be a local, \"./\"-relative path that never escapes the root", e.Source)
	}
	if e.Policy.Installation != codexInstallation || e.Policy.Authentication != codexAuthentication ||
		len(e.Policy.Products) == 0 || e.Category == "" {
		t.Errorf("entry must carry policy.installation, policy.authentication and category: %+v", e)
	}
	manifest := filepath.Join(repoRoot(t), "plugins", "codex",
		filepath.FromSlash(strings.TrimPrefix(e.Source.Path, "./")), ".codex-plugin", "plugin.json")
	if _, err := os.Stat(manifest); err != nil {
		t.Errorf("source %q does not resolve to a plugin manifest at %s: %v", e.Source.Path, manifest, err)
	}
}

// TestOpenCodeWiringMatchesRegistrar is the one-owner assertion for the
// OpenCode surface: the generated TS constants must carry exactly the
// entry internal/mcp's registrar wrote into opencode.json, and the
// hand-written glue must consume them rather than restate them.
func TestOpenCodeWiringMatchesRegistrar(t *testing.T) {
	files := generateOnce(t)
	ts := string(fileByPath(t, files, "opencode/src/wiring.generated.ts"))

	w, err := deriveWiring()
	if err != nil {
		t.Fatalf("deriveWiring: %v", err)
	}
	for _, needle := range []string{
		`export const OBSERVER_MCP_SERVER_NAME = "` + mcp.ServerName + `";`,
		`type: "` + w.openCodeMCP.Type + `" as const,`,
		`command: ` + tsStringList(w.openCodeMCP.Command) + `,`,
	} {
		if !strings.Contains(ts, needle) {
			t.Errorf("generated wiring is missing %q", needle)
		}
	}

	glue, err := os.ReadFile(filepath.Join(repoRoot(t), "plugins", "opencode", "src", "index.ts"))
	if err != nil {
		t.Fatalf("read hand-written glue: %v", err)
	}
	src := string(glue)
	if !strings.Contains(src, `from "./wiring.generated.js"`) {
		t.Error("index.ts must import the generated constants, not restate the wiring")
	}
	for _, forbidden := range []string{`"` + pathBinary + `"`, `"serve"`} {
		if strings.Contains(src, forbidden) {
			t.Errorf("index.ts hard-codes %s — that belongs in the generated module", forbidden)
		}
	}
	if !strings.Contains(src, "if (mcp[OBSERVER_MCP_SERVER_NAME] !== undefined) return;") {
		t.Error("index.ts must skip when the server is already configured (the double-wiring guard)")
	}

	// The hand-written package.json version has to track the same release
	// number the generated manifests carry, because
	// scripts/sync-npm-version.sh stamps them together.
	pkgRaw, err := os.ReadFile(filepath.Join(repoRoot(t), "plugins", "opencode", "package.json"))
	if err != nil {
		t.Fatalf("read package.json: %v", err)
	}
	var pkg struct {
		Name        string `json:"name"`
		Version     string `json:"version"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(pkgRaw, &pkg); err != nil {
		t.Fatalf("package.json is not valid JSON: %v", err)
	}
	if pkg.Name != openCodePackageName {
		t.Errorf("package name = %q, want %q", pkg.Name, openCodePackageName)
	}
	if want := committedVersion(t); pkg.Version != want {
		t.Errorf("package version = %q, want npm/observer/package.json's %q — run scripts/sync-npm-version.sh", pkg.Version, want)
	}
	for _, needle := range []string{"PATH", "installs no binary"} {
		if !strings.Contains(pkg.Description, needle) {
			t.Errorf("package description must state the binary prerequisite; missing %q", needle)
		}
	}
}

// The detector itself (savingsClaim, savingsPhrases, percentFigure,
// efficiencyVocabulary) lives in honesty.go — NOT in this test file — so
// scripts/assemble-plugins-repo.sh can run the SAME rule over the final
// assembled public tree via `plugingen -honesty-check <dir>`.

// TestSavingsClaimDetector pins the detector itself, positives AND the
// legitimate strings our listings actually contain, so tightening it never
// silently starts rejecting honest copy.
func TestSavingsClaimDetector(t *testing.T) {
	caught := []string{
		"cuts context usage by 40%",
		"saves 30% tokens",
		"40 percent cheaper token spend",
		"compression saves you money",
		"measured token savings on every turn",
	}
	for _, s := range caught {
		if savingsClaim(s) == "" {
			t.Errorf("detector missed a savings claim: %q", s)
		}
	}
	// Negative cases. The first two are verbatim from the committed tree
	// (the Cursor README's decode snippet and a version string); the rest
	// are honest copy that must stay publishable.
	clean := []string{
		`printf '%s' "$(grep -o 'config=[^&]*' deeplink.txt | cut -d= -f2-)" | base64 -d`,
		`"version": "1.28.0"`,
		"100% local — nothing is uploaded and no account is required",
		"Each value is percent-escaped before it reaches a query parser",
		"the observer MCP tool schema loads twice per turn",
		"Claude Code v2.1.220's PreCompact payload carries no occurrence id",
	}
	for _, s := range clean {
		if got := savingsClaim(s); got != "" {
			t.Errorf("detector false-positived on honest copy %q (matched %q)", s, got)
		}
	}
}

// TestHonestyCheckTree covers the -honesty-check entry point
// scripts/assemble-plugins-repo.sh runs over the FINAL assembled tree.
// The first case is the exact bypass that motivated sharing the checker:
// a percentage claim in a README, carrying none of the literal banned
// phrases, which the assembler's own literal-only list accepted.
func TestHonestyCheckTree(t *testing.T) {
	write := func(t *testing.T, dir, rel, body string) {
		t.Helper()
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	clean := "# Landing page\n\n**" + binaryPrereqSentence + ".** Wiring only.\n"

	t.Run("clean tree passes", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "README.md", clean)
		write(t, dir, "nested/README.md", clean)
		write(t, dir, "manifest.json", `{"version":"1.2.3"}`+"\n")
		if err := honestyCheckTree(dir); err != nil {
			t.Errorf("clean tree rejected: %v", err)
		}
	})
	t.Run("percentage claim in the landing README", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "README.md", clean+"\nObserver cuts context usage by 40% on every turn.\n")
		err := honestyCheckTree(dir)
		if err == nil {
			t.Fatal("want a failure on a percentage-flavoured efficiency claim, got nil")
		}
		if !strings.Contains(err.Error(), "README.md") || !strings.Contains(err.Error(), "savings claim") {
			t.Errorf("error does not name the offending file and claim: %v", err)
		}
	})
	t.Run("claim in a non-README file", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "README.md", clean)
		write(t, dir, "opencode/src/index.ts", "// saves tokens on every turn\n")
		if err := honestyCheckTree(dir); err == nil {
			t.Error("want a failure: every file is scanned, not just READMEs")
		}
	})
	t.Run("README without the prerequisite sentence", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "README.md", "# Landing page\n\nInstall it.\n")
		err := honestyCheckTree(dir)
		if err == nil {
			t.Fatal("want a failure when a README omits the prerequisite sentence")
		}
		if !strings.Contains(err.Error(), binaryPrereqSentence) {
			t.Errorf("error does not quote the missing sentence: %v", err)
		}
	})
	t.Run("empty tree is not a vacuous pass", func(t *testing.T) {
		if err := honestyCheckTree(t.TempDir()); err == nil {
			t.Error("want a failure: a tree with no files cannot have passed a scan")
		}
	})
	t.Run("missing directory", func(t *testing.T) {
		if err := honestyCheckTree(filepath.Join(t.TempDir(), "absent")); err == nil {
			t.Error("want a failure for a non-existent directory")
		}
	})
}

// TestListingsStateThePrerequisiteAndClaimNoSavings is the honesty gate the
// plan requires of every published listing (§0 + §4): each README states
// the binary prerequisite in the generator's own canonical words, and
// nothing anywhere claims token savings — a claim this project measured
// and RETRACTED.
//
// Scope note: the scan covers the GENERATED tree AND the hand-written
// OpenCode package files. package.json's `description` is a published
// listing (it is what npm renders on the package page), so excluding it
// would leave the one listing plugingen does not own ungated.
func TestListingsStateThePrerequisiteAndClaimNoSavings(t *testing.T) {
	type listing struct {
		path string
		body string
		// isListing marks a surface a user reads before installing: it
		// must carry the prerequisite sentence verbatim.
		isListing bool
	}

	var listings []listing
	for _, f := range generateOnce(t) {
		listings = append(listings, listing{
			path:      "plugins/" + f.Path,
			body:      string(f.Data),
			isListing: strings.HasSuffix(f.Path, "README.md"),
		})
	}

	// The hand-written OpenCode package. Its description AND its README
	// are what npm shows, so both are scanned; the source files are
	// scanned for savings claims too (comments get copied into docs).
	root := repoRoot(t)
	for _, rel := range append(openCodeHandWrittenFiles(), "opencode/README.md") {
		raw, err := os.ReadFile(filepath.Join(root, "plugins", filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("read %s: %v", rel, err)
			continue
		}
		listings = append(listings, listing{path: "plugins/" + rel, body: string(raw)})
	}
	// package.json's description is the npm listing text — checked for the
	// prerequisite sentence like any other listing.
	pkgRaw, err := os.ReadFile(filepath.Join(root, "plugins", "opencode", "package.json"))
	if err != nil {
		t.Fatalf("read package.json: %v", err)
	}
	var pkg struct {
		Description string `json:"description"`
	}
	if err := json.Unmarshal(pkgRaw, &pkg); err != nil {
		t.Fatalf("package.json is not valid JSON: %v", err)
	}
	listings = append(listings, listing{
		path:      "plugins/opencode/package.json#description",
		body:      pkg.Description,
		isListing: true,
	})

	for _, l := range listings {
		if got := savingsClaim(l.body); got != "" {
			t.Errorf("%s makes a savings claim (%s) — the compression-savings claim is RETRACTED", l.path, got)
		}
		if !l.isListing {
			continue
		}
		// One canonical sentence, asserted verbatim — not two loose
		// substrings that a rewrite could satisfy without saying it.
		if !strings.Contains(l.body, binaryPrereqSentence) {
			t.Errorf("%s does not carry the canonical prerequisite sentence %q", l.path, binaryPrereqSentence)
		}
		if !strings.Contains(l.body, "wiring only") && !strings.Contains(l.body, "wiring layer") {
			t.Errorf("%s does not say it is wiring only", l.path)
		}
	}
}

// ---------------------------------------------------------------------
// Coverage wave A (plan §7): kimi-code, qoder, devin, droid, openclaw,
// antigravity.
//
// Local vendor-validator status, run by hand against the committed tree
// the way §8 ran Anthropic's and OpenAI's (a passing validator is NOT a
// live install):
//
//   qodercli plugins validate plugins/qoder/superbased
//     → `Plugin "superbased" is valid and ready to install.` (exit 0),
//       and it names `.mcp.json` as a recognised convention component
//       loaded as "mcp-servers (1 servers)" — which is the dotted
//       spelling this surface emits.
//   qodercli plugins marketplace add <assembled tree>  (qodercli 1.1.5,
//   throwaway HOME) — the one place a vendor CLI was driven end to end
//   rather than only validated:
//     → WITHOUT a root .qoder-plugin/marketplace.json, the follow-up
//       `plugins install superbased@superbased` staged the CLAUDE CODE
//       plugin (.claude-plugin/plugin.json + hooks/hooks.json full of
//       `observer hook claude-code …`) — the documented fallback order
//       doing exactly what it says.
//     → WITH it, the same two commands staged .qoder-plugin/plugin.json
//       + .mcp.json and nothing else, at the plugin manifest's own
//       version. That is the catalog TestQoderMarketplaceShape pins.
//   agy plugin validate plugins/antigravity/superbased
//     → `[ok] … ✔ mcpServers : 1 processed` (exit 0). Note it RESOLVES
//       the MCP command on PATH: with `observer` absent it fails with
//       "command \"observer\" not found on PATH", which is the plugin's
//       stated prerequisite doing exactly what it says.
//
//   droid / devin / kimi expose no validate verb. openclaw's CLI hangs
//   even on `openclaw plugins --help` in this environment (the known
//   OpenClaw runtime block), so no OpenClaw validator was run — its
//   manifest is pinned structurally below instead.
//
// Neither vendor binary is invoked from these tests: CI has neither, and
// a test that shells out to a real CLI would run against a real HOME.
// ---------------------------------------------------------------------

// topLevelKeys decodes body's top-level JSON object keys, failing the test
// if it is not a JSON object.
func topLevelKeys(t *testing.T, label string, body []byte) map[string]json.RawMessage {
	t.Helper()
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(body, &keys); err != nil {
		t.Fatalf("%s is not a valid JSON object: %v", label, err)
	}
	return keys
}

// assertOnlyKeys fails for any key outside the documented set — the guard
// that keeps an undocumented field from being invented into a published
// manifest.
func assertOnlyKeys(t *testing.T, label string, keys map[string]json.RawMessage, documented ...string) {
	t.Helper()
	allowed := map[string]bool{}
	for _, k := range documented {
		allowed[k] = true
	}
	for k := range keys {
		if !allowed[k] {
			t.Errorf("%s carries key %q, which is not in that vendor's documented field list", label, k)
		}
	}
}

// assertStdioEntry pins a surface's MCP entry to `want` and re-asserts the
// PATH-resolution deviation every published manifest shares.
func assertStdioEntry(t *testing.T, label string, body []byte, want mcpServer) {
	t.Helper()
	var doc struct {
		MCPServers map[string]mcpServer `json:"mcpServers"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("%s is not valid JSON: %v", label, err)
	}
	entry, ok := doc.MCPServers[mcp.ServerName]
	if !ok {
		t.Fatalf("%s has no %q server (keys: %v)", label, mcp.ServerName, doc.MCPServers)
	}
	if !sameMCPServer(entry, want) {
		t.Errorf("%s entry %s != %s", label, commandLine(entry), commandLine(want))
	}
	if entry.Command != pathBinary {
		t.Errorf("%s: a published plugin must resolve the binary from PATH; command = %q", label, entry.Command)
	}
}

// assertListingDescription pins the canonical prerequisite sentence in a
// manifest description — the honesty rule for a listing a user reads
// before installing.
func assertListingDescription(t *testing.T, label, description string) {
	t.Helper()
	for _, needle := range []string{binaryPrereqSentence, "installs no binary"} {
		if !strings.Contains(description, needle) {
			t.Errorf("%s must state the binary prerequisite; missing %q", label, needle)
		}
	}
}

// waveAStdio returns the launch the registrar-less wave-A surfaces carry.
func waveAStdio(t *testing.T) mcpServer {
	t.Helper()
	w, err := deriveWiring()
	if err != nil {
		t.Fatalf("deriveWiring: %v", err)
	}
	stdio, err := canonicalStdio(w)
	if err != nil {
		t.Fatalf("canonicalStdio: %v", err)
	}
	return stdio
}

// TestKimiPluginShape pins the grounded kimi.plugin.json contract: the
// documented fields and nothing else, the documented id pattern, and the
// MCP server embedded in the manifest (Kimi is the one wave-A surface
// whose manifest carries mcpServers directly).
func TestKimiPluginShape(t *testing.T) {
	files := generateOnce(t)
	const rel = kimiSurfaceDir + "/" + pluginDir + "/kimi.plugin.json"
	raw := fileByPath(t, files, rel)

	keys := topLevelKeys(t, rel, raw)
	assertOnlyKeys(t, rel, keys,
		"name", "version", "description", "keywords", "homepage", "license", "interface", "mcpServers")
	// The four fields Kimi documents as UNSUPPORTED surface as diagnostics
	// if present, and `hooks` would name a receiver observer does not have.
	for _, forbidden := range []string{"tools", "apps", "inject", "configFile", "hooks"} {
		if _, ok := keys[forbidden]; ok {
			t.Errorf("%s declares %q — unsupported by Kimi, or unserviceable by observer", rel, forbidden)
		}
	}

	var m kimiPluginManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// "Must match [a-z0-9][a-z0-9_-]{0,63}" — quoted from the manifest doc.
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`).MatchString(m.Name) {
		t.Errorf("name %q does not match Kimi's documented id pattern", m.Name)
	}
	if m.Name != pluginName {
		t.Errorf("name = %q, want %q", m.Name, pluginName)
	}
	if want := committedVersion(t); m.Version != want {
		t.Errorf("version = %q, want %q", m.Version, want)
	}
	if m.Interface.DisplayName == "" || m.Interface.DeveloperName == "" {
		t.Errorf("interface is under-filled: %+v", m.Interface)
	}
	if !strings.HasPrefix(m.Interface.WebsiteURL, "https://") {
		t.Errorf("interface.websiteURL %q must be an absolute https URL", m.Interface.WebsiteURL)
	}
	assertListingDescription(t, rel, m.Description)
	assertStdioEntry(t, rel, raw, waveAStdio(t))
}

// TestQoderPluginShape pins the Qoder contract, whose non-obvious half is
// that MCP is a FILE and NOT a manifest field — the one correction this
// wave made to the research note.
func TestQoderPluginShape(t *testing.T) {
	files := generateOnce(t)
	const manifestRel = qoderSurfaceDir + "/" + pluginDir + "/.qoder-plugin/plugin.json"
	raw := fileByPath(t, files, manifestRel)

	keys := topLevelKeys(t, manifestRel, raw)
	assertOnlyKeys(t, manifestRel, keys,
		"name", "version", "description", "homepage", "repository", "license", "keywords")
	if _, ok := keys["mcpServers"]; ok {
		t.Errorf("%s declares mcpServers — Qoder's documented manifest has no such field; MCP is bundled by .mcp.json", manifestRel)
	}
	if _, ok := keys["hooks"]; ok {
		t.Errorf("%s declares hooks — observer has no Qoder hook receiver", manifestRel)
	}

	var m qoderPluginManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m.Name != pluginName {
		t.Errorf("name = %q, want %q", m.Name, pluginName)
	}
	// "cannot contain spaces; kebab-case recommended".
	if strings.ContainsAny(m.Name, " \t") || strings.ToLower(m.Name) != m.Name {
		t.Errorf("name %q must be space-free kebab-case", m.Name)
	}
	if want := committedVersion(t); m.Version != want {
		t.Errorf("version = %q, want %q", m.Version, want)
	}
	assertListingDescription(t, manifestRel, m.Description)

	// The MCP file is the DOTTED name. `qodercli plugins validate` reports
	// `.mcp.json` as the recognised convention component; a plain
	// `mcp.json` appears nowhere in Qoder's docs (Droid is the reverse —
	// see TestDroidPluginShape).
	assertStdioEntry(t, qoderSurfaceDir+"/"+pluginDir+"/.mcp.json",
		fileByPath(t, files, qoderSurfaceDir+"/"+pluginDir+"/.mcp.json"), waveAStdio(t))
	assertGeneratorEmitsNo(t, files, qoderSurfaceDir+"/"+pluginDir+"/mcp.json",
		"Qoder documents the dotted .mcp.json only")
}

// TestQoderMarketplaceShape pins the root catalog that shadows Qoder's
// fallback to `.claude-plugin/marketplace.json`.
//
// The fall-through is not hypothetical. Against the installed qodercli
// 1.1.5, under a throwaway HOME, `qodercli plugins marketplace add
// <assembled tree>` + `qodercli plugins install superbased@superbased`
// staged the CLAUDE CODE plugin — `.claude-plugin/plugin.json` and a
// `hooks/hooks.json` full of `observer hook claude-code …` — into Qoder's
// plugin cache. With this catalog present the same two commands staged
// `.qoder-plugin/plugin.json` + `.mcp.json` and no hooks. The schema
// asserted below is the one the CLI validates against, read out of the
// shipped binary (see qoder.go's file comment).
func TestQoderMarketplaceShape(t *testing.T) {
	files := generateOnce(t)
	const catalogRel = qoderSurfaceDir + "/.qoder-plugin/marketplace.json"
	raw := fileByPath(t, files, catalogRel)

	keys := topLevelKeys(t, catalogRel, raw)
	assertOnlyKeys(t, catalogRel, keys, "name", "owner", "metadata", "plugins")
	// The catalog's own prose lives under metadata, NOT at the top level:
	// that is the one shape difference from the Claude Code catalog.
	if _, ok := keys["description"]; ok {
		t.Errorf("%s carries a top-level description — Qoder's schema puts it in metadata.description", catalogRel)
	}

	var m qoderMarketplaceManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m.Name != marketplaceName {
		t.Errorf("marketplace name = %q, want %q", m.Name, marketplaceName)
	}
	// `^[a-z0-9][-a-z0-9._]*$`, ≤100 chars, no spaces.
	if !regexp.MustCompile(`^[a-z0-9][-a-z0-9._]*$`).MatchString(m.Name) || len(m.Name) > 100 {
		t.Errorf("marketplace name %q does not satisfy Qoder's name regex", m.Name)
	}
	// Reserved / impersonating names the CLI refuses outright.
	for _, reserved := range []string{
		"qoder-marketplace", "qoder-plugins", "qoder-plugins-official",
		"inline", "builtin", "local", "flag",
	} {
		if strings.EqualFold(m.Name, reserved) {
			t.Errorf("marketplace name %q is reserved by qodercli", m.Name)
		}
	}
	for _, prefix := range []string{"qoder-enterprise-", "qoderwork-enterprise-"} {
		if strings.HasPrefix(strings.ToLower(m.Name), prefix) {
			t.Errorf("marketplace name %q uses the reserved %q prefix", m.Name, prefix)
		}
	}
	// owner is REQUIRED by Qoder's schema (Droid's is optional).
	if m.Owner.Name == "" {
		t.Error("owner.name is a required Qoder marketplace field")
	}

	if len(m.Plugins) != 1 {
		t.Fatalf("want exactly one catalog entry, got %d", len(m.Plugins))
	}
	e := m.Plugins[0]
	if e.Name != pluginName {
		t.Errorf("entry name = %q, want %q", e.Name, pluginName)
	}
	if strings.Contains(e.Name, " ") {
		t.Errorf("entry name %q contains a space, which Qoder refuses", e.Name)
	}
	if e.Source != "./"+qoderPluginPath {
		t.Errorf("entry source = %q, want %q — the plugin lives in a SUBDIRECTORY of the marketplace root; \"./\"+pluginDir would resolve the Claude Code plugin instead", e.Source, "./"+qoderPluginPath)
	}
	// The source resolves against the MARKETPLACE root, which for this
	// catalog is plugins/ itself (see qoder.go).
	if err := qoderCatalogSourceResolves(filepath.Join(repoRoot(t), "plugins"), e.Source); err != nil {
		t.Error(err)
	}
	assertListingDescription(t, catalogRel+"#plugins[0]", e.Description)

	// No version on the entry: qodercli resolves the pin from the plugin's
	// own manifest (verified live — `plugins list` printed the manifest's
	// version for a catalog entry that carried none), so a version here
	// would be a second number to stamp for no gain.
	var entries []json.RawMessage
	if err := json.Unmarshal(keys["plugins"], &entries); err != nil {
		t.Fatalf("%s: plugins is not a JSON array: %v", catalogRel, err)
	}
	entryKeys := topLevelKeys(t, catalogRel+"#plugins[0]", entries[0])
	if _, ok := entryKeys["version"]; ok {
		t.Errorf("%s: the catalog entry carries a version — the plugin's own manifest is the pin, like the Codex and Droid catalogs", catalogRel)
	}
	assertOnlyKeys(t, catalogRel+"#plugins[0]", entryKeys,
		"name", "source", "description", "category", "homepage", "tags")
}

// TestDevinPluginShape pins the Devin contract, including the two
// deliberate absences: the manifest's mcpServers field (which Devin reads
// IN ADDITION to the root convention this plugin ships, so setting both
// would declare the server twice) and hooks.json.
func TestDevinPluginShape(t *testing.T) {
	files := generateOnce(t)
	const manifestRel = devinSurfaceDir + "/" + pluginDir + "/.devin-plugin/plugin.json"
	raw := fileByPath(t, files, manifestRel)

	keys := topLevelKeys(t, manifestRel, raw)
	assertOnlyKeys(t, manifestRel, keys,
		"name", "version", "description", "author", "homepage", "repository", "license", "keywords")
	if _, ok := keys["mcpServers"]; ok {
		t.Errorf("%s sets mcpServers as well as shipping mcp_config.json — Devin reads the manifest field IN ADDITION to the root convention, so that declares one server twice", manifestRel)
	}

	var m devinPluginManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m.Name != pluginName {
		t.Errorf("name = %q, want %q", m.Name, pluginName)
	}
	if want := committedVersion(t); m.Version != want {
		t.Errorf("version = %q, want %q", m.Version, want)
	}
	// Devin documents author as {name, email} — no url key.
	if m.Author.Name == "" || m.Author.Email == "" {
		t.Errorf("author must carry the documented {name, email}: %+v", m.Author)
	}
	authorKeys := topLevelKeys(t, manifestRel+"#author", keys["author"])
	assertOnlyKeys(t, manifestRel+"#author", authorKeys, "name", "email")
	assertListingDescription(t, manifestRel, m.Description)

	assertStdioEntry(t, devinSurfaceDir+"/"+pluginDir+"/mcp_config.json",
		fileByPath(t, files, devinSurfaceDir+"/"+pluginDir+"/mcp_config.json"), waveAStdio(t))
	assertGeneratorEmitsNo(t, files, devinSurfaceDir+"/"+pluginDir+"/hooks.json",
		"observer has no Devin hook receiver, and Devin's own hooks.v1.json is unwired in the shipped CLI")
}

// TestDroidPluginShape pins the Droid contract. Droid is the ONE wave-A
// surface with a real observer registrar, so its MCP entry is asserted
// against ~/.factory/mcp.json's registrar output rather than
// canonicalStdio — and against the undotted file name Factory documents.
func TestDroidPluginShape(t *testing.T) {
	files := generateOnce(t)
	const manifestRel = droidSurfaceDir + "/" + droidPluginDir + "/.factory-plugin/plugin.json"
	raw := fileByPath(t, files, manifestRel)

	keys := topLevelKeys(t, manifestRel, raw)
	assertOnlyKeys(t, manifestRel, keys,
		"name", "description", "version", "author", "homepage", "repository", "license", "keywords")
	if _, ok := keys["mcpServers"]; ok {
		t.Errorf("%s declares mcpServers — Droid's manifest has no such field; MCP is bundled by a root mcp.json", manifestRel)
	}

	var m droidPluginManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m.Name != pluginName {
		t.Errorf("name = %q, want %q", m.Name, pluginName)
	}
	if want := committedVersion(t); m.Version != want {
		t.Errorf("version = %q, want %q", m.Version, want)
	}
	// "author (object with name)" is all Factory documents.
	assertOnlyKeys(t, manifestRel+"#author", topLevelKeys(t, manifestRel+"#author", keys["author"]), "name")
	assertListingDescription(t, manifestRel, m.Description)

	// The registrar half: this is the one-owner assertion droid uniquely
	// gets in this wave.
	w, err := deriveWiring()
	if err != nil {
		t.Fatalf("deriveWiring: %v", err)
	}
	assertStdioEntry(t, droidSurfaceDir+"/"+droidPluginDir+"/mcp.json",
		fileByPath(t, files, droidSurfaceDir+"/"+droidPluginDir+"/mcp.json"), w.droidMCP)
	// The undotted spelling is Factory's native one; `.mcp.json` is the
	// Claude-compat alias Droid translates on copy. Emitting both would be
	// two spellings of one server (Qoder is the exact reverse).
	assertGeneratorEmitsNo(t, files, droidSurfaceDir+"/"+droidPluginDir+"/.mcp.json",
		"mcp.json is Factory's native name; .mcp.json is the alias it translates")
	assertGeneratorEmitsNo(t, files, droidSurfaceDir+"/"+droidPluginDir+"/hooks/hooks.json",
		"observer has no Droid hook receiver")

	// The catalog.
	const catalogRel = droidSurfaceDir + "/.factory-plugin/marketplace.json"
	var mkt droidMarketplaceManifest
	if err := json.Unmarshal(fileByPath(t, files, catalogRel), &mkt); err != nil {
		t.Fatalf("%s is not valid JSON: %v", catalogRel, err)
	}
	if mkt.Name != marketplaceName {
		t.Errorf("marketplace name = %q, want %q", mkt.Name, marketplaceName)
	}
	if mkt.Owner.Name == "" {
		t.Error("owner.name is the documented contact field")
	}
	if len(mkt.Plugins) != 1 {
		t.Fatalf("want exactly one catalog entry, got %d", len(mkt.Plugins))
	}
	e := mkt.Plugins[0]
	if e.Name != pluginName {
		t.Errorf("entry name = %q, want %q", e.Name, pluginName)
	}
	assertListingDescription(t, catalogRel+"#plugins[0].description", e.Description)
	// "Relative path sources must stay inside the marketplace directory."
	if err := droidCatalogSourceResolves(filepath.Join(repoRoot(t), "plugins", droidSurfaceDir), e.Source); err != nil {
		t.Error(err)
	}
	for _, bad := range []string{"../elsewhere", "superbased", "/abs/path", "./a/../../b"} {
		if err := droidCatalogSourceResolves(filepath.Join(repoRoot(t), "plugins", droidSurfaceDir), bad); err == nil {
			t.Errorf("droidCatalogSourceResolves accepted %q, which escapes or is not \"./\"-relative", bad)
		}
	}
}

// TestOpenClawManifestShape pins OpenClaw's own manifest schema: the two
// REQUIRED fields present and non-vacuous, the entry's `transport`
// discriminator, and the npm-metadata fields deliberately absent (the
// manifest reference states they belong in package.json).
func TestOpenClawManifestShape(t *testing.T) {
	files := generateOnce(t)
	const rel = openClawSurfaceDir + "/" + pluginDir + "/openclaw.plugin.json"
	raw := fileByPath(t, files, rel)

	keys := topLevelKeys(t, rel, raw)
	assertOnlyKeys(t, rel, keys,
		"id", "configSchema", "name", "description", "version", "activation", "mcpServers")
	// "A missing or invalid manifest blocks config validation and is
	// treated as a plugin error", so both required fields must be there.
	for _, required := range []string{"id", "configSchema"} {
		if _, ok := keys[required]; !ok {
			t.Errorf("%s is missing the REQUIRED field %q", rel, required)
		}
	}
	// Not part of this manifest — the docs put npm install metadata in
	// package.json instead.
	for _, forbidden := range []string{"homepage", "repository", "license", "keywords", "displayName", "icon", "catalog", "hooks"} {
		if _, ok := keys[forbidden]; ok {
			t.Errorf("%s declares %q, which openclaw.plugin.json does not document (or which observer cannot serve)", rel, forbidden)
		}
	}

	var m openClawManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m.ID != pluginName {
		t.Errorf("id = %q, want %q — it is also the plugins.entries key", m.ID, pluginName)
	}
	if m.ConfigSchema.Type != "object" || m.ConfigSchema.AdditionalProperties {
		t.Errorf("configSchema must be the closed object the docs' minimal example uses: %+v", m.ConfigSchema)
	}
	if m.ConfigSchema.Properties == nil {
		t.Error("configSchema.properties must marshal as {} rather than null")
	}
	if want := committedVersion(t); m.Version != want {
		t.Errorf("version = %q, want %q", m.Version, want)
	}
	if !m.Activation.OnStartup {
		t.Error("activation.onStartup must be set deliberately; a statically-declared MCP server needs it true")
	}
	assertListingDescription(t, rel, m.Description)

	entry, ok := m.MCPServers[mcp.ServerName]
	if !ok {
		t.Fatalf("%s has no %q server", rel, mcp.ServerName)
	}
	if entry.Transport != openClawStdioTransport {
		t.Errorf("transport = %q, want %q — OpenClaw's entry shape carries a transport discriminator", entry.Transport, openClawStdioTransport)
	}
	stdio := waveAStdio(t)
	if entry.Command != stdio.Command || strings.Join(entry.Args, " ") != strings.Join(stdio.Args, " ") {
		t.Errorf("entry %s %v != canonical launch %s", entry.Command, entry.Args, commandLine(stdio))
	}
	if entry.Command != pathBinary {
		t.Errorf("a published plugin must resolve the binary from PATH; command = %q", entry.Command)
	}
}

// TestAntigravityPluginShape pins the one-field marker manifest, the
// mcp_config.json entry, and the absence of any version number (the format
// documents no version key — an omission that is the format, not a gap).
func TestAntigravityPluginShape(t *testing.T) {
	files := generateOnce(t)
	const manifestRel = antigravitySurfaceDir + "/" + pluginDir + "/plugin.json"
	raw := fileByPath(t, files, manifestRel)

	keys := topLevelKeys(t, manifestRel, raw)
	assertOnlyKeys(t, manifestRel, keys, "name")

	var m antigravityPluginManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m.Name != pluginName {
		t.Errorf("name = %q, want %q — it is the agy plugin enable|disable|uninstall argument", m.Name, pluginName)
	}
	// The CLI page's documented pattern.
	if !regexp.MustCompile(`^[a-zA-Z0-9-_]+$`).MatchString(m.Name) {
		t.Errorf("name %q does not match Antigravity's documented ^[a-zA-Z0-9-_]+$", m.Name)
	}

	assertStdioEntry(t, antigravitySurfaceDir+"/"+pluginDir+"/mcp_config.json",
		fileByPath(t, files, antigravitySurfaceDir+"/"+pluginDir+"/mcp_config.json"), waveAStdio(t))
	assertGeneratorEmitsNo(t, files, antigravitySurfaceDir+"/"+pluginDir+"/hooks.json",
		"observer has no Antigravity hook receiver")

	// No version key anywhere in this surface's JSON.
	for _, f := range files {
		if !strings.HasPrefix(f.Path, antigravitySurfaceDir+"/") || !strings.HasSuffix(f.Path, ".json") {
			continue
		}
		if _, ok := topLevelKeys(t, f.Path, f.Data)["version"]; ok {
			t.Errorf("%s carries a version key, but Antigravity's plugin.json documents only `name`", f.Path)
		}
	}
}

// TestAntigravityAvoidsCodexAgentsPluginsCollision is the durable encoding
// of the wave-A collision decision (see the header comment in
// antigravity.go).
//
// Antigravity's WORKSPACE plugin directory is `.agents/plugins/` — the
// same directory Codex reads its marketplace catalog from, and
// scripts/assemble-plugins-repo.sh puts that catalog at the public
// repository root. Codex only ever reads the one file it names, so its
// side is safe; what Antigravity does with a LOOSE FILE in a plugins
// directory is NOT documented anywhere, and we decline to assert safety on
// an inference.
//
// Antigravity's documented install resolves nothing from a repository root
// (`agy plugin install <local dir>` stages into the user's own home), so
// the collision simply never has to be created. This test fails if a
// future change starts creating it.
func TestAntigravityAvoidsCodexAgentsPluginsCollision(t *testing.T) {
	const agentsPlugins = ".agents/plugins/"
	codexCatalog := "codex/" + agentsPlugins + "marketplace.json"

	sawCatalog := false
	for _, f := range generateOnce(t) {
		if !strings.Contains(f.Path, agentsPlugins) {
			continue
		}
		if f.Path == codexCatalog {
			sawCatalog = true
			continue
		}
		t.Errorf("%s lives under %s, which is BOTH Codex's catalog directory and Antigravity's workspace plugin directory; "+
			"only Codex's marketplace.json may live there (see antigravity.go)", f.Path, agentsPlugins)
	}
	if !sawCatalog {
		t.Errorf("expected the Codex catalog at %s — if it moved, revisit the collision decision in antigravity.go", codexCatalog)
	}
	for _, f := range generateOnce(t) {
		if strings.HasPrefix(f.Path, antigravitySurfaceDir+"/") && strings.Contains(f.Path, ".agents") {
			t.Errorf("%s puts an Antigravity artifact in a .agents path; this surface ships as a plain directory the user copies or `agy plugin install`s", f.Path)
		}
	}
}

// TestWaveASurfacesDeclareNoHooks is the honesty gate for the capability
// observer does NOT have on these tools: `observer hook <tool>` accepts
// claude-code, cursor, codex and hermes and nothing else, so a hook
// declared on any wave-A surface would point that tool at a command that
// does not exist. Several of these formats document a hooks vocabulary,
// which is exactly why this is asserted rather than assumed.
func TestWaveASurfacesDeclareNoHooks(t *testing.T) {
	waveA := []string{
		kimiSurfaceDir, qoderSurfaceDir, devinSurfaceDir,
		droidSurfaceDir, openClawSurfaceDir, antigravitySurfaceDir,
	}
	for _, f := range generateOnce(t) {
		inWaveA := false
		for _, dir := range waveA {
			if strings.HasPrefix(f.Path, dir+"/") {
				inWaveA = true
				break
			}
		}
		if !inWaveA || !strings.HasSuffix(f.Path, ".json") {
			continue
		}
		if strings.Contains(f.Path, "hooks") {
			t.Errorf("%s is a hooks manifest, but observer has no hook receiver for that tool", f.Path)
		}
		if _, ok := topLevelKeys(t, f.Path, f.Data)["hooks"]; ok {
			t.Errorf("%s declares a `hooks` key, but observer has no hook receiver for that tool", f.Path)
		}
	}
}

// assertGeneratorEmitsNo fails if the generator produces rel. Used for the
// near-miss file names each vendor pair makes easy to get wrong (Droid's
// undotted mcp.json vs Qoder's dotted .mcp.json) and for the hook files no
// wave-A surface may ship.
func assertGeneratorEmitsNo(t *testing.T, files []outFile, rel, why string) {
	t.Helper()
	for _, f := range files {
		if f.Path == rel {
			t.Errorf("generator emits %s, which it must not: %s", rel, why)
		}
	}
}

// ---------------------------------------------------------------------
// Coverage wave B (plan §7): the config LISTINGS — crush, kiro-cli,
// copilot-cli, kilo-code, roo-code, open-interpreter. None of these tools
// has a package format, so the artifact is a README carrying the exact
// config block; what these tests pin is that the block is the registrar's
// launch and each vendor's own key vocabulary, not a hand-typed guess.
// ---------------------------------------------------------------------

// waveBSurfaceDirs are the wave-B listing directories.
func waveBSurfaceDirs() []string {
	return []string{
		crushSurfaceDir, kiroSurfaceDir, copilotCLISurfaceDir,
		kiloCodeSurfaceDir, rooSurfaceDir, openInterpreterSurfaceDir,
	}
}

// waveCSurfaceDirs are the wave-C directories.
func waveCSurfaceDirs() []string {
	return []string{copilotSurfaceDir, coworkSurfaceDir, piSurfaceDir, commandCodeSurfaceDir}
}

// TestWaveBListingsAreReadmeOnlyAndCarryTheLaunch pins the shape of the
// whole wave: exactly one file per surface (the README), the canonical
// launch inside it, and the prerequisite sentence (also covered globally,
// asserted here so a missing surface fails loudly rather than silently
// dropping out of the global loop).
func TestWaveBListingsAreReadmeOnlyAndCarryTheLaunch(t *testing.T) {
	files := generateOnce(t)
	stdio := waveAStdio(t)
	launch := commandLine(stdio)

	for _, dir := range waveBSurfaceDirs() {
		var got []string
		for _, f := range files {
			if strings.HasPrefix(f.Path, dir+"/") {
				got = append(got, f.Path)
			}
		}
		if len(got) != 1 || got[0] != dir+"/README.md" {
			t.Errorf("%s should be a README-only listing, got %v", dir, got)
			continue
		}
		body := string(fileByPath(t, files, dir+"/README.md"))
		if !strings.Contains(body, launch) {
			t.Errorf("%s/README.md does not carry the registrar launch %q", dir, launch)
		}
		if !strings.Contains(body, binaryPrereqSentence) {
			t.Errorf("%s/README.md does not carry the prerequisite sentence", dir)
		}
	}
}

// TestCrushBlockShape pins Crush's documented vocabulary: the top-level
// `mcp` key, the required `type: stdio` discriminator, and the $schema the
// first-party samples carry.
func TestCrushBlockShape(t *testing.T) {
	stdio := waveAStdio(t)
	raw, err := renderCrushConfigJSON(stdio)
	if err != nil {
		t.Fatalf("renderCrushConfigJSON: %v", err)
	}
	keys := topLevelKeys(t, "crush.json", raw)
	assertOnlyKeys(t, "crush.json", keys, "$schema", "mcp")

	var doc struct {
		Schema string                   `json:"$schema"`
		MCP    map[string]crushMCPEntry `json:"mcp"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc.Schema != "https://charm.land/crush.json" {
		t.Errorf("$schema = %q, want the URL the first-party samples use", doc.Schema)
	}
	entry, ok := doc.MCP[mcp.ServerName]
	if !ok {
		t.Fatalf("no %q entry under the `mcp` key", mcp.ServerName)
	}
	if entry.Type != "stdio" {
		t.Errorf("type = %q, want %q (Crush's required transport discriminator)", entry.Type, "stdio")
	}
	if entry.Command != pathBinary || strings.Join(entry.Args, " ") != strings.Join(stdio.Args, " ") {
		t.Errorf("entry %s %v != the registrar launch %s", entry.Command, entry.Args, commandLine(stdio))
	}
	// `mcpServers` is the OTHER vendors' key; Crush uses `mcp`.
	if _, wrong := keys["mcpServers"]; wrong {
		t.Error("crush.json must use the top-level key `mcp`, not `mcpServers`")
	}
}

// TestKiroBlockShape pins the Cline-shaped mcpServers block Kiro reads.
func TestKiroBlockShape(t *testing.T) {
	stdio := waveAStdio(t)
	raw, err := renderKiroMCPJSON(stdio)
	if err != nil {
		t.Fatalf("renderKiroMCPJSON: %v", err)
	}
	assertOnlyKeys(t, "kiro mcp.json", topLevelKeys(t, "kiro mcp.json", raw), "mcpServers")
	assertStdioEntry(t, "kiro mcp.json", raw, stdio)
}

// TestRooBlockShape pins the same for Roo Code, and that no auto-approval
// list is published on a user's behalf.
func TestRooBlockShape(t *testing.T) {
	stdio := waveAStdio(t)
	raw, err := renderRooMCPJSON(stdio)
	if err != nil {
		t.Fatalf("renderRooMCPJSON: %v", err)
	}
	assertOnlyKeys(t, ".roo/mcp.json", topLevelKeys(t, ".roo/mcp.json", raw), "mcpServers")
	assertStdioEntry(t, ".roo/mcp.json", raw, stdio)
	if strings.Contains(string(raw), "alwaysAllow") {
		t.Error(".roo/mcp.json must not publish an alwaysAllow list — auto-approval is the user's trust decision")
	}
}

// TestCopilotCLIUsesMCPServersNotServers is the mutation proof for the
// cross-product trap. GitHub Copilot CLI reads `mcpServers`; VS Code reads
// `servers`; the wrong key fails SILENTLY. Both halves are asserted
// against LITERALS (not the constants the generator uses), so swapping the
// constants — or "harmonising" the two surfaces — fails here.
func TestCopilotCLIUsesMCPServersNotServers(t *testing.T) {
	stdio := waveAStdio(t)

	cliRaw, err := renderCopilotCLIConfigJSON(stdio)
	if err != nil {
		t.Fatalf("renderCopilotCLIConfigJSON: %v", err)
	}
	cliKeys := topLevelKeys(t, "copilot-cli mcp-config.json", cliRaw)
	if _, ok := cliKeys["mcpServers"]; !ok {
		t.Errorf("Copilot CLI config must use the top-level key \"mcpServers\"; got %v", keyNames(cliKeys))
	}
	if _, ok := cliKeys["servers"]; ok {
		t.Error("Copilot CLI config carries \"servers\" — GitHub's docs call that key UNSUPPORTED for the CLI, and it fails silently")
	}

	vsRaw, err := renderCopilotWorkspaceJSON(stdio)
	if err != nil {
		t.Fatalf("renderCopilotWorkspaceJSON: %v", err)
	}
	vsKeys := topLevelKeys(t, ".vscode/mcp.json", vsRaw)
	if _, ok := vsKeys["servers"]; !ok {
		t.Errorf("VS Code mcp.json must use the top-level key \"servers\"; got %v", keyNames(vsKeys))
	}
	if _, ok := vsKeys["mcpServers"]; ok {
		t.Error("VS Code mcp.json carries \"mcpServers\" — that is Copilot CLI's key, and VS Code will not read it")
	}

	// The entry itself: Copilot CLI's documented local shape.
	var cli struct {
		MCPServers map[string]copilotCLIEntry `json:"mcpServers"`
	}
	if err := json.Unmarshal(cliRaw, &cli); err != nil {
		t.Fatalf("decode copilot-cli config: %v", err)
	}
	entry, ok := cli.MCPServers[mcp.ServerName]
	if !ok {
		t.Fatalf("no %q entry", mcp.ServerName)
	}
	if entry.Type != "local" {
		t.Errorf("type = %q, want \"local\" (Copilot CLI's discriminator for a command-launched server)", entry.Type)
	}
	if len(entry.Tools) == 0 {
		t.Error("every first-party local example carries `tools`; omitting it publishes a config whose tool exposure the docs do not define")
	}
	if entry.Command != pathBinary {
		t.Errorf("command = %q, want the PATH binary %q", entry.Command, pathBinary)
	}

	// Both READMEs must NAME the trap, so a reader who lands on the wrong
	// page is told before they paste.
	files := generateOnce(t)
	cliBody := string(fileByPath(t, files, copilotCLISurfaceDir+"/README.md"))
	vsBody := string(fileByPath(t, files, copilotSurfaceDir+"/README.md"))
	for _, tc := range []struct {
		label, body string
	}{{"copilot-cli README", cliBody}, {"copilot README", vsBody}} {
		if !strings.Contains(tc.body, "mcpServers") || !strings.Contains(tc.body, "servers") {
			t.Errorf("%s does not name both spellings of the cross-product trap", tc.label)
		}
	}
}

// keyNames renders a decoded key set for an error message.
func keyNames(keys map[string]json.RawMessage) []string {
	out := make([]string, 0, len(keys))
	for k := range keys {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestKiloCodeBlockIsTheOpenCodeRegistrarEntry pins the transposition:
// Kilo's `mcp` entry shape IS OpenCode's, so the published block must be
// what observer's OpenCode registrar wrote — argv array, not command+args.
func TestKiloCodeBlockIsTheOpenCodeRegistrarEntry(t *testing.T) {
	w, err := deriveWiring()
	if err != nil {
		t.Fatalf("deriveWiring: %v", err)
	}
	raw, err := renderKiloCodeConfigJSON(w.openCodeMCP)
	if err != nil {
		t.Fatalf("renderKiloCodeConfigJSON: %v", err)
	}
	assertOnlyKeys(t, "kilo.jsonc", topLevelKeys(t, "kilo.jsonc", raw), "mcp")

	var doc struct {
		MCP map[string]openCodeServer `json:"mcp"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	entry, ok := doc.MCP[mcp.ServerName]
	if !ok {
		t.Fatalf("no %q entry under the `mcp` key", mcp.ServerName)
	}
	if entry.Type != "local" {
		t.Errorf("type = %q, want \"local\"", entry.Type)
	}
	if strings.Join(entry.Command, "\x00") != strings.Join(w.openCodeMCP.Command, "\x00") {
		t.Errorf("command %v != the OpenCode registrar's %v", entry.Command, w.openCodeMCP.Command)
	}
	if !entry.Enabled {
		t.Error("entry is not enabled — the registrar writes enabled=true")
	}
	// Kilo's environment key is `environment`, not `env`; we set neither,
	// so neither spelling may appear.
	if strings.Contains(string(raw), `"env"`) {
		t.Error("kilo.jsonc carries `env` — Kilo's key is `environment`, and observer's server needs neither")
	}
}

// TestOpenInterpreterBlockIsTheCodexRegistrarTable pins that the published
// TOML is the Codex registrar's own table text and still decodes to the
// entry that registrar wrote.
func TestOpenInterpreterBlockIsTheCodexRegistrarTable(t *testing.T) {
	w, err := deriveWiring()
	if err != nil {
		t.Fatalf("deriveWiring: %v", err)
	}
	block := w.codexTOMLBlock
	if !strings.Contains(block, codexServerTableHeader) {
		t.Fatalf("the lifted block has no %s header:\n%s", codexServerTableHeader, block)
	}
	// The WHOLE entry, not just command+args: decoding the lifted text
	// must give back the registrar's entry field for field, env included.
	// Comparing only the two keys we happen to emit today would accept a
	// lift that dropped a nested [mcp_servers.observer.env] table — the
	// exact truncation the extractor's sub-table rule prevents.
	lifted := decodeCodexServerTable(t, block)
	if !sameMCPServer(lifted, w.codexMCP) {
		t.Errorf("the lifted table decodes to %+v, the registrar wrote %+v", lifted, w.codexMCP)
	}
	if len(lifted.Env) != len(w.codexMCP.Env) {
		t.Errorf("env = %v, the registrar wrote %v", lifted.Env, w.codexMCP.Env)
	}
	// The listing must point at Open Interpreter's OWN home, never ~/.codex.
	body := string(fileByPath(t, generateOnce(t), openInterpreterSurfaceDir+"/README.md"))
	if !strings.Contains(body, "~/.openinterpreter/config.toml") {
		t.Error("the Open Interpreter listing does not name ~/.openinterpreter/config.toml")
	}
	// And the published block IS the lifted text, byte for byte.
	if !strings.Contains(body, block) {
		t.Errorf("the listing does not carry the lifted block verbatim:\n%s", block)
	}
}

// decodeCodexServerTable decodes lifted [mcp_servers.observer] table text
// into the canonical stdio shape — the FULL entry, so a dropped nested
// table shows up as a missing field rather than passing unnoticed.
func decodeCodexServerTable(t *testing.T, block string) mcpServer {
	t.Helper()
	var root struct {
		MCPServers map[string]mcpServer `toml:"mcp_servers"`
	}
	if err := toml.Unmarshal([]byte(block), &root); err != nil {
		t.Fatalf("the lifted block is not valid TOML: %v\n%s", err, block)
	}
	entry, ok := root.MCPServers[mcp.ServerName]
	if !ok {
		t.Fatalf("the lifted block has no [mcp_servers.%s] table:\n%s", mcp.ServerName, block)
	}
	return entry
}

// TestOpenInterpreterExtractorCarriesNestedTables is the mutation proof
// for the lift's table boundary.
//
// A TOML table ends at the next header — but a MAP-valued key on the
// entry is written as its own header, `[mcp_servers.observer.env]`, after
// the entry's scalar keys. A boundary rule of "stop at the first `[`"
// therefore publishes a truncated entry whose env silently vanished. The
// Codex registrar writes no env today, so this drives the extractor with
// a synthetic config in exactly the shape BurntSushi's encoder produces:
// indented headers, nested table after the scalars, an unrelated table
// following.
func TestOpenInterpreterExtractorCarriesNestedTables(t *testing.T) {
	const body = `model = "o4-mini"

  [mcp_servers.other]
    command = "/opt/other"

  [mcp_servers.observer]
    args = ["serve"]
    command = "observer"

    [mcp_servers.observer.env]
      OBSERVER_CONFIG = "/etc/observer.toml"

  [mcp_servers.zzz]
    command = "/opt/zzz"
`
	block, err := liftCodexServerTable(body)
	if err != nil {
		t.Fatalf("liftCodexServerTable: %v", err)
	}
	// Boundary: the nested table is IN, the next sibling entry is OUT.
	if !strings.Contains(block, "[mcp_servers.observer.env]") {
		t.Errorf("the lifted block dropped the nested env table:\n%s", block)
	}
	if strings.Contains(block, "zzz") || strings.Contains(block, "other") {
		t.Errorf("the lifted block ran past the end of the entry:\n%s", block)
	}

	// Semantics, not just substrings: the block must decode to the whole
	// entry, env included.
	got := decodeCodexServerTable(t, block)
	want := mcpServer{
		Command: "observer",
		Args:    []string{"serve"},
		Env:     map[string]string{"OBSERVER_CONFIG": "/etc/observer.toml"},
	}
	if !sameMCPServer(got, want) {
		t.Errorf("the lifted block decodes to %+v, want %+v", got, want)
	}

	// A table nested under a DIFFERENT server is not ours, even though its
	// header shares the `mcp_servers.` prefix.
	const sibling = `  [mcp_servers.observer]
    command = "observer"

  [mcp_servers.observerish.env]
    NOPE = "1"
`
	block, err = liftCodexServerTable(sibling)
	if err != nil {
		t.Fatalf("liftCodexServerTable (sibling): %v", err)
	}
	if strings.Contains(block, "observerish") {
		t.Errorf("the lift swallowed a different server's nested table:\n%s", block)
	}
}

// ---------------------------------------------------------------------
// Coverage wave C (plan §7): the special formats and the two documented
// decisions NOT to ship a package.
// ---------------------------------------------------------------------

// TestCopilotInstallURIDecodesToTheSamePayload is the mutation proof for
// the VS Code install-URI encoding: decoding the emitted link must give
// back byte-identical JSON, and that JSON must be the registrar's entry.
func TestCopilotInstallURIDecodesToTheSamePayload(t *testing.T) {
	stdio := waveAStdio(t)
	uri, err := copilotInstallURI(stdio)
	if err != nil {
		t.Fatalf("copilotInstallURI: %v", err)
	}
	const scheme = "vscode:mcp/install?"
	if !strings.HasPrefix(uri, scheme) {
		t.Fatalf("uri %q does not start with the documented scheme %q", uri, scheme)
	}
	want, err := copilotInstallPayloadJSON(stdio)
	if err != nil {
		t.Fatalf("copilotInstallPayloadJSON: %v", err)
	}
	decoded, err := url.PathUnescape(strings.TrimPrefix(uri, scheme))
	if err != nil {
		t.Fatalf("the emitted link does not percent-decode: %v", err)
	}
	if decoded != string(want) {
		t.Errorf("decoded payload\n  %s\nwant\n  %s", decoded, want)
	}
	// Nothing in the link may survive un-escaped that a query parser would
	// eat: the JSON delimiters must all be percent-encoded.
	for _, raw := range []string{"{", "}", `"`, ":", ",", "[", "]"} {
		if strings.Contains(strings.TrimPrefix(uri, scheme), raw) {
			t.Errorf("the encoded payload still contains a raw %q", raw)
		}
	}

	var payload copilotInstallPayload
	if err := json.Unmarshal([]byte(decoded), &payload); err != nil {
		t.Fatalf("the decoded payload is not valid JSON: %v", err)
	}
	if payload.Name != mcp.ServerName {
		t.Errorf("name = %q, want %q", payload.Name, mcp.ServerName)
	}
	if payload.Type != "stdio" {
		t.Errorf("type = %q, want \"stdio\"", payload.Type)
	}
	if payload.Command != pathBinary || strings.Join(payload.Args, " ") != strings.Join(stdio.Args, " ") {
		t.Errorf("payload launch %s %v != the registrar's %s", payload.Command, payload.Args, commandLine(stdio))
	}

	// The committed link file and the README must carry the same link.
	files := generateOnce(t)
	fromFile := strings.TrimSpace(string(fileByPath(t, files, copilotSurfaceDir+"/install-uri.txt")))
	if fromFile != uri {
		t.Errorf("install-uri.txt carries\n  %s\nbut the generator built\n  %s", fromFile, uri)
	}
	if !strings.Contains(string(fileByPath(t, files, copilotSurfaceDir+"/README.md")), uri) {
		t.Error("the copilot README does not carry the install link")
	}
}

// TestEncodeURIComponentMatchesJavaScript pins the encoder against
// JavaScript's own behaviour, which is what the VS Code recipe specifies.
// Go's url.QueryEscape differs on every row below, which is exactly why
// this function exists.
func TestEncodeURIComponentMatchesJavaScript(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`{"a":1}`, "%7B%22a%22%3A1%7D"},
		{"a b", "a%20b"},   // QueryEscape would write "a+b"
		{"!'()*", "!'()*"}, // QueryEscape would percent-encode all five
		{"-_.~", "-_.~"},   // unreserved, both agree
		{"/", "%2F"},       //
		{"é", "%C3%A9"},    // UTF-8 bytes, uppercase hex
	} {
		if got := encodeURIComponent(tc.in); got != tc.want {
			t.Errorf("encodeURIComponent(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestCoworkManifestShape pins the .mcpb manifest against the spec in
// anthropics/mcpb — including the field-name CORRECTION the research note
// got wrong (`manifest_version`, not `mcpb_version`).
func TestCoworkManifestShape(t *testing.T) {
	files := generateOnce(t)
	const rel = coworkSurfaceDir + "/" + pluginDir + "/manifest.json"
	raw := fileByPath(t, files, rel)

	keys := topLevelKeys(t, rel, raw)
	assertOnlyKeys(t, rel, keys,
		"manifest_version", "name", "display_name", "version", "description",
		"long_description", "author", "homepage", "documentation", "license",
		"keywords", "server", "compatibility")
	if _, wrong := keys["mcpb_version"]; wrong {
		t.Error("manifest carries `mcpb_version` — the current spec in anthropics/mcpb spells it `manifest_version`")
	}
	for _, required := range []string{"manifest_version", "name", "version", "description", "author", "server"} {
		if _, ok := keys[required]; !ok {
			t.Errorf("%s omits the REQUIRED field %q", rel, required)
		}
	}

	var m coworkManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m.ManifestVersion != "0.3" {
		t.Errorf("manifest_version = %q, want \"0.3\"", m.ManifestVersion)
	}
	if m.Name != pluginName {
		t.Errorf("name = %q, want %q", m.Name, pluginName)
	}
	if want := committedVersion(t); m.Version != want {
		t.Errorf("version = %q, want %q", m.Version, want)
	}
	if m.Author.Name == "" {
		t.Error("author.name is required")
	}
	if m.Server.Type != "binary" {
		t.Errorf("server.type = %q, want \"binary\"", m.Server.Type)
	}
	if m.Server.MCPConfig.Command != pathBinary {
		t.Errorf("server.mcp_config.command = %q, want the PATH binary %q", m.Server.MCPConfig.Command, pathBinary)
	}
	if m.Server.EntryPoint != pathBinary {
		t.Errorf("server.entry_point = %q, want %q (this bundle ships no packaged server)", m.Server.EntryPoint, pathBinary)
	}
	assertListingDescription(t, rel, m.Description)

	// The LIVE-VERIFY gate (plan §7): the README may not claim Cowork
	// coverage, and must say plainly that nothing here has been verified.
	body := string(fileByPath(t, files, coworkSurfaceDir+"/README.md"))
	for _, needle := range []string{"UNVERIFIED", "inference", "never been installed"} {
		if !strings.Contains(body, needle) {
			t.Errorf("the cowork README must state its unverified status; missing %q", needle)
		}
	}
}

// TestPiAndCommandCodeShipNoPackage pins the two wave-C decisions: each is
// a README and nothing else, and each README carries its evidence rather
// than an apology.
func TestPiAndCommandCodeShipNoPackage(t *testing.T) {
	files := generateOnce(t)
	for _, dir := range []string{piSurfaceDir, commandCodeSurfaceDir} {
		var got []string
		for _, f := range files {
			if strings.HasPrefix(f.Path, dir+"/") {
				got = append(got, f.Path)
			}
		}
		if len(got) != 1 || got[0] != dir+"/README.md" {
			t.Errorf("%s must ship a README and nothing else, got %v", dir, got)
		}
	}

	pi := string(fileByPath(t, files, piSurfaceDir+"/README.md"))
	for _, needle := range []string{"No MCP", "observer hook pi", "already complete"} {
		if !strings.Contains(pi, needle) {
			t.Errorf("the pi README must give its evidence; missing %q", needle)
		}
	}

	cc := string(fileByPath(t, files, commandCodeSurfaceDir+"/README.md"))
	for _, needle := range []string{"Why there is no Mod", "~/.commandcode/mcp.json", "observer init"} {
		if !strings.Contains(cc, needle) {
			t.Errorf("the command-code README must give its evidence; missing %q", needle)
		}
	}
}

// TestCommandCodeListingMatchesItsRegistrar pins that the command-code
// block is the entry the REAL registrar wrote, not a canonicalStdio copy.
func TestCommandCodeListingMatchesItsRegistrar(t *testing.T) {
	w, err := deriveWiring()
	if err != nil {
		t.Fatalf("deriveWiring: %v", err)
	}
	if w.commandCodeMCP.Command == "" {
		t.Fatal("the command-code registrar wrote no entry")
	}
	raw, err := renderCommandCodeMCPJSON(w.commandCodeMCP)
	if err != nil {
		t.Fatalf("renderCommandCodeMCPJSON: %v", err)
	}
	assertOnlyKeys(t, "commandcode mcp.json", topLevelKeys(t, "commandcode mcp.json", raw), "mcpServers")
	assertStdioEntry(t, "commandcode mcp.json", raw, w.commandCodeMCP)
}

// TestWaveBAndCSurfacesDeclareNoHooks extends the wave-A rule to every new
// surface: no hook file, no `hooks` key, because `observer hook` still
// accepts four tools and none of these is one of them.
func TestWaveBAndCSurfacesDeclareNoHooks(t *testing.T) {
	dirs := append(waveBSurfaceDirs(), waveCSurfaceDirs()...)
	for _, f := range generateOnce(t) {
		inWave := false
		for _, dir := range dirs {
			if strings.HasPrefix(f.Path, dir+"/") {
				inWave = true
				break
			}
		}
		if !inWave || !strings.HasSuffix(f.Path, ".json") {
			continue
		}
		if strings.Contains(f.Path, "hooks") {
			t.Errorf("%s is a hooks manifest, but observer has no hook receiver for that tool", f.Path)
		}
		if _, ok := topLevelKeys(t, f.Path, f.Data)["hooks"]; ok {
			t.Errorf("%s declares a `hooks` key, but observer has no hook receiver for that tool", f.Path)
		}
	}
}

// TestRootPlacementsDoNotCollide guards the public-repo transpose that
// scripts/assemble-plugins-repo.sh performs: every artifact that has to
// sit at the REPOSITORY ROOT must have a distinct name, and no plugin
// directory may nest inside another. Droid's directory is the reason this
// test exists — a bare `<pluginDir>` source would have landed Droid's
// plugin in the Claude Code plugin's directory, handing Droid a
// hooks/hooks.json built for another tool.
func TestRootPlacementsDoNotCollide(t *testing.T) {
	// Directory-shaped root entries, as the assembler places them.
	dirs := map[string]string{
		"claude-code": pluginDir,
		"codex":       codexPluginPath,
		"droid":       droidPluginDir,
		// Qoder's plugin stays in a subdirectory, but its catalog names
		// that subdirectory from the ROOT — so if the source were ever
		// "simplified" to "./"+pluginDir it would resolve the Claude Code
		// plugin. Checked here with the others rather than trusted.
		"qoder": qoderPluginPath,
	}
	for aName, a := range dirs {
		for bName, b := range dirs {
			if aName >= bName {
				continue
			}
			if a == b {
				t.Errorf("%s and %s both place a plugin at %q — two vendors' component files would merge into one directory", aName, bName, a)
				continue
			}
			if strings.HasPrefix(a+"/", b+"/") || strings.HasPrefix(b+"/", a+"/") {
				t.Errorf("%s (%q) and %s (%q) nest — a vendor scanning its own plugin root would see the other's files", aName, a, bName, b)
			}
		}
	}

	// File-shaped root entries.
	rootFiles := []string{
		".claude-plugin/marketplace.json",
		".agents/plugins/marketplace.json",
		".factory-plugin/marketplace.json",
		".qoder-plugin/marketplace.json",
		"gemini-extension.json",
		"kimi.plugin.json",
		"openclaw.plugin.json",
		".devin-plugin/plugin.json",
		"mcp_config.json",
		"README.md",
		"LICENSE",
		".gitignore",
	}
	seen := map[string]bool{}
	for _, f := range rootFiles {
		if seen[f] {
			t.Errorf("two surfaces claim the repository-root path %q", f)
		}
		seen[f] = true
	}
}
