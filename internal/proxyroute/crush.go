package proxyroute

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// resolveCrushConfigPath returns the platform-native global Crush config
// (crush.json). Precedence: CRUSH_CONFIG env (a full file path) >
// $XDG_CONFIG_HOME/crush/crush.json > homeDir/.config/crush/crush.json.
// Crush (charmbracelet/crush) stores its global config under the XDG config
// dir on Linux/macOS; the proxy/route writer runs on the daemon OS. (The
// project-local .crush/crush.json is a per-repo override, out of scope for
// a global route write.)
func resolveCrushConfigPath(homeDir string) string {
	if env := strings.TrimSpace(os.Getenv("CRUSH_CONFIG")); env != "" {
		return env
	}
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		return filepath.Join(xdg, "crush", "crush.json")
	}
	return filepath.Join(homeDir, ".config", "crush", "crush.json")
}

// RegisterCrush points Crush's OpenAI-compatible provider at the observer
// proxy by ADDITIVELY setting `base_url` on the `providers.openai` entry in
// crush.json. It preserves every other key semantically (round-trips the
// whole document through map[string]any) and NEVER reads, writes, or moves
// an API key — crush.json embeds literal provider keys, so only the
// base_url field of the openai provider is touched.
//
// Guard semantics mirror RegisterKimiCode:
//   - config file absent      → ConfigMissing (benign skip)
//   - no providers.openai     → Error (nothing to route)
//   - base_url absent         → add it (Added)
//   - base_url already loopback/observer → AlreadySet (idempotent)
//   - base_url points elsewhere → Error (refuse to overwrite a foreign URL)
//
// Backs the file up to crush.json.bak before the write, then writes via
// temp+rename.
func (r *Registrar) RegisterCrush() RegistrationResult {
	path := resolveCrushConfigPath(r.opts.HomeDir)
	want := openAICompatProxyURL(r.opts.ProxyPort)
	res := RegistrationResult{Tool: "crush", ConfigPath: path, BaseURL: want, DryRun: r.opts.DryRun}

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			res.ConfigMissing = true
			return res
		}
		res.Error = fmt.Errorf("proxyroute.crush: read: %w", err)
		return res
	}
	root := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &root); err != nil {
			res.Error = fmt.Errorf("proxyroute.crush: parse %s: %w", path, err)
			return res
		}
	}

	providers, _ := root["providers"].(map[string]any)
	if providers == nil {
		res.Error = fmt.Errorf("proxyroute.crush: %s has no providers.openai to route", path)
		return res
	}
	openai, _ := providers["openai"].(map[string]any)
	if openai == nil {
		res.Error = fmt.Errorf("proxyroute.crush: %s has no providers.openai to route", path)
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
			res.AlreadySet = true
			res.BaseURL = prior
			return res
		default:
			res.Error = fmt.Errorf(
				"proxyroute.crush: providers.openai.base_url already set to %q; refusing to overwrite a non-observer URL", prior,
			)
			return res
		}
	}

	if r.opts.DryRun {
		res.Added = true
		return res
	}
	if err := os.WriteFile(path+".bak", raw, 0o600); err != nil {
		res.Error = fmt.Errorf("proxyroute.crush: backup: %w", err)
		return res
	}
	openai["base_url"] = want
	providers["openai"] = openai
	root["providers"] = providers

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		res.Error = fmt.Errorf("proxyroute.crush: encode: %w", err)
		return res
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		res.Error = fmt.Errorf("proxyroute.crush: write: %w", err)
		return res
	}
	if err := os.Rename(tmp, path); err != nil {
		res.Error = fmt.Errorf("proxyroute.crush: rename: %w", err)
		return res
	}
	res.Added = true
	return res
}
