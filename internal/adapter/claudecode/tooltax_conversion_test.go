package claudecode

import (
	"sort"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/tooltax"
)

// preTooltaxActionMap is a VERBATIM capture of the package-private
// `actionMap` literal as it stood immediately before WP-T3 replaced it
// with tooltax.For(models.ToolClaudeCode). It exists solely so the
// conversion carries an old-vs-new equality pin: every (native name →
// action type) pair the adapter resolved before the conversion must
// still resolve identically after it.
//
// Do NOT "keep this up to date" with new tool names. It is a frozen
// historical fixture; new names belong in internal/tooltax's table and
// are pinned by preTooltaxAdditions below.
var preTooltaxActionMap = map[string]string{
	"Read":            models.ActionReadFile,
	"Write":           models.ActionWriteFile,
	"Edit":            models.ActionEditFile,
	"MultiEdit":       models.ActionEditFile,
	"NotebookEdit":    models.ActionEditFile,
	"Bash":            models.ActionRunCommand,
	"PowerShell":      models.ActionRunCommand,
	"powershell":      models.ActionRunCommand,
	"pwsh":            models.ActionRunCommand,
	"cmd":             models.ActionRunCommand,
	"cmd.exe":         models.ActionRunCommand,
	"sh":              models.ActionRunCommand,
	"Grep":            models.ActionSearchText,
	"Glob":            models.ActionSearchFiles,
	"WebSearch":       models.ActionWebSearch,
	"WebFetch":        models.ActionWebFetch,
	"Agent":           models.ActionSpawnSubagent,
	"TaskCreate":      models.ActionTodoUpdate,
	"TaskUpdate":      models.ActionTodoUpdate,
	"TaskList":        models.ActionTodoUpdate,
	"TaskGet":         models.ActionTodoUpdate,
	"TaskOutput":      models.ActionTodoUpdate,
	"TaskStop":        models.ActionTodoUpdate,
	"TodoWrite":       models.ActionTodoUpdate,
	"AskUserQuestion": models.ActionAskUser,
	"EnterPlanMode":   models.ActionPermissionMode,
	"ExitPlanMode":    models.ActionPermissionMode,
}

// preTooltaxAdditions is the exact set of native names the tooltax
// table adds on top of preTooltaxActionMap. These are the measured
// unknown-bucket gap the taxonomy plan §0 identified (the
// Monitor/SendMessage/Skill/ToolSearch/Schedule harness family) plus
// the lower-case / snake_case spellings the corpus carries. Pinning
// the delta EXACTLY means an unreviewed row landing on claude-code is
// as loud as a deleted one.
var preTooltaxAdditions = map[string]string{
	"Artifact":         tooltax.ActionHarnessCall,
	"CronCreate":       tooltax.ActionSchedule,
	"CronDelete":       tooltax.ActionSchedule,
	"CronList":         tooltax.ActionSchedule,
	"EnterWorktree":    tooltax.ActionWorktreeCreate,
	"ExitWorktree":     tooltax.ActionWorktreeRemove,
	"Monitor":          tooltax.ActionAgentControl,
	"PushNotification": tooltax.ActionNotification,
	"ScheduleWakeup":   tooltax.ActionSchedule,
	"SendMessage":      tooltax.ActionAgentMessage,
	"SendUserFile":     tooltax.ActionHarnessCall,
	"Skill":            tooltax.ActionSkillInvoke,
	"StructuredOutput": tooltax.ActionHarnessCall,
	"ToolSearch":       tooltax.ActionToolSearch,
	"Workflow":         tooltax.ActionHarnessCall,
	"apply_patch":      tooltax.ActionEditFile,
	"bash":             tooltax.ActionRunCommand,
	"edit_file":        tooltax.ActionEditFile,
	"exec":             tooltax.ActionRunCommand,
	"glob":             tooltax.ActionSearchFiles,
	"grep":             tooltax.ActionSearchText,
	"read_file":        tooltax.ActionReadFile,
	"run_command":      tooltax.ActionRunCommand,
	"shell":            tooltax.ActionRunCommand,
	"web_fetch":        tooltax.ActionWebFetch,
	"web_search":       tooltax.ActionWebSearch,
	"write_file":       tooltax.ActionWriteFile,
}

// TestActionMapPreservesPreTooltaxFixture is the WP-T3 conversion pin:
// the tooltax-sourced actionMap must resolve every pre-conversion pair
// to the SAME action type. A tooltax edit that changes or drops a
// claude-code mapping fails here.
func TestActionMapPreservesPreTooltaxFixture(t *testing.T) {
	names := sortedKeys(preTooltaxActionMap)
	for _, name := range names {
		want := preTooltaxActionMap[name]
		got, ok := actionMap[name]
		if !ok {
			t.Errorf("actionMap[%q] missing after tooltax conversion; want %q", name, want)
			continue
		}
		if got != want {
			t.Errorf("actionMap[%q] = %q after tooltax conversion; want %q", name, got, want)
		}
		// The Resolve path (used by anything that does not go through
		// the map) must agree with the map for the same pair.
		if at, ok := tooltax.ResolveActionType(models.ToolClaudeCode, name); !ok || at != want {
			t.Errorf("tooltax.ResolveActionType(claude-code, %q) = %q, ok=%v; want %q, ok=true",
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
	for _, name := range sortedKeys(actionMap) {
		if _, ok := want[name]; !ok {
			t.Errorf("actionMap has unreviewed entry %q = %q; add a tooltax row review then list it in preTooltaxAdditions",
				name, actionMap[name])
		}
	}
	for _, name := range sortedKeys(want) {
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

// TestActionMapCarriesNoGlobRows guards the one behavioural assumption
// the conversion makes: tooltax.For returns LITERAL rows only, so the
// `mcp__*` glob never lands in the map and toolUseEvent's
// models.IsMCPToolName fallback still owns unlisted MCP names.
func TestActionMapCarriesNoGlobRows(t *testing.T) {
	for name := range actionMap {
		if len(name) > 0 && name[len(name)-1] == '*' {
			t.Errorf("actionMap contains glob row %q; tooltax.For must return literals only", name)
		}
	}
	const unlisted = "mcp__someserver__sometool"
	if _, ok := actionMap[unlisted]; ok {
		t.Errorf("actionMap resolved %q; the IsMCPToolName fallback must own unlisted MCP names", unlisted)
	}
	if !models.IsMCPToolName(unlisted) {
		t.Fatalf("models.IsMCPToolName(%q) = false; the fallback branch is dead", unlisted)
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
