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
// Rewriting model.baseUrl ALONE is insufficient on the live build: Qwen Code
// resolves the active model by the (id, baseUrl) pair, so once model.baseUrl is
// the proxy URL it needs a matching modelProviders entry with the SAME baseUrl,
// else it warns "no longer matches any provider … using the first id match" and
// goes direct to api.openai.com, bypassing the proxy (grounded NEGATIVE probe
// 2026-07-09). So on the rewrite path the writer ALSO retargets every
// modelProviders openai-lane entry currently on the known default host to the
// proxy URL — carrying each entry's own id + envKey untouched (the operator's
// real OpenAI key still forwards through the proxy). Providers on other hosts
// (deepseek / z.ai / dashscope) are left alone.
//
// It NEVER touches an api key or an envKey. Backs the file up to
// settings.json.bak, then writes via temp+rename (preserving every other key).
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

	// Make the rewritten model.baseUrl resolvable: retarget every openai-lane
	// provider on the known default host to the proxy URL (keeps id + envKey).
	// If none matched (e.g. an empty modelProviders map), synthesize a matching
	// entry for the selected model so it still resolves against the proxy.
	matched := retargetQwenOpenAIProviders(root, qwenDefaultBaseURL, want)
	if matched == 0 {
		ensureQwenProxyProvider(root, model, want)
	}

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

// qwenOpenAIProviderList returns the mutable slice of openai-lane provider
// entries in the parsed settings map, tolerating both live schema shapes:
// the array form (modelProviders.openai = [ {...}, ... ], the operator's live
// build) and the newer object form (modelProviders.openai = {protocol, models:
// [...]}). It returns nil when no openai lane is present. The returned slice
// aliases the map's backing storage, so mutating an entry's fields mutates the
// document in place (map/slice values in map[string]any are references).
func qwenOpenAIProviderList(root map[string]any) []any {
	mp, _ := root["modelProviders"].(map[string]any)
	if mp == nil {
		return nil
	}
	switch v := mp["openai"].(type) {
	case []any:
		return v
	case map[string]any:
		if models, ok := v["models"].([]any); ok {
			return models
		}
	}
	return nil
}

// retargetQwenOpenAIProviders rewrites the baseUrl of every openai-lane provider
// entry whose baseUrl currently equals from → to, leaving each entry's id, name,
// envKey, and any unknown fields untouched. It returns the number of entries
// changed. Providers on any other host are not touched.
func retargetQwenOpenAIProviders(root map[string]any, from, to string) int {
	changed := 0
	for _, e := range qwenOpenAIProviderList(root) {
		entry, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if b, _ := entry["baseUrl"].(string); strings.TrimSpace(b) == from {
			entry["baseUrl"] = to
			changed++
		}
	}
	return changed
}

// ensureQwenProxyProvider appends a minimal openai-lane provider entry pointing
// at the proxy URL for the currently-selected model, so that a config with no
// openai-default provider (e.g. an empty modelProviders map) still resolves
// model.baseUrl. The entry's id mirrors model.name (how Qwen Code matches a
// provider to the active model); envKey defaults to OPENAI_API_KEY. It creates
// the modelProviders map / openai array as needed and preserves the existing
// schema shape (array vs {models:[]}). No-op if it cannot determine the shape
// to write into.
func ensureQwenProxyProvider(root map[string]any, model map[string]any, proxyURL string) {
	id, _ := model["name"].(string)
	id = strings.TrimSpace(id)
	if id == "" {
		id = "observer-proxy"
	}
	entry := map[string]any{
		"id":      id,
		"name":    id,
		"baseUrl": proxyURL,
		"envKey":  "OPENAI_API_KEY",
	}

	mp, _ := root["modelProviders"].(map[string]any)
	if mp == nil {
		mp = map[string]any{}
		root["modelProviders"] = mp
	}
	switch v := mp["openai"].(type) {
	case []any:
		mp["openai"] = append(v, entry)
	case map[string]any:
		models, _ := v["models"].([]any)
		v["models"] = append(models, entry)
	default:
		// Absent openai lane: seed the array form the operator's build uses.
		mp["openai"] = []any{entry}
	}
}
