package proxyroute

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// kimiKey is a deliberately non-secret-looking fixture value; the point is
// the writer must leave it byte-identical (never touch the api key).
const kimiKey = "routed-key-abc"

func newKimiRegistrar(t *testing.T, home string) *Registrar {
	t.Helper()
	t.Setenv("KIMI_CODE_HOME", "") // force homeDir/.kimi-code resolution
	r, err := NewRegistrar(RegisterOptions{ProxyPort: 8820, HomeDir: home})
	if err != nil {
		t.Fatalf("NewRegistrar: %v", err)
	}
	return r
}

func writeKimiConfig(t *testing.T, home, body string) string {
	t.Helper()
	dir := filepath.Join(home, ".kimi-code")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const kimiBaseConfig = "default_model = \"openai/gpt-4o\"\n\n" +
	"[providers.openai]\n" +
	"type = \"openai\"\n" +
	"api_key = \"" + kimiKey + "\"\n\n" +
	"[models.\"openai/gpt-4o\"]\n" +
	"context = 128000\n\n" +
	"[thinking]\n" +
	"enabled = true\n"

func TestRegisterKimiCodeAddsBaseURLAndBacksUp(t *testing.T) {
	home := t.TempDir()
	path := writeKimiConfig(t, home, kimiBaseConfig)
	r := newKimiRegistrar(t, home)

	res := r.RegisterKimiCode()
	if res.Error != nil {
		t.Fatalf("RegisterKimiCode: %v", res.Error)
	}
	if !res.Added || res.AlreadySet || res.ConfigMissing {
		t.Fatalf("want Added; got Added=%v AlreadySet=%v ConfigMissing=%v", res.Added, res.AlreadySet, res.ConfigMissing)
	}
	if res.BaseURL != "http://127.0.0.1:8820/v1" {
		t.Errorf("BaseURL = %q", res.BaseURL)
	}

	// .bak preserves the original (no base_url).
	bak, err := os.ReadFile(path + ".bak")
	if err != nil || strings.Contains(string(bak), "base_url") {
		t.Errorf("backup missing or already mutated: %v", err)
	}

	var doc map[string]any
	out, _ := os.ReadFile(path)
	if err := toml.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	openai := doc["providers"].(map[string]any)["openai"].(map[string]any)
	if openai["base_url"] != "http://127.0.0.1:8820/v1" {
		t.Errorf("base_url not added: %v", openai["base_url"])
	}
	if openai["api_key"] != kimiKey {
		t.Errorf("api_key clobbered: %v (writer must never touch keys)", openai["api_key"])
	}
	if openai["type"] != "openai" {
		t.Errorf("sibling provider key lost: %v", openai["type"])
	}
	if _, ok := doc["thinking"]; !ok {
		t.Errorf("sibling top-level [thinking] table lost")
	}
	if _, ok := doc["models"]; !ok {
		t.Errorf("sibling [models.*] table lost")
	}
}

func TestRegisterKimiCodeIdempotent(t *testing.T) {
	home := t.TempDir()
	path := writeKimiConfig(t, home, kimiBaseConfig)
	r := newKimiRegistrar(t, home)
	if res := r.RegisterKimiCode(); res.Error != nil || !res.Added {
		t.Fatalf("first run: err=%v added=%v", res.Error, res.Added)
	}
	// Second run must be a no-op (base_url now already the observer URL).
	res := r.RegisterKimiCode()
	if res.Error != nil {
		t.Fatalf("second run err: %v", res.Error)
	}
	if !res.AlreadySet || res.Added {
		t.Errorf("re-run should be AlreadySet no-op, got Added=%v AlreadySet=%v", res.Added, res.AlreadySet)
	}
	// The idempotent no-op must not overwrite the .bak from the first write
	// with a now-mutated copy... it simply returns before writing.
	bak, _ := os.ReadFile(path + ".bak")
	if strings.Contains(string(bak), "127.0.0.1") {
		t.Errorf(".bak should still hold the pre-route config")
	}
}

func TestRegisterKimiCodeRefusesForeignURL(t *testing.T) {
	home := t.TempDir()
	cfg := "[providers.openai]\napi_key = \"" + kimiKey + "\"\nbase_url = \"https://api.moonshot.cn/v1\"\n"
	path := writeKimiConfig(t, home, cfg)
	r := newKimiRegistrar(t, home)
	res := r.RegisterKimiCode()
	if res.Error == nil {
		t.Fatal("expected refusal on a foreign base_url")
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Errorf("refusal must not write a .bak")
	}
	out, _ := os.ReadFile(path)
	if !strings.Contains(string(out), "api.moonshot.cn") {
		t.Errorf("refusal must not mutate the file")
	}
}

func TestRegisterKimiCodeMissingConfigSkips(t *testing.T) {
	home := t.TempDir() // no ~/.kimi-code/config.toml
	r := newKimiRegistrar(t, home)
	res := r.RegisterKimiCode()
	if res.Error != nil {
		t.Fatalf("missing config should be a benign skip, got err: %v", res.Error)
	}
	if !res.ConfigMissing || res.Added || res.AlreadySet {
		t.Errorf("want ConfigMissing skip; got ConfigMissing=%v Added=%v AlreadySet=%v", res.ConfigMissing, res.Added, res.AlreadySet)
	}
}

func TestRegisterKimiCodeNoProviderErrors(t *testing.T) {
	home := t.TempDir()
	writeKimiConfig(t, home, "default_model = \"openai/gpt-4o\"\n")
	r := newKimiRegistrar(t, home)
	if res := r.RegisterKimiCode(); res.Error == nil {
		t.Fatal("expected error when [providers.openai] absent")
	}
}

func TestRegisterKimiCodeDryRunDoesNotWrite(t *testing.T) {
	home := t.TempDir()
	path := writeKimiConfig(t, home, kimiBaseConfig)
	t.Setenv("KIMI_CODE_HOME", "")
	r, _ := NewRegistrar(RegisterOptions{ProxyPort: 8820, HomeDir: home, DryRun: true})
	res := r.RegisterKimiCode()
	if res.Error != nil || !res.Added {
		t.Fatalf("dry run: err=%v added=%v", res.Error, res.Added)
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Errorf("dry run must not write a .bak")
	}
	out, _ := os.ReadFile(path)
	if strings.Contains(string(out), "127.0.0.1") {
		t.Errorf("dry run mutated the file")
	}
}
