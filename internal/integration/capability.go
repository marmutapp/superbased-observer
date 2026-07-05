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

// TranscriptTier records whether a completed session's full message
// content is re-readable from the tool's own on-disk files at handoff
// time — the session-handoff Phase 0 P0.1 classification
// (docs/plans/session-handoff-phase0-findings-2026-07-03.md), live-
// grounded 2026-07-03 on a 328-session corpus. The zero value is the
// honest floor: action-derived facts only.
type TranscriptTier string

const (
	// TranscriptActionsOnly: no grounded full-content transcript — the
	// handoff degrades to the metadata carry mode. Zero value.
	TranscriptActionsOnly TranscriptTier = ""
	// TranscriptPartial: content exists on disk but needs work or gating
	// to reconstruct (copilot's patch-log replay; antigravity's
	// CLI-readable / desktop-encrypted split).
	TranscriptPartial TranscriptTier = "partial"
	// TranscriptFull: the full message stream is re-readable now.
	TranscriptFull TranscriptTier = "full"
)

// InjectKind names a handoff delivery lane (plan §10). Dispatch is on
// this shape, never on tool name.
type InjectKind string

const (
	// InjectFile writes HANDOFF-<shortid>.md at the project root — the
	// universal floor every adapter supports.
	InjectFile InjectKind = "file"
	// InjectMCP serves the handover through the continue_session MCP tool
	// (P2; requires an Implemented MCP target).
	InjectMCP InjectKind = "mcp"
	// InjectHook arms a SessionStart additionalContext delivery (P3;
	// hook-lane budget 8KB per Phase 0 D-P0.2).
	InjectHook InjectKind = "hook"
	// InjectPrompt prepends the handover via the tool's `observer <x>`
	// launcher (P3).
	InjectPrompt InjectKind = "prompt"
)

// LaunchMode names HOW a launchable tool receives the handover when started
// in the embedded web terminal. It is a capability of the tool's CLI, not a
// tool-name branch (CLAUDE.md #3).
type LaunchMode int

const (
	// LaunchSeeded (zero value): the launcher injects the handover as the
	// tool's first interactive prompt via its promptInjection descriptor
	// (leading/trailing positional or a flag value). Requires the tool to
	// declare the InjectPrompt lane. The common case — every tool with a
	// grounded interactive-seed contract.
	LaunchSeeded LaunchMode = iota
	// LaunchDocAssisted: the tool's interactive TUI has NO initial-prompt
	// seed (e.g. hermes' `--tui` takes no message — an upstream gap), so the
	// launcher writes the handover doc (file lane) + prints a pointer, then
	// opens the interactive TUI for the user to reference/paste. An honest
	// fallback for TUIs that cannot be auto-seeded; it does NOT declare the
	// InjectPrompt lane (nothing is injected).
	LaunchDocAssisted
)

// LaunchSpec declares that a tool can be started IN-PROCESS from the
// dashboard's embedded web terminal (docs/session-handoff.md launch
// section). Subcommand is the `observer <x>` launcher verb the dashboard
// spawns in a PTY with `--continue-from <id>` — the launcher's stdio is
// wired to the PTY so the tool renders straight into the browser. It is
// present ONLY for tools whose launcher has a grounded, verified continue
// contract (a Seeded interactive-prompt seed, OR a DocAssisted TUI open); a
// non-nil Launch is the single capability the dashboard dispatches on
// (CLAUDE.md #3 — never a tool-name branch). A nil *LaunchSpec means "not
// launchable in-terminal" (the honest floor: the tool can still be handed
// off via file/MCP/hook).
type LaunchSpec struct {
	// Subcommand is the observer launcher verb (e.g. "claude", "codex",
	// "gemini", "pi"). It must match cmd/observer's continueFromLauncher
	// wiring — the registry_coverage_test pins the two can never drift.
	Subcommand string
	// Mode is how the handover reaches the launched tool. Zero value =
	// LaunchSeeded (prompt injection). LaunchDocAssisted marks a tool whose
	// TUI cannot be auto-seeded (doc written + TUI opened).
	Mode LaunchMode
}

// HandoffCapability is an adapter's session-handoff row: transcript
// readability plus the delivery lanes grounded for it. A zero value means
// actions-only carry with file delivery (the floor), never a fabricated
// capability.
type HandoffCapability struct {
	Transcript TranscriptTier
	Inject     []InjectKind
	// Launch, when non-nil, declares the tool is startable in the
	// dashboard's embedded web terminal (a LaunchSpec carrying the
	// `observer <x>` launcher verb). Nil = not launchable in-terminal.
	// Populated only for launchers with a verified --continue-from
	// contract (claude-code, codex, gemini-cli, pi).
	Launch *LaunchSpec
	// Note documents gating/partial status.
	Note string
}

// Launchable reports whether the tool can be started in the dashboard's
// embedded web terminal (a grounded LaunchSpec is present). Dashboard and
// coverage tests dispatch on this shape, never on tool name.
func (h HandoffCapability) Launchable() bool { return h.Launch != nil }

// Lanes returns the grounded delivery lanes, always including the
// universal file lane.
func (h HandoffCapability) Lanes() []InjectKind {
	for _, k := range h.Inject {
		if k == InjectFile {
			return h.Inject
		}
	}
	return append([]InjectKind{InjectFile}, h.Inject...)
}

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
