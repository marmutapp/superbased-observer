package cowork

import (
	"sort"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/tooltax"
)

// preTooltaxActionMap is a VERBATIM capture of the package-private
// `actionMap` literal as it stood immediately before WP-T3 replaced it
// with tooltax.For(models.ToolCowork). It exists solely so the
// conversion carries an old-vs-new equality pin: every (native name →
// action type) pair the adapter resolved before the conversion must
// still resolve identically after it.
//
// The SEMANTIC REMAPS are the load-bearing part of this fixture. The
// taxonomy plan §0/§3 records that cowork deliberately re-buckets some
// names away from their claude-code meaning; WP-T3 preserved that by
// adding cowork-SPECIFIC tooltax rows rather than by changing cowork's
// output. TestCoworkSemanticRemapsSurvive asserts the divergence is
// still real.
//
// Do NOT "keep this up to date" with new tool names. It is a frozen
// historical fixture; new names belong in internal/tooltax's table and
// are pinned by preTooltaxAdditions below.
var preTooltaxActionMap = map[string]string{
	"Read":                                  models.ActionReadFile,
	"Write":                                 models.ActionWriteFile,
	"Edit":                                  models.ActionEditFile,
	"NotebookEdit":                          models.ActionEditFile,
	"Bash":                                  models.ActionRunCommand,
	"mcp__workspace__bash":                  models.ActionRunCommand,
	"PowerShell":                            models.ActionRunCommand,
	"powershell":                            models.ActionRunCommand,
	"pwsh":                                  models.ActionRunCommand,
	"cmd":                                   models.ActionRunCommand,
	"cmd.exe":                               models.ActionRunCommand,
	"sh":                                    models.ActionRunCommand,
	"Grep":                                  models.ActionSearchText,
	"Glob":                                  models.ActionSearchFiles,
	"WebSearch":                             models.ActionWebSearch,
	"WebFetch":                              models.ActionWebFetch,
	"mcp__workspace__web_fetch":             models.ActionWebFetch,
	"Agent":                                 models.ActionSpawnSubagent,
	"Task":                                  models.ActionSpawnSubagent,
	"TaskOutput":                            models.ActionTodoUpdate,
	"TaskStop":                              models.ActionTodoUpdate,
	"TaskCreate":                            models.ActionTodoUpdate,
	"TaskUpdate":                            models.ActionTodoUpdate,
	"TaskList":                              models.ActionTodoUpdate,
	"TaskGet":                               models.ActionTodoUpdate,
	"TodoWrite":                             models.ActionTodoUpdate,
	"AskUserQuestion":                       models.ActionAskUser,
	"Skill":                                 models.ActionMCPCall,
	"ToolSearch":                            models.ActionMCPCall,
	"mcp__cowork__request_cowork_directory": models.ActionMCPCall,
	"mcp__cowork__present_files":            models.ActionMCPCall,
	"mcp__cowork__allow_cowork_file_delete": models.ActionMCPCall,
	"mcp__visualize__show_widget":           models.ActionMCPCall,
	"mcp__visualize__read_me":               models.ActionMCPCall,
}

// preTooltaxAdditions is the exact set of native names the tooltax
// table adds on top of preTooltaxActionMap: the lower-case /
// snake_case spellings the corpus carries, plus one more cowork MCP
// helper. Pinning the delta EXACTLY means an unreviewed row landing on
// cowork is as loud as a deleted one.
var preTooltaxAdditions = map[string]string{
	"apply_patch":                    tooltax.ActionEditFile,
	"bash":                           tooltax.ActionRunCommand,
	"edit_file":                      tooltax.ActionEditFile,
	"exec":                           tooltax.ActionRunCommand,
	"glob":                           tooltax.ActionSearchFiles,
	"grep":                           tooltax.ActionSearchText,
	"mcp__cowork__send_user_message": tooltax.ActionMCPCall,
	"read_file":                      tooltax.ActionReadFile,
	"run_command":                    tooltax.ActionRunCommand,
	"shell":                          tooltax.ActionRunCommand,
	"web_fetch":                      tooltax.ActionWebFetch,
	"web_search":                     tooltax.ActionWebSearch,
	"write_file":                     tooltax.ActionWriteFile,
}

// TestActionMapPreservesPreTooltaxFixture is the WP-T3 conversion pin:
// the tooltax-sourced actionMap must resolve every pre-conversion pair
// to the SAME action type. A tooltax edit that changes or drops a
// cowork mapping fails here.
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
		if at, ok := tooltax.ResolveActionType(models.ToolCowork, name); !ok || at != want {
			t.Errorf("tooltax.ResolveActionType(cowork, %q) = %q, ok=%v; want %q, ok=true",
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

// TestCoworkSemanticRemapsSurvive is the explicit proof that WP-T3
// preserved cowork's deliberate divergence instead of flattening it
// onto the canonical claude-code meaning. Each row states BOTH sides:
// what cowork must emit, and what the same native name means on
// claude-code. If a future tooltax edit ever collapses the two, this
// fails.
func TestCoworkSemanticRemapsSurvive(t *testing.T) {
	cases := []struct {
		native     string
		wantCowork string
		wantCC     string
		why        string
	}{
		{
			native:     "mcp__workspace__bash",
			wantCowork: models.ActionRunCommand,
			wantCC:     "", // claude-code has no row: the mcp__* glob owns it
			why:        "the built-in Bash tool routed through the workspace MCP server",
		},
		{
			native:     "mcp__workspace__web_fetch",
			wantCowork: models.ActionWebFetch,
			wantCC:     "",
			why:        "the MCP twin of WebFetch",
		},
		{
			native:     "Skill",
			wantCowork: models.ActionMCPCall,
			wantCC:     tooltax.ActionSkillInvoke,
			why:        "an Anthropic-platform tool cowork surfaces as a plain MCP call",
		},
		{
			native:     "ToolSearch",
			wantCowork: models.ActionMCPCall,
			wantCC:     tooltax.ActionToolSearch,
			why:        "the deferred-tool loader cowork surfaces as a plain MCP call",
		},
	}
	for _, c := range cases {
		got, ok := actionMap[c.native]
		if !ok || got != c.wantCowork {
			t.Errorf("cowork %q = %q (ok=%v); want %q — %s", c.native, got, ok, c.wantCowork, c.why)
		}
		ccGot, ccOK := tooltax.For(models.ToolClaudeCode)[c.native]
		if c.wantCC == "" {
			if ccOK {
				t.Errorf("claude-code gained a literal row for %q (%q); the cowork remap is no longer a divergence",
					c.native, ccGot)
			}
			continue
		}
		if !ccOK || ccGot != c.wantCC {
			t.Errorf("claude-code %q = %q (ok=%v); want %q — the cowork remap must stay a DIVERGENCE",
				c.native, ccGot, ccOK, c.wantCC)
		}
		if ccGot == c.wantCowork {
			t.Errorf("cowork's semantic remap of %q collapsed onto the claude-code meaning %q", c.native, ccGot)
		}
	}
}

// TestActionMapCarriesNoGlobRows guards the one behavioural assumption
// the conversion makes: tooltax.For returns LITERAL rows only, so the
// `mcp__*` glob never lands in the map and the tool_use branch's
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

func sortedTaxKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
