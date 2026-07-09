package proxyroute

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func newRegistrar(t *testing.T, port int, force bool) (*Registrar, string) {
	t.Helper()
	home := t.TempDir()
	r, err := NewRegistrar(RegisterOptions{
		ProxyPort: port,
		HomeDir:   home,
		Force:     force,
	})
	if err != nil {
		t.Fatalf("NewRegistrar: %v", err)
	}
	return r, home
}

func TestNewRegistrar_Validation(t *testing.T) {
	if _, err := NewRegistrar(RegisterOptions{ProxyPort: 0}); err == nil {
		t.Error("port 0 should error")
	}
	if _, err := NewRegistrar(RegisterOptions{ProxyPort: 70000}); err == nil {
		t.Error("port 70000 should error")
	}
}

// TestRegisterCodex_FreshFile creates ~/.codex/config.toml from scratch
// and verifies the custom provider + top-level model_provider are both
// written.
func TestRegisterCodex_FreshFile(t *testing.T) {
	r, home := newRegistrar(t, 8820, false)
	res := r.RegisterCodex()
	if res.Error != nil {
		t.Fatalf("RegisterCodex: %v", res.Error)
	}
	if !res.Added || res.AlreadySet {
		t.Errorf("expected Added=true AlreadySet=false: %+v", res)
	}
	if res.BaseURL != "http://127.0.0.1:8820/v1" {
		t.Errorf("BaseURL: %q", res.BaseURL)
	}

	body, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := toml.Unmarshal(body, &root); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if root["model_provider"] != ProviderName {
		t.Errorf("top-level model_provider: %v want %q", root["model_provider"], ProviderName)
	}
	ours := root["model_providers"].(map[string]any)[ProviderName].(map[string]any)
	if ours["base_url"] != "http://127.0.0.1:8820/v1" {
		t.Errorf("base_url: %v", ours["base_url"])
	}
	if ours["wire_api"] != "responses" {
		t.Errorf("wire_api: %v", ours["wire_api"])
	}
	if ours["requires_openai_auth"] != true {
		t.Errorf("requires_openai_auth: %v", ours["requires_openai_auth"])
	}
	if _, hasReserved := root["model_providers"].(map[string]any)["openai"]; hasReserved {
		t.Errorf("fresh file should not write reserved [model_providers.openai]")
	}
}

// TestRegisterCodex_PreservesUnrelated round-trips a config with
// unrelated sections and confirms they survive.
func TestRegisterCodex_PreservesUnrelated(t *testing.T) {
	r, home := newRegistrar(t, 8920, false)
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.toml")
	prior := `model = "gpt-5.5"

[mcp_servers.observer]
command = "/usr/local/bin/observer"
args = ["serve"]
`
	if err := os.WriteFile(path, []byte(prior), 0o600); err != nil {
		t.Fatal(err)
	}

	res := r.RegisterCodex()
	if res.Error != nil {
		t.Fatalf("RegisterCodex: %v", res.Error)
	}
	if !res.Added {
		t.Errorf("expected Added: %+v", res)
	}

	body, _ := os.ReadFile(path)
	var root map[string]any
	if err := toml.Unmarshal(body, &root); err != nil {
		t.Fatalf("parse: %v: %s", err, body)
	}
	if root["model"] != "gpt-5.5" {
		t.Errorf("top-level model lost: %v", root["model"])
	}
	if servers, _ := root["mcp_servers"].(map[string]any); servers["observer"] == nil {
		t.Errorf("mcp_servers.observer lost")
	}
	if root["model_provider"] != ProviderName {
		t.Errorf("model_provider not set: %v", root["model_provider"])
	}
}

// TestRegisterCodex_RejectsReservedOpenAIBlock fails when the config
// contains [model_providers.openai] (which codex 0.128.0+ refuses to
// load). Force removes it.
func TestRegisterCodex_RejectsReservedOpenAIBlock(t *testing.T) {
	r, home := newRegistrar(t, 8820, false)
	dir := filepath.Join(home, ".codex")
	_ = os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, "config.toml")
	prior := `[model_providers.openai]
base_url = "http://127.0.0.1:8920/v1"
`
	_ = os.WriteFile(path, []byte(prior), 0o600)

	res := r.RegisterCodex()
	if res.Error == nil {
		t.Fatalf("expected error: %+v", res)
	}
	if !strings.Contains(res.Error.Error(), "reserved built-in") {
		t.Errorf("error should mention reserved built-in: %v", res.Error)
	}

	rf, _ := NewRegistrar(RegisterOptions{ProxyPort: 8820, HomeDir: home, Force: true})
	res2 := rf.RegisterCodex()
	if res2.Error != nil {
		t.Fatalf("force: %v", res2.Error)
	}
	body, _ := os.ReadFile(path)
	var root map[string]any
	_ = toml.Unmarshal(body, &root)
	providers := root["model_providers"].(map[string]any)
	if _, ok := providers["openai"]; ok {
		t.Errorf("force should remove reserved [model_providers.openai]")
	}
	if _, ok := providers[ProviderName]; !ok {
		t.Errorf("force should add custom provider")
	}
}

// TestRegisterCodex_IdempotentSameURL re-registration with the same
// port and an already-set top-level model_provider returns AlreadySet.
func TestRegisterCodex_IdempotentSameURL(t *testing.T) {
	r, _ := newRegistrar(t, 8820, false)
	if res := r.RegisterCodex(); res.Error != nil {
		t.Fatalf("first: %v", res.Error)
	}
	res := r.RegisterCodex()
	if res.Error != nil {
		t.Fatalf("second: %v", res.Error)
	}
	if !res.AlreadySet {
		t.Errorf("expected AlreadySet: %+v", res)
	}
	if res.Added {
		t.Errorf("re-register should not Add: %+v", res)
	}
}

// TestRegisterCodex_DifferentLoopbackPort treats an existing 127.0.0.1
// base_url on the custom provider but a different port as AlreadySet.
func TestRegisterCodex_DifferentLoopbackPort(t *testing.T) {
	r, home := newRegistrar(t, 8820, false)
	dir := filepath.Join(home, ".codex")
	_ = os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, "config.toml")
	prior := `model_provider = "openai-observer"

[model_providers.openai-observer]
base_url = "http://127.0.0.1:8920/v1"
`
	_ = os.WriteFile(path, []byte(prior), 0o600)

	res := r.RegisterCodex()
	if res.Error != nil {
		t.Fatalf("RegisterCodex: %v", res.Error)
	}
	if !res.AlreadySet {
		t.Errorf("expected AlreadySet for loopback collision: %+v", res)
	}
	if res.BaseURL != "http://127.0.0.1:8920/v1" {
		t.Errorf("BaseURL should reflect existing: %q", res.BaseURL)
	}

	body, _ := os.ReadFile(path)
	if !strings.Contains(string(body), `base_url = "http://127.0.0.1:8920/v1"`) {
		t.Errorf("file mutated: %s", body)
	}
}

// TestRegisterCodex_NonLoopbackRefuses errors when the custom provider
// already has a non-loopback base_url, unless --force.
func TestRegisterCodex_NonLoopbackRefuses(t *testing.T) {
	r, home := newRegistrar(t, 8820, false)
	dir := filepath.Join(home, ".codex")
	_ = os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, "config.toml")
	prior := `model_provider = "openai-observer"

[model_providers.openai-observer]
base_url = "https://my-llm-gateway.example.com/v1"
`
	_ = os.WriteFile(path, []byte(prior), 0o600)

	res := r.RegisterCodex()
	if res.Error == nil {
		t.Fatalf("expected error: %+v", res)
	}
	if !strings.Contains(res.Error.Error(), "already set") {
		t.Errorf("error message: %v", res.Error)
	}

	rf, _ := NewRegistrar(RegisterOptions{ProxyPort: 8820, HomeDir: home, Force: true})
	res2 := rf.RegisterCodex()
	if res2.Error != nil {
		t.Fatalf("force: %v", res2.Error)
	}
	if !res2.Added {
		t.Errorf("force should Add: %+v", res2)
	}
	body, _ := os.ReadFile(path)
	if !strings.Contains(string(body), "127.0.0.1:8820") {
		t.Errorf("force did not write: %s", body)
	}
}

// TestRegisterCodex_RefusesThirdPartyModelProvider preserves a top-level
// model_provider that points at something other than openai-observer
// unless --force.
func TestRegisterCodex_RefusesThirdPartyModelProvider(t *testing.T) {
	r, home := newRegistrar(t, 8820, false)
	dir := filepath.Join(home, ".codex")
	_ = os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, "config.toml")
	prior := `model_provider = "anthropic"
`
	_ = os.WriteFile(path, []byte(prior), 0o600)

	res := r.RegisterCodex()
	if res.Error == nil {
		t.Fatalf("expected error: %+v", res)
	}
	if !strings.Contains(res.Error.Error(), "anthropic") {
		t.Errorf("error should mention conflicting provider: %v", res.Error)
	}

	rf, _ := NewRegistrar(RegisterOptions{ProxyPort: 8820, HomeDir: home, Force: true})
	res2 := rf.RegisterCodex()
	if res2.Error != nil {
		t.Fatalf("force: %v", res2.Error)
	}
	body, _ := os.ReadFile(path)
	var root map[string]any
	_ = toml.Unmarshal(body, &root)
	if root["model_provider"] != ProviderName {
		t.Errorf("force should overwrite model_provider: %v", root["model_provider"])
	}
}

// TestRegisterCodex_ProviderRightModelProviderWrong handles the half-state
// where the custom provider block is correct but the top-level switch
// isn't pointing at it yet (e.g. user manually wrote the provider but
// forgot to flip model_provider). We complete the registration.
func TestRegisterCodex_ProviderRightModelProviderWrong(t *testing.T) {
	r, home := newRegistrar(t, 8820, false)
	dir := filepath.Join(home, ".codex")
	_ = os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, "config.toml")
	prior := `[model_providers.openai-observer]
base_url = "http://127.0.0.1:8820/v1"
`
	_ = os.WriteFile(path, []byte(prior), 0o600)

	res := r.RegisterCodex()
	if res.Error != nil {
		t.Fatalf("RegisterCodex: %v", res.Error)
	}
	if !res.Added {
		t.Errorf("should add (model_provider needed flipping): %+v", res)
	}
	body, _ := os.ReadFile(path)
	var root map[string]any
	_ = toml.Unmarshal(body, &root)
	if root["model_provider"] != ProviderName {
		t.Errorf("model_provider not flipped: %v", root["model_provider"])
	}
}

// TestRegisterCodex_DryRun computes the result without touching disk.
func TestRegisterCodex_DryRun(t *testing.T) {
	home := t.TempDir()
	r, _ := NewRegistrar(RegisterOptions{ProxyPort: 8820, HomeDir: home, DryRun: true})
	res := r.RegisterCodex()
	if res.Error != nil {
		t.Fatalf("RegisterCodex: %v", res.Error)
	}
	if !res.Added {
		t.Errorf("dry-run should report Added=true (intended action): %+v", res)
	}
	if !res.DryRun {
		t.Errorf("DryRun flag should propagate: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "config.toml")); !os.IsNotExist(err) {
		t.Errorf("dry-run wrote the file: err=%v", err)
	}
}

func TestIsObserverBaseURL(t *testing.T) {
	cases := map[string]bool{
		"http://127.0.0.1:8820/v1":   true,
		"http://localhost:8821/v1":   true,
		"http://[::1]:8822/v1":       true,
		"https://api.openai.com/v1":  false,
		"https://my-llm.example.com": false,
		"":                           false,
		"::not-a-url::":              false,
	}
	for url, want := range cases {
		if got := IsObserverBaseURL(url); got != want {
			t.Errorf("IsObserverBaseURL(%q) = %v want %v", url, got, want)
		}
	}
}

func TestCodexHint_ContainsExpectedKeys(t *testing.T) {
	h := CodexHint(8920)
	for _, want := range []string{
		"127.0.0.1:8920",
		"openai-observer",
		"model_providers.openai-observer",
		"requires_openai_auth = true",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("hint missing %q: %s", want, h)
		}
	}
}
