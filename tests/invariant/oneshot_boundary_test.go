// Boundary invariants for the npx one-shot usage report
// (docs/plans/npx-one-shot-report-plan-2026-07-30.md §2.4, WP4).
//
// Two independent guarantees are pinned here:
//
//  1. internal/oneshot stays a PURE package (no SQL, no HTTP, no
//     os/exec, no fsnotify, and none of the three package imports that
//     would defeat the point of the boundary). This duplicates
//     internal/oneshot/imports_test.go belt-and-braces (plan §4): if that
//     package's own test is ever deleted or skipped, this still fails.
//  2. cmd/observer/usage.go — the one-shot's only stateful boundary —
//     never REFERENCES (as code, not as a comment) any of the identifiers
//     that would reintroduce a side effect the feature promises not to
//     have: touching ~/.observer, registering a hook, talking to the org
//     or aggregate egress rails, standing up the dashboard/proxy/indexer,
//     stamping org identity, opting back into the one network-capable
//     capture path, mutating the process environment, or writing config.
package invariant

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 1. internal/oneshot purity (belt-and-braces with its own imports_test.go)
// ---------------------------------------------------------------------------

// oneshotForbiddenImports mirrors internal/oneshot/imports_test.go's own
// forbidden list exactly. Kept as a literal duplicate rather than a shared
// helper on purpose: the whole point of "belt-and-braces" is that this
// suite does not depend on that package's test file continuing to exist.
var oneshotForbiddenImports = []string{
	"database/sql",
	"net/http",
	"os/exec",
	"github.com/fsnotify/fsnotify",
	"github.com/marmutapp/superbased-observer/internal/store",
	"github.com/marmutapp/superbased-observer/internal/db",
	"github.com/marmutapp/superbased-observer/internal/intelligence/cost",
}

// TestOneShotPurePackageImports enforces plan §2.1/§2.4/§4: internal/oneshot
// imports none of the forbidden set. Uses the house assertNoImport helper
// (tests/invariant/aggregate_test.go), same package.
func TestOneShotPurePackageImports(t *testing.T) {
	t.Parallel()
	dir := filepath.Join("..", "..", "internal", "oneshot")
	for _, forbidden := range oneshotForbiddenImports {
		assertNoImport(t, dir, forbidden)
	}
}

// ---------------------------------------------------------------------------
// 2. cmd/observer/usage.go — forbidden-identifier AST scan
// ---------------------------------------------------------------------------

// forbiddenSymbol describes one identifier (bare) or qualified selector
// (Qualifier.Name) that must never appear as CODE in the scanned file.
//
// Qualifier == "" matches a BARE identifier wherever it appears in the
// AST — which, because go/ast.Walk visits a *ast.SelectorExpr's X and Sel
// fields as their own *ast.Ident nodes, also catches a package used only
// as a qualifier (e.g. a bare "orgclient" mention as orgclient.PushOnce's
// X) and a method/option name reached through any qualifier (e.g.
// anything.WithNetworkRecovery(...)) without needing one entry per
// possible qualifier.
//
// Qualifier != "" matches only a *ast.SelectorExpr whose X is exactly that
// identifier and whose Sel matches Name (exact, or as a prefix when
// Prefix is set — the config.Write* family).
type forbiddenSymbol struct {
	Qualifier string
	Name      string
	Prefix    bool
}

// usageForbiddenSymbols is the exact set from the plan (§2.4, §4, WP4 AC):
// every side effect this command promises never to touch. config.Write* is
// the separate "never writes config" assertion folded into the same list.
var usageForbiddenSymbols = []forbiddenSymbol{
	{Name: "loadConfigAndDB"},
	{Qualifier: "hook", Name: "Register"},
	{Name: "orgclient"},
	{Name: "aggregateclient"},
	{Qualifier: "dashboard", Name: "New"},
	{Qualifier: "proxy", Name: "New"},
	{Qualifier: "indexing", Name: "New"},
	{Qualifier: "identity", Name: "NewStamper"},
	{Name: "WithNetworkRecovery"},
	{Qualifier: "os", Name: "Setenv"},
	{Qualifier: "config", Name: "Write", Prefix: true},
}

// scanForbiddenSymbols parses src as Go source (filename is used only for
// parser diagnostics — src need not exist on disk, which is what lets the
// fixture tests below exercise the checker without ever touching the real
// cmd/observer/usage.go) and returns one "<pos>: <matched text>" entry per
// AST match of a forbidden symbol.
//
// It inspects only *ast.SelectorExpr and *ast.Ident nodes. It NEVER looks
// at comment text: go/parser never turns a comment into an *ast.Ident or
// *ast.SelectorExpr, so a doc comment that merely MENTIONS one of these
// names (several legitimately do — see usage.go's doc comments explaining
// why loadConfigAndDB is not used) cannot trigger a false positive. This is
// the AST-not-raw-string distinction the plan requires.
func scanForbiddenSymbols(t *testing.T, filename string, src []byte, forbidden []forbiddenSymbol) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}

	var hits []string
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.SelectorExpr:
			xIdent, ok := node.X.(*ast.Ident)
			if !ok {
				return true
			}
			for _, f := range forbidden {
				if f.Qualifier == "" || f.Qualifier != xIdent.Name {
					continue
				}
				if symbolNameMatches(f, node.Sel.Name) {
					hits = append(hits, fmt.Sprintf("%s: %s.%s", fset.Position(node.Pos()), xIdent.Name, node.Sel.Name))
				}
			}
		case *ast.Ident:
			for _, f := range forbidden {
				if f.Qualifier != "" {
					continue
				}
				if symbolNameMatches(f, node.Name) {
					hits = append(hits, fmt.Sprintf("%s: %s", fset.Position(node.Pos()), node.Name))
				}
			}
		}
		return true
	})
	return hits
}

func symbolNameMatches(f forbiddenSymbol, name string) bool {
	if f.Prefix {
		return strings.HasPrefix(name, f.Name)
	}
	return name == f.Name
}

// TestOneShotUsageCmdNeverReferencesForbiddenSymbols is the real assertion (plan
// §2.4/§4, WP4 AC): cmd/observer/usage.go — the one-shot's only stateful
// boundary — references none of usageForbiddenSymbols anywhere in its AST.
func TestOneShotUsageCmdNeverReferencesForbiddenSymbols(t *testing.T) {
	t.Parallel()
	path := filepath.Join("..", "..", "cmd", "observer", "usage.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if hits := scanForbiddenSymbols(t, path, src, usageForbiddenSymbols); len(hits) > 0 {
		t.Errorf("cmd/observer/usage.go references forbidden symbol(s) (plan §2.4 zero-side-effect list):\n%s",
			strings.Join(hits, "\n"))
	}
}

// ---------------------------------------------------------------------------
// 3. Prove the checker CAN fire — checker-on-fixture, never on real usage.go
// ---------------------------------------------------------------------------

// TestOneShotScanForbiddenSymbolsCatchesDeliberateViolations feeds
// scanForbiddenSymbols small in-memory fixture sources — never the real
// cmd/observer/usage.go — each containing exactly one deliberate violation,
// and asserts the checker flags it. This is what proves
// TestOneShotUsageCmdNeverReferencesForbiddenSymbols is a live tripwire and not a
// vacuously-passing assertion (plan §4: "fail loudly when deliberately
// violated"). Covers more than the required two, including one per
// matching shape (qualified exact, qualified prefix, bare identifier, bare
// identifier used only as a selector qualifier, and bare identifier used
// only as a selector's method/option name).
func TestOneShotScanForbiddenSymbolsCatchesDeliberateViolations(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		src  string
		want string // substring expected in the reported hit
	}{
		{
			name: "os.Setenv (qualified, exact)",
			src: `package fixture

func demo() {
	os.Setenv("OBSERVER_OBSERVER_DB_PATH", "/tmp/live.db")
}
`,
			want: "os.Setenv",
		},
		{
			name: "config.Write* (qualified, prefix family)",
			src: `package fixture

func demo() {
	config.WriteGlobal(cfg)
}
`,
			want: "config.WriteGlobal",
		},
		{
			name: "hook.Register (qualified, exact)",
			src: `package fixture

func demo() {
	hook.Register(paths)
}
`,
			want: "hook.Register",
		},
		{
			name: "bare package reference orgclient (used only as a qualifier)",
			src: `package fixture

func demo() {
	orgclient.PushOnce(ctx)
}
`,
			want: "orgclient",
		},
		{
			name: "bare package reference aggregateclient",
			src: `package fixture

func demo() {
	_ = aggregateclient.Submit
}
`,
			want: "aggregateclient",
		},
		{
			name: "WithNetworkRecovery reached through an arbitrary qualifier",
			src: `package fixture

func demo() {
	opts = append(opts, antigravity.WithNetworkRecovery(true))
}
`,
			want: "WithNetworkRecovery",
		},
		{
			name: "bare loadConfigAndDB call",
			src: `package fixture

func demo() {
	loadConfigAndDB("", nil)
}
`,
			want: "loadConfigAndDB",
		},
		{
			name: "dashboard.New / proxy.New / indexing.New / identity.NewStamper",
			src: `package fixture

func demo() {
	_ = dashboard.New(nil)
	_ = proxy.New(nil)
	_ = indexing.New(nil)
	_ = identity.NewStamper(nil)
}
`,
			want: "dashboard.New",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hits := scanForbiddenSymbols(t, "fixture.go", []byte(tc.src), usageForbiddenSymbols)
			if len(hits) == 0 {
				t.Fatalf("checker did NOT fire on a deliberate violation containing %q — the tripwire is dead", tc.want)
			}
			var found bool
			for _, h := range hits {
				if strings.Contains(h, tc.want) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("hits = %v, want one containing %q", hits, tc.want)
			}
		})
	}
}

// TestOneShotScanForbiddenSymbolsNoFalsePositiveOnCleanSource is the companion
// sanity check: legitimate, textually-similar usage (config.Load /
// os.Getenv / a comment that MENTIONS a forbidden name) must NOT be
// flagged. Without this, the checker could be trivially "too broad" and
// the real test would be silently useless (everything always fails, or a
// future refactor is blocked by noise).
func TestOneShotScanForbiddenSymbolsNoFalsePositiveOnCleanSource(t *testing.T) {
	t.Parallel()
	src := []byte(`package fixture

// This comment mentions loadConfigAndDB, hook.Register, orgclient,
// aggregateclient, dashboard.New, proxy.New, indexing.New,
// identity.NewStamper, WithNetworkRecovery, os.Setenv, and config.Write —
// on purpose, to prove comment text is never mistaken for code.
func demo() {
	cfg, _ := config.Load(config.LoadOptions{})
	_ = os.Getenv("HOME")
	_ = cfg
}
`)
	if hits := scanForbiddenSymbols(t, "fixture.go", src, usageForbiddenSymbols); len(hits) != 0 {
		t.Errorf("false positive(s) on clean source (comment-only mentions + legitimate config.Load/os.Getenv): %v", hits)
	}
}
