package proxyroute

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newQwenRegistrar(t *testing.T, home string) *Registrar {
	t.Helper()
	t.Setenv("QWEN_HOME", "") // force homeDir/.qwen resolution
	r, err := NewRegistrar(RegisterOptions{ProxyPort: 8820, HomeDir: home})
	if err != nil {
		t.Fatalf("NewRegistrar: %v", err)
	}
	return r
}

func writeQwenConfig(t *testing.T, home, body string) string {
	t.Helper()
	dir := filepath.Join(home, ".qwen")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func qwenConfig(baseURL string) string {
	return `{
  "model": {
    "name": "qwen3-coder-plus",
    "baseUrl": "` + baseURL + `",
    "apiKey": "routed-key-ghi"
  },
  "ui": {"theme": "dark"}
}`
}

func TestRegisterQwenCodeRewritesKnownDefaultAndBacksUp(t *testing.T) {
	home := t.TempDir()
	path := writeQwenConfig(t, home, qwenConfig("https://api.openai.com/v1"))
	r := newQwenRegistrar(t, home)

	res := r.RegisterQwenCode()
	if res.Error != nil {
		t.Fatalf("RegisterQwenCode: %v", res.Error)
	}
	if !res.Added {
		t.Fatalf("want Added, got %+v", res)
	}
	if res.PriorBaseURL != "https://api.openai.com/v1" {
		t.Errorf("PriorBaseURL = %q", res.PriorBaseURL)
	}

	bak, err := os.ReadFile(path + ".bak")
	if err != nil || !strings.Contains(string(bak), "api.openai.com") {
		t.Errorf("backup missing or wrong: %v", err)
	}

	var doc map[string]any
	out, _ := os.ReadFile(path)
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	model := doc["model"].(map[string]any)
	if model["baseUrl"] != "http://127.0.0.1:8820/v1" {
		t.Errorf("baseUrl not rewritten: %v", model["baseUrl"])
	}
	if model["apiKey"] != "routed-key-ghi" {
		t.Errorf("apiKey clobbered: %v", model["apiKey"])
	}
	if model["name"] != "qwen3-coder-plus" {
		t.Errorf("sibling model key lost: %v", model["name"])
	}
	if _, ok := doc["ui"]; !ok {
		t.Errorf("sibling top-level ui block lost")
	}
}

func TestRegisterQwenCodeIdempotent(t *testing.T) {
	home := t.TempDir()
	writeQwenConfig(t, home, qwenConfig("http://127.0.0.1:8820/v1"))
	r := newQwenRegistrar(t, home)
	res := r.RegisterQwenCode()
	if res.Error != nil {
		t.Fatalf("err: %v", res.Error)
	}
	if !res.AlreadySet || res.Added {
		t.Errorf("already-observer should be AlreadySet no-op, got Added=%v AlreadySet=%v", res.Added, res.AlreadySet)
	}
}

func TestRegisterQwenCodeAlreadyObserverDifferentPortNoOp(t *testing.T) {
	home := t.TempDir()
	path := writeQwenConfig(t, home, qwenConfig("http://127.0.0.1:9999/v1"))
	r := newQwenRegistrar(t, home)
	res := r.RegisterQwenCode()
	if res.Error != nil {
		t.Fatalf("err: %v", res.Error)
	}
	if !res.AlreadySet || res.Added {
		t.Errorf("other-local-observer should be left alone (AlreadySet), got %+v", res)
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Errorf("no-op must not write a .bak")
	}
}

func TestRegisterQwenCodeRefusesCustomHost(t *testing.T) {
	home := t.TempDir()
	path := writeQwenConfig(t, home, qwenConfig("https://dashscope.aliyuncs.com/compatible-mode/v1"))
	r := newQwenRegistrar(t, home)
	res := r.RegisterQwenCode()
	if res.Error == nil {
		t.Fatal("expected refusal on a custom (non-default) host")
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Errorf("refusal must not write a .bak")
	}
	out, _ := os.ReadFile(path)
	if !strings.Contains(string(out), "dashscope.aliyuncs.com") {
		t.Errorf("refusal must not mutate the file")
	}
}

func TestRegisterQwenCodeMissingConfigSkips(t *testing.T) {
	home := t.TempDir()
	r := newQwenRegistrar(t, home)
	res := r.RegisterQwenCode()
	if res.Error != nil {
		t.Fatalf("missing config should be benign skip: %v", res.Error)
	}
	if !res.ConfigMissing {
		t.Errorf("want ConfigMissing, got %+v", res)
	}
}

func TestRegisterQwenCodeNoBaseURLErrors(t *testing.T) {
	home := t.TempDir()
	writeQwenConfig(t, home, `{"model":{"name":"qwen3"}}`)
	r := newQwenRegistrar(t, home)
	if res := r.RegisterQwenCode(); res.Error == nil {
		t.Fatal("expected error when model.baseUrl absent")
	}
}

func TestRegisterQwenCodeDryRunDoesNotWrite(t *testing.T) {
	home := t.TempDir()
	path := writeQwenConfig(t, home, qwenConfig("https://api.openai.com/v1"))
	t.Setenv("QWEN_HOME", "")
	r, _ := NewRegistrar(RegisterOptions{ProxyPort: 8820, HomeDir: home, DryRun: true})
	res := r.RegisterQwenCode()
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
