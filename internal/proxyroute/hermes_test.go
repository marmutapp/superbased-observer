package proxyroute

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func newHermesRegistrar(t *testing.T, home string) *Registrar {
	t.Helper()
	t.Setenv("HERMES_HOME", "") // force homeDir/.hermes resolution
	r, err := NewRegistrar(RegisterOptions{ProxyPort: 8820, HomeDir: home})
	if err != nil {
		t.Fatalf("NewRegistrar: %v", err)
	}
	return r
}

func writeHermesConfig(t *testing.T, home, body string) string {
	t.Helper()
	dir := filepath.Join(home, ".hermes")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDeriveHermesUpstream(t *testing.T) {
	cases := []struct {
		in             string
		id, root, tail string
		ok             bool
	}{
		{"https://openrouter.ai/api/v1", "openrouter", "https://openrouter.ai", "/api/v1", true},
		{"https://openrouter.ai/api/", "openrouter", "https://openrouter.ai", "/api", true},
		{"https://api.together.xyz/v1", "api", "https://api.together.xyz", "/v1", true},
		{"http://localhost:1234", "localhost", "http://localhost:1234", "", true},
		{"not a url", "", "", "", false},
		{"ftp://x", "x", "ftp://x", "", true}, // scheme+host present; caller gates on http(s) usage
	}
	for _, tc := range cases {
		id, root, tail, ok := deriveHermesUpstream(tc.in)
		if ok != tc.ok || id != tc.id || root != tc.root || tail != tc.tail {
			t.Errorf("deriveHermesUpstream(%q) = (%q,%q,%q,%v), want (%q,%q,%q,%v)",
				tc.in, id, root, tail, ok, tc.id, tc.root, tc.tail, tc.ok)
		}
	}
}

// providerKey is a deliberately non-secret-looking value so the test fixture
// isn't redacted; the point is the writer must leave it byte-identical.
const providerKey = "routed-key-xyz"

func TestRegisterHermesRewritesBaseURLAndBacksUp(t *testing.T) {
	home := t.TempDir()
	cfg := "model:\n" +
		"  name: deepseek\n" +
		"  base_url: https://openrouter.ai/api/v1\n" +
		"  api_key: " + providerKey + "\n" +
		"plugins:\n" +
		"  enabled: [superbased-observer]\n"
	path := writeHermesConfig(t, home, cfg)
	r := newHermesRegistrar(t, home)

	res := r.RegisterHermes()
	if res.Error != nil {
		t.Fatalf("RegisterHermes: %v", res.Error)
	}
	if !res.Added {
		t.Fatalf("expected Added")
	}
	if res.BaseURL != "http://127.0.0.1:8820/up/openrouter/api/v1" {
		t.Errorf("BaseURL = %q", res.BaseURL)
	}
	if res.UpstreamID != "openrouter" || res.UpstreamRoot != "https://openrouter.ai" {
		t.Errorf("upstream = %q / %q", res.UpstreamID, res.UpstreamRoot)
	}
	if res.PriorBaseURL != "https://openrouter.ai/api/v1" {
		t.Errorf("PriorBaseURL = %q", res.PriorBaseURL)
	}

	// .bak preserves the original.
	bak, err := os.ReadFile(path + ".bak")
	if err != nil || !strings.Contains(string(bak), "https://openrouter.ai/api/v1") {
		t.Errorf("backup missing or wrong: %v", err)
	}

	// The written config carries the new base_url and the UNTOUCHED key.
	var doc map[string]any
	out, _ := os.ReadFile(path)
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	model := doc["model"].(map[string]any)
	if model["base_url"] != "http://127.0.0.1:8820/up/openrouter/api/v1" {
		t.Errorf("base_url not rewritten: %v", model["base_url"])
	}
	if model["api_key"] != providerKey {
		t.Errorf("api_key clobbered: %v (writer must never touch keys)", model["api_key"])
	}
	if model["name"] != "deepseek" {
		t.Errorf("sibling model.name lost: %v", model["name"])
	}
	if _, ok := doc["plugins"]; !ok {
		t.Errorf("sibling top-level key plugins lost")
	}
}

func TestRegisterHermesAlreadyRoutedIsNoOp(t *testing.T) {
	home := t.TempDir()
	path := writeHermesConfig(t, home, "model:\n  base_url: http://127.0.0.1:8820/up/openrouter/api/v1\n")
	r := newHermesRegistrar(t, home)
	res := r.RegisterHermes()
	if res.Error != nil {
		t.Fatalf("err: %v", res.Error)
	}
	if !res.AlreadySet || res.Added {
		t.Errorf("expected AlreadySet no-op, got Added=%v AlreadySet=%v", res.Added, res.AlreadySet)
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Errorf("no-op must not write a .bak")
	}
}

func TestRegisterHermesNoBaseURLErrors(t *testing.T) {
	home := t.TempDir()
	writeHermesConfig(t, home, "model:\n  name: deepseek\n")
	r := newHermesRegistrar(t, home)
	res := r.RegisterHermes()
	if res.Error == nil {
		t.Fatal("expected error when model.base_url absent")
	}
}

func TestRegisterHermesMissingFileErrors(t *testing.T) {
	home := t.TempDir() // no .hermes/config.yaml
	r := newHermesRegistrar(t, home)
	res := r.RegisterHermes()
	if res.Error == nil {
		t.Fatal("expected error when config.yaml absent")
	}
}

func TestRegisterHermesDryRunDoesNotWrite(t *testing.T) {
	home := t.TempDir()
	path := writeHermesConfig(t, home, "model:\n  base_url: https://openrouter.ai/api/v1\n")
	t.Setenv("HERMES_HOME", "")
	r, _ := NewRegistrar(RegisterOptions{ProxyPort: 8820, HomeDir: home, DryRun: true})
	res := r.RegisterHermes()
	if res.Error != nil || !res.Added {
		t.Fatalf("dry run: err=%v added=%v", res.Error, res.Added)
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Errorf("dry run must not write a .bak")
	}
	out, _ := os.ReadFile(path)
	if !strings.Contains(string(out), "https://openrouter.ai/api/v1") {
		t.Errorf("dry run mutated the file")
	}
}

func TestHermesHint(t *testing.T) {
	h := HermesHint(8820, "https://openrouter.ai/api/v1")
	for _, want := range []string{"[proxy.upstreams]", "openrouter = \"https://openrouter.ai\"", "http://127.0.0.1:8820/up/openrouter/api/v1"} {
		if !strings.Contains(h, want) {
			t.Errorf("hint missing %q\n%s", want, h)
		}
	}
	if empty := HermesHint(8820, ""); !strings.Contains(empty, "no model.base_url") {
		t.Errorf("empty-base hint = %q", empty)
	}
}
