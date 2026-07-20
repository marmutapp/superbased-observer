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
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/marmutapp/superbased-observer/internal/proxyroute"
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

// codexConfigsRoutingToProxy returns every codex config FILE that currently
// routes codex AT the observer proxy for THIS launch — a persistent route that
// `--no-proxy-route` does NOT neutralize (B2-3). It covers BOTH shapes observer
// can write: the legacy top-level `openai_base_url` key (what `observer codex
// --write-config` writes) AND the managed provider shape (`model_provider =
// "openai-observer"` + `[model_providers.openai-observer] base_url = ...`, what
// `observer init`/internal/proxyroute writes).
//
// profile is the ACTIVE `-p/--profile <name>` for this launch (finding 3a): when
// set, codex layers `$CODEX_HOME/<name>.config.toml` ON TOP of the base
// config.toml (codex CONFIG_PROFILE_V2), so the effective route is the merge and
// the profile file may be the actual offender. When profile is "", only the base
// config.toml is consulted. Matching is loopback- and suffix-tolerant (see
// urlRoutesToProxy). Missing/unreadable files are skipped (best-effort). The
// returned list is the DISTINCT set of files whose effective value routes to us.
func codexConfigsRoutingToProxy(codexHomeRootsList []string, proxyURL, profile string) []string {
	if strings.TrimSpace(proxyURL) == "" {
		return nil
	}
	var out []string
	seen := map[string]struct{}{}
	for _, root := range codexHomeRootsList {
		for _, src := range codexEffectiveRoutedSources(root, profile) {
			if src.file == "" || !urlRoutesToProxy(src.url, proxyURL) {
				continue
			}
			if _, dup := seen[src.file]; dup {
				continue
			}
			seen[src.file] = struct{}{}
			out = append(out, src.file)
		}
	}
	return out
}

// codexActiveProfile parses the active codex profile from a launch's forwarded
// args (finding 3a). codex's `-p/--profile` is a GLOBAL flag that layers
// `$CODEX_HOME/<name>.config.toml` on top of the base config (CONFIG_PROFILE_V2).
// It accepts every spelling clap does: `-p NAME`, `-p=NAME`, the ATTACHED short
// form `-pNAME` (e.g. `-pwork` — finding N4), `--profile NAME`, and
// `--profile=NAME`. The scan STOPS at the first bare `--` (mirrors
// argsAreCodexHeadless) and returns "" when no profile is set.
//
// The scan is value-flag aware: a `-p…`-looking token that is actually the VALUE
// of a preceding value-taking flag (e.g. `-m -pfoo`, model=`-pfoo`) is skipped
// via codexValueFlags rather than mis-parsed as the profile flag (finding N4).
func codexActiveProfile(args []string) string {
	i := 0
	for i < len(args) {
		a := args[i]
		if a == "--" {
			return ""
		}
		switch {
		case a == "-p" || a == "--profile":
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		case strings.HasPrefix(a, "--profile="):
			return strings.TrimPrefix(a, "--profile=")
		case strings.HasPrefix(a, "-p="):
			return strings.TrimPrefix(a, "-p=")
		case strings.HasPrefix(a, "-p"):
			// Attached short form `-pNAME` (`-p` bare and `-p=` are handled above).
			// codex short flags are single-letter, so `-pX` is unambiguously -p=X.
			return strings.TrimPrefix(a, "-p")
		case codexValueFlags[a]:
			// A DIFFERENT value-taking flag (e.g. `-m gpt`): skip its separate value
			// token so a `-p…`-looking value isn't mistaken for the profile flag.
			i += 2
			continue
		}
		i++
	}
	return ""
}

// codexRouteConfig is the routing-relevant subset of a codex config.toml (or a
// profile overlay file) — the two shapes observer can write.
type codexRouteConfig struct {
	OpenAIBaseURL  string `toml:"openai_base_url"`
	ModelProvider  string `toml:"model_provider"`
	ModelProviders map[string]struct {
		BaseURL string `toml:"base_url"`
	} `toml:"model_providers"`
}

// codexRoutedSource is one base_url codex would route through, tagged with the
// file that provided it (for the fail-closed copy).
type codexRoutedSource struct {
	url  string
	file string
}

// decodeCodexRouteConfig parses the routing subset of a codex config file,
// returning the zero value when the file is missing or unreadable (best-effort).
func decodeCodexRouteConfig(path string) codexRouteConfig {
	var cfg codexRouteConfig
	if path == "" {
		return cfg
	}
	if _, err := os.Stat(path); err != nil {
		return cfg
	}
	_, _ = toml.DecodeFile(path, &cfg)
	return cfg
}

// codexEffectiveRoutedSources returns every base_url codex would actually route
// through for a launch with the given CODEX_HOME root and active profile,
// tagged with the source file. It layers the profile overlay
// `<root>/<profile>.config.toml` ON TOP of the base `<root>/config.toml`
// (finding 3a): a non-empty profile openai_base_url / model_provider overrides
// the base, and provider tables union with the profile winning per name. Only
// the SELECTED provider's base_url is returned — an unselected provider table is
// inert and must not trigger a conflict.
func codexEffectiveRoutedSources(root, profile string) []codexRoutedSource {
	baseFile := filepath.Join(root, "config.toml")
	base := decodeCodexRouteConfig(baseFile)

	var profFile string
	var prof codexRouteConfig
	if strings.TrimSpace(profile) != "" {
		profFile = filepath.Join(root, profile+".config.toml")
		prof = decodeCodexRouteConfig(profFile)
	}

	// Top-level openai_base_url: profile wins when it sets a non-empty value.
	openaiURL, openaiSrc := strings.TrimSpace(base.OpenAIBaseURL), baseFile
	if v := strings.TrimSpace(prof.OpenAIBaseURL); v != "" {
		openaiURL, openaiSrc = v, profFile
	}

	// Selected provider: profile wins when it sets a non-empty value.
	modelProvider := strings.TrimSpace(base.ModelProvider)
	if v := strings.TrimSpace(prof.ModelProvider); v != "" {
		modelProvider = v
	}

	// Provider tables: union, profile overriding by name.
	type provSrc struct{ url, file string }
	providers := map[string]provSrc{}
	for name, p := range base.ModelProviders {
		if v := strings.TrimSpace(p.BaseURL); v != "" {
			providers[name] = provSrc{v, baseFile}
		}
	}
	for name, p := range prof.ModelProviders {
		if v := strings.TrimSpace(p.BaseURL); v != "" {
			providers[name] = provSrc{v, profFile}
		}
	}

	var out []codexRoutedSource
	if openaiURL != "" {
		out = append(out, codexRoutedSource{openaiURL, openaiSrc})
	}
	if modelProvider != "" {
		if p, ok := providers[modelProvider]; ok {
			out = append(out, codexRoutedSource{p.url, p.file})
		}
	}
	return out
}

// isLoopbackHost reports whether h is one of the interchangeable spellings of
// the local machine. The observer proxy binds loopback-only, so any of these
// with the proxy's port is the same route.
func isLoopbackHost(h string) bool {
	switch strings.ToLower(strings.TrimSpace(h)) {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

// urlRoutesToProxy reports whether got (a base_url read from config.toml) points
// at the observer proxy identified by proxyURL. It treats the loopback host
// spellings 127.0.0.1 / localhost / [::1] as equivalent, requires the same
// port, and accepts the path with or without a trailing "/v1" (trailing-slash
// tolerant). A non-loopback host or a different port never matches. When
// proxyURL is not a usable loopback URL (unexpected — the wrapper always passes
// http://127.0.0.1:<port>), it falls back to the exact accepted-string forms.
func urlRoutesToProxy(got, proxyURL string) bool {
	got = strings.TrimSpace(got)
	if got == "" {
		return false
	}
	proxyU, err := url.Parse(strings.TrimSpace(proxyURL))
	if err != nil || proxyU.Port() == "" || !isLoopbackHost(proxyU.Hostname()) {
		return matchesAnyURL(got, acceptedProxyBaseURLs(proxyURL))
	}
	gotU, err := url.Parse(got)
	if err != nil {
		return false
	}
	if !isLoopbackHost(gotU.Hostname()) || gotU.Port() != proxyU.Port() {
		return false
	}
	p := strings.TrimRight(gotU.EscapedPath(), "/")
	return p == "" || p == "/v1"
}

// codexNoProxyRouteConflict returns a non-nil error when --no-proxy-route is
// requested but a persistent $CODEX_HOME/config.toml still routes
// openai_base_url to the observer proxy. Codex 0.130+ reads that file, so
// launching would KEEP routing through the proxy DESPITE the flag — silently
// capturing turns the operator asked NOT to capture. We FAIL CLOSED (B3-1):
// refuse to launch (the caller returns a non-zero exit) and name the exact
// file(s) + key + how to revert. We deliberately do NOT inject a stock-default
// override to neutralize it — codex's effective default base URL depends on the
// auth shape (API-key vs ChatGPT-Plus JWT), so a forced value could mis-route a
// ChatGPT-Plus session. The honest refusal wins over a silent, possibly-wrong
// override. Returns nil (launch proceeds) when no config routes to the proxy.
func codexNoProxyRouteConflict(codexHomeRootsList []string, proxyURL, profile string) error {
	offenders := codexConfigsRoutingToProxy(codexHomeRootsList, proxyURL, profile)
	if len(offenders) == 0 {
		return nil
	}
	joined := strings.Join(offenders, ", ")
	return fmt.Errorf(
		"observer codex: refusing to launch under --no-proxy-route — %s still routes codex to the observer proxy (%s), either via the top-level openai_base_url key or via model_provider=%q + [model_providers.%s].base_url, and codex 0.130+ reads that file, so it would KEEP routing through the proxy and capture turns you asked not to capture. This was written by `observer codex --write-config` or `observer init` (or hand-edited); remove that routing from %s (or restore its config.toml.bak.* backup) and re-run.",
		joined, primaryAcceptedURL(acceptedProxyBaseURLs(proxyURL)),
		proxyroute.ProviderName, proxyroute.ProviderName, joined,
	)
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
