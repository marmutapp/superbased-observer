package hermesplugin

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestWritePlugin_RoundTripDropsBothFiles pins the embed -> write -> read
// loop. After WritePlugin(tempDir), both plugin.yaml and __init__.py
// must exist with content matching the embed.FS copies.
func TestWritePlugin_RoundTripDropsBothFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := WritePlugin(dir, ""); err != nil {
		t.Fatalf("WritePlugin: %v", err)
	}

	wantFiles := []string{"__init__.py", "plugin.yaml"}
	got, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var gotNames []string
	for _, e := range got {
		gotNames = append(gotNames, e.Name())
	}
	sort.Strings(gotNames)
	if len(gotNames) != len(wantFiles) {
		t.Fatalf("ReadDir got %v, want %v", gotNames, wantFiles)
	}
	for i, w := range wantFiles {
		if gotNames[i] != w {
			t.Errorf("file %d: got %q, want %q", i, gotNames[i], w)
		}
	}

	// plugin.yaml must carry the manifest keys.
	yml, err := os.ReadFile(filepath.Join(dir, "plugin.yaml"))
	if err != nil {
		t.Fatalf("ReadFile yaml: %v", err)
	}
	for _, key := range []string{"name:", "version:", "description:"} {
		if !strings.Contains(string(yml), key) {
			t.Errorf("plugin.yaml missing %q", key)
		}
	}
	if !strings.Contains(string(yml), "superbased-observer") {
		t.Error("plugin.yaml doesn't mention superbased-observer as the plugin name")
	}

	// __init__.py must carry the canonical hook registrations and
	// the OBSERVER_BIN env-var read.
	py, err := os.ReadFile(filepath.Join(dir, "__init__.py"))
	if err != nil {
		t.Fatalf("ReadFile init: %v", err)
	}
	for _, marker := range []string{
		"OBSERVER_BIN",
		"TIMEOUT_SECONDS",
		"def register(ctx):",
		`ctx.register_hook("on_session_start"`,
		`ctx.register_hook("on_session_end"`,
		`ctx.register_hook("post_tool_call"`,
		`ctx.register_hook("post_api_request"`,
		`ctx.register_hook("subagent_stop"`,
		`"observer hook hermes`, // confirm subprocess command shape
	} {
		// The subprocess cmd is constructed via list args, not a
		// single string — adjust the marker check to use the list
		// form instead.
		if marker == `"observer hook hermes` {
			if !strings.Contains(string(py), `"hook", "hermes"`) {
				t.Errorf("__init__.py missing subprocess command marker %q", `"hook", "hermes"`)
			}
			continue
		}
		if !strings.Contains(string(py), marker) {
			t.Errorf("__init__.py missing %q", marker)
		}
	}
}

// TestWritePlugin_SubstitutesObserverBinDefault confirms that
// WritePlugin renders the resolved binary path into __init__.py's
// OBSERVER_BIN default — the V4 install-time wiring that lets the
// hook subprocess run without depending on PATH. Validation 2026-06-06
// caught the original bug on Windows where the npm shim on PATH was
// broken; the plugin would load but every hook subprocess silently
// no-op'd. This test pins the substitution end-to-end.
//
// Three scenarios:
//   - Linux-shaped absolute path lands verbatim with no escapes.
//   - Windows-shaped path with backslashes lands with each backslash
//     doubled (Python string-literal escape).
//   - Empty defaultObserverBin falls back to the bare "observer"
//     PATH-lookup default for backwards compatibility with unit tests.
//
// In every case, the placeholder marker `{{OBSERVER_BIN_DEFAULT}}`
// MUST NOT remain in the rendered file — leaking the marker would mean
// the plugin tries to spawn a binary literally named "{{...}}" at
// runtime and the subprocess silently no-op's exactly like V4 itself.
func TestWritePlugin_SubstitutesObserverBinDefault(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		defaultBin  string
		wantLiteral string // expected substring after substitution
	}{
		{
			name:        "linux_abs_path",
			defaultBin:  "/usr/local/bin/observer",
			wantLiteral: `"OBSERVER_BIN", "/usr/local/bin/observer"`,
		},
		{
			name:        "windows_abs_path_with_backslashes",
			defaultBin:  `C:\Program Files\superbased\observer.exe`,
			wantLiteral: `"OBSERVER_BIN", "C:\\Program Files\\superbased\\observer.exe"`,
		},
		{
			name:        "embedded_double_quote",
			defaultBin:  `/tmp/weird "name"/observer`,
			wantLiteral: `"OBSERVER_BIN", "/tmp/weird \"name\"/observer"`,
		},
		{
			name:        "empty_falls_back_to_observer",
			defaultBin:  "",
			wantLiteral: `"OBSERVER_BIN", "observer"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := WritePlugin(dir, tc.defaultBin); err != nil {
				t.Fatalf("WritePlugin: %v", err)
			}
			py, err := os.ReadFile(filepath.Join(dir, "__init__.py"))
			if err != nil {
				t.Fatalf("ReadFile init: %v", err)
			}
			if strings.Contains(string(py), "{{OBSERVER_BIN_DEFAULT}}") {
				t.Errorf("placeholder leaked into rendered __init__.py for case %q", tc.name)
			}
			if !strings.Contains(string(py), tc.wantLiteral) {
				t.Errorf("__init__.py missing rendered default %q", tc.wantLiteral)
			}
		})
	}
}

// TestWritePlugin_Idempotent confirms re-running against an existing
// install overwrites the files rather than appending or failing.
// Mirrors the `observer init --hermes` re-run case after an upgrade.
func TestWritePlugin_Idempotent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := WritePlugin(dir, ""); err != nil {
		t.Fatalf("first WritePlugin: %v", err)
	}
	// Trash plugin.yaml with arbitrary content; second WritePlugin
	// must restore it.
	bogus := filepath.Join(dir, "plugin.yaml")
	if err := os.WriteFile(bogus, []byte("stale content from a prior version"), 0o600); err != nil {
		t.Fatalf("trash plugin.yaml: %v", err)
	}
	if err := WritePlugin(dir, ""); err != nil {
		t.Fatalf("second WritePlugin: %v", err)
	}
	data, err := os.ReadFile(bogus)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(data), "stale content") {
		t.Error("WritePlugin didn't overwrite stale plugin.yaml")
	}
	if !strings.Contains(string(data), "superbased-observer") {
		t.Error("WritePlugin overwrote but the new content is missing the name marker")
	}
}

// TestFiles_ReturnsBothEmbedded pins the Files() helper that the install
// command uses to show the operator what got written.
func TestFiles_ReturnsBothEmbedded(t *testing.T) {
	t.Parallel()
	files := Files()
	if len(files) != 2 {
		t.Errorf("Files() len = %d, want 2", len(files))
	}
	sort.Strings(files)
	want := []string{"__init__.py", "plugin.yaml"}
	for i, w := range want {
		if i >= len(files) || files[i] != w {
			t.Errorf("Files()[%d] = %q, want %q", i, files[i], w)
		}
	}
}
