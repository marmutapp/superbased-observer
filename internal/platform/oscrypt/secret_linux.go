//go:build linux

package oscrypt

import (
	"errors"
	"os"
	"os/exec"
	"strings"
)

// retrieve pulls the OSCrypt secret on Linux. Tries WSL-via-PowerShell
// first if the runtime looks like WSL2 with Windows interop, else
// falls back to the native libsecret/secret-tool path with optional
// "peanuts" fallback for headless servers.
func retrieve(cfg AppKeyConfig) (Secret, error) {
	if isWSL() {
		if secret, err := retrieveWSL(cfg); err == nil && len(secret) > 0 {
			return secret, nil
		}
		// fall through to native linux on WSL helper failure
	}
	return retrieveLinuxNative(cfg)
}

// retrieveLinuxNative pulls the secret via the `secret-tool` CLI.
// Falls back to the literal "peanuts" when the keystore is absent
// and cfg.LinuxFallbackToPeanuts is set.
func retrieveLinuxNative(cfg AppKeyConfig) (Secret, error) {
	service := cfg.AppName + " Safe Storage"
	out, err := exec.Command("secret-tool", "lookup", "application", service).Output()
	if err == nil {
		secret := strings.TrimRight(string(out), "\n\r")
		if secret != "" {
			return Secret(secret), nil
		}
	}
	if cfg.LinuxFallbackToPeanuts {
		return Secret("peanuts"), nil
	}
	return nil, ErrNoBackend
}

// isWSL reports whether the running process is inside a WSL2 distro
// with Windows interop enabled. Either signal is sufficient.
func isWSL() bool {
	if _, err := os.Stat("/proc/sys/fs/binfmt_misc/WSLInterop"); err == nil {
		return true
	}
	body, err := os.ReadFile("/proc/version")
	if err != nil {
		return false
	}
	lower := strings.ToLower(string(body))
	return strings.Contains(lower, "microsoft") || strings.Contains(lower, "wsl")
}

// retrieveWSL invokes `powershell.exe` to read Local State, base64-
// decode os_crypt.encrypted_key, strip the 5-byte "DPAPI" prefix,
// CryptUnprotectData the rest, and emit the unwrapped key as base64
// on stdout.
//
// The PowerShell session runs as the Windows user that owns the WSL
// session — single-user dev machines, that's the same identity that
// owns the app's DPAPI-encrypted blob. No interactive prompts.
func retrieveWSL(cfg AppKeyConfig) (Secret, error) {
	if _, err := exec.LookPath("powershell.exe"); err != nil {
		return nil, errors.New("oscrypt.retrieveWSL: powershell.exe not on PATH (corporate-locked WSL?)")
	}
	candidates := defaultWindowsLocalStatePaths(cfg)
	if len(cfg.WindowsLocalStatePaths) > 0 {
		candidates = append(candidates, cfg.WindowsLocalStatePaths...)
	}
	var lastErr error
	for _, path := range candidates {
		winPath := wslToWindowsPath(path)
		if winPath == "" {
			continue
		}
		if _, err := os.Stat(path); err != nil {
			continue
		}
		script := buildPowerShellUnwrapScript(winPath)
		out, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script).Output()
		if err != nil {
			lastErr = err
			continue
		}
		b64 := strings.TrimSpace(strings.ReplaceAll(string(out), "\r", ""))
		if b64 == "" {
			lastErr = errors.New("powershell returned empty key")
			continue
		}
		decoded, err := doDecodeBase64(b64)
		if err != nil {
			lastErr = err
			continue
		}
		return Secret(decoded), nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, ErrNotFound
}

// defaultWindowsLocalStatePaths enumerates likely Local State
// locations under cross-mount-detected Windows homes.
func defaultWindowsLocalStatePaths(cfg AppKeyConfig) []string {
	homes := windowsHomesUnderMnt()
	var paths []string
	for _, home := range homes {
		paths = append(
			paths,
			home+"/AppData/Roaming/"+cfg.AppName+"/Local State",
			home+"/AppData/Local/"+cfg.AppName+"/User Data/Local State",
			home+"/AppData/Local/"+cfg.AppName+"/Local State",
		)
	}
	return paths
}

// windowsHomesUnderMnt enumerates /mnt/c/Users/<name> dirs that look
// like real user homes (skip Default, Public, etc.).
func windowsHomesUnderMnt() []string {
	const root = "/mnt/c/Users"
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	skipNames := map[string]struct{}{
		"All Users":           {},
		"Default":             {},
		"Default User":        {},
		"Public":              {},
		"WsiAccount":          {},
		"desktop.ini":         {},
		"CodexSandboxOffline": {},
	}
	var homes []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, skip := skipNames[e.Name()]; skip {
			continue
		}
		homes = append(homes, root+"/"+e.Name())
	}
	return homes
}

// wslToWindowsPath converts /mnt/c/foo/bar to C:\foo\bar. Returns ""
// for paths that don't look like cross-mount.
func wslToWindowsPath(p string) string {
	if !strings.HasPrefix(p, "/mnt/") || len(p) < 7 {
		return ""
	}
	drive := strings.ToUpper(string(p[5]))
	rest := strings.ReplaceAll(p[6:], "/", `\`)
	if !strings.HasPrefix(rest, `\`) {
		rest = `\` + rest
	}
	return drive + ":" + rest
}

// buildPowerShellUnwrapScript returns the inline PowerShell that
// reads the Local State JSON, peels the DPAPI prefix, and unwraps.
// Single-quoted path keeps Windows backslashes intact under bash
// escaping.
func buildPowerShellUnwrapScript(winPath string) string {
	return `
$ErrorActionPreference = "Stop"
$path = '` + winPath + `'
if (-not (Test-Path -LiteralPath $path)) { exit 2 }
$ls = Get-Content -Raw -LiteralPath $path | ConvertFrom-Json
if (-not $ls.os_crypt) { exit 3 }
if (-not $ls.os_crypt.encrypted_key) { exit 4 }
$enc = [Convert]::FromBase64String($ls.os_crypt.encrypted_key)
if ($enc.Length -lt 6) { exit 5 }
$prefix = [System.Text.Encoding]::ASCII.GetString($enc[0..4])
if ($prefix -ne "DPAPI") { exit 6 }
$wrapped = $enc[5..($enc.Length-1)]
Add-Type -AssemblyName System.Security
$plain = [System.Security.Cryptography.ProtectedData]::Unprotect($wrapped, $null, "CurrentUser")
[Convert]::ToBase64String($plain)
`
}
