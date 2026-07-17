package hermes

import (
	"strings"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// normalizeToolName maps a Hermes tool name (the `function.name` value
// inside an assistant message's `tool_calls` JSON, or the `tool_name`
// kwarg passed to a post_tool_call hook) onto the normalized action
// type the rest of observer consumes.
//
// The mapping table comes from docs/hermes-adapter-plan.md §4 (and the
// 2026-06-05 reality check in §17.1 confirmed every tool name in our
// live fixture matches the plan's expected shape). Unknown tools fall
// through to ActionMCPCall — most user-added Hermes tools are MCP-shaped
// (Hermes is itself an MCP host) so this is a sensible default; the
// caller still preserves the raw name in ToolEvent.RawToolName so the
// dashboard can surface it verbatim.
//
// Special prefix rules (run before the literal switch):
//
//   - `mcp_<server>_<tool>` → ActionMCPCall. Hermes prefixes every
//     MCP-exposed tool with `mcp_<server>_` so they're discoverable as
//     a class; the suffix carries the upstream server:tool name.
//   - `browser_<verb>`      → ActionBrowserAction. Hermes ships
//     `browser_navigate`, `browser_click`, `browser_type`,
//     `browser_screenshot`, etc. — all map to the same coarse type.
//
// All comparisons are case-sensitive lowercase. Hermes tool names are
// lower_snake_case by convention; if a future Hermes release breaks
// this convention the caller is expected to lower-case before calling
// us (we don't normalise here to keep the function pure and the
// per-tool dispatch deterministic).
func normalizeToolName(raw string) string {
	// Prefix rules first — they win over literal matches when both
	// would apply (none of the literal mappings overlap with these
	// prefixes today).
	switch {
	case strings.HasPrefix(raw, "mcp_"):
		return models.ActionMCPCall
	case strings.HasPrefix(raw, "browser_"):
		return models.ActionBrowserAction
	}

	switch raw {
	// --- File tools (plan §4.1) ----------------------------------------
	case "read_file":
		return models.ActionReadFile
	case "write_file":
		return models.ActionWriteFile
	case "patch", "apply_diff":
		// patch carries {path, old_string, new_string} (Claude-Code-Edit
		// style, NOT a unified diff — the plan §11.3 had it as `diff`
		// but reality is the structured shape).
		return models.ActionEditFile
	case "search_files":
		return models.ActionSearchFiles

	// --- Terminal / code-exec tools (plan §4.2 + §4.8) ------------------
	case "terminal", "process", "execute_code":
		return models.ActionRunCommand

	// --- Web tools (plan §4.3) ------------------------------------------
	case "web_search":
		return models.ActionWebSearch
	case "web_extract":
		return models.ActionWebFetch

	// --- Browser / computer-use tools (plan §4.4 + §4.11) ----------------
	// Most of these are caught by the `browser_` prefix rule above; the
	// literal `computer_use` case below covers Hermes's parallel
	// computer-use tool (CDP-backed full-desktop driver).
	case "computer_use":
		return models.ActionBrowserAction

	// --- Agent / delegation tools (plan §4.5 + §4.11) -------------------
	case "delegate_task", "mixture_of_agents":
		return models.ActionSpawnSubagent
	case "todo":
		return models.ActionTodoUpdate

	// --- Memory / session tools (plan §4.6) -----------------------------
	case "memory", "session_search":
		// Hermes's memory subsystem is conceptually MCP-shaped — a tool
		// that calls into a persistent backend behind the agent — so we
		// fold it into ActionMCPCall. The dashboard can still
		// distinguish via RawToolName.
		return models.ActionMCPCall
	case "clarify":
		// Hermes's clarification primitive — when the agent decides it
		// needs to ask the operator a follow-up question before
		// proceeding. Maps cleanly to ActionAskUser.
		return models.ActionAskUser

	// --- Media tools (plan §4.7) -----------------------------------------
	// All media tools (vision_analyze, image_generate, text_to_speech,
	// video_generate, video_analyze) fold to ActionMCPCall today. If/
	// when observer grows a distinct ActionMediaCall (or per-modality
	// types), upgrade these. Tracked as a follow-up.
	case "vision_analyze", "image_generate", "text_to_speech",
		"video_generate", "video_analyze":
		return models.ActionMCPCall

	// --- Messaging tools (plan §4.10) -----------------------------------
	case "send_message":
		// Gateway-bridge send — Telegram / Discord / Slack / etc.
		// outbound. ActionMCPCall today; could become a dedicated
		// ActionMessagingDispatch in v2 if cross-platform messaging
		// analytics becomes a thing.
		return models.ActionMCPCall

	// --- Other tools (plan §4.11) ---------------------------------------
	case "skill_view", "skill_manage", "skills_list":
		// Hermes's per-session skill library management. ActionMCPCall
		// for now.
		return models.ActionMCPCall
	case "cronjob":
		// Scheduling primitive. ActionMCPCall today.
		return models.ActionMCPCall

	default:
		// Unknown tools fall through to ActionMCPCall. The raw name is
		// preserved in ToolEvent.RawToolName by the caller so the
		// dashboard's tool-name surface still works; the action type
		// just defaults to the most-likely category.
		return models.ActionMCPCall
	}
}
