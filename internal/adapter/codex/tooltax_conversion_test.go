package codex

import (
	"sort"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/tooltax"
)

// preTooltaxActionMap is a VERBATIM capture of the package-private
// `actionMap` literal as it stood immediately before WP-T3 replaced it
// with tooltax.For(models.ToolCodex). It exists solely so the
// conversion carries an old-vs-new equality pin: every (native name →
// action type) pair the adapter resolved before the conversion must
// still resolve identically after it.
//
// Do NOT "keep this up to date" with new tool names. It is a frozen
// historical fixture; new names belong in internal/tooltax's table and
// are pinned by preTooltaxAdditions below.
var preTooltaxActionMap = map[string]string{
	// Core tools
	"shell":       models.ActionRunCommand,
	"apply_patch": models.ActionEditFile,
	"file_read":   models.ActionReadFile,
	"file_write":  models.ActionWriteFile,
	"web_search":  models.ActionWebSearch,
	// Synonyms observed in newer Codex builds and IDE extensions
	"exec":           models.ActionRunCommand,
	"execute":        models.ActionRunCommand,
	"command":        models.ActionRunCommand,
	"read_file":      models.ActionReadFile,
	"open_file":      models.ActionReadFile,
	"write_file":     models.ActionWriteFile,
	"create_file":    models.ActionWriteFile,
	"edit_file":      models.ActionEditFile,
	"patch":          models.ActionEditFile,
	"replace":        models.ActionEditFile,
	"search":         models.ActionSearchText,
	"grep":           models.ActionSearchText,
	"find_text":      models.ActionSearchText,
	"find_in_files":  models.ActionSearchText,
	"file_search":    models.ActionSearchFiles,
	"find":           models.ActionSearchFiles,
	"glob":           models.ActionSearchFiles,
	"list_files":     models.ActionSearchFiles,
	"list_directory": models.ActionSearchFiles,
	"web_fetch":      models.ActionWebFetch,
	"fetch_url":      models.ActionWebFetch,
	// Function-call names emitted via response_item.payload.type=function_call
	"shell_command":                models.ActionRunCommand,
	"update_plan":                  models.ActionTodoUpdate,
	"list_mcp_resources":           models.ActionMCPCall,
	"list_mcp_resource_templates":  models.ActionMCPCall,
	"search_past_outputs":          models.ActionMCPCall,
	"get_session_summary":          models.ActionMCPCall,
	"get_project_patterns":         models.ActionMCPCall,
	"get_last_test_result":         models.ActionMCPCall,
	"get_session_recovery_context": models.ActionMCPCall,
	"get_cost_summary":             models.ActionMCPCall,
	"check_command_freshness":      models.ActionMCPCall,
	"get_failure_context":          models.ActionMCPCall,
	"load_workspace_dependencies":  models.ActionMCPCall,
	"view_image":                   models.ActionReadFile,
	// exec_command is the modern Codex Desktop (>=v0.130) shell tool name
	"exec_command": models.ActionRunCommand,
	// Windows interpreters surfaced by the shell tool's function_call envelope
	"powershell": models.ActionRunCommand,
	"pwsh":       models.ActionRunCommand,
	"cmd.exe":    models.ActionRunCommand,
}

// preTooltaxAdditions is the exact set of native names the tooltax
// table adds on top of preTooltaxActionMap. These are the measured
// unknown-bucket gap the taxonomy plan §0 identified — the codex
// wait/wait_agent/spawn_agent/send_message/write_stdin orchestration
// family (~1,900 corpus rows), the thread-fleet control verbs, and the
// event-suffix names (patch_apply_end / web_search_end /
// mcp_tool_call_end) the adapter writes into raw_tool_name. Pinning
// the delta EXACTLY means an unreviewed row landing on codex is as
// loud as a deleted one.
var preTooltaxAdditions = map[string]string{
	"Bash":                 tooltax.ActionRunCommand,
	"bash":                 tooltax.ActionRunCommand,
	"cmd":                  tooltax.ActionRunCommand,
	"followup_task":        tooltax.ActionAgentMessage,
	"get_action_details":   tooltax.ActionMCPCall,
	"get_file":             tooltax.ActionMCPCall,
	"imagegen":             tooltax.ActionHarnessCall,
	"interrupt_agent":      tooltax.ActionAgentControl,
	"js":                   tooltax.ActionMCPCall,
	"list_actions_around":  tooltax.ActionMCPCall,
	"list_agents":          tooltax.ActionAgentControl,
	"list_threads":         tooltax.ActionAgentControl,
	"mcp_tool_call_end":    tooltax.ActionMCPCall,
	"patch_apply_end":      tooltax.ActionEditFile,
	"read_thread":          tooltax.ActionAgentControl,
	"read_thread_terminal": tooltax.ActionAgentControl,
	"request_user_input":   tooltax.ActionAskUser,
	"run_command":          tooltax.ActionRunCommand,
	"send_message":         tooltax.ActionAgentMessage,
	"sh":                   tooltax.ActionRunCommand,
	"spawn_agent":          tooltax.ActionSpawnSubagent,
	"wait":                 tooltax.ActionSubagentWait,
	"wait_agent":           tooltax.ActionSubagentWait,
	"web_search_end":       tooltax.ActionWebSearch,
	"write_stdin":          tooltax.ActionStdinWrite,
}

// TestActionMapPreservesPreTooltaxFixture is the WP-T3 conversion pin:
// the tooltax-sourced actionMap must resolve every pre-conversion pair
// to the SAME action type. A tooltax edit that changes or drops a codex
// mapping fails here.
func TestActionMapPreservesPreTooltaxFixture(t *testing.T) {
	for _, name := range sortedTaxKeys(preTooltaxActionMap) {
		want := preTooltaxActionMap[name]
		got, ok := actionMap[name]
		if !ok {
			t.Errorf("actionMap[%q] missing after tooltax conversion; want %q", name, want)
			continue
		}
		if got != want {
			t.Errorf("actionMap[%q] = %q after tooltax conversion; want %q", name, got, want)
		}
		if at, ok := tooltax.ResolveActionType(models.ToolCodex, name); !ok || at != want {
			t.Errorf("tooltax.ResolveActionType(codex, %q) = %q, ok=%v; want %q, ok=true",
				name, at, ok, want)
		}
	}
}

// TestActionMapTooltaxAdditions pins the delta EXACTLY in both
// directions: actionMap must equal preTooltaxActionMap ∪
// preTooltaxAdditions, no more and no less.
func TestActionMapTooltaxAdditions(t *testing.T) {
	want := make(map[string]string, len(preTooltaxActionMap)+len(preTooltaxAdditions))
	for k, v := range preTooltaxActionMap {
		want[k] = v
	}
	for k, v := range preTooltaxAdditions {
		if old, dup := want[k]; dup {
			t.Fatalf("fixture bug: %q is in BOTH the pre-tooltax map (%q) and the additions set (%q)",
				k, old, v)
		}
		want[k] = v
	}
	for _, name := range sortedTaxKeys(actionMap) {
		if _, ok := want[name]; !ok {
			t.Errorf("actionMap has unreviewed entry %q = %q; review the tooltax row then list it in preTooltaxAdditions",
				name, actionMap[name])
		}
	}
	for _, name := range sortedTaxKeys(want) {
		got, ok := actionMap[name]
		if !ok {
			t.Errorf("actionMap lost entry %q (want %q)", name, want[name])
			continue
		}
		if got != want[name] {
			t.Errorf("actionMap[%q] = %q; want %q", name, got, want[name])
		}
	}
}

// TestRunCommandAliasesStayRunCommand guards extractTarget's
// capability branch (`actionMap[toolName] == models.ActionRunCommand`,
// adapter.go): it is the one place a MAP VALUE — not just a lookup hit
// — drives adapter behaviour, so every shell alias the conversion adds
// must genuinely be run_command.
func TestRunCommandAliasesStayRunCommand(t *testing.T) {
	shells := []string{
		"shell", "shell_command", "exec_command", "exec", "execute", "command",
		"powershell", "pwsh", "cmd.exe", "cmd", "sh", "bash", "Bash", "run_command",
	}
	for _, name := range shells {
		if got := actionMap[name]; got != models.ActionRunCommand {
			t.Errorf("actionMap[%q] = %q; want %q so extractTarget's command branch fires",
				name, got, models.ActionRunCommand)
		}
	}
}

// TestActionMapCarriesNoGlobRows guards the one behavioural assumption
// the conversion makes: tooltax.For returns LITERAL rows only, so the
// `mcp__*` glob never lands in the map (codex allow-lists MCP names by
// hand and never relied on the glob).
func TestActionMapCarriesNoGlobRows(t *testing.T) {
	for name := range actionMap {
		if len(name) > 0 && name[len(name)-1] == '*' {
			t.Errorf("actionMap contains glob row %q; tooltax.For must return literals only", name)
		}
	}
}

func sortedTaxKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestOpenInterpreterSharesTheCodexVocabulary pins the §2.1 boundary
// seam: `codex.NewOpenInterpreter()` retags THIS parser as
// models.ToolOpenInterpreter, so it resolves every native name through
// the same tooltax-sourced actionMap keyed on models.ToolCodex. The
// taxonomy table therefore has to carry an IDENTICAL open-interpreter
// vocabulary, or the dashboard would categorise the two rebadges of the
// same binary differently.
func TestOpenInterpreterSharesTheCodexVocabulary(t *testing.T) {
	oi := tooltax.For(models.ToolOpenInterpreter)
	if len(oi) != len(actionMap) {
		t.Errorf("open-interpreter has %d tooltax names, codex has %d — the rebadged "+
			"parser resolves through the codex map, so the two vocabularies must match",
			len(oi), len(actionMap))
	}
	for _, name := range sortedTaxKeys(actionMap) {
		got, ok := oi[name]
		if !ok {
			t.Errorf("tooltax has no open-interpreter row for %q (codex maps it to %q)",
				name, actionMap[name])
			continue
		}
		if got != actionMap[name] {
			t.Errorf("drift: %q — codex says %q, open-interpreter says %q",
				name, actionMap[name], got)
		}
	}
}
