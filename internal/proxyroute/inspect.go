package proxyroute

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// inspect.go — read-only route inspectors for the Arc 4 P6b managed-integrity
// probe (plan §9). They report whether a managed node's AI-tool proxy routes
// still point at an observer proxy or have DRIFTED away (repointed at a
// non-loopback host = a bypass of the managed proxy). They never write, and
// they return only a coarse state per tool — never the config value — so the
// caller can build the content-floored managed-integrity wire.
//
// EVIDENCE, not prevention: a developer who owns the machine can edit these
// configs back and forth; this catches the ordinary repoint and feeds the admin
// a signal (§5 MDM gate is the actual lock).

// RouteState classifies one tool's current proxy-route posture.
type RouteState string

const (
	// RouteOurs: the tool points at an observer proxy (any loopback port).
	RouteOurs RouteState = "ours"
	// RouteDrifted: the tool points at a non-loopback host — a bypass of the
	// managed proxy (repointed at a third-party endpoint or direct upstream).
	RouteDrifted RouteState = "drifted"
	// RouteAbsent: no route configured (config or key missing). A tool that was
	// never routed is not drift; it is simply not captured.
	RouteAbsent RouteState = "absent"
)

// RouteStatus is one tool's inspected route posture. Tool is the adapter
// identity (e.g. "claude-code", "codex") — not secret.
type RouteStatus struct {
	Tool  string
	State RouteState
}

// classifyBaseURL maps a raw base-URL string to a RouteState. An empty value is
// Absent; a loopback observer URL (any port — matching the Register* tolerance
// for a second install on a different port) is Ours; anything else is Drifted.
func classifyBaseURL(raw string) RouteState {
	if raw == "" {
		return RouteAbsent
	}
	if IsObserverBaseURL(raw) {
		return RouteOurs
	}
	return RouteDrifted
}

// InspectClaudeRoute reads env.ANTHROPIC_BASE_URL from
// <homeDir>/.claude/settings.json (the value RegisterClaudeCode writes) and
// classifies it. A missing file, missing env block, or missing key is Absent.
func InspectClaudeRoute(homeDir string) RouteStatus {
	res := RouteStatus{Tool: "claude-code", State: RouteAbsent}
	raw, err := os.ReadFile(filepath.Join(homeDir, ".claude", "settings.json"))
	if err != nil {
		return res
	}
	var settings struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		return res
	}
	res.State = classifyBaseURL(settings.Env["ANTHROPIC_BASE_URL"])
	return res
}

// InspectCodexRoute resolves the base_url of the provider that
// <homeDir>/.codex/config.toml's top-level model_provider points at and
// classifies it. No model_provider, no matching provider block, or no base_url
// is Absent; a non-loopback base_url is Drifted.
func InspectCodexRoute(homeDir string) RouteStatus {
	res := RouteStatus{Tool: "codex", State: RouteAbsent}
	raw, err := os.ReadFile(filepath.Join(homeDir, ".codex", "config.toml"))
	if err != nil {
		return res
	}
	root := map[string]any{}
	if err := toml.Unmarshal(raw, &root); err != nil {
		return res
	}
	mp, _ := root["model_provider"].(string)
	if mp == "" {
		return res
	}
	providers, _ := root["model_providers"].(map[string]any)
	block, _ := providers[mp].(map[string]any)
	base, _ := block["base_url"].(string)
	res.State = classifyBaseURL(base)
	return res
}

// InspectRoutes runs every read-only route inspector for homeDir and returns
// one RouteStatus per tool. The managed-integrity probe counts the Drifted ones.
func InspectRoutes(homeDir string) []RouteStatus {
	return []RouteStatus{
		InspectClaudeRoute(homeDir),
		InspectCodexRoute(homeDir),
	}
}
