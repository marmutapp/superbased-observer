package oscrypt

import (
	"encoding/base64"
	"errors"
	"strings"
)

// AppKeyConfig parameterizes secret retrieval per app. Antigravity's
// values are hardcoded inside the antigravity adapter; this package
// stays app-agnostic so it can serve future Chromium-derived adapters
// (Cursor, Windsurf, etc.) without modification.
type AppKeyConfig struct {
	// AppName is the user-facing application name as it appears in
	// the keychain service name ("{AppName} Safe Storage").
	AppName string
	// KeychainAccounts is the list of account names to try in order
	// against the macOS keychain. Different versions of the same app
	// have used both "{AppName}" and "{AppName} Key"; supply both.
	KeychainAccounts []string
	// WindowsLocalStatePaths is the list of candidate Local State
	// file paths to try in order. Resolved relative to the running
	// platform's standard app-data dirs by the caller (or by the
	// per-OS retrieve fallback when empty).
	WindowsLocalStatePaths []string
	// LinuxFallbackToPeanuts enables the Chromium "no Secret Service
	// running" fallback to the literal string "peanuts" as the secret.
	// Most apps inherit this default; opt out only when known wrong.
	LinuxFallbackToPeanuts bool
}

// Secret is the raw bytes returned by the OS keystore. May be 16, 24,
// 32, or other lengths depending on the app's storage choice. Callers
// pass it through DecryptCTR which tries direct + PBKDF2-derived keys
// at multiple lengths.
type Secret []byte

// Zero overwrites the secret bytes in-place. Best-effort — Go runtime
// may have copied. Callers should also restrict secret-bearing
// variables' lifetime via short scopes and avoid logging.
func (s Secret) Zero() {
	for i := range s {
		s[i] = 0
	}
}

// ErrNoBackend signals that the host has no working keystore for the
// configured app — callers should warn the user and fall back to
// "data found but cannot decrypt" rather than crash.
var ErrNoBackend = errors.New("oscrypt: no working keystore backend on this host")

// ErrNotFound signals that the keystore exists but does not contain
// an entry for the requested AppName.
var ErrNotFound = errors.New("oscrypt: app entry not found in keystore")

// RetrieveSecret pulls the OSCrypt secret for cfg.AppName from the
// platform's keystore. Implementation lives in per-OS files
// (secret_darwin.go, secret_linux.go, secret_windows.go); each
// supplies the platform-specific `retrieve` helper this thin wrapper
// dispatches to.
func RetrieveSecret(cfg AppKeyConfig) (Secret, error) {
	if strings.TrimSpace(cfg.AppName) == "" {
		return nil, errors.New("oscrypt.RetrieveSecret: AppName is required")
	}
	return retrieve(cfg)
}

// doDecodeBase64 wraps stdlib base64 with two trims so callers don't
// have to babysit whitespace from CLI shell-outs (`security`,
// `secret-tool`, `powershell.exe` — all of them tend to add trailing
// newlines). Returns "" without error if input is empty.
func doDecodeBase64(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	// Try standard, then URL-safe — different OS keystores use either.
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.URLEncoding.DecodeString(s)
}
