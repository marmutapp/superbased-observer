package codexanalytics

import (
	"fmt"
	"os"
	"strings"
)

// apiKeyEnv is the environment variable the admin uses to supply the analytics
// key without writing it into the server TOML. Both surfaces use Bearer auth;
// the key KIND differs (ChatGPT-Enterprise: a key scoped
// codex.enterprise.analytics.read; OpenAI-org: an admin key sk-admin…) but the
// header is identical, so one env var + one resolver serves both.
const apiKeyEnv = "CODEX_ANALYTICS_API_KEY"

// ResolveKey returns the analytics API key from the env var (preferred) or the
// secret file at keyFile. It never logs or returns the key in an error.
func ResolveKey(keyFile string) (string, error) {
	if v := strings.TrimSpace(os.Getenv(apiKeyEnv)); v != "" {
		return v, nil
	}
	if keyFile != "" {
		b, err := os.ReadFile(keyFile)
		if err != nil {
			return "", fmt.Errorf("codexanalytics: read api_key_file: %w", err)
		}
		if v := strings.TrimSpace(string(b)); v != "" {
			return v, nil
		}
	}
	return "", fmt.Errorf("codexanalytics: no API key (set %s or api_key_file)", apiKeyEnv)
}

// authHeader returns the (header, value) pair. Both Codex analytics surfaces
// authenticate with Authorization: Bearer (NOT x-api-key — that is CC's
// Anthropic convention). The one place the secret is formatted onto the wire.
func authHeader(key string) (string, string) {
	return "Authorization", "Bearer " + key
}
