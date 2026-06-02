// Package proxyroute writes proxy-routing configuration into AI coding
// tools' own config files so their API requests transit the observer's
// reverse proxy. Today it covers Codex (~/.codex/config.toml's
// [model_providers.openai] base_url). Other tools route via env vars
// (Claude Code's ANTHROPIC_BASE_URL) and remain hint-only — see
// printProxyRoutingHint in cmd/observer/init.go.
package proxyroute
