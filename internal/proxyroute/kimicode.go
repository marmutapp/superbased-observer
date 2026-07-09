package proxyroute

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// openAICompatProxyURL is the OpenAI-compatible base URL the probe-route
// writers point a tool at: the proxy's /v1 endpoint on loopback (the proxy
// refuses non-loopback connections — see internal/proxy/proxy.go).
func openAICompatProxyURL(port int) string {
	return fmt.Sprintf("http://127.0.0.1:%d/v1", port)
}

// resolveKimiCodeConfigPath returns <kimi-home>/config.toml. Precedence:
// KIMI_CODE_HOME env > homeDir/.kimi-code. The proxy/route writer runs on
// the daemon OS (Linux for the cross-OS WSL case), where kimi-code stores
// its config under ~/.kimi-code.
func resolveKimiCodeConfigPath(homeDir string) string {
	if env := strings.TrimSpace(os.Getenv("KIMI_CODE_HOME")); env != "" {
		return filepath.Join(env, "config.toml")
	}
	return filepath.Join(homeDir, ".kimi-code", "config.toml")
}

// RegisterKimiCode points kimi-code's OpenAI-compatible provider at the
// observer proxy by ADDITIVELY writing `base_url` under [providers.openai]
// in ~/.kimi-code/config.toml. kimi-code carries NO base_url key today, so
// the common path is a pure add — it NEVER touches the provider's api_key,
// the [models.*] blocks, [thinking], or any other key.
//
// Guard semantics:
//   - config file absent   → ConfigMissing (benign skip; kimi-code not set up)
//   - no [providers.openai] → Error (nothing to route)
//   - base_url absent       → add it (Added)
//   - base_url already a loopback/observer URL → AlreadySet (idempotent; a
//     different local port is left untouched, not clobbered)
//   - base_url points elsewhere → Error (refuse to overwrite a foreign URL)
//
// Backs the file up to config.toml.bak before the (additive) write, mirroring
// the codex + hermes writers' .bak discipline, then writes via temp+rename.
func (r *Registrar) RegisterKimiCode() RegistrationResult {
	path := resolveKimiCodeConfigPath(r.opts.HomeDir)
	want := openAICompatProxyURL(r.opts.ProxyPort)
	res := RegistrationResult{Tool: "kimi-code", ConfigPath: path, BaseURL: want, DryRun: r.opts.DryRun}

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			res.ConfigMissing = true
			return res
		}
		res.Error = fmt.Errorf("proxyroute.kimicode: read: %w", err)
		return res
	}
	root := map[string]any{}
	if len(raw) > 0 {
		if err := toml.Unmarshal(raw, &root); err != nil {
			res.Error = fmt.Errorf("proxyroute.kimicode: parse %s: %w", path, err)
			return res
		}
	}

	providers, _ := root["providers"].(map[string]any)
	if providers == nil {
		res.Error = fmt.Errorf("proxyroute.kimicode: %s has no [providers.openai] to route", path)
		return res
	}
	openai, _ := providers["openai"].(map[string]any)
	if openai == nil {
		res.Error = fmt.Errorf("proxyroute.kimicode: %s has no [providers.openai] to route", path)
		return res
	}

	if prior, _ := openai["base_url"].(string); strings.TrimSpace(prior) != "" {
		res.PriorBaseURL = prior
		switch {
		case prior == want:
			res.AlreadySet = true
			res.BaseURL = prior
			return res
		case IsObserverBaseURL(prior):
			// Another local observer install (maybe a different port).
			// Leave it — don't clobber a working loopback route.
			res.AlreadySet = true
			res.BaseURL = prior
			return res
		default:
			res.Error = fmt.Errorf(
				"proxyroute.kimicode: [providers.openai].base_url already set to %q; refusing to overwrite a non-observer URL", prior,
			)
			return res
		}
	}

	if r.opts.DryRun {
		res.Added = true
		return res
	}
	if err := os.WriteFile(path+".bak", raw, 0o600); err != nil {
		res.Error = fmt.Errorf("proxyroute.kimicode: backup: %w", err)
		return res
	}
	openai["base_url"] = want
	providers["openai"] = openai
	root["providers"] = providers

	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(root); err != nil {
		res.Error = fmt.Errorf("proxyroute.kimicode: encode: %w", err)
		return res
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		res.Error = fmt.Errorf("proxyroute.kimicode: write: %w", err)
		return res
	}
	if err := os.Rename(tmp, path); err != nil {
		res.Error = fmt.Errorf("proxyroute.kimicode: rename: %w", err)
		return res
	}
	res.Added = true
	return res
}
