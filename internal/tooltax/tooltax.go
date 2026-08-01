package tooltax

import (
	"sort"
	"strings"
)

// Surface says WHERE a native tool lives, independently of what it does
// (Category). The two dimensions are orthogonal on purpose: cowork's
// `mcp__workspace__bash` is Surface mcp but Category cmd, and cline-cli's
// `team_*` fleet primitives are Surface orchestration but Category mcp
// (that adapter routes them through mcp_call).
type Surface string

const (
	// SurfaceBuiltin is a tool the agent host itself implements.
	SurfaceBuiltin Surface = "builtin"
	// SurfaceMCP is a tool reached over the Model Context Protocol.
	SurfaceMCP Surface = "mcp"
	// SurfaceOrchestration is a multi-agent / thread-fleet primitive.
	SurfaceOrchestration Surface = "orchestration"
	// SurfaceMeta is a harness/lifecycle affordance that is not agent
	// work at all (scheduling, notifications, turn markers).
	SurfaceMeta Surface = "meta"
)

// surfaceOrder is the canonical display order of the surfaces.
var surfaceOrder = []Surface{
	SurfaceBuiltin, SurfaceMCP, SurfaceOrchestration, SurfaceMeta,
}

// Surfaces returns the four canonical surfaces in display order.
func Surfaces() []Surface {
	out := make([]Surface, len(surfaceOrder))
	copy(out, surfaceOrder)
	return out
}

// Entry is one row of the canonical taxonomy table.
type Entry struct {
	// Tool is the canonical adapter name (models.Tool* value). Empty
	// means "any tool" — a fallback row.
	Tool string
	// Native is the native tool name as the source tool emits it (what
	// lands in actions.raw_tool_name). A trailing "*" makes it a prefix
	// glob; glob rows sort last in the table.
	Native string
	// ActionType is the canonical action type (a models.Action* value).
	ActionType string
	// Category is the canonical category. It is always
	// CategoryForActionType(ActionType) — the field is materialised so
	// consumers (and the generated TS mirror) never have to do the
	// lookup, and TestRowCategoryMatchesActionType pins the equality.
	Category string
	// Surface is where the tool lives.
	Surface Surface
}

// IsGlob reports whether the row's Native is a prefix glob.
func (e Entry) IsGlob() bool { return strings.HasSuffix(e.Native, "*") }

// matches reports whether the row applies to (tool, native) for an
// exact-name lookup.
func (e Entry) matches(tool, native string) bool {
	if e.Tool != "" && e.Tool != tool {
		return false
	}
	if e.IsGlob() {
		return strings.HasPrefix(native, strings.TrimSuffix(e.Native, "*"))
	}
	return e.Native == native
}

// entry builds a row, materialising Category from the action-type
// registry so the ~500 rows below can never drift category-vs-action.
func entry(tool, native, actionType string, surface Surface) Entry {
	return Entry{
		Tool:       tool,
		Native:     native,
		ActionType: actionType,
		Category:   CategoryForActionType(actionType),
		Surface:    surface,
	}
}

// normalizeKey collapses the spelling variants of one tool name onto a
// single lookup key: lower-cased, with `_`, `-`, `.` and whitespace
// stripped. It is a superset of the per-adapter normalizers already in
// the tree (internal/adapter/commandcode/records.go:250 strips `_`/`-`/
// space; antigravity/classify.go:427, gemini/parser.go, grok, kimicode,
// qwencode strip `_`/`-`), and additionally folds `.` so `cmd.exe`,
// `cmd_exe` and `cmdexe` — all three of which appear in adapter
// switches today — resolve to the same row.
func normalizeKey(name string) string {
	key := strings.ToLower(strings.TrimSpace(name))
	key = strings.ReplaceAll(key, "_", "")
	key = strings.ReplaceAll(key, "-", "")
	key = strings.ReplaceAll(key, ".", "")
	return strings.Join(strings.Fields(key), "")
}

// Resolve maps a (tool, native tool name) pair onto its canonical
// taxonomy row, reporting whether a row matched.
//
// The table is walked TOP-DOWN in two passes (CLAUDE.md rule 5):
//
//  1. exact Native match — tool-specific rows come first in the table,
//     so a tool-specific mapping always beats a tool-less fallback;
//  2. normalizeKey match over LITERAL rows only — so `Read`, `read`,
//     `read_file` and `ReadFile` all land on the same row for adapters
//     that normalize before switching.
//
// Glob rows (`mcp__*`) sort last in the table and therefore only fire
// when no literal row claimed the name. They participate in pass 1
// ONLY, and deliberately so: normalizeKey strips `_`, which would turn
// the `mcp__*` prefix into a bare `mcp` prefix and silently reintroduce
// the loose "name starts with mcp ⇒ it's an MCP call" heuristic that
// several adapters carry and that this table exists to replace with
// identities. A name that is not literally in the `mcp__` namespace
// gets no row rather than a guess.
//
// When the match came from the normalized pass the returned Entry
// carries the TABLE's spelling of Native, not the caller's.
func Resolve(tool, native string) (Entry, bool) {
	if native == "" {
		return Entry{}, false
	}
	for _, e := range table {
		if e.matches(tool, native) {
			return e, true
		}
	}
	key := normalizeKey(native)
	if key == "" {
		return Entry{}, false
	}
	for _, e := range table {
		if e.IsGlob() || (e.Tool != "" && e.Tool != tool) {
			continue
		}
		if normalizeKey(e.Native) == key {
			return e, true
		}
	}
	return Entry{}, false
}

// ResolveActionType is the convenience form of Resolve for the adapters:
// it returns the canonical action type, or ActionUnknown plus false when
// no row matched. This is the shape the per-adapter private actionMaps
// are replaced by in WP-T3.
func ResolveActionType(tool, native string) (string, bool) {
	e, ok := Resolve(tool, native)
	if !ok {
		return ActionUnknown, false
	}
	return e.ActionType, true
}

// For returns the native-name → action-type map for one tool: every
// LITERAL row that applies to it, with tool-specific rows overriding
// tool-less fallbacks. Glob rows are excluded — they are not names, and
// a caller doing map lookups must fall back to Resolve for them.
//
// This is the drop-in replacement for the package-private `actionMap`s
// in internal/adapter/* (WP-T3).
func For(tool string) map[string]string {
	out := make(map[string]string)
	// Walk in reverse so earlier (more specific) rows overwrite later
	// (fallback) ones, matching Resolve's top-down precedence.
	for i := len(table) - 1; i >= 0; i-- {
		e := table[i]
		if e.IsGlob() || (e.Tool != "" && e.Tool != tool) {
			continue
		}
		out[e.Native] = e.ActionType
	}
	return out
}

// Table returns a defensive copy of the ordered taxonomy table.
func Table() []Entry {
	out := make([]Entry, len(table))
	copy(out, table)
	return out
}

// Tools returns every tool that has at least one tool-specific row,
// sorted. Tool-less fallback rows do not contribute a name.
func Tools() []string {
	seen := make(map[string]struct{})
	for _, e := range table {
		if e.Tool != "" {
			seen[e.Tool] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// CategoriesForTool returns the canonical categories a tool's DECLARED
// vocabulary can express, in Categories() display order — the plan's §4
// "coverage-depth honesty" input, so a shallow-capture adapter is not
// misread as "doesn't use tools".
//
// Only tool-SPECIFIC rows count. The tool-less fallback rows (`read_file`,
// `bash`, `grep`, … ) and the global `mcp__*` glob apply to every tool at
// Resolve time, but crediting them here would give every tool — including
// one with no declared vocabulary at all — an identical non-zero baseline
// and destroy the honest zero that distinguishes "this adapter's capture
// is shallow" from "we have not mapped this adapter yet".
func CategoriesForTool(tool string) []string {
	if tool == "" {
		return []string{}
	}
	seen := make(map[string]struct{})
	for _, e := range table {
		if e.Tool == tool {
			seen[e.Category] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for _, c := range categoryOrder {
		if _, ok := seen[c]; ok {
			out = append(out, c)
		}
	}
	return out
}

// CoverageDepth reports how many DISTINCT canonical categories a tool's
// vocabulary can express. It is len(CategoriesForTool(tool)); the two must
// never disagree, so it delegates.
func CoverageDepth(tool string) int {
	return len(CategoriesForTool(tool))
}
