package tooltax_test

import (
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/policy"
	"github.com/marmutapp/superbased-observer/internal/tooltax"
)

// mcpNameCorpus is a corpus-derived list of names/targets exercised
// against the MCP identity parser. The `mcp__…` entries are verbatim
// raw_tool_name values from the live action corpus (2026-07-31); the
// colon entries are verbatim actions.target values from codex rows
// (node_repl:js ×148 etc.); the rest are the degenerate and
// non-MCP shapes the parsers must survive.
var mcpNameCorpus = []string{
	// canonical mcp__server__tool — measured, 2,761 raw_tool_name rows.
	"mcp__observer__get_file",
	"mcp__observer__search_past_outputs",
	"mcp__observer__get_session_recovery_context",
	"mcp__observer__retrieve_stashed",
	"mcp__claude-in-chrome__computer",
	"mcp__claude-in-chrome__javascript_tool",
	"mcp__claude-in-chrome__tabs_context_mcp",
	"mcp__claude-in-chrome__read_network_requests",
	"mcp__Claude_Browser__resize_window",
	"mcp__ccd_session__mark_chapter",
	"mcp__ccd_session__spawn_task",
	"mcp__cowork__request_cowork_directory",
	"mcp__cowork__allow_cowork_file_delete",
	"mcp__visualize__show_widget",
	"mcp__workspace__bash",
	"mcp__workspace__web_fetch",
	// degenerate mcp__ forms.
	"mcp__",
	"mcp__server",
	"mcp____tool",
	// colon form — measured, 151 actions.target rows (codex).
	"node_repl:js",
	"node_repl:js_add_node_module_dir",
	"codex:list_mcp_resources",
	"observer:get_session_summary",
	"server:",
	":tool",
	// non-MCP shapes.
	"",
	"   ",
	"Bash",
	"deep-research",
	"pptx",
	"read_file",
	"/home/marmutapp/superbased-observer/internal/tooltax/table.go",
	"C:\\programsx\\superbased-observer\\main.go",
	"  mcp__observer__get_file  ",
}

// TestIsMCPToolNameDelegationIsBehaviourPreserving is the equivalence
// proof for the models.IsMCPToolName → tooltax.MCPIdentity delegation.
// legacyIsMCPToolName is the pre-delegation body verbatim
// (internal/models/models.go, `strings.HasPrefix(name, "mcp__")`).
func TestIsMCPToolNameDelegationIsBehaviourPreserving(t *testing.T) {
	legacyIsMCPToolName := func(name string) bool {
		return strings.HasPrefix(name, "mcp__")
	}
	for _, name := range mcpNameCorpus {
		want := legacyIsMCPToolName(name)
		if got := models.IsMCPToolName(name); got != want {
			t.Errorf("models.IsMCPToolName(%q) = %v; legacy body gives %v", name, got, want)
		}
		if got := tooltax.IsMCPToolName(name); got != want {
			t.Errorf("tooltax.IsMCPToolName(%q) = %v; legacy body gives %v", name, got, want)
		}
		if _, _, ok := tooltax.MCPIdentity(name); ok != want {
			t.Errorf("tooltax.MCPIdentity(%q) ok = %v; legacy prefix test gives %v", name, ok, want)
		}
	}
}

// TestMCPIdentityDoesNotTrim pins the deliberate asymmetry: MCPIdentity
// works on RAW tool names (a leading space is a different name, and
// models.IsMCPToolName never trimmed), while MCPIdentityFromTarget
// trims because targets are display-normalised.
func TestMCPIdentityDoesNotTrim(t *testing.T) {
	if _, _, ok := tooltax.MCPIdentity("  mcp__observer__get_file"); ok {
		t.Error("MCPIdentity must not trim — that would change models.IsMCPToolName's behaviour")
	}
	server, tool, ok := tooltax.MCPIdentityFromTarget("  mcp__observer__get_file  ")
	if !ok || server != "observer" || tool != "get_file" {
		t.Errorf("MCPIdentityFromTarget(padded) = (%q, %q, %v)", server, tool, ok)
	}
}

func TestMCPIdentity(t *testing.T) {
	cases := []struct {
		raw            string
		server, tool   string
		ok             bool
		wantFromTarget bool
	}{
		{"mcp__observer__get_file", "observer", "get_file", true, true},
		// A tool half containing "__" keeps it: split on the FIRST pair.
		{"mcp__a__b__c", "a", "b__c", true, true},
		{"mcp__server", "server", "", true, true},
		{"mcp__", "", "", true, true},
		// Degenerate: the first "__" is at offset 0, so there is no
		// server half. Go's historical parse (policy.MCPServerFromTarget's
		// `i > 0` guard) treats the whole remainder as the server; the
		// dashboard's TS parser splits it into {server:"", tool:"tool"}.
		// The delegation preserves the GO behaviour — see
		// TestMCPIdentityMatchesTheDashboardParser for the exclusion.
		{"mcp____tool", "__tool", "", true, true},
		{"Bash", "", "", false, false},
		{"", "", "", false, false},
		{"node_repl:js", "", "", false, true},
	}
	for _, c := range cases {
		server, tool, ok := tooltax.MCPIdentity(c.raw)
		if server != c.server || tool != c.tool || ok != c.ok {
			t.Errorf("MCPIdentity(%q) = (%q, %q, %v); want (%q, %q, %v)",
				c.raw, server, tool, ok, c.server, c.tool, c.ok)
		}
		if _, _, gotOK := tooltax.MCPIdentityFromTarget(c.raw); gotOK != c.wantFromTarget {
			t.Errorf("MCPIdentityFromTarget(%q) ok = %v; want %v", c.raw, gotOK, c.wantFromTarget)
		}
	}
}

// TestMCPIdentityFromTargetColonForm pins the SECOND form the corpus
// evidence kept: `server:tool`, 151 measured actions.target rows, all
// from the codex adapter.
func TestMCPIdentityFromTargetColonForm(t *testing.T) {
	cases := []struct{ target, server, tool string }{
		{"node_repl:js", "node_repl", "js"},
		{"node_repl:js_add_node_module_dir", "node_repl", "js_add_node_module_dir"},
		{"codex:list_mcp_resources", "codex", "list_mcp_resources"},
		{"observer:get_session_summary", "observer", "get_session_summary"},
		{"server:", "server", ""},
	}
	for _, c := range cases {
		server, tool, ok := tooltax.MCPIdentityFromTarget(c.target)
		if !ok || server != c.server || tool != c.tool {
			t.Errorf("MCPIdentityFromTarget(%q) = (%q, %q, %v); want (%q, %q, true)",
				c.target, server, tool, ok, c.server, c.tool)
		}
	}
	// A leading colon has no server half.
	if _, _, ok := tooltax.MCPIdentityFromTarget(":tool"); ok {
		t.Error("MCPIdentityFromTarget(\":tool\") must not match — no server half")
	}
}

// TestSlashFormIsDropped is the evidence-backed behaviour decision this
// work package makes for the NEW owner, pinned so it is deliberate
// rather than accidental.
//
// Corpus evidence (read-only, 2026-07-31, 321,675 action rows):
//
//	SELECT COUNT(*) FROM actions
//	  WHERE action_type='mcp_call' AND target LIKE '%/%';            -- 0
//	SELECT COUNT(*) FROM actions WHERE raw_tool_name LIKE '%/%';     -- 0
//	SELECT COUNT(*) FROM actions
//	  WHERE action_type='mcp_call' AND target LIKE '%:%';            -- 151
//	SELECT COUNT(*) FROM actions
//	  WHERE raw_tool_name LIKE 'mcp\_\_%' ESCAPE '\';                -- 2,761
//
// Zero rows depended on the `server/tool` shape, and "/" being the path
// separator made it actively harmful: a path-shaped mcp_call target
// would have yielded its first path segment as a "server", fabricating
// a taint origin. Dropping it strictly REMOVES a false-positive class.
func TestSlashFormIsDropped(t *testing.T) {
	for _, target := range []string{
		"server/tool",
		"home/marmutapp/notes.md",
		"/home/marmutapp/superbased-observer/internal/tooltax/table.go",
	} {
		if server, tool, ok := tooltax.MCPIdentityFromTarget(target); ok {
			t.Errorf("MCPIdentityFromTarget(%q) = (%q, %q, true); the slash form is dropped "+
				"(0 corpus rows) — a path must never yield a server name", target, server, tool)
		}
	}
}

// TestPolicyParsersMatchTooltax is the EQUIVALENCE CONFORMANCE test that
// stands in for a delegation.
//
// internal/policy could not be made to delegate to tooltax:
// internal/policy/imports_test.go::TestPackageImports_Bounded pins that
// package at ZERO observer imports (guard spec §17.1, "the strongest
// form of the invariant"), so importing tooltax — even a dependency-free
// pure-data package — fails the build gate. Per the WP-T1 escape hatch,
// policy.MCPServerFromTarget / MCPToolFromTarget are therefore left
// untouched and equivalence is pinned from THIS side instead.
//
// Every corpus-derived input must agree between the two implementations.
// The ONE tolerated divergence is the `server/tool` shape, which tooltax
// drops on zero-corpus-row evidence (TestSlashFormIsDropped) and policy
// still accepts; those inputs are skipped here and named explicitly, so
// the exception cannot silently widen.
func TestPolicyParsersMatchTooltax(t *testing.T) {
	slashDivergence := func(s string) bool {
		tr := strings.TrimSpace(s)
		if strings.HasPrefix(tr, "mcp__") {
			return false
		}
		i := strings.Index(tr, "/")
		j := strings.Index(tr, ":")
		// policy uses IndexAny("/:"), so a slash only diverges when it
		// is the FIRST separator and is not at offset 0.
		return i > 0 && (j < 0 || i < j)
	}
	checked := 0
	for _, name := range mcpNameCorpus {
		if slashDivergence(name) {
			t.Logf("skipping documented slash-form divergence: %q", name)
			continue
		}
		checked++
		wantServer, wantTool, _ := tooltax.MCPIdentityFromTarget(name)
		if got := policy.MCPServerFromTarget(name); got != wantServer {
			t.Errorf("policy.MCPServerFromTarget(%q) = %q; tooltax owner gives %q",
				name, got, wantServer)
		}
		if got := policy.MCPToolFromTarget(name); got != wantTool {
			t.Errorf("policy.MCPToolFromTarget(%q) = %q; tooltax owner gives %q",
				name, got, wantTool)
		}
	}
	if checked < len(mcpNameCorpus)-2 {
		t.Errorf("only %d of %d corpus names were compared — the slash exception widened",
			checked, len(mcpNameCorpus))
	}
}

// TestMCPIdentityMatchesTheDashboardParser pins the Go owner against the
// TypeScript one it will replace (web/src/lib/actions.ts:120-160
// mcpIdentity): for an `mcp__…` name, split on the first "__" after the
// prefix, and fall back to {server: rest, tool: ""} when there is none.
//
// ONE known divergence, excluded below and recorded for WP-T2: on the
// degenerate `mcp____tool` (first separator at offset 0) Go's historical
// `i > 0` guard yields server="__tool", tool="", while TS's `i < 0`
// guard yields server="", tool="tool". Zero corpus rows have this shape;
// the delegation preserves the Go behaviour, and WP-T2 aligns TS to Go
// rather than the reverse.
func TestMCPIdentityMatchesTheDashboardParser(t *testing.T) {
	tsLike := func(raw string) (string, string, bool) {
		if !strings.HasPrefix(raw, "mcp__") {
			return "", "", false
		}
		rest := raw[len("mcp__"):]
		i := strings.Index(rest, "__")
		if i < 0 {
			return rest, "", true
		}
		return rest[:i], rest[i+2:], true
	}
	for _, name := range mcpNameCorpus {
		if name == "mcp____tool" {
			continue // documented divergence, see the doc comment.
		}
		wantServer, wantTool, wantOK := tsLike(name)
		server, tool, ok := tooltax.MCPIdentity(name)
		if server != wantServer || tool != wantTool || ok != wantOK {
			t.Errorf("MCPIdentity(%q) = (%q, %q, %v); actions.ts mcpIdentity gives (%q, %q, %v)",
				name, server, tool, ok, wantServer, wantTool, wantOK)
		}
	}
}
