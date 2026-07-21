package clinecli

import (
	"strings"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// normalizeToolName maps a Cline CLI tool name (the `name` field on a
// `tool_use` content block, or the `tool_call.name` value inside a
// hooks.jsonl payload) onto the normalized action type the rest of
// observer consumes.
//
// Mapping table source: docs/plans/cline-cli-adapter-plan-2026-06-06.md
// §5 — 10 core tools + 18 team_* coordination tools. Plus prefix
// rules for the open-ended `mcp_<server>_<tool>` taxonomy that
// Cline CLI's MCP client exposes (cline_mcp_settings.json's registered
// servers prepend `mcp_<server>_` to every tool they surface).
//
// Note the plural names: Cline CLI ships `read_files` (batch), not
// `read_file`. `run_commands` (batch), not `run_command`. `apply_patch`
// (multi-hunk). The single-form names belong to the cline-vscode
// extension — DON'T be tempted to share a mapping with
// internal/adapter/cline/. The two products' tool taxonomies are
// genuinely different surfaces. See the doc.go cross-reference.
//
// Unknown tools fall through to ActionMCPCall (cursor + hermes
// precedent — most user-added tools are MCP-shaped). The caller
// preserves the raw name in ToolEvent.RawToolName so the dashboard
// can still surface it verbatim.
//
// All comparisons are case-sensitive lowercase; Cline CLI tool names
// are lower_snake_case by convention.
func normalizeToolName(raw string) string {
	// Prefix rules first — they win over literal matches when both
	// would apply (none of the literal mappings overlap with these
	// prefixes today).
	if strings.HasPrefix(raw, "mcp_") {
		return models.ActionMCPCall
	}

	switch raw {
	// --- Core tools (10) -------------------------------------------------
	// These are the always-on tools the Cline CLI core registers. See
	// the @cline/core source for the canonical list.
	case "read_files":
		// Batch read — input shape is {files: [{path: ...}, ...]}.
		// Plural name is intentional; not a typo of the cline-vscode
		// extension's `read_file`.
		return models.ActionReadFile
	case "run_commands":
		// Batch terminal exec — input shape is {commands: [{command: ...}, ...]}.
		return models.ActionRunCommand
	case "apply_patch":
		// Multi-hunk patch tool — input shape carries one or more
		// {path, old_string, new_string} structured edits.
		return models.ActionEditFile
	case "editor":
		// New-file or full-rewrite editor primitive. Plan §5 notes it
		// "dispatches by sub-arg; some calls are new-file → write_file"
		// — until parse-time sub-arg dispatch lands (Phase 2 commit 6),
		// default the action type to ActionEditFile and let the
		// content-block walker upgrade to ActionWriteFile when the
		// input shape signals a new-file write.
		return models.ActionEditFile
	case "search_codebase":
		// Project-wide search — distinct from cline-vscode's
		// `search_files` (regex over file bodies) and `list_files`
		// (directory listing). Cline CLI fuses both into the single
		// `search_codebase` primitive. Map to ActionSearchFiles
		// (file-discovery) since the input typically carries a
		// pattern + a starting path; a future ActionSearchText
		// upgrade can split it if the input is content-shaped.
		return models.ActionSearchFiles
	case "fetch_web_content":
		return models.ActionWebFetch
	case "ask_question":
		// Assistant asks the operator a clarifying question before
		// continuing. ActionAskUser (NOT ActionUserPrompt — that's
		// the inbound user-side prompt). Matches the cline-vscode
		// adapter's mapping of `ask_followup_question`.
		return models.ActionAskUser
	case "skills":
		// User-installed pluggable skills. MCP-shaped.
		return models.ActionMCPCall
	case "spawn_agent":
		// Spawn a sub-agent inside the same Cline CLI process.
		// Distinct from team_spawn_teammate (which spawns a
		// teammate in the surrounding team runtime). Both map to
		// ActionSpawnSubagent because both create a child agent.
		return models.ActionSpawnSubagent
	case "submit_and_exit":
		// Terminal success signal — the agent submits its final
		// answer and the session ends.
		return models.ActionTaskComplete

	// --- Team tools (18) -------------------------------------------------
	// Cline CLI's team runtime exposes 18 inter-agent coordination
	// primitives. For v1 they all route to ActionMCPCall (or
	// ActionSpawnSubagent in the one spawn case) because they're
	// inter-agent plumbing, not user-visible file/command ops. v2
	// could surface dedicated team-comm action types (team_message,
	// team_run_task, team_outcome) if dashboard demand emerges.
	case "team_spawn_teammate":
		// Like spawn_agent but for a team-runtime teammate (named,
		// addressable via the mailbox abstraction). Still a
		// subagent creation event.
		return models.ActionSpawnSubagent
	case "team_attach_outcome_fragment",
		"team_await_runs",
		"team_broadcast",
		"team_cancel_run",
		"team_cleanup",
		"team_create_outcome",
		"team_finalize_outcome",
		"team_list_outcomes",
		"team_list_runs",
		"team_mission_log",
		"team_read_mailbox",
		"team_review_outcome_fragment",
		"team_run_task",
		"team_send_message",
		"team_shutdown_teammate",
		"team_status",
		"team_task":
		return models.ActionMCPCall

	default:
		// Unknown tools fall through to ActionMCPCall. This catches
		// the open-ended MCP-server-registered tool surface (each
		// server adds an arbitrary set of tools at startup) AND any
		// future core / team tool a Cline CLI release adds. The raw
		// name is preserved in ToolEvent.RawToolName by the caller
		// so the dashboard's tool-name surface still works; the
		// action type just defaults to the most-likely category.
		return models.ActionMCPCall
	}
}
