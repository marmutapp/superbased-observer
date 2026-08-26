package evalcore

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoForbiddenImports pins evalcore's purity (CLAUDE.md §1, plan
// docs/plans/org-eval-service-comprehensive-plan-2026-08-20.md §2.1):
// internal/evalcore is the scorer registry + runner shared by the node eval
// plane (internal/obs/eval) and the org eval service
// (internal/orgserver/orgeval), and must not import database/sql, net/http,
// or fsnotify — persistence and the judge's network call belong to the
// caller, reached only via the injected JudgeClient interface.
func TestNoForbiddenImports(t *testing.T) {
	forbidden := []string{"database/sql", "net/http", "github.com/fsnotify/fsnotify"}
	fset := token.NewFileSet()
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, f := range matches {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for _, imp := range af.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			for _, bad := range forbidden {
				if path == bad {
					t.Errorf("%s imports forbidden %q — internal/evalcore must stay pure", f, bad)
				}
			}
		}
	}
}
