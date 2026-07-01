package copilotanalytics

import (
	"fmt"
	"os"
	"strings"
)

// apiKeyEnv is the environment variable the admin uses to supply the GitHub token
// without writing it into the server TOML. A classic PAT, fine-grained PAT, or a
// GitHub App installation token works; the scopes differ by surface (engagement:
// read:org / read:enterprise / manage_billing:copilot; seats + billing:
// manage_billing:copilot / read:org). One env var + one resolver serves all three
// because the header is identical.
const apiKeyEnv = "COPILOT_ANALYTICS_TOKEN"

// githubAPIVersion is the pinned GitHub REST API version header value.
const githubAPIVersion = "2022-11-28"

// ResolveKey returns the GitHub token from the env var (preferred) or the secret
// file at keyFile. It never logs or returns the key in an error.
func ResolveKey(keyFile string) (string, error) {
	if v := strings.TrimSpace(os.Getenv(apiKeyEnv)); v != "" {
		return v, nil
	}
	if keyFile != "" {
		b, err := os.ReadFile(keyFile)
		if err != nil {
			return "", fmt.Errorf("copilotanalytics: read api_key_file: %w", err)
		}
		if v := strings.TrimSpace(string(b)); v != "" {
			return v, nil
		}
	}
	return "", fmt.Errorf("copilotanalytics: no GitHub token (set %s or api_key_file)", apiKeyEnv)
}
