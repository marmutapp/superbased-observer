package ccanalytics

import (
	"fmt"
	"os"
	"strings"
)

// apiKeyEnv is the environment variable the admin uses to supply the analytics
// API key without writing it into the server TOML.
const apiKeyEnv = "CC_ANALYTICS_API_KEY"

// Valid api_kind values selecting the auth path.
const (
	KindEnterprise = "enterprise"
	KindAdmin      = "admin"
)

// ResolveKey returns the analytics API key from the env var (preferred) or the
// secret file at keyFile. It never logs or returns the key in an error.
func ResolveKey(keyFile string) (string, error) {
	if v := strings.TrimSpace(os.Getenv(apiKeyEnv)); v != "" {
		return v, nil
	}
	if keyFile != "" {
		b, err := os.ReadFile(keyFile)
		if err != nil {
			return "", fmt.Errorf("ccanalytics: read api_key_file: %w", err)
		}
		if v := strings.TrimSpace(string(b)); v != "" {
			return v, nil
		}
	}
	return "", fmt.Errorf("ccanalytics: no API key (set %s or api_key_file)", apiKeyEnv)
}

// ValidateKind checks the configured api_kind and returns a normalized value.
// The Claude Code Analytics endpoint takes the Admin API key only (research
// findings §5 — there is no separate "Analytics API key" and the endpoint is
// not Enterprise-gated), so "admin" and the empty default both resolve to
// KindAdmin. KindEnterprise is accepted for forward-compat but targets the same
// endpoint today.
func ValidateKind(kind string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case KindEnterprise:
		return KindEnterprise, nil
	case KindAdmin, "":
		return KindAdmin, nil
	default:
		return "", fmt.Errorf("ccanalytics: invalid api_kind %q (want enterprise|admin)", kind)
	}
}

// authHeader returns the (header, value) pair for the Admin API key. The Claude
// Code Analytics endpoint authenticates with x-api-key (NOT Authorization:
// Bearer) — confirmed against the docs' own curl. The one place that touches
// the secret, kept auditable.
func authHeader(key string) (string, string) {
	return "x-api-key", key
}
