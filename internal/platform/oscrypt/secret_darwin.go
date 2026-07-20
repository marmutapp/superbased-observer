//go:build darwin

package oscrypt

import (
	"errors"
	"os/exec"
	"strings"
)

// retrieve pulls the secret from the macOS Login Keychain via the
// `security` CLI. Tries each cfg.KeychainAccounts entry in order;
// first hit wins. First invocation triggers a Keychain prompt; the
// user can click "Always Allow" to make subsequent runs silent.
func retrieve(cfg AppKeyConfig) (Secret, error) {
	service := cfg.AppName + " Safe Storage"
	accounts := cfg.KeychainAccounts
	if len(accounts) == 0 {
		accounts = []string{cfg.AppName, cfg.AppName + " Key"}
	}
	var lastErr error
	for _, acc := range accounts {
		out, err := exec.Command(
			"security", "find-generic-password",
			"-w",          // password value only
			"-s", service, // service
			"-a", acc, // account
		).Output()
		if err != nil {
			lastErr = err
			continue
		}
		b64 := strings.TrimSpace(string(out))
		if b64 == "" {
			lastErr = errors.New("oscrypt.retrieve(darwin): empty keychain entry")
			continue
		}
		// Try base64-decoded first (most Chromium forks); fall back
		// to raw bytes for forks that store unencoded data.
		if decoded, err := doDecodeBase64(b64); err == nil && len(decoded) > 0 {
			return Secret(decoded), nil
		}
		return Secret(b64), nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, ErrNotFound
}
