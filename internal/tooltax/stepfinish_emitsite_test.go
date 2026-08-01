package tooltax_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/tooltax"
)

// adapterRootForEmitSiteScan / modelsRootForEmitSiteScan are relative to
// this package directory (internal/tooltax), same convention as
// internal/db/asstbackfillgen/main_test.go's repoRoot.
const (
	adapterRootForEmitSiteScan = "../adapter"
	modelsRootForEmitSiteScan  = "../models"
)

// ratifiedEmitSiteFamilies lists the RawToolName predicates this guard
// verifies emit sites against internal/tooltax/table.go. Each entry is
// scoped to a CONCRETE, ALREADY-TRIAGED finding, never a blanket "every
// literal RawToolName" scan — a from-scratch widened scan (2026-07-31,
// same session that added the qwen-code row below) proved why: with the
// predicate removed entirely, ~99 harness-boundary marker literals
// (user_prompt, session_start, assistant_message, system_prompt, ...)
// across a dozen adapters have NO tooltax row at all (tooltax tracks
// TOOL CALLS, not synthesized message-boundary markers) and would turn
// into false failures under this guard's "must have a row" requirement.
// That same widened scan found exactly ONE real mismatch beyond the
// families already listed here — the qwen-code entry this comment
// documents — so the two entries below are the full, triaged set as of
// that scan; a future family must be triaged the same way before its
// predicate is added.
var ratifiedEmitSiteFamilies = []struct {
	match func(raw string) bool
	why   string
}{
	{
		match: func(raw string) bool { return strings.HasSuffix(raw, ".step_finish") },
		why:   "WP-T4 harness_call family (opencode / kilo-code-cli step_finish)",
	},
	{
		match: func(raw string) bool { return raw == "qwen-code.slash_command" },
		why:   "WP-T6 finding — user_prompt_expansion family (qwencode/adapter.go emitSlashCommand)",
	},
}

// isRatifiedEmitSiteRawName reports whether raw matches one of
// ratifiedEmitSiteFamilies.
func isRatifiedEmitSiteRawName(raw string) bool {
	for _, f := range ratifiedEmitSiteFamilies {
		if f.match(raw) {
			return true
		}
	}
	return false
}

// TestStepFinishEmitSitesAgreeWithTooltax is the WP-T6/B4 class guard.
//
// WP-T4 ratified `<tool>.step_finish` as ActionHarnessCall in the
// tooltax table (table.go's "opencode"/"kilo-code-cli" step_finish
// rows) and migration 077 backfilled the historical rows — but the
// EMIT SITES in internal/adapter/opencode/adapter.go and
// internal/adapter/kilocode/adapter.go still hardcoded
// models.ActionUnknown, so every NEW row kept landing unknown after
// the backfill (live proof: kilo-code-cli 68 harness_call/2 unknown
// post-probe, opencode 52/4). internal/tooltax/corpus_rows_test.go
// only asserts the TABLE, never an emit site, so it passed straight
// through the regression — the
// feedback_mutation_proof_vs_adversarial_review shape exactly.
//
// This test closes that hole for every RATIFIED family in
// ratifiedEmitSiteFamilies above (originally just step_finish; widened
// 2026-07-31 to also cover qwen-code's slash_command emit site once
// that finding was fixed and triaged — see that var's doc comment for
// why the scope stays an explicit list rather than every literal
// RawToolName): it walks every adapter source with go/ast, finds each
// models.ToolEvent composite literal whose RawToolName is a ratified
// string literal and whose ActionType/Tool are hardcoded models.*
// selectors (not computed), resolves the literal Tool selector to its
// string value from internal/models's own const declarations (parsed
// the same way, so a renamed constant can't silently desync the two
// sides), and requires the emit site's ActionType to AGREE with
// tooltax.Resolve(tool, rawName) — never a copy of the expected value,
// so a future re-ratification of a covered family can't drift against
// the emit sites again the way B4 did.
func TestStepFinishEmitSitesAgreeWithTooltax(t *testing.T) {
	toolValues := loadStringConstValues(t, modelsRootForEmitSiteScan, "Tool")
	actionValues := loadStringConstValues(t, modelsRootForEmitSiteScan, "Action")
	if len(toolValues) == 0 || len(actionValues) == 0 {
		t.Fatal("no models.Tool*/Action* constants found — the const scan is broken")
	}

	fset := token.NewFileSet()
	checked := 0

	err := filepath.Walk(adapterRootForEmitSiteScan, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Errorf("parse %s: %v", path, perr)
			return nil
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			raw, hasRaw := emitSiteLitFieldString(lit, "RawToolName")
			if !hasRaw || !isRatifiedEmitSiteRawName(raw) {
				return true
			}
			actionSel, hasAction := emitSiteLitFieldSelector(lit, "ActionType")
			if !hasAction {
				// A computed ActionType is legitimate — the per-adapter
				// conformance tests pin those; nothing in a ratified
				// family currently does this, so surface it loudly
				// rather than silently skip a shape change.
				t.Errorf("%s:%d: RawToolName %q has a non-literal ActionType — "+
					"this emit-site class guard cannot verify it; "+
					"update this test if the emit site shape changed intentionally",
					path, fset.Position(lit.Pos()).Line, raw)
				return true
			}
			toolSel, hasTool := emitSiteLitFieldSelector(lit, "Tool")
			if !hasTool {
				t.Errorf("%s:%d: RawToolName %q has no literal Tool field — "+
					"cannot resolve which tooltax row applies",
					path, fset.Position(lit.Pos()).Line, raw)
				return true
			}
			actionIdent := strings.TrimPrefix(actionSel, "models.")
			toolIdent := strings.TrimPrefix(toolSel, "models.")
			actionVal, ok := actionValues[actionIdent]
			if !ok {
				t.Errorf("%s:%d: ActionType selector %q does not resolve to a known "+
					"models.Action* constant value", path, fset.Position(lit.Pos()).Line, actionSel)
				return true
			}
			toolVal, ok := toolValues[toolIdent]
			if !ok {
				t.Errorf("%s:%d: Tool selector %q does not resolve to a known "+
					"models.Tool* constant value", path, fset.Position(lit.Pos()).Line, toolSel)
				return true
			}
			e, found := tooltax.Resolve(toolVal, raw)
			if !found {
				t.Errorf("%s:%d: %s %q has no tooltax row — a ratified emit-site "+
					"family must be added to internal/tooltax/table.go before an "+
					"adapter emits it", path, fset.Position(lit.Pos()).Line, toolVal, raw)
				return true
			}
			checked++
			if actionVal != e.ActionType {
				t.Errorf("%s:%d: drift: %s %q emits ActionType %s (%q) but "+
					"internal/tooltax/table.go says %q (WP-T6/B4 regression shape — the "+
					"emit site must agree with the ratified tooltax row)",
					path, fset.Position(lit.Pos()).Line, toolVal, raw, actionSel, actionVal, e.ActionType)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", adapterRootForEmitSiteScan, err)
	}
	if checked == 0 {
		t.Fatal("no ratified emit site reached the tooltax table — the guard is vacuous " +
			"(did a RawToolName predicate or the emit-site shape change?)")
	}
}

// emitSiteLitFieldString returns a composite literal's field value when
// it is a plain string literal.
func emitSiteLitFieldString(lit *ast.CompositeLit, field string) (string, bool) {
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != field {
			continue
		}
		bl, ok := kv.Value.(*ast.BasicLit)
		if !ok || bl.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(bl.Value)
		if err != nil {
			return "", false
		}
		return s, true
	}
	return "", false
}

// emitSiteLitFieldSelector returns a composite literal's field value
// (formatted "pkg.Sel") when it is a qualified identifier such as
// models.ActionHarnessCall.
func emitSiteLitFieldSelector(lit *ast.CompositeLit, field string) (string, bool) {
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != field {
			continue
		}
		sel, ok := kv.Value.(*ast.SelectorExpr)
		if !ok {
			return "", false
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		return pkg.Name + "." + sel.Sel.Name, true
	}
	return "", false
}

// loadStringConstValues parses every non-test .go file in dir and
// returns identifier -> value for top-level string-literal const specs
// whose name starts with prefix (e.g. "Tool" or "Action"). Parsing the
// real internal/models source (instead of importing the package and
// maintaining a hand-written mirror) means a renamed or added constant
// is picked up automatically and can never quietly fall out of sync
// with this scan.
func loadStringConstValues(t *testing.T, dir, prefix string) map[string]string {
	t.Helper()
	out := map[string]string{}
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", dir, err)
	}
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, d := range f.Decls {
				gd, ok := d.(*ast.GenDecl)
				if !ok || gd.Tok != token.CONST {
					continue
				}
				for _, spec := range gd.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, name := range vs.Names {
						if !strings.HasPrefix(name.Name, prefix) || i >= len(vs.Values) {
							continue
						}
						bl, ok := vs.Values[i].(*ast.BasicLit)
						if !ok || bl.Kind != token.STRING {
							continue
						}
						s, err := strconv.Unquote(bl.Value)
						if err != nil {
							continue
						}
						out[name.Name] = s
					}
				}
			}
		}
	}
	return out
}
