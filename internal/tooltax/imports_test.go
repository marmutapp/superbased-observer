package tooltax

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoForbiddenImports is the STRICTEST import pin in the repo.
//
// internal/routing, internal/predict and internal/cachewarm forbid
// database/sql, net/http and fsnotify. tooltax forbids all of those AND
// every package inside this module — including internal/models.
//
// That is not stylistic. internal/models must be able to import tooltax
// (models.IsMCPToolName delegates to tooltax.MCPIdentity), and so must
// internal/policy, internal/adapter/*, internal/store and the dashboard.
// Any project-internal import here would create an import cycle the
// moment one of those delegations lands, so the pin is load-bearing:
// it is the reason the canonical action types and tool ids are
// re-declared as string constants in actiontype.go / table.go instead of
// aliasing models.Action* / models.Tool*. The conformance tests in
// package tooltax_test pin those values to models so the duplication
// cannot drift.
func TestNoForbiddenImports(t *testing.T) {
	const modulePrefix = "github.com/marmutapp/superbased-observer/"

	forbidden := []string{
		"database/sql",
		"net/http",
		"github.com/fsnotify/fsnotify",
	}

	fset := token.NewFileSet()
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("no .go files found — the pin would vacuously pass")
	}
	checked := 0
	for _, f := range matches {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		checked++
		af, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for _, imp := range af.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			for _, bad := range forbidden {
				if path == bad {
					t.Errorf("%s imports forbidden %q — internal/tooltax must stay pure", f, bad)
				}
			}
			if strings.HasPrefix(path, modulePrefix) {
				t.Errorf("%s imports project-internal %q — internal/tooltax must import NOTHING "+
					"from this module (see doc.go: internal/models imports tooltax, so any "+
					"internal import here is a cycle)", f, path)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no non-test .go files checked — the pin would vacuously pass")
	}
}
