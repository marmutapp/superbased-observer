package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareClaudeEnv_OAuthInjectedFromCredentials(t *testing.T) {
	dir := t.TempDir()
	credsPath := filepath.Join(dir, ".credentials.json")
	if err := os.WriteFile(credsPath, []byte(`{
  "claudeAiOauth": {
    "accessToken": "sk-ant-oat01-test-abc123",
    "expiresAt": 9999999999999,
    "refreshToken": "ignored",
    "scopes": ["user:inference"],
    "subscriptionType": "max"
  }
}`), 0o600); err != nil {
		t.Fatalf("write creds: %v", err)
	}

	parent := []string{"PATH=/usr/bin", "HOME=" + dir}
	env, info, err := prepareClaudeEnv(parent, "http://127.0.0.1:8820", credsPath)
	if err != nil {
		t.Fatalf("prepareClaudeEnv: %v", err)
	}
	if !info.BaseURLSet || info.BaseURLPreset {
		t.Fatalf("expected BaseURLSet=true Preset=false, got %+v", info)
	}
	if !info.OAuthInjected || info.OAuthPreset {
		t.Fatalf("expected OAuthInjected=true Preset=false, got %+v", info)
	}
	if info.CredentialsErr != nil {
		t.Fatalf("unexpected credentials err: %v", info.CredentialsErr)
	}

	got := envMap(env)
	if got["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:8820" {
		t.Errorf("ANTHROPIC_BASE_URL = %q, want %q", got["ANTHROPIC_BASE_URL"], "http://127.0.0.1:8820")
	}
	if got["ANTHROPIC_AUTH_TOKEN"] != "sk-ant-oat01-test-abc123" {
		t.Errorf("ANTHROPIC_AUTH_TOKEN = %q, want %q", got["ANTHROPIC_AUTH_TOKEN"], "sk-ant-oat01-test-abc123")
	}
	if got["PATH"] != "/usr/bin" {
		t.Errorf("PATH = %q, want preserved", got["PATH"])
	}
}

// TestPrepareClaudeEnv_StaleTokenNotExported pins the D13 fix: an
// EXPIRED stored OAuth token is never re-exported (a stale env token
// suppresses Claude Code's own refresh and the session 401s); the
// launcher reports it via OAuthStale and routes BASE_URL-only.
func TestPrepareClaudeEnv_StaleTokenNotExported(t *testing.T) {
	dir := t.TempDir()
	credsPath := filepath.Join(dir, ".credentials.json")
	if err := os.WriteFile(credsPath, []byte(`{
  "claudeAiOauth": {
    "accessToken": "sk-ant-oat01-stale",
    "expiresAt": 1000000000000
  }
}`), 0o600); err != nil {
		t.Fatalf("write creds: %v", err)
	}

	env, info, err := prepareClaudeEnv([]string{"PATH=/usr/bin"}, "http://127.0.0.1:8820", credsPath)
	if err != nil {
		t.Fatalf("prepareClaudeEnv: %v", err)
	}
	if !info.OAuthStale || info.OAuthInjected || info.CredentialsErr != nil {
		t.Fatalf("expected OAuthStale only, got %+v", info)
	}
	got := envMap(env)
	if tok, present := got["ANTHROPIC_AUTH_TOKEN"]; present {
		t.Errorf("stale token leaked into env: %q", tok)
	}
	if got["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:8820" {
		t.Errorf("BASE_URL routing must survive the stale-token path")
	}
}

func TestPrepareClaudeEnv_RespectsExistingBaseURL(t *testing.T) {
	dir := t.TempDir()
	credsPath := filepath.Join(dir, ".credentials.json")
	if err := os.WriteFile(credsPath, []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-oat01-test"}}`), 0o600); err != nil {
		t.Fatalf("write creds: %v", err)
	}

	parent := []string{"ANTHROPIC_BASE_URL=http://existing:9999"}
	env, info, err := prepareClaudeEnv(parent, "http://127.0.0.1:8820", credsPath)
	if err != nil {
		t.Fatalf("prepareClaudeEnv: %v", err)
	}
	if info.BaseURLSet || !info.BaseURLPreset {
		t.Fatalf("expected BaseURLPreset=true, got %+v", info)
	}
	got := envMap(env)
	if got["ANTHROPIC_BASE_URL"] != "http://existing:9999" {
		t.Errorf("ANTHROPIC_BASE_URL clobbered: got %q", got["ANTHROPIC_BASE_URL"])
	}
	// OAuth still injected even when base URL was preset.
	if !info.OAuthInjected {
		t.Errorf("expected OAuthInjected=true, got %+v", info)
	}
}

func TestPrepareClaudeEnv_RespectsExistingAuthToken(t *testing.T) {
	dir := t.TempDir()
	credsPath := filepath.Join(dir, ".credentials.json")
	if err := os.WriteFile(credsPath, []byte(`{"claudeAiOauth":{"accessToken":"from-disk"}}`), 0o600); err != nil {
		t.Fatalf("write creds: %v", err)
	}

	parent := []string{"ANTHROPIC_AUTH_TOKEN=user-explicit"}
	env, info, err := prepareClaudeEnv(parent, "http://127.0.0.1:8820", credsPath)
	if err != nil {
		t.Fatalf("prepareClaudeEnv: %v", err)
	}
	if info.OAuthInjected || !info.OAuthPreset {
		t.Fatalf("expected OAuthPreset=true Injected=false, got %+v", info)
	}
	got := envMap(env)
	if got["ANTHROPIC_AUTH_TOKEN"] != "user-explicit" {
		t.Errorf("ANTHROPIC_AUTH_TOKEN clobbered: got %q", got["ANTHROPIC_AUTH_TOKEN"])
	}
}

func TestPrepareClaudeEnv_NoCredentialsFile_APIKeyUser(t *testing.T) {
	parent := []string{"ANTHROPIC_API_KEY=sk-ant-api03-test"}
	env, info, err := prepareClaudeEnv(parent, "http://127.0.0.1:8820", "/nonexistent/path/.credentials.json")
	if err != nil {
		t.Fatalf("prepareClaudeEnv: %v", err)
	}
	if info.OAuthInjected {
		t.Errorf("OAuth shouldn't be injected when creds file is missing")
	}
	if info.CredentialsErr != nil {
		t.Errorf("missing file shouldn't be an error: %v", info.CredentialsErr)
	}
	if !info.BaseURLSet {
		t.Errorf("BASE_URL still must be set for API-key users")
	}
	got := envMap(env)
	if got["ANTHROPIC_API_KEY"] != "sk-ant-api03-test" {
		t.Errorf("ANTHROPIC_API_KEY mangled: got %q", got["ANTHROPIC_API_KEY"])
	}
	if _, present := got["ANTHROPIC_AUTH_TOKEN"]; present {
		t.Errorf("ANTHROPIC_AUTH_TOKEN should not be set when creds file is missing")
	}
}

func TestPrepareClaudeEnv_MalformedCredentialsSurfacesErr(t *testing.T) {
	dir := t.TempDir()
	credsPath := filepath.Join(dir, ".credentials.json")
	if err := os.WriteFile(credsPath, []byte(`not json at all`), 0o600); err != nil {
		t.Fatalf("write creds: %v", err)
	}
	parent := []string{}
	_, info, err := prepareClaudeEnv(parent, "http://127.0.0.1:8820", credsPath)
	if err != nil {
		t.Fatalf("prepareClaudeEnv: %v", err)
	}
	if info.CredentialsErr == nil {
		t.Fatalf("expected CredentialsErr for malformed JSON")
	}
	if info.OAuthInjected {
		t.Errorf("OAuth shouldn't be injected when parse fails")
	}
}

func TestPrepareClaudeEnv_EmptyAccessTokenIgnored(t *testing.T) {
	dir := t.TempDir()
	credsPath := filepath.Join(dir, ".credentials.json")
	if err := os.WriteFile(credsPath, []byte(`{"claudeAiOauth":{"accessToken":""}}`), 0o600); err != nil {
		t.Fatalf("write creds: %v", err)
	}
	_, info, err := prepareClaudeEnv(nil, "http://127.0.0.1:8820", credsPath)
	if err != nil {
		t.Fatalf("prepareClaudeEnv: %v", err)
	}
	if info.OAuthInjected {
		t.Errorf("empty accessToken should not inject")
	}
}

func TestSplitHostPortFromURL(t *testing.T) {
	cases := []struct {
		in   string
		host string
		port string
		ok   bool
	}{
		{"http://127.0.0.1:8820", "127.0.0.1", "8820", true},
		{"https://example.com:443/v1", "example.com", "443", true},
		{"http://localhost:8820/", "localhost", "8820", true},
		{"127.0.0.1:8820", "127.0.0.1", "8820", true},
		{"no-port-here", "", "", false},
		{"", "", "", false},
	}
	for _, tc := range cases {
		host, port, ok := splitHostPortFromURL(tc.in)
		if ok != tc.ok || host != tc.host || port != tc.port {
			t.Errorf("splitHostPortFromURL(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.in, host, port, ok, tc.host, tc.port, tc.ok)
		}
	}
}

func TestClaudeCredentialsPath_HonorsConfigDirEnv(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/tmp/fake-claude")
	t.Setenv("ANTHROPIC_CONFIG_DIR", "")
	// filepath.ToSlash normalizes the Windows-host separators (the daemon
	// runs on Linux/WSL where the join is already '/'): the invariant under
	// test is that the configured dir is honored, not the OS separator.
	got := claudeCredentialsPath()
	if !strings.HasPrefix(filepath.ToSlash(got), "/tmp/fake-claude") {
		t.Errorf("CLAUDE_CONFIG_DIR not honored: %q", got)
	}

	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("ANTHROPIC_CONFIG_DIR", "/tmp/fake-anthropic")
	got = claudeCredentialsPath()
	if !strings.HasPrefix(filepath.ToSlash(got), "/tmp/fake-anthropic") {
		t.Errorf("ANTHROPIC_CONFIG_DIR not honored: %q", got)
	}
}

func envMap(kvs []string) map[string]string {
	out := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		idx := strings.IndexByte(kv, '=')
		if idx <= 0 {
			continue
		}
		out[kv[:idx]] = kv[idx+1:]
	}
	return out
}
