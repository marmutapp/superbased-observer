package proxyroute

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// hermesConfigFile is the per-home Hermes config file (mirrors
// internal/hook.HermesConfigFile).
const hermesConfigFile = "config.yaml"

// resolveHermesConfigPath returns <hermes-home>/config.yaml. Precedence:
// HERMES_HOME env > homeDir/.hermes. (The Windows %LOCALAPPDATA%\hermes
// case is handled by the hook package's resolver; the proxy/route writer
// runs on the daemon OS, which for the cross-OS WSL case is Linux.)
func resolveHermesConfigPath(homeDir string) string {
	if env := strings.TrimSpace(os.Getenv("HERMES_HOME")); env != "" {
		return filepath.Join(env, hermesConfigFile)
	}
	return filepath.Join(homeDir, ".hermes", hermesConfigFile)
}

// deriveHermesUpstream splits a provider base URL (e.g.
// "https://openrouter.ai/api/v1") into the routing id ("openrouter"), the
// upstream host root for [proxy.upstreams] ("https://openrouter.ai"), and
// the path tail carried in the /up/<id> prefix ("/api/v1"). The split keeps
// the forwarded URL byte-identical to the direct call regardless of whether
// the base ends in /v1: the proxy strips /up/<id>, leaving <tail>/<client
// suffix>, which joins onto the host root. Returns ok=false for a URL with
// no usable scheme+host.
func deriveHermesUpstream(baseURL string) (id, upstreamRoot, pathTail string, ok bool) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", "", "", false
	}
	host := u.Hostname()
	label := host
	if i := strings.IndexByte(host, '.'); i > 0 {
		label = host[:i]
	}
	id = sanitizeRouteID(label)
	if id == "" {
		return "", "", "", false
	}
	upstreamRoot = u.Scheme + "://" + u.Host
	pathTail = strings.TrimRight(u.Path, "/") // "/api/v1" or ""
	return id, upstreamRoot, pathTail, true
}

// sanitizeRouteID keeps a route id to [a-z0-9-] so it is a clean single
// path segment.
func sanitizeRouteID(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// RegisterHermes points Hermes' model.base_url at the observer proxy using a
// /up/<id> prefix (Phase C), so its OpenRouter-bound (OpenAI-shaped) traffic
// is captured + compressed. It NEVER touches api keys, env, or any sibling
// config key — only model.base_url, and only after backing the file up to
// config.yaml.bak.
//
// It requires an EXISTING model.base_url to route: that value names the real
// upstream (e.g. https://openrouter.ai/api/v1), which the result reports as
// UpstreamRoot/UpstreamID so the operator can add the matching
// [proxy.upstreams] line to observer's own config. Without a base_url there
// is nothing to route (hermes is on a default provider), so it errors with
// guidance instead of guessing.
//
// AlreadySet (no write) when model.base_url is already a 127.0.0.1 observer
// URL. The result's Added=true means model.base_url was rewritten.
func (r *Registrar) RegisterHermes() RegistrationResult {
	path := resolveHermesConfigPath(r.opts.HomeDir)
	res := RegistrationResult{Tool: "hermes", ConfigPath: path, DryRun: r.opts.DryRun}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			res.Error = fmt.Errorf("proxyroute.hermes: %s not found — configure Hermes (model.base_url) before routing it", path)
			return res
		}
		res.Error = fmt.Errorf("proxyroute.hermes: read: %w", err)
		return res
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		res.Error = fmt.Errorf("proxyroute.hermes: parse %s: %w", path, err)
		return res
	}
	if doc == nil {
		doc = map[string]any{}
	}

	model, _ := doc["model"].(map[string]any)
	if model == nil {
		model = map[string]any{}
	}
	prior, _ := model["base_url"].(string)
	prior = strings.TrimSpace(prior)
	if prior == "" {
		res.Error = fmt.Errorf("proxyroute.hermes: %s has no model.base_url to route — set your provider's base_url first, then re-run", path)
		return res
	}
	res.PriorBaseURL = prior

	// Already routed through a local observer? Leave it (don't double-wrap).
	if IsObserverBaseURL(prior) {
		res.AlreadySet = true
		res.BaseURL = prior
		return res
	}

	id, upstreamRoot, pathTail, ok := deriveHermesUpstream(prior)
	if !ok {
		res.Error = fmt.Errorf("proxyroute.hermes: model.base_url %q is not a usable http(s) URL", prior)
		return res
	}
	want := fmt.Sprintf("http://127.0.0.1:%d/up/%s%s", r.opts.ProxyPort, id, pathTail)
	res.BaseURL = want
	res.UpstreamID = id
	res.UpstreamRoot = upstreamRoot

	if r.opts.DryRun {
		res.Added = true
		return res
	}

	// Back up before the destructive base_url overwrite (mirrors the codex
	// writer's .bak discipline) so the prior provider URL is recoverable.
	if err := os.WriteFile(path+".bak", data, 0o600); err != nil {
		res.Error = fmt.Errorf("proxyroute.hermes: backup: %w", err)
		return res
	}
	model["base_url"] = want
	doc["model"] = model
	out, err := yaml.Marshal(doc)
	if err != nil {
		res.Error = fmt.Errorf("proxyroute.hermes: encode: %w", err)
		return res
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		res.Error = fmt.Errorf("proxyroute.hermes: write: %w", err)
		return res
	}
	if err := os.Rename(tmp, path); err != nil {
		res.Error = fmt.Errorf("proxyroute.hermes: rename: %w", err)
		return res
	}
	res.Added = true
	return res
}

// HermesHint returns no-mutation guidance for routing Hermes through the
// proxy: the two coordinated edits (model.base_url + the [proxy.upstreams]
// line derived from the current provider URL). priorBaseURL is the operator's
// current model.base_url; when empty, the hint explains it must be set first.
func HermesHint(port int, priorBaseURL string) string {
	var b strings.Builder
	fmt.Fprintln(&b, "next: route Hermes through the observer proxy (OpenRouter-bound traffic).")
	if strings.TrimSpace(priorBaseURL) == "" {
		fmt.Fprintln(&b, "  Hermes has no model.base_url set — configure your provider first, then re-run.")
		return b.String()
	}
	id, upstreamRoot, pathTail, ok := deriveHermesUpstream(priorBaseURL)
	if !ok {
		fmt.Fprintf(&b, "  current model.base_url %q is not a usable URL.\n", priorBaseURL)
		return b.String()
	}
	fmt.Fprintf(&b, "  1) add to observer config.toml:\n")
	fmt.Fprintf(&b, "       [proxy.upstreams]\n")
	fmt.Fprintf(&b, "       %s = %q\n", id, upstreamRoot)
	fmt.Fprintf(&b, "  2) set in ~/.hermes/config.yaml under model:\n")
	fmt.Fprintf(&b, "       base_url: http://127.0.0.1:%d/up/%s%s\n", port, id, pathTail)
	fmt.Fprintln(&b, "  (keep your api_key untouched; observer never reads it)")
	return b.String()
}
