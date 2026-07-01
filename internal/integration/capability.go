package integration

// This file holds the Phase-0-discovery capability vocabulary added on top
// of the proxy-route seed in integration.go. Every type here is DATA: a
// named-constant enum or a small value struct. No behaviour, no I/O — the
// writers that consume these (cmd/observer init, internal/hook register,
// the MCP registrar, the cross-adapter doctor) live at the boundary and
// dispatch on the capability SHAPE, never on tool name (CLAUDE.md rule #3).
//
// Honesty convention (operator directive, 2026-06-26): cells are sourced
// from in-repo adapter code + docs. A ZERO value means "no grounded
// capability" — which is EITHER genuinely unsupported (e.g. cursor talks
// only to its own backend → no proxy route) OR pending a capability-
// discovery spike against a live install. The two are distinguished by the
// per-adapter comments in the registry, not by a magic value: we do NOT
// fabricate a capability we could not ground.

// HookMechanism names how observer hooks attach to a tool's own config, or
// HookNone for the watcher/SQLite-only adapters (the majority). Each
// non-None value maps to exactly one format-writer that Phase 2 will
// dispatch to from a Capabilities() walk, replacing the hardcoded
// switch in internal/hook/register.go.
type HookMechanism string

const (
	// HookNone: captured via the watcher (+ SQLite backfill) only; no hook
	// config is written into the tool. This is the honest default for most
	// adapters, not a missing feature.
	HookNone HookMechanism = ""
	// HookClaudeSettings: Claude Code's ~/.claude/settings.json "hooks"
	// block (registerClaudeCode). Carries the Windows wsl.exe bridge
	// variant — see CrossOSBridge on HookSpec.
	HookClaudeSettings HookMechanism = "claude_settings_json"
	// HookCursor: Cursor's hooks config (registerCursor).
	HookCursor HookMechanism = "cursor_hooks"
	// HookCodexConfig: Codex's ~/.codex/config.toml [features].hooks
	// (registerCodex); verified flag name is [features].hooks, NOT
	// codex_hooks (project_codex_hook_envelope memory).
	HookCodexConfig HookMechanism = "codex_config_toml"
	// HookHermesPlugin: Hermes' embedded Python plugin written under
	// ~/.hermes plus the plugins.enabled allow-list entry (RegisterHermes
	// + RegisterHermesPluginEnabled). A genuinely per-vendor format.
	HookHermesPlugin HookMechanism = "hermes_embedded_plugin"
	// HookClineCLIJSONL: Cline CLI's opt-in hooks.jsonl tail. The receiver
	// code exists (internal/adapter/clinecli/hook.go) but is NOT yet
	// auto-wired by init — one of the two "receiver exists, not wired"
	// items the 2026-06-26 review names; Phase 2 closes it.
	HookClineCLIJSONL HookMechanism = "cline_cli_hooks_jsonl"
)

// HookSpec describes a tool's hook-registration capability. A zero-value
// HookSpec (Mechanism == HookNone) means watcher/SQLite-only. CrossOSBridge
// records that the tool, when registered from a foreign OS (Windows AI tool
// + WSL daemon), must register a `wsl.exe -d <distro> -- <observer> hook …`
// bridge so the hook executes in the daemon's OS-context (CLAUDE.md hook-
// registration note). It is a CAPABILITY FLAG, not a tool branch.
type HookSpec struct {
	Mechanism     HookMechanism
	CrossOSBridge bool
	// AutoWired is false for a mechanism whose receiver exists but init does
	// not yet register it (cline-cli today). Lets the doctor report "capable
	// but not auto-wired" honestly instead of claiming coverage.
	AutoWired bool
}

// MCPFormat names the on-disk shape a client uses to store MCP server
// config. Phase 1 reuses ONE writer per format across every client that
// shares it (the agnostic win): the JSON {"mcpServers":{…}} object is
// near-universal; codex and hermes are the two per-vendor exceptions.
type MCPFormat string

const (
	// MCPServersJSON: the shared {"mcpServers": {…}} JSON object used by
	// Claude Code (~/.claude.json) and Cursor (~/.cursor/mcp.json), and the
	// likely shape for several other clients pending Phase-1 confirmation.
	MCPServersJSON MCPFormat = "mcp_servers_json"
	// MCPCodexTOML: Codex's [mcp_servers] table in ~/.codex/config.toml.
	MCPCodexTOML MCPFormat = "codex_config_toml"
	// MCPHermesYAML: Hermes' mcp_servers map in ~/.hermes/config.yaml.
	MCPHermesYAML MCPFormat = "hermes_config_yaml"
	// MCPOpenCodeJSON: OpenCode's own "mcp" object in
	// ~/.config/opencode/opencode.json — typed local-command servers
	// ({"type":"local","command":[…],"enabled":true}), NOT the shared
	// {"mcpServers":{…}} shape. Has its own writer (registerOpenCodeJSON).
	MCPOpenCodeJSON MCPFormat = "opencode_json"
)

// MCPTarget records where/how a client stores MCP server config. A nil
// *MCPTarget on a Capability means "no grounded MCP target" (the client
// has no MCP support, OR support is unconfirmed pending Phase-1 discovery —
// see the per-adapter registry comment). Implemented distinguishes the
// clients observer can write TODAY (a registrar/init writer exists) from
// any future Phase-1 candidate added as a data row before its writer.
type MCPTarget struct {
	Format MCPFormat
	// PathHint documents the config location relative to the user's home
	// (e.g. ".claude.json", ".cursor/mcp.json") for the doctor/matrix; the
	// authoritative path resolution stays in internal/mcp/locate.
	PathHint string
	// Implemented is true when init/the MCP registrar can write this target
	// now (claude-code, cursor, codex, hermes). false marks a client we
	// have grounded as MCP-capable but not yet wired a writer for.
	Implemented bool
	// CrossOSBridge marks a client whose MCP config can ALSO be written from
	// a foreign-OS daemon via a `wsl.exe -d <distro> -- <linux-bin>` bridge
	// command (mirrors HookSpec.CrossOSBridge). When true, the `<tool>-
	// windows` virtual target resolves the Windows-side config path through
	// crossmount and writes the bridge command, so a Windows VS Code client
	// (e.g. Cline) can reach a WSL-resident observer MCP server over stdio.
	// A capability FLAG, not a tool branch (CLAUDE.md #3).
	CrossOSBridge bool
}

// NativeRails is the three-rail native-console telemetry bitset
// (docs/native-console-integration-template.md). Most adapters are small
// vendors with no admin/usage API → every field false (enrollment-only,
// correct, not a hole). Phase 4 keeps this as a LEDGER only; no new vendor
// poller is built this work-stream (operator decision 2026-06-26).
type NativeRails struct {
	// A = native node telemetry (usage-export / OTel).
	A bool
	// B = managed-config distribution (managed-settings / MDM).
	B bool
	// C = org analytics API (server-side poller).
	C bool
	// Note documents gating/partial status (e.g. codex Rail A config-gated,
	// copilot rails partial).
	Note string
}

// Any reports whether at least one native-console rail exists for the tool.
func (n NativeRails) Any() bool { return n.A || n.B || n.C }

// TokenTier records the best available token/cost capture tier for a tool
// plus any honest known gap. It is the ledger Phase 5 measures shrinkage
// against; the cost ENGINE (cost.ComputeBreakdown) is already agnostic, so
// only this capture/parse layer is per-adapter.
type TokenTier struct {
	// Best names the strongest capture source: "proxy" (api_turns wall-clock
	// + exact usage), "debug_log", "events_jsonl", "sqlite", "transcript",
	// "proto" (decrypt-gated). "" = unknown / un-audited.
	Best string
	// Gap is a short honest description of a known hole ("" = no known gap):
	// e.g. "no cache tier", "model often blank", "decrypt-gated",
	// "sparse task tokens", "OpenAI-gross net-vs-cached fix pending".
	Gap string
}
