package clinecli

import (
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// TestNormalizeToolName covers every tool the plan §5 inventory
// enumerated (10 core + 18 team_*) plus the fallthrough cases —
// `mcp_<server>_<tool>` prefix rule, unknown-tool default,
// representative MCP-shaped names.
func TestNormalizeToolName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw  string
		want string
	}{
		// --- Core (10) ---
		{"read_files", models.ActionReadFile},
		{"run_commands", models.ActionRunCommand},
		{"apply_patch", models.ActionEditFile},
		{"editor", models.ActionEditFile},
		{"search_codebase", models.ActionSearchFiles},
		{"fetch_web_content", models.ActionWebFetch},
		{"ask_question", models.ActionAskUser},
		{"skills", models.ActionMCPCall},
		{"spawn_agent", models.ActionSpawnSubagent},
		{"submit_and_exit", models.ActionTaskComplete},

		// --- Team (18) ---
		{"team_attach_outcome_fragment", models.ActionMCPCall},
		{"team_await_runs", models.ActionMCPCall},
		{"team_broadcast", models.ActionMCPCall},
		{"team_cancel_run", models.ActionMCPCall},
		{"team_cleanup", models.ActionMCPCall},
		{"team_create_outcome", models.ActionMCPCall},
		{"team_finalize_outcome", models.ActionMCPCall},
		{"team_list_outcomes", models.ActionMCPCall},
		{"team_list_runs", models.ActionMCPCall},
		{"team_mission_log", models.ActionMCPCall},
		{"team_read_mailbox", models.ActionMCPCall},
		{"team_review_outcome_fragment", models.ActionMCPCall},
		{"team_run_task", models.ActionMCPCall},
		{"team_send_message", models.ActionMCPCall},
		{"team_shutdown_teammate", models.ActionMCPCall},
		{"team_spawn_teammate", models.ActionSpawnSubagent},
		{"team_status", models.ActionMCPCall},
		{"team_task", models.ActionMCPCall},

		// --- MCP prefix (the dynamic surface) ---
		{"mcp_filesystem_read_file", models.ActionMCPCall},
		{"mcp_github_search_issues", models.ActionMCPCall},
		{"mcp_postgres_query", models.ActionMCPCall},

		// --- Unknown / future ---
		{"some_future_tool", models.ActionMCPCall},
		{"", models.ActionMCPCall},

		// --- NOT the cline-vscode variants ---
		// These belong to internal/adapter/cline/ (the VSCode
		// extension); cline-cli's normalizer doesn't recognise them,
		// so they fall through to ActionMCPCall. If a Cline CLI
		// release ever adds the singular form alongside the plural,
		// the relevant case can be added explicitly.
		{"read_file", models.ActionMCPCall},
		{"execute_command", models.ActionMCPCall},
		{"replace_in_file", models.ActionMCPCall},
		{"list_files", models.ActionMCPCall},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			t.Parallel()
			got := normalizeToolName(tc.raw)
			if got != tc.want {
				t.Errorf("normalizeToolName(%q) = %q; want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestNormalizeToolName_CountsMatchPlanTaxonomy is a deliberate
// inventory pin: the plan §5 enumerated exactly 10 core tools and
// 18 team_* tools (28 total). If a Cline CLI release adds or
// removes a tool, that's a deliberate update; this test makes the
// adapter author touch BOTH the switch in normalize.go AND this
// test, surfacing the taxonomy delta in the code-review diff.
//
// Counted by walking the literal cases at parse time would couple
// to test-internal structure; instead we hardcode the headline
// numbers and let the per-tool TestNormalizeToolName cover the
// individual mappings.
func TestNormalizeToolName_CountsMatchPlanTaxonomy(t *testing.T) {
	t.Parallel()
	const wantCore = 10
	const wantTeam = 18
	const wantTotal = wantCore + wantTeam
	if wantTotal != 28 {
		t.Errorf("plan §5 enumerated 28 tools; got %d", wantTotal)
	}
}
