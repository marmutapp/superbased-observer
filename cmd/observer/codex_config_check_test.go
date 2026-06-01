package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// configCheckFixture builds a fresh tmp CODEX_HOME for a single
// branch-test of checkCodexConfigTOMLBaseURL. The fixture returns
// the codex_home path so the test can pass it as the single-element
// slice to the helper.
type configCheckFixture struct {
	t         *testing.T
	codexHome string
}

func newConfigCheckFixture(t *testing.T) *configCheckFixture {
	t.Helper()
	base := t.TempDir()
	codexHome := filepath.Join(base, "codex_home")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatal(err)
	}
	return &configCheckFixture{t: t, codexHome: codexHome}
}

func (f *configCheckFixture) writeConfigTOML(body string) {
	f.t.Helper()
	path := filepath.Join(f.codexHome, "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

const testProxyURL = "http://127.0.0.1:8820"

// TestCheckCodexConfigTOML_NoFileWarns: when config.toml is absent
// the operator hasn't set up routing at all. Warn with the
// create-config-toml instruction so they know what to add.
func TestCheckCodexConfigTOML_NoFileWarns(t *testing.T) {
	f := newConfigCheckFixture(t)
	warn := checkCodexConfigTOMLBaseURL([]string{f.codexHome}, testProxyURL)
	if !strings.Contains(warn, "does not exist") {
		t.Errorf("warn should name the missing file: %s", warn)
	}
	if !strings.Contains(warn, `openai_base_url = "http://127.0.0.1:8820/v1"`) {
		t.Errorf("warn should suggest the v1-suffixed URL: %s", warn)
	}
	if !strings.Contains(warn, "V6-2") {
		t.Errorf("warn should reference V6-2: %s", warn)
	}
}

// TestCheckCodexConfigTOML_MissingKeyWarns: file present but
// openai_base_url not set. Most common operator misconfig.
func TestCheckCodexConfigTOML_MissingKeyWarns(t *testing.T) {
	f := newConfigCheckFixture(t)
	f.writeConfigTOML(`model = "gpt-5-codex"` + "\n")
	warn := checkCodexConfigTOMLBaseURL([]string{f.codexHome}, testProxyURL)
	if !strings.Contains(warn, "no openai_base_url") {
		t.Errorf("warn should name the missing key: %s", warn)
	}
	if !strings.Contains(warn, `Add `+"`"+`openai_base_url = "http://127.0.0.1:8820/v1"`) {
		t.Errorf("warn should suggest adding the key: %s", warn)
	}
}

// TestCheckCodexConfigTOML_WrongURLWarns: file present and key set,
// but URL points elsewhere. Show both got and want.
func TestCheckCodexConfigTOML_WrongURLWarns(t *testing.T) {
	f := newConfigCheckFixture(t)
	f.writeConfigTOML(`openai_base_url = "https://api.openai.com/v1"` + "\n")
	warn := checkCodexConfigTOMLBaseURL([]string{f.codexHome}, testProxyURL)
	if !strings.Contains(warn, `openai_base_url="https://api.openai.com/v1"`) {
		t.Errorf("warn should show current value: %s", warn)
	}
	if !strings.Contains(warn, "http://127.0.0.1:8820/v1") {
		t.Errorf("warn should show expected value: %s", warn)
	}
}

// TestCheckCodexConfigTOML_CorrectURLSilent: happy path — file
// present, key matches, no warning. Pins the "zero stderr on a clean
// host" operator transparency contract from v1.7.4.
func TestCheckCodexConfigTOML_CorrectURLSilent(t *testing.T) {
	f := newConfigCheckFixture(t)
	f.writeConfigTOML(`openai_base_url = "http://127.0.0.1:8820/v1"` + "\n")
	if warn := checkCodexConfigTOMLBaseURL([]string{f.codexHome}, testProxyURL); warn != "" {
		t.Errorf("expected silent success, got warning: %s", warn)
	}
}

// TestCheckCodexConfigTOML_TrailingSlashTolerated: operator might
// write the URL with or without trailing slash, with or without /v1.
// All four shapes are accepted.
func TestCheckCodexConfigTOML_TrailingSlashTolerated(t *testing.T) {
	cases := []string{
		`openai_base_url = "http://127.0.0.1:8820"`,
		`openai_base_url = "http://127.0.0.1:8820/"`,
		`openai_base_url = "http://127.0.0.1:8820/v1"`,
		`openai_base_url = "http://127.0.0.1:8820/v1/"`,
	}
	for _, tc := range cases {
		t.Run(tc, func(t *testing.T) {
			f := newConfigCheckFixture(t)
			f.writeConfigTOML(tc + "\n")
			if warn := checkCodexConfigTOMLBaseURL([]string{f.codexHome}, testProxyURL); warn != "" {
				t.Errorf("expected silent, got: %s", warn)
			}
		})
	}
}

// TestCheckCodexConfigTOML_UnreadableFileSilentBestEffort: the file
// exists but TOML parsing fails. The check is diagnostic-only —
// must NEVER fail the wrapper or noise the operator with a non-
// actionable parser error. Silent is the right behavior.
func TestCheckCodexConfigTOML_UnreadableFileSilentBestEffort(t *testing.T) {
	f := newConfigCheckFixture(t)
	f.writeConfigTOML("this is not valid toml = [\nbroken")
	if warn := checkCodexConfigTOMLBaseURL([]string{f.codexHome}, testProxyURL); warn != "" {
		t.Errorf("expected silent best-effort, got: %s", warn)
	}
}

// TestCheckCodexConfigTOML_MultiRootIteration: when crossmount
// returns multiple CODEX_HOME roots, every misconfigured one
// surfaces a warning, joined by newlines. Mirrors the WSL2 case
// where /home/u/.codex AND /mnt/c/Users/u/.codex both exist.
func TestCheckCodexConfigTOML_MultiRootIteration(t *testing.T) {
	a := newConfigCheckFixture(t)
	b := newConfigCheckFixture(t)
	a.writeConfigTOML(`model = "gpt-5-codex"` + "\n")
	b.writeConfigTOML(`openai_base_url = "https://api.openai.com/v1"` + "\n")

	warn := checkCodexConfigTOMLBaseURL([]string{a.codexHome, b.codexHome}, testProxyURL)
	if !strings.Contains(warn, "no openai_base_url") {
		t.Errorf("missing-key warn for first root absent: %s", warn)
	}
	if !strings.Contains(warn, "https://api.openai.com/v1") {
		t.Errorf("wrong-URL warn for second root absent: %s", warn)
	}
	if strings.Count(warn, "\n") < 1 {
		t.Errorf("expected multi-line warning, got: %s", warn)
	}
}

// TestCheckCodexConfigTOML_EmptyRootsListSilent pins the trivial
// guard: zero roots → zero warnings, no panic.
func TestCheckCodexConfigTOML_EmptyRootsListSilent(t *testing.T) {
	if warn := checkCodexConfigTOMLBaseURL(nil, testProxyURL); warn != "" {
		t.Errorf("expected silent for empty roots, got: %s", warn)
	}
}
