package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteCodexConfigBaseURL_CreatesFileWhenMissing(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	target := "http://127.0.0.1:8820/v1"

	backup, err := writeCodexConfigBaseURL(configPath, target, "stamp")
	if err != nil {
		t.Fatalf("writeCodexConfigBaseURL: %v", err)
	}
	if backup != "" {
		t.Fatalf("expected empty backup path for fresh create, got %q", backup)
	}

	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	if !strings.Contains(string(got), `openai_base_url = "http://127.0.0.1:8820/v1"`) {
		t.Fatalf("written file missing key: %s", got)
	}
	if !strings.Contains(string(got), "observer codex") {
		t.Fatalf("written file missing header comment: %s", got)
	}
}

func TestWriteCodexConfigBaseURL_AppendsWhenKeyMissing(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	original := "# operator's existing config\nmodel = \"gpt-5-codex\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	target := "http://127.0.0.1:8820/v1"

	backup, err := writeCodexConfigBaseURL(configPath, target, "stamp")
	if err != nil {
		t.Fatalf("writeCodexConfigBaseURL: %v", err)
	}
	if backup == "" {
		t.Fatalf("expected backup path when file pre-existed")
	}
	gotBackup, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(gotBackup) != original {
		t.Fatalf("backup mismatch: got %q want %q", gotBackup, original)
	}
	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	s := string(got)
	if !strings.Contains(s, "model = \"gpt-5-codex\"") {
		t.Fatalf("lost operator content: %s", s)
	}
	if !strings.Contains(s, `openai_base_url = "http://127.0.0.1:8820/v1"`) {
		t.Fatalf("did not append key: %s", s)
	}
}

func TestWriteCodexConfigBaseURL_ReplacesWrongValue(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	original := "model = \"gpt-5-codex\"\nopenai_base_url = \"https://api.openai.com/v1\"\nother = 1\n"
	if err := os.WriteFile(configPath, []byte(original), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	target := "http://127.0.0.1:8820/v1"

	if _, err := writeCodexConfigBaseURL(configPath, target, "stamp"); err != nil {
		t.Fatalf("writeCodexConfigBaseURL: %v", err)
	}
	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	s := string(got)
	if !strings.Contains(s, `openai_base_url = "http://127.0.0.1:8820/v1"`) {
		t.Fatalf("did not replace value: %s", s)
	}
	if strings.Contains(s, "https://api.openai.com/v1") {
		t.Fatalf("old value still present: %s", s)
	}
	if !strings.Contains(s, "model = \"gpt-5-codex\"") || !strings.Contains(s, "other = 1") {
		t.Fatalf("lost surrounding content: %s", s)
	}
}

func TestWriteCodexConfigBaseURL_IgnoresKeyInsideSection(t *testing.T) {
	// A subsection like [model_provider.openrouter] might also carry
	// openai_base_url; we must NOT mutate it. Only the top-level key
	// is the V6-2 target.
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	original := strings.Join([]string{
		`model = "gpt-5-codex"`,
		``,
		`[model_provider.openrouter]`,
		`openai_base_url = "https://openrouter.ai/api/v1"`,
		``,
	}, "\n")
	if err := os.WriteFile(configPath, []byte(original), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	target := "http://127.0.0.1:8820/v1"

	if _, err := writeCodexConfigBaseURL(configPath, target, "stamp"); err != nil {
		t.Fatalf("writeCodexConfigBaseURL: %v", err)
	}
	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	s := string(got)
	if !strings.Contains(s, `[model_provider.openrouter]`) || !strings.Contains(s, `"https://openrouter.ai/api/v1"`) {
		t.Fatalf("touched in-section key: %s", s)
	}
	if !strings.Contains(s, `openai_base_url = "http://127.0.0.1:8820/v1"`) {
		t.Fatalf("did not append top-level key: %s", s)
	}
}

func TestReplaceTopLevelOpenAIBaseURL_PreservesIndentAndComments(t *testing.T) {
	in := []byte("# header\nopenai_base_url = \"old\" # trailing\nfoo = 1\n")
	out, replaced := replaceTopLevelOpenAIBaseURL(in, "new")
	if !replaced {
		t.Fatalf("expected replaced=true")
	}
	got := string(out)
	if !strings.Contains(got, `openai_base_url = "new"`) {
		t.Fatalf("did not rewrite: %s", got)
	}
	if !strings.Contains(got, "# header") {
		t.Fatalf("dropped header comment: %s", got)
	}
	if !strings.Contains(got, "foo = 1") {
		t.Fatalf("dropped following key: %s", got)
	}
}

func TestReplaceTopLevelOpenAIBaseURL_NoMatchReturnsUntouched(t *testing.T) {
	in := []byte("model = \"x\"\n[provider]\nopenai_base_url = \"sect\"\n")
	out, replaced := replaceTopLevelOpenAIBaseURL(in, "new")
	if replaced {
		t.Fatalf("should not have replaced an in-section key")
	}
	if string(out) != string(in) {
		t.Fatalf("input mutated: got %q want %q", out, in)
	}
}
