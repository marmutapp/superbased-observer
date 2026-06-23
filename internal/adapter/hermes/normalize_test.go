package hermes

import (
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// TestNormalizeToolName_ExhaustiveMapping pins every Hermes tool name
// the adapter knows about to its normalized ActionType. New mappings
// added to normalizeToolName MUST be paired with a row here — otherwise
// regressions can land silently (a renamed Hermes tool that falls
// through to the default ActionMCPCall would silently mis-categorize
// every event using it).
//
// The fixture rows are sourced from:
//   - docs/hermes-adapter-plan.md §4 (the 70+ tool taxonomy)
//   - The §0.5 reality-check confirmed in testdata/hermes/ that the
//     live fixture corpus contains: patch, read_file, search_files,
//     terminal, web_extract, web_search, write_file. Those seven are
//     marked "live" in the comments below to make the
//     fixture-corpus coverage explicit; the rest are synthetic per
//     the plan.
func TestNormalizeToolName_ExhaustiveMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		want string
	}{
		// File tools (plan §4.1) — live in fixtures
		{"read_file", models.ActionReadFile},       // live
		{"write_file", models.ActionWriteFile},     // live
		{"patch", models.ActionEditFile},           // live
		{"apply_diff", models.ActionEditFile},      // synthetic alias
		{"search_files", models.ActionSearchFiles}, // live

		// Terminal / code-exec (plan §4.2 + §4.8) — terminal is live
		{"terminal", models.ActionRunCommand},     // live
		{"process", models.ActionRunCommand},      // synthetic
		{"execute_code", models.ActionRunCommand}, // synthetic

		// Web tools (plan §4.3) — both live in fixtures
		{"web_search", models.ActionWebSearch}, // live
		{"web_extract", models.ActionWebFetch}, // live

		// Browser / computer-use (plan §4.4 + §4.11)
		{"computer_use", models.ActionBrowserAction},

		// Agent / delegation (plan §4.5 + §4.11)
		{"delegate_task", models.ActionSpawnSubagent},
		{"mixture_of_agents", models.ActionSpawnSubagent},
		{"todo", models.ActionTodoUpdate},

		// Memory / session (plan §4.6)
		{"memory", models.ActionMCPCall},
		{"session_search", models.ActionMCPCall},
		{"clarify", models.ActionAskUser},

		// Media (plan §4.7) — all bucketed under ActionMCPCall today
		{"vision_analyze", models.ActionMCPCall},
		{"image_generate", models.ActionMCPCall},
		{"text_to_speech", models.ActionMCPCall},
		{"video_generate", models.ActionMCPCall},
		{"video_analyze", models.ActionMCPCall},

		// Messaging + skill mgmt + cron (plan §4.10 + §4.11)
		{"send_message", models.ActionMCPCall},
		{"skill_view", models.ActionMCPCall},
		{"skill_manage", models.ActionMCPCall},
		{"skills_list", models.ActionMCPCall},
		{"cronjob", models.ActionMCPCall},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := normalizeToolName(tc.name); got != tc.want {
				t.Errorf("normalizeToolName(%q) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

// TestNormalizeToolName_BrowserPrefix verifies that every browser_*
// verb (the dozen-ish documented tools plus future additions) maps to
// ActionBrowserAction. Hermes ships browser_{navigate, click, type,
// screenshot, scroll, wait, evaluate, back, forward, …} — the prefix
// rule means we don't need to enumerate each.
func TestNormalizeToolName_BrowserPrefix(t *testing.T) {
	t.Parallel()
	verbs := []string{
		"browser_navigate", "browser_click", "browser_type",
		"browser_screenshot", "browser_scroll", "browser_wait",
		"browser_evaluate", "browser_back", "browser_forward",
		"browser_close", "browser_new_tab",
		// Forward-compat: any future browser_* verb hits the prefix
		// rule before falling into the default ActionMCPCall bucket.
		"browser_some_future_verb",
	}
	for _, v := range verbs {
		t.Run(v, func(t *testing.T) {
			t.Parallel()
			if got := normalizeToolName(v); got != models.ActionBrowserAction {
				t.Errorf("normalizeToolName(%q) = %q, want %q", v, got, models.ActionBrowserAction)
			}
		})
	}
}

// TestNormalizeToolName_MCPPrefix verifies the mcp_<server>_<tool>
// prefix rule. Hermes auto-prefixes every MCP-exposed tool with
// mcp_<server>_ so they're partitioned as a class; the resulting raw
// name is consumed by the dashboard's per-server breakdown without
// further parsing here.
func TestNormalizeToolName_MCPPrefix(t *testing.T) {
	t.Parallel()
	names := []string{
		"mcp_filesystem_read",
		"mcp_github_create_issue",
		"mcp_slack_post_message",
		"mcp_my_custom_server_tool_name",
		// Forward-compat
		"mcp_anything_at_all",
	}
	for _, n := range names {
		t.Run(n, func(t *testing.T) {
			t.Parallel()
			if got := normalizeToolName(n); got != models.ActionMCPCall {
				t.Errorf("normalizeToolName(%q) = %q, want %q", n, got, models.ActionMCPCall)
			}
		})
	}
}

// TestNormalizeToolName_UnknownDefaultsToMCPCall pins the fall-through
// default. Unknown tools fold into ActionMCPCall because most
// user-added Hermes tools ARE MCP-shaped (Hermes is itself an MCP
// host); the raw name is preserved by the caller in
// ToolEvent.RawToolName so the dashboard's tool-name surface keeps
// working. If a future Hermes release ships a tool family that
// breaks this assumption, add an explicit case in normalizeToolName.
func TestNormalizeToolName_UnknownDefaultsToMCPCall(t *testing.T) {
	t.Parallel()
	unknowns := []string{
		"some_brand_new_tool",
		"completely_made_up",
		"WeirdCasingFromAFuture", // case-sensitive — won't match read_file even though that's close
		// Empty string is a degenerate input — Hermes shouldn't produce
		// it, but if it does the default applies and the caller's
		// validation catches it.
		"",
	}
	for _, n := range unknowns {
		t.Run("unknown="+n, func(t *testing.T) {
			t.Parallel()
			if got := normalizeToolName(n); got != models.ActionMCPCall {
				t.Errorf("normalizeToolName(%q) = %q, want %q (default fall-through)", n, got, models.ActionMCPCall)
			}
		})
	}
}

// TestNormalizeToolName_CaseSensitivity pins the lowercase-only
// contract. The live fixture (§0.5 reality check) shows every Hermes
// tool name is lower_snake_case, and we hard-match without
// lowercasing to keep the function pure and the dispatch
// deterministic. If a future Hermes release breaks this convention,
// the caller is expected to lower-case before invoking us.
func TestNormalizeToolName_CaseSensitivity(t *testing.T) {
	t.Parallel()
	// PascalCase / mixed-case variants of known names must fall through
	// to the default ActionMCPCall rather than silently matching the
	// lowercase entry.
	mismatches := []string{
		"Read_File",
		"WRITE_FILE",
		"Terminal",
		"WebSearch",
	}
	for _, n := range mismatches {
		t.Run(n, func(t *testing.T) {
			t.Parallel()
			if got := normalizeToolName(n); got != models.ActionMCPCall {
				t.Errorf("normalizeToolName(%q) = %q, want %q (case-sensitive miss should default)", n, got, models.ActionMCPCall)
			}
		})
	}
}
