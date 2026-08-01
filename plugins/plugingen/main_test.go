package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/mcp"
)

// repoRoot returns the repository root (two levels up from plugins/plugingen).
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("abs repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s has no go.mod: %v", root, err)
	}
	return root
}

func committedVersion(t *testing.T) string {
	t.Helper()
	v, err := readVersion(filepath.Join(repoRoot(t), "npm", "observer", "package.json"))
	if err != nil {
		t.Fatalf("readVersion: %v", err)
	}
	return v
}

func generateOnce(t *testing.T) []outFile {
	t.Helper()
	files, err := generate(committedVersion(t))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("generate returned no files")
	}
	return files
}

// TestGeneratedTreeMatchesCommitted is the golden-file gate: a fresh run of
// the generator must reproduce every committed file under plugins/ byte for
// byte. This is the same contract `make verify-plugins-build` enforces in
// CI, asserted here so `go test ./plugins/...` alone catches the drift.
func TestGeneratedTreeMatchesCommitted(t *testing.T) {
	root := repoRoot(t)
	for _, f := range generateOnce(t) {
		path := filepath.Join(root, "plugins", filepath.FromSlash(f.Path))
		got, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("committed file missing: plugins/%s (%v) — run `make plugins-build`", f.Path, err)
			continue
		}
		if !bytes.Equal(got, f.Data) {
			t.Errorf("plugins/%s drifted from a fresh generator run — run `make plugins-build` and commit", f.Path)
		}
	}
}

// TestCommittedTreeHasNoStrayFiles catches the other drift direction: a
// generated file that was renamed or dropped but left behind on disk. The
// build-into-temp gate cannot see deletions, so it is asserted here.
func TestCommittedTreeHasNoStrayFiles(t *testing.T) {
	root := repoRoot(t)
	want := map[string]bool{}
	for _, f := range generateOnce(t) {
		want[f.Path] = true
	}
	// The OpenCode package's SDK glue is hand-written by design (see
	// plugins/plugingen/opencode.go): plugingen owns the wiring constants
	// and the README, not the npm plumbing. Listing them here means adding
	// another hand-written file is a deliberate edit, not a silent gap.
	for _, rel := range openCodeHandWrittenFiles() {
		want[rel] = true
		if _, err := os.Stat(filepath.Join(root, "plugins", filepath.FromSlash(rel))); err != nil {
			t.Errorf("declared hand-written file plugins/%s is missing: %v", rel, err)
		}
	}
	err := filepath.WalkDir(filepath.Join(root, "plugins"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// plugingen is the only hand-written directory in the tree.
			if d.Name() == "plugingen" {
				return filepath.SkipDir
			}
			// Local build/install artefacts of the OpenCode package are
			// never committed; skip them so a developer's `npm install`
			// does not fail this test.
			if d.Name() == "node_modules" || d.Name() == "dist" {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == "package-lock.json" {
			return nil
		}
		rel, relErr := filepath.Rel(filepath.Join(root, "plugins"), path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if !want[rel] {
			t.Errorf("plugins/%s is not produced by plugingen — delete it or teach the generator about it", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk plugins/: %v", err)
	}
}

// versionBearingInventory is the full set of paths under plugins/ that
// carry the release version: the generated manifests, derived from the
// tree itself, plus the declared hand-written ones.
func versionBearingInventory(t *testing.T) []string {
	t.Helper()
	paths := versionBearingPaths(generateOnce(t), committedVersion(t))
	paths = append(paths, handWrittenVersionBearingPaths()...)
	sort.Strings(paths)
	return paths
}

// TestVersionBearingInventoryIsComplete checks the derivation against the
// disk: every file under plugins/ that carries the release version must be
// either a generated artifact the walker found or a declared hand-written
// one. It is the guard that keeps handWrittenVersionBearingPaths honest —
// generate() never reads the repository, so that list cannot be derived.
func TestVersionBearingInventoryIsComplete(t *testing.T) {
	root := filepath.Join(repoRoot(t), "plugins")
	version := committedVersion(t)
	declared := map[string]bool{}
	for _, p := range versionBearingInventory(t) {
		declared[p] = true
	}

	for _, rel := range handWrittenVersionBearingPaths() {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("declared hand-written version-bearing file plugins/%s is missing: %v", rel, err)
			continue
		}
		var doc any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Errorf("plugins/%s is not valid JSON: %v", rel, err)
			continue
		}
		if !jsonCarriesVersion(doc, version) {
			t.Errorf("plugins/%s is declared version-bearing but carries no %q version field", rel, version)
		}
	}

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "plugingen" || d.Name() == "node_modules" || d.Name() == "dist" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".json") || d.Name() == "package-lock.json" {
			return nil
		}
		raw, readErr := os.ReadFile(path) //nolint:gosec // G304: path comes from a walk of the repo's own plugins/ tree.
		if readErr != nil {
			return readErr
		}
		var doc any
		if json.Unmarshal(raw, &doc) != nil {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if jsonCarriesVersion(doc, version) && !declared[rel] {
			t.Errorf("plugins/%s carries the release version but is in neither the generated nor the hand-written inventory — the version-stamping prose and the release stamper would both miss it", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk plugins/: %v", err)
	}
}

// TestSyncNpmVersionStampsEveryVersionBearingManifest pins the release
// stamper's hard-coded path list against the generator's own inventory.
//
// scripts/sync-npm-version.sh enumerates the plugin manifests it stamps as
// literal paths, because the release job that runs it has Node but no Go
// toolchain and so cannot ask the generator. Nothing connected the two
// lists, and they drifted: five coverage-wave manifests were generated for
// releases the stamper had never heard of. This test is that connection —
// it reads the script, extracts the plugins/… paths it names, and requires
// SET EQUALITY in both directions.
//
// The parse is deliberately tolerant rather than a shell parser: comment
// lines are stripped (they discuss paths the script does NOT stamp — the
// Antigravity manifest, the Droid catalog — and would otherwise show up as
// phantom entries), and every plugins/….json path in what remains is taken
// as stamped. Both the assignment lines and their "stamped …" echoes name
// the same paths, so the set is unchanged by counting either.
func TestSyncNpmVersionStampsEveryVersionBearingManifest(t *testing.T) {
	scriptPath := filepath.Join(repoRoot(t), "scripts", "sync-npm-version.sh")
	raw, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read %s: %v", scriptPath, err)
	}

	var code strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		code.WriteString(line)
		code.WriteString("\n")
	}

	pathRe := regexp.MustCompile(`plugins/[A-Za-z0-9._/-]+\.json`)
	stamped := map[string]bool{}
	for _, m := range pathRe.FindAllString(code.String(), -1) {
		stamped[strings.TrimPrefix(m, "plugins/")] = true
	}
	if len(stamped) == 0 {
		t.Fatalf("%s names no plugins/ manifest at all — the parse broke, not the script", scriptPath)
	}

	want := map[string]bool{}
	for _, p := range versionBearingInventory(t) {
		want[p] = true
	}

	for p := range want {
		if !stamped[p] {
			t.Errorf("plugins/%s carries the release version but scripts/sync-npm-version.sh does not stamp it — a release would leave it on the previous number", p)
		}
	}
	for p := range stamped {
		if !want[p] {
			t.Errorf("scripts/sync-npm-version.sh stamps plugins/%s, which carries no release version any more — drop it from the script", p)
		}
	}
}

// TestVersionStampingProseIsDerived pins that the index README's version
// list is rendered FROM the inventory rather than typed: every path in the
// inventory appears in the generated prose. The prose claimed nine files
// and a four-manifest stamper while the truth was ten and eleven.
func TestVersionStampingProseIsDerived(t *testing.T) {
	body := string(fileByPath(t, generateOnce(t), "README.md"))
	for _, p := range versionBearingInventory(t) {
		if !strings.Contains(body, "`"+p+"`") {
			t.Errorf("the generated README does not list the version-bearing file %q", p)
		}
	}
	if strings.Contains(body, "only the FIRST FOUR") {
		t.Error("the README still carries the retracted \"stamps only the FIRST FOUR\" claim")
	}
}

// TestGenerateIsDeterministic proves the drift gate cannot flap: two runs
// in the same process must be byte-identical. Map iteration order, time and
// the throwaway sandbox HOME path are the three things that could leak.
func TestGenerateIsDeterministic(t *testing.T) {
	version := committedVersion(t)
	first, err := generate(version)
	if err != nil {
		t.Fatalf("generate (1): %v", err)
	}
	for run := 2; run <= 4; run++ {
		next, err := generate(version)
		if err != nil {
			t.Fatalf("generate (%d): %v", run, err)
		}
		if len(next) != len(first) {
			t.Fatalf("run %d produced %d files, run 1 produced %d", run, len(next), len(first))
		}
		for i := range first {
			if next[i].Path != first[i].Path {
				t.Fatalf("run %d file %d is %s, run 1 had %s", run, i, next[i].Path, first[i].Path)
			}
			if !bytes.Equal(next[i].Data, first[i].Data) {
				t.Errorf("run %d differs from run 1 for %s", run, first[i].Path)
			}
		}
	}
}

// TestNoAbsolutePathsLeak guards the sandbox: the temp HOME the registrars
// wrote into must never appear in a published manifest, and neither may a
// developer's home directory.
func TestNoAbsolutePathsLeak(t *testing.T) {
	needles := []string{os.TempDir(), "/tmp/", "/home/", "C:\\Users"}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		needles = append(needles, home)
	}
	for _, f := range generateOnce(t) {
		for _, needle := range needles {
			if needle == "" {
				continue
			}
			if bytes.Contains(f.Data, []byte(needle)) {
				t.Errorf("%s contains an absolute path fragment %q", f.Path, needle)
			}
		}
	}
}

// TestTrailingNewlineDiscipline pins the newline rule the byte-for-byte
// gate depends on: exactly one trailing newline, no CRLF.
func TestTrailingNewlineDiscipline(t *testing.T) {
	for _, f := range generateOnce(t) {
		if len(f.Data) == 0 {
			t.Errorf("%s is empty", f.Path)
			continue
		}
		if f.Data[len(f.Data)-1] != '\n' {
			t.Errorf("%s does not end with a newline", f.Path)
		}
		if bytes.HasSuffix(f.Data, []byte("\n\n")) {
			t.Errorf("%s ends with more than one newline", f.Path)
		}
		if bytes.Contains(f.Data, []byte("\r")) {
			t.Errorf("%s contains a CR", f.Path)
		}
	}
}

// fileByPath returns the generated artifact at rel, failing if absent.
func fileByPath(t *testing.T, files []outFile, rel string) []byte {
	t.Helper()
	for _, f := range files {
		if f.Path == rel {
			return f.Data
		}
	}
	t.Fatalf("generator produced no %s", rel)
	return nil
}

// TestPluginManifestShape asserts the grounded Claude Code plugin manifest
// contract: `name` is required and kebab-case, the version tracks the npm
// package, and the description states the binary prerequisite (the plan's
// honesty rule).
func TestPluginManifestShape(t *testing.T) {
	files := generateOnce(t)
	raw := fileByPath(t, files, "claude-code/"+pluginDir+"/.claude-plugin/plugin.json")

	var m pluginManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("plugin.json is not valid JSON: %v", err)
	}
	if m.Name != pluginName {
		t.Errorf("name = %q, want %q", m.Name, pluginName)
	}
	if strings.ToLower(m.Name) != m.Name || strings.ContainsAny(m.Name, " _") {
		t.Errorf("name %q is not kebab-case", m.Name)
	}
	if want := committedVersion(t); m.Version != want {
		t.Errorf("version = %q, want npm/observer/package.json's %q", m.Version, want)
	}
	if m.Author.Name == "" {
		t.Error("author.name is required by the manifest schema")
	}
	if m.License != "Apache-2.0" {
		t.Errorf("license = %q, want the repo's Apache-2.0", m.License)
	}
	// The canonical sentence, verbatim — the same one every README and the
	// npm listing carry (binaryPrereqSentence).
	for _, needle := range []string{binaryPrereqSentence, "installs no binary"} {
		if !strings.Contains(m.Description, needle) {
			t.Errorf("description must state the binary prerequisite; missing %q", needle)
		}
	}
}

// TestMCPManifestMatchesRegistrar is the one-owner assertion for the MCP
// half: the plugin's .mcp.json entry must be exactly what internal/mcp's
// Registrar writes, modulo the PATH-vs-absolute binary deviation.
func TestMCPManifestMatchesRegistrar(t *testing.T) {
	files := generateOnce(t)
	raw := fileByPath(t, files, "claude-code/"+pluginDir+"/.mcp.json")

	var doc struct {
		MCPServers map[string]mcpServer `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf(".mcp.json is not valid JSON: %v", err)
	}
	entry, ok := doc.MCPServers[mcp.ServerName]
	if !ok {
		t.Fatalf(".mcp.json has no %q server (keys: %v)", mcp.ServerName, doc.MCPServers)
	}

	w, err := deriveWiring()
	if err != nil {
		t.Fatalf("deriveWiring: %v", err)
	}
	if entry.Command != w.claudeCodeMCP.Command {
		t.Errorf("command = %q, registrar wrote %q", entry.Command, w.claudeCodeMCP.Command)
	}
	if strings.Join(entry.Args, " ") != strings.Join(w.claudeCodeMCP.Args, " ") {
		t.Errorf("args = %v, registrar wrote %v", entry.Args, w.claudeCodeMCP.Args)
	}
	if entry.Command != pathBinary {
		t.Errorf("a published plugin must resolve the binary from PATH; command = %q", entry.Command)
	}
}

// TestHooksManifestMatchesRegistrar is the one-owner assertion for the hook
// half: every event internal/hook registers into ~/.claude/settings.json
// appears in the plugin's hooks/hooks.json with the same command, and the
// file is wrapped in the documented outer "hooks" key.
func TestHooksManifestMatchesRegistrar(t *testing.T) {
	files := generateOnce(t)
	raw := fileByPath(t, files, "claude-code/"+pluginDir+"/hooks/hooks.json")

	type hookCmd struct {
		Type    string `json:"type"`
		Command string `json:"command"`
	}
	type hookGroup struct {
		Matcher string    `json:"matcher"`
		Hooks   []hookCmd `json:"hooks"`
	}
	var doc struct {
		Hooks map[string][]hookGroup `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("hooks.json is not valid JSON: %v", err)
	}
	if len(doc.Hooks) == 0 {
		t.Fatal("hooks.json has an empty outer \"hooks\" object")
	}

	w, err := deriveWiring()
	if err != nil {
		t.Fatalf("deriveWiring: %v", err)
	}
	if len(doc.Hooks) != len(w.claudeCodeHooks) {
		t.Errorf("hooks.json declares %d events, the registrar registers %d",
			len(doc.Hooks), len(w.claudeCodeHooks))
	}
	for event := range w.claudeCodeHooks {
		groups, ok := doc.Hooks[event]
		if !ok {
			t.Errorf("registrar event %q is missing from hooks.json", event)
			continue
		}
		if len(groups) != 1 || len(groups[0].Hooks) != 1 {
			t.Errorf("event %q: want exactly one group with one command, got %+v", event, groups)
			continue
		}
		cmd := groups[0].Hooks[0]
		if cmd.Type != "command" {
			t.Errorf("event %q: type = %q, want \"command\"", event, cmd.Type)
		}
		if !strings.HasPrefix(cmd.Command, pathBinary+" hook claude-code ") {
			t.Errorf("event %q: command %q is not a PATH-resolved observer hook", event, cmd.Command)
		}
	}
}

// TestMarketplaceSourceResolves pins the relative-source contract: the
// entry's source must start with "./", must never use "..", and must point
// at a directory that actually holds the plugin manifest. Relative sources
// resolve against the marketplace ROOT (the dir containing .claude-plugin/),
// which is why the plugin lives beneath it.
func TestMarketplaceSourceResolves(t *testing.T) {
	files := generateOnce(t)
	raw := fileByPath(t, files, "claude-code/.claude-plugin/marketplace.json")

	var m struct {
		Name    string `json:"name"`
		Owner   author `json:"owner"`
		Plugins []struct {
			Name    string `json:"name"`
			Source  string `json:"source"`
			Version string `json:"version"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("marketplace.json is not valid JSON: %v", err)
	}
	if m.Name != marketplaceName {
		t.Errorf("marketplace name = %q, want %q", m.Name, marketplaceName)
	}
	// Anthropic reserves these names for official marketplaces.
	for _, reserved := range []string{
		"claude-code-marketplace", "claude-code-plugins", "claude-plugins-official",
		"claude-plugins-community", "claude-community", "anthropic-marketplace",
		"anthropic-plugins", "agent-skills", "anthropic-agent-skills",
		"knowledge-work-plugins", "life-sciences", "claude-for-legal",
		"claude-for-financial-services", "financial-services-plugins",
		"first-party-plugins", "healthcare",
	} {
		if m.Name == reserved {
			t.Errorf("marketplace name %q is reserved for official Anthropic use", m.Name)
		}
	}
	if m.Owner.Name == "" {
		t.Error("owner.name is a required marketplace field")
	}
	if len(m.Plugins) != 1 {
		t.Fatalf("want exactly one catalog entry, got %d", len(m.Plugins))
	}
	e := m.Plugins[0]
	if e.Name != pluginName {
		t.Errorf("entry name = %q, want %q", e.Name, pluginName)
	}
	if !strings.HasPrefix(e.Source, "./") {
		t.Errorf("relative source %q must start with \"./\"", e.Source)
	}
	if strings.Contains(e.Source, "..") {
		t.Errorf("source %q escapes the marketplace root; \"..\" is forbidden", e.Source)
	}
	// The source resolves against the marketplace root — the directory
	// holding .claude-plugin/ — which in this tree is plugins/claude-code.
	manifest := filepath.Join(repoRoot(t), "plugins", "claude-code",
		filepath.FromSlash(strings.TrimPrefix(e.Source, "./")), ".claude-plugin", "plugin.json")
	if _, err := os.Stat(manifest); err != nil {
		t.Errorf("source %q does not resolve to a plugin manifest at %s: %v", e.Source, manifest, err)
	}
	if want := committedVersion(t); e.Version != want {
		t.Errorf("entry version = %q, want %q", e.Version, want)
	}
}

// TestCursorDeeplink pins the deeplink contract: exact scheme + handler,
// the `name` parameter matching the stable MCP server id, and a `config`
// parameter that base64-decodes to precisely the server object the Cursor
// registrar writes. Any drift here is a silently-wrong one-click install.
func TestCursorDeeplink(t *testing.T) {
	files := generateOnce(t)
	raw := fileByPath(t, files, "cursor/deeplink.txt")

	link := strings.TrimSuffix(string(raw), "\n")
	if strings.Contains(link, "\n") {
		t.Fatal("deeplink.txt must be a single line")
	}
	const prefix = "cursor://anysphere.cursor-deeplink/mcp/install?"
	if !strings.HasPrefix(link, prefix) {
		t.Fatalf("deeplink %q does not start with %q", link, prefix)
	}
	q, err := url.ParseQuery(strings.TrimPrefix(link, prefix))
	if err != nil {
		t.Fatalf("deeplink query does not parse: %v", err)
	}
	if got := q.Get("name"); got != mcp.ServerName {
		t.Errorf("name = %q, want %q", got, mcp.ServerName)
	}
	decoded, err := base64.StdEncoding.DecodeString(q.Get("config"))
	if err != nil {
		t.Fatalf("config is not standard padded base64: %v", err)
	}

	w, err := deriveWiring()
	if err != nil {
		t.Fatalf("deriveWiring: %v", err)
	}
	want, err := json.Marshal(w.cursorMCP)
	if err != nil {
		t.Fatalf("marshal registrar entry: %v", err)
	}
	if !bytes.Equal(decoded, want) {
		t.Errorf("config decodes to %s, the cursor registrar writes %s", decoded, want)
	}
}

// TestReadVersionRejectsGarbage covers the version-source seam.
func TestReadVersionRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		body string
	}{
		{"not json", "nope"},
		{"no version key", `{"name":"x"}`},
		{"empty version", `{"version":""}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "-")+".json")
			if err := os.WriteFile(p, []byte(tc.body), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			if _, err := readVersion(p); err == nil {
				t.Error("want an error, got nil")
			}
		})
	}
	if _, err := readVersion(filepath.Join(dir, "absent.json")); err == nil {
		t.Error("missing file: want an error, got nil")
	}
}

// TestRunWritesTree exercises the writer end of the generator against a
// throwaway output dir, so `-out` (the drift gate's entry point) is covered.
func TestRunWritesTree(t *testing.T) {
	out := t.TempDir()
	versionFile := filepath.Join(repoRoot(t), "npm", "observer", "package.json")
	if err := run(out, versionFile); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, f := range generateOnce(t) {
		got, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(f.Path)))
		if err != nil {
			t.Errorf("run did not write %s: %v", f.Path, err)
			continue
		}
		if !bytes.Equal(got, f.Data) {
			t.Errorf("run wrote different bytes for %s", f.Path)
		}
	}
}
