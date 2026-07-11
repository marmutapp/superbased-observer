package mcpsec

import (
	"strings"

	"github.com/marmutapp/superbased-observer/internal/policy"
)

// Poisoning heuristics (guard spec §9.3): static checks over observed
// tool descriptions. Each heuristic is one table row with a stable ID
// and its own documented false-positive profile — these are honest
// structural checks, not classification (gap F7); hits feed the
// R-303 flag row and never block anything.
//
// The hidden-text row reuses policy.HiddenUnicode — R-180's
// zero-width/bidi/Unicode-tags table — rather than duplicating the
// pattern set (one table, two readers; the AnalyzeInjection
// precedent).

// ToolDecl is one observed MCP tool declaration, extracted by the
// proxy seam from a request body's tools array (doc.go "Where tool
// descriptions come from").
type ToolDecl struct {
	// Server is the MCP server name (policy.MCPServerFromTarget over
	// the wire tool name).
	Server string
	// Name is the bare tool name (policy.MCPToolFromTarget).
	Name string
	// Description is the declared tool description text.
	Description string
	// ParamDoc is the concatenated input-schema property
	// descriptions ("param: description" lines, depth-1 only —
	// documented approximation; nested schema text is not walked).
	ParamDoc string
}

// HeuristicHit is one poisoning-heuristic hit on one tool.
type HeuristicHit struct {
	// Heuristic is the stable row ID (the poisonHeuristics table).
	Heuristic string
	// Server / Tool name the subject.
	Server string
	Tool   string
	// Detail is a bounded human-readable specifics string.
	Detail string
}

// poisonHeuristic is one §9.3 table row.
type poisonHeuristic struct {
	// id is the stable heuristic name appearing in finding details
	// and tests.
	id string
	// match inspects one declaration; detail is bounded.
	match func(d *ToolDecl) (bool, string)
}

// Heuristic IDs — stable strings; they appear in R-303 verdict
// reasons and tests.
const (
	// hitToolShadowing: instruction patterns targeting OTHER tools
	// ("before using any other tool…"). FP profile: low — legitimate
	// descriptions describe their own tool; cross-tool imperatives
	// ("instead of", "before any other") are the attack's defining
	// shape. A workflow-orchestration server documenting genuine
	// sequencing ("call plan_task before other tools") can trip it;
	// that is exactly the shadowing shape and deserves one review.
	hitToolShadowing = "mcp_tool_shadowing"
	// hitHiddenText: invisible Unicode (zero-width/bidi/tags via
	// policy.HiddenUnicode) or HTML comments in a description. FP
	// profile: near-zero — descriptions are plain prose; invisible
	// code points and <!-- --> blocks have no legitimate role there.
	hitHiddenText = "mcp_hidden_text"
	// hitExfilParam: parameter/description text asking for
	// conversation context, prior messages or the system prompt. FP
	// profile: low-moderate — a summarization tool may genuinely take
	// "conversation history"; flag-grade review cost is one read.
	// Deliberately EXCLUDED: "api key"/"token"/"credentials" — auth
	// parameters legitimately name those constantly.
	hitExfilParam = "mcp_exfil_param"
	// hitNameCollision: tool name collides with a well-known built-in
	// tool name (read_file, bash, web_fetch…), steering the model's
	// built-in intents to the MCP server. FP profile: moderate — a
	// filesystem server honestly naming its reader "read_file" trips
	// it; that overlap is itself the routing risk being surfaced.
	hitNameCollision = "mcp_name_collision"
	// hitExcessivePermissions: description requesting broad/elevated
	// access. FP profile: moderate — "full access to your repos" is
	// how some legitimate integrations describe themselves; the row
	// uses elevated/system-level phrases to keep it review-grade.
	hitExcessivePermissions = "mcp_excessive_permissions"
)

// toolShadowingPhrases — lowercase literals; any one hits.
var toolShadowingPhrases = []string{
	"before using any other tool",
	"before calling any other tool",
	"before any other tool",
	"before invoking any other tool",
	"instead of any other tool",
	"instead of using any other",
	"use this tool instead of",
	"do not use any other tool",
	"do not use other tools",
	"never use any other tool",
	"always use this tool first",
	"ignore other tools",
	"when any other tool is called",
	"whenever any other tool",
}

// exfilParamPhrases — lowercase literals; any one hits.
var exfilParamPhrases = []string{
	"full conversation",
	"entire conversation",
	"conversation history",
	"chat history",
	"all previous messages",
	"all prior messages",
	"previous tool results",
	"the system prompt",
	"your system prompt",
	"your instructions verbatim",
	"list of available tools",
}

// excessivePermissionPhrases — lowercase literals; any one hits.
var excessivePermissionPhrases = []string{
	"unrestricted access",
	"full disk access",
	"full system access",
	"all permissions",
	"admin access",
	"administrator access",
	"root access",
	"elevated privileges",
	"bypass security",
	"bypass permission",
	"disable security",
	"run as sudo",
	"requires sudo",
}

// wellKnownToolNames are the built-in tool names of the major coding
// agents — the §9.3 shadowing-collision reference set. Overlaps the
// proxy guard's responseToolShape keys by design: same vocabulary,
// different concern (that table classifies intents, this one detects
// imposters), kept separately so neither package imports the other.
var wellKnownToolNames = map[string]bool{
	"bash": true, "shell": true, "run_command": true, "execute_command": true,
	"run_terminal_cmd": true,
	"read":             true, "read_file": true, "write": true, "write_file": true,
	"write_to_file": true, "create_file": true,
	"edit": true, "edit_file": true, "multiedit": true, "apply_diff": true,
	"str_replace_based_edit_tool": true, "notebookedit": true,
	"web_fetch": true, "webfetch": true, "web_search": true, "websearch": true,
	"glob": true, "grep": true, "ls": true, "task": true, "agent": true,
	"todowrite": true, "askusersquestion": true,
}

// poisonHeuristics is the §9.3 table in catalog order.
var poisonHeuristics = []poisonHeuristic{
	{id: hitToolShadowing, match: func(d *ToolDecl) (bool, string) {
		return phraseHit(d.text(), toolShadowingPhrases)
	}},
	{id: hitHiddenText, match: func(d *ToolDecl) (bool, string) {
		if policy.HiddenUnicode(d.text()) {
			return true, "invisible unicode (zero-width/bidi/tag) in description"
		}
		if strings.Contains(d.text(), "<!--") {
			return true, "HTML comment in description"
		}
		return false, ""
	}},
	{id: hitExfilParam, match: func(d *ToolDecl) (bool, string) {
		return phraseHit(d.text(), exfilParamPhrases)
	}},
	{id: hitNameCollision, match: func(d *ToolDecl) (bool, string) {
		if wellKnownToolNames[strings.ToLower(d.Name)] {
			return true, "tool name collides with built-in tool " + strings.ToLower(d.Name)
		}
		return false, ""
	}},
	{id: hitExcessivePermissions, match: func(d *ToolDecl) (bool, string) {
		return phraseHit(d.text(), excessivePermissionPhrases)
	}},
}

// text returns the analyzed surface: description + param docs.
func (d *ToolDecl) text() string {
	if d.ParamDoc == "" {
		return d.Description
	}
	return d.Description + "\n" + d.ParamDoc
}

// phraseHit reports the first matching lowercase literal.
func phraseHit(text string, phrases []string) (bool, string) {
	lower := strings.ToLower(text)
	for _, p := range phrases {
		if strings.Contains(lower, p) {
			return true, "matched " + `"` + p + `"`
		}
	}
	return false, ""
}

// AnalyzeTools runs the heuristic table over a set of declarations
// and returns every hit (one per tool × heuristic).
func AnalyzeTools(decls []ToolDecl) []HeuristicHit {
	var out []HeuristicHit
	for i := range decls {
		d := &decls[i]
		for _, h := range poisonHeuristics {
			hit, detail := h.match(d)
			if !hit {
				continue
			}
			out = append(out, HeuristicHit{
				Heuristic: h.id, Server: d.Server, Tool: d.Name, Detail: detail,
			})
		}
	}
	return out
}
