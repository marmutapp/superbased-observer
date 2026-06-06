//go:build windows

package oscrypt

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/billgraziano/dpapi"
)

// retrieve reads Local State, base64-decodes os_crypt.encrypted_key,
// strips the literal "DPAPI" 5-byte prefix, and DPAPI-unwraps the
// rest under the current user's scope. Tries cfg.WindowsLocalStatePaths
// in order, then falls back to the standard %APPDATA%/<app>/Local
// State and %LOCALAPPDATA%/<app>/User Data/Local State paths.
func retrieve(cfg AppKeyConfig) (Secret, error) {
	candidates := cfg.WindowsLocalStatePaths
	if len(candidates) == 0 {
		appdata := os.Getenv("APPDATA")
		localApp := os.Getenv("LOCALAPPDATA")
		if appdata != "" {
			candidates = append(
				candidates,
				filepath.Join(appdata, cfg.AppName, "Local State"),
				filepath.Join(appdata, cfg.AppName, "User Data", "Local State"),
			)
		}
		if localApp != "" {
			candidates = append(
				candidates,
				filepath.Join(localApp, cfg.AppName, "Local State"),
				filepath.Join(localApp, cfg.AppName, "User Data", "Local State"),
			)
		}
	}

	var lastErr error
	for _, path := range candidates {
		body, err := os.ReadFile(path)
		if err != nil {
			lastErr = err
			continue
		}
		var state struct {
			OSCrypt struct {
				EncryptedKey string `json:"encrypted_key"`
			} `json:"os_crypt"`
		}
		if err := json.Unmarshal(body, &state); err != nil {
			lastErr = err
			continue
		}
		if state.OSCrypt.EncryptedKey == "" {
			lastErr = errors.New("oscrypt.retrieve(windows): os_crypt.encrypted_key missing")
			continue
		}
		raw, err := doDecodeBase64(state.OSCrypt.EncryptedKey)
		if err != nil {
			lastErr = err
			continue
		}
		if len(raw) < 6 {
			lastErr = errors.New("oscrypt.retrieve(windows): encrypted_key too short for DPAPI prefix")
			continue
		}
		if string(raw[:5]) != "DPAPI" {
			lastErr = errors.New("oscrypt.retrieve(windows): encrypted_key missing DPAPI prefix")
			continue
		}
		unwrapped, err := dpapi.DecryptBytes(raw[5:])
		if err != nil {
			lastErr = err
			continue
		}
		return Secret(unwrapped), nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, ErrNotFound
}
