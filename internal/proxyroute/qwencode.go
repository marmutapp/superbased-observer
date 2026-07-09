package proxyroute

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// qwenDefaultBaseURL is the stock value Qwen Code ships in
// ~/.qwen/settings.json's model.baseUrl. The writer only rewrites this
// known default (or an existing observer URL) — never a custom host.
const qwenDefaultBaseURL = "https://api.openai.com/v1"

// resolveQwenConfigPath returns <qwen-home>/settings.json. Precedence:
// QWEN_HOME env > homeDir/.qwen.
func resolveQwenConfigPath(homeDir string) string {
	if env := strings.TrimSpace(os.Getenv("QWEN_HOME")); env != "" {
		return filepath.Join(env, "settings.json")
	}
	return filepath.Join(homeDir, ".qwen", "settings.json")
}

// RegisterQwenCode points Qwen Code at the observer proxy by rewriting
// `model.baseUrl` in ~/.qwen/settings.json. Unlike the other probe writers
// this MODIFIES an existing key: live-grounded 2026-07-10, the OPENAI_BASE_URL
// env knob is INERT (settings.json's model.baseUrl takes precedence and pins
// the host), so the persisted baseUrl is the only working lane.
//
// Because a rewrite is destructive, the guard is stricter than the add-only
// writers — it rewrites ONLY when the current value is safe to replace:
//   - config file absent                     → ConfigMissing (benign skip)
//   - no model.baseUrl                        → Error (nothing to route)
//   - baseUrl == the known default (api.openai.com/v1) → rewrite (Added)
//   - baseUrl already this observer URL       → AlreadySet (idempotent)
//   - baseUrl already a different loopback URL → AlreadySet (don't clobber
//     another local observer install)
//   - baseUrl is any other custom host        → Error (refuse)
//
// It NEVER touches the api key. Backs the file up to settings.json.bak, then
// writes via temp+rename (preserving every other settings key).
func (r *Registrar) RegisterQwenCode() RegistrationResult {
	path := resolveQwenConfigPath(r.opts.HomeDir)
	want := openAICompatProxyURL(r.opts.ProxyPort)
	res := RegistrationResult{Tool: "qwen-code", ConfigPath: path, BaseURL: want, DryRun: r.opts.DryRun}

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			res.ConfigMissing = true
			return res
		}
		res.Error = fmt.Errorf("proxyroute.qwencode: read: %w", err)
		return res
	}
	root := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &root); err != nil {
			res.Error = fmt.Errorf("proxyroute.qwencode: parse %s: %w", path, err)
			return res
		}
	}

	model, _ := root["model"].(map[string]any)
	if model == nil {
		res.Error = fmt.Errorf("proxyroute.qwencode: %s has no model.baseUrl to route", path)
		return res
	}
	prior, _ := model["baseUrl"].(string)
	prior = strings.TrimSpace(prior)
	if prior == "" {
		res.Error = fmt.Errorf("proxyroute.qwencode: %s has no model.baseUrl to route", path)
		return res
	}
	res.PriorBaseURL = prior

	switch {
	case prior == want:
		res.AlreadySet = true
		res.BaseURL = prior
		return res
	case IsObserverBaseURL(prior):
		// Another local observer install (maybe a different port). Leave it.
		res.AlreadySet = true
		res.BaseURL = prior
		return res
	case prior != qwenDefaultBaseURL:
		res.Error = fmt.Errorf(
			"proxyroute.qwencode: model.baseUrl is %q (a custom host), not the known default %q; refusing to overwrite",
			prior, qwenDefaultBaseURL,
		)
		return res
	}

	if r.opts.DryRun {
		res.Added = true
		return res
	}
	if err := os.WriteFile(path+".bak", raw, 0o600); err != nil {
		res.Error = fmt.Errorf("proxyroute.qwencode: backup: %w", err)
		return res
	}
	model["baseUrl"] = want
	root["model"] = model

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		res.Error = fmt.Errorf("proxyroute.qwencode: encode: %w", err)
		return res
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		res.Error = fmt.Errorf("proxyroute.qwencode: write: %w", err)
		return res
	}
	if err := os.Rename(tmp, path); err != nil {
		res.Error = fmt.Errorf("proxyroute.qwencode: rename: %w", err)
		return res
	}
	res.Added = true
	return res
}
