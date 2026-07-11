// codex_config_check.go — V6-2 pre-flight: warn if
// $CODEX_HOME/config.toml lacks `openai_base_url` pointing at the
// proxy.
//
// V6-2 (docs/observer-platform-issues-v6.md): codex 0.130+ silently
// drops the wrapper's argv-injected `-c openai_base_url=…` override.
// The outer `codex exec` parses the override but the inner
// `codex app-server` child reads its own config from
// $CODEX_HOME/config.toml, so only the file value reaches the HTTP
// client. The wrapper's pre-flight reads the file, detects the
// missing/wrong key, and emits ONE stderr line naming the manual
// fix. Honors --no-app-server-check.
//
// True fix is upstream: codex should forward -c overrides to the
// inner. Until then, the operator either edits config.toml manually
// (one-time) or accepts zero captures. This check makes the
// otherwise-silent capture loss loudly visible at pre-flight.

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// codexConfigMisconfig records one misconfigured CODEX_HOME root so the
// wrapper can both warn AND auto-fix from the same scan.
type codexConfigMisconfig struct {
	ConfigPath   string
	Status       configTOMLStatus
	CurrentValue string // populated only when Status==configTOMLOK (mismatch case)
	WantURL      string // the URL we'd write if --write-config is set
}

// findCodexConfigMisconfigs returns every CODEX_HOME root whose
// config.toml is missing or mis-pointing openai_base_url. Unreadable
// files are skipped (best-effort).
func findCodexConfigMisconfigs(codexHomeRootsList []string, proxyURL string) []codexConfigMisconfig {
	if len(codexHomeRootsList) == 0 {
		return nil
	}
	wantURLs := acceptedProxyBaseURLs(proxyURL)
	primary := primaryAcceptedURL(wantURLs)
	var out []codexConfigMisconfig
	for _, root := range codexHomeRootsList {
		configPath := filepath.Join(root, "config.toml")
		got, status := readConfigTOMLBaseURL(configPath)
		switch status {
		case configTOMLOK:
			if matchesAnyURL(got, wantURLs) {
				continue
			}
			out = append(out, codexConfigMisconfig{
				ConfigPath: configPath, Status: status,
				CurrentValue: got, WantURL: primary,
			})
		case configTOMLMissingKey, configTOMLMissingFile:
			out = append(out, codexConfigMisconfig{
				ConfigPath: configPath, Status: status, WantURL: primary,
			})
		case configTOMLUnreadable:
			// Skip — best-effort.
		}
	}
	return out
}

// checkCodexConfigTOMLBaseURL inspects every plausible CODEX_HOME for
// a config.toml file and verifies the top-level openai_base_url key
// matches the proxy. Returns one stderr-ready warning line per
// misconfigured root, or "" when all roots are correctly set up
// (silent happy path).
//
// Tolerance: trailing slash differences accepted; "<proxy>" and
// "<proxy>/v1" both treated as correct. File missing is a warning
// (operator hasn't run codex from this home yet). File unreadable is
// silent (best-effort — observer must not fail the wrapper on FS
// hiccup).
//
// codexHomeRoots is the caller-supplied list (typically from
// codexHomeRoots() in codex_capture_check.go) so this function is
// platform-agnostic and trivial to test.
func checkCodexConfigTOMLBaseURL(codexHomeRootsList []string, proxyURL string) string {
	misconfigs := findCodexConfigMisconfigs(codexHomeRootsList, proxyURL)
	if len(misconfigs) == 0 {
		return ""
	}
	var warnings []string
	for _, m := range misconfigs {
		switch m.Status {
		case configTOMLOK:
			warnings = append(warnings, fmt.Sprintf(
				"observer codex: %s sets openai_base_url=%q but the proxy is %s. Codex 0.130+ silently drops the -c openai_base_url override (V6-2); update the file or expect 0 captures. See docs/codex-shared-app-server-gotcha.md.",
				m.ConfigPath, m.CurrentValue, m.WantURL,
			))
		case configTOMLMissingKey:
			warnings = append(warnings, fmt.Sprintf(
				"observer codex: %s has no openai_base_url; codex 0.130+ silently drops the -c override (V6-2). Add `openai_base_url = %q` to the file or expect 0 captures. See docs/codex-shared-app-server-gotcha.md.",
				m.ConfigPath, m.WantURL,
			))
		case configTOMLMissingFile:
			warnings = append(warnings, fmt.Sprintf(
				"observer codex: %s does not exist; codex 0.130+ silently drops the -c openai_base_url override (V6-2). Create the file with `openai_base_url = %q` or expect 0 captures. See docs/codex-shared-app-server-gotcha.md.",
				m.ConfigPath, m.WantURL,
			))
		}
	}
	return strings.Join(warnings, "\n")
}

type configTOMLStatus int

const (
	configTOMLOK configTOMLStatus = iota
	configTOMLMissingKey
	configTOMLMissingFile
	configTOMLUnreadable
)

// readConfigTOMLBaseURL parses configPath and returns the top-level
// openai_base_url value. Returns the appropriate status code per
// failure mode so the caller can craft a specific warning.
func readConfigTOMLBaseURL(configPath string) (string, configTOMLStatus) {
	if _, err := os.Stat(configPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", configTOMLMissingFile
		}
		return "", configTOMLUnreadable
	}
	var cfg struct {
		OpenAIBaseURL string `toml:"openai_base_url"`
	}
	if _, err := toml.DecodeFile(configPath, &cfg); err != nil {
		return "", configTOMLUnreadable
	}
	if strings.TrimSpace(cfg.OpenAIBaseURL) == "" {
		return "", configTOMLMissingKey
	}
	return cfg.OpenAIBaseURL, configTOMLOK
}

// acceptedProxyBaseURLs returns every URL form an operator might
// legitimately set in config.toml to point at our proxy. The wrapper
// itself injects `<proxy>/v1`; operators sometimes write `<proxy>`
// and rely on codex to append the path. Both work for codex's HTTP
// routing, so both are accepted.
func acceptedProxyBaseURLs(proxyURL string) []string {
	base := strings.TrimRight(proxyURL, "/")
	if base == "" {
		return nil
	}
	return []string{base, base + "/v1"}
}

func primaryAcceptedURL(urls []string) string {
	if len(urls) == 0 {
		return ""
	}
	// Prefer the "/v1" form — matches what the wrapper itself injects.
	for _, u := range urls {
		if strings.HasSuffix(u, "/v1") {
			return u
		}
	}
	return urls[0]
}

// matchesAnyURL accepts trailing-slash differences ("foo/" == "foo").
func matchesAnyURL(got string, wants []string) bool {
	got = strings.TrimRight(strings.TrimSpace(got), "/")
	for _, w := range wants {
		if strings.TrimRight(w, "/") == got {
			return true
		}
	}
	return false
}
