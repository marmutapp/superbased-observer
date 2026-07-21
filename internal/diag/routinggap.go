package diag

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/marmutapp/superbased-observer/internal/proxyroute"
)

// Routed-but-down gap check (usability arc P4.7 / review row L3).
//
// Live-verified 2026-06-11: a durably-routed Claude Code with the
// daemon down does NOT bypass to the real API and does NOT fail fast
// — it hangs retrying the dead proxy port (>120s, empty output),
// while hook capture keeps writing the DB directly. From inside the
// daemon "routed AND down right now" is unobservable (a down daemon
// serves no checks), so the honest in-daemon signal is the
// retrospective gap that exact behavior produces: a routed tool's
// capture kept landing in the window while ZERO proxy turns arrived.

// routedIntegration pairs one durable-routing config with the
// api_turns provider its traffic lands under. The provider pairing is
// conservative: other tools on the same provider can mask a gap, so a
// WARN here is always a real anomaly, never noise.
type routedIntegration struct {
	tool     string
	provider string
}

// checkProxyRoutingGap compares 24h capture activity against 24h
// proxy turns for every durably-routed tool.
func checkProxyRoutingGap(ctx context.Context, database *sql.DB, homeDir string) Check {
	const name = "proxy routing gap"
	if database == nil {
		return Check{Name: name, Status: StatusFail, Message: "no database handle"}
	}

	var routed []routedIntegration
	var detail []string
	if claudeRoutedToObserver(homeDir) {
		routed = append(routed, routedIntegration{tool: "claude-code", provider: "anthropic"})
	}
	if codexRoutedToObserver(homeDir) {
		routed = append(routed, routedIntegration{tool: "codex", provider: "openai"})
	}
	if len(routed) == 0 {
		return Check{Name: name, Status: StatusOK, Message: "no tools durably routed — nothing to compare"}
	}

	cutoff := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339Nano)
	gaps := 0
	for _, ri := range routed {
		var actions, turns int
		if err := database.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM actions WHERE tool = ? AND timestamp >= ?`,
			ri.tool, cutoff).Scan(&actions); err != nil {
			return Check{Name: name, Status: StatusFail, Message: "query actions: " + err.Error()}
		}
		if err := database.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM api_turns WHERE provider = ? AND timestamp >= ?`,
			ri.provider, cutoff).Scan(&turns); err != nil {
			return Check{Name: name, Status: StatusFail, Message: "query api_turns: " + err.Error()}
		}
		switch {
		case actions > 0 && turns == 0:
			gaps++
			detail = append(detail,
				fmt.Sprintf("%s: %d captured actions but 0 %s proxy turns in 24h — the proxy was unreachable while the tool ran", ri.tool, actions, ri.provider),
				"a routed tool with the daemon down HANGS (no bypass, no fast fail — live-verified); keep the daemon running or remove the route",
				"fix: `observer start`, or undo routing via `observer uninstall --"+ri.tool+"` / the Compression page")
		case actions == 0:
			detail = append(detail, fmt.Sprintf("%s: idle in the last 24h (nothing to compare)", ri.tool))
		default:
			detail = append(detail, fmt.Sprintf("%s: %d actions / %d %s proxy turns in 24h", ri.tool, actions, turns, ri.provider))
		}
	}
	if gaps > 0 {
		return Check{
			Name:    name,
			Status:  StatusWarn,
			Message: fmt.Sprintf("%d routed tool(s) captured activity with zero proxy traffic in 24h", gaps),
			Details: detail,
		}
	}
	return Check{Name: name, Status: StatusOK, Message: "routed tools show proxy traffic consistent with capture", Details: detail}
}

// claudeRoutedToObserver reads <home>/.claude/settings.json and
// reports whether its env.ANTHROPIC_BASE_URL points at an observer
// loopback proxy. Read-only, tolerant: any parse problem reads as
// not-routed.
func claudeRoutedToObserver(homeDir string) bool {
	raw, err := os.ReadFile(filepath.Join(homeDir, ".claude", "settings.json"))
	if err != nil {
		return false
	}
	var settings struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		return false
	}
	return proxyroute.IsObserverBaseURL(settings.Env["ANTHROPIC_BASE_URL"])
}

// codexRoutedToObserver reads <home>/.codex/config.toml and reports
// whether the active model_provider's base_url points at an observer
// loopback proxy. Read-only, tolerant.
func codexRoutedToObserver(homeDir string) bool {
	raw, err := os.ReadFile(filepath.Join(homeDir, ".codex", "config.toml"))
	if err != nil {
		return false
	}
	var cfg struct {
		ModelProvider  string `toml:"model_provider"`
		ModelProviders map[string]struct {
			BaseURL string `toml:"base_url"`
		} `toml:"model_providers"`
	}
	if err := toml.Unmarshal(raw, &cfg); err != nil {
		return false
	}
	mp, ok := cfg.ModelProviders[cfg.ModelProvider]
	return ok && proxyroute.IsObserverBaseURL(mp.BaseURL)
}
