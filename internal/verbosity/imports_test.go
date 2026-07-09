package verbosity

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoForbiddenImports pins the module-boundary discipline (CLAUDE.md
// §1): internal/verbosity is pure logic. It must not import database/sql,
// net/http, or fsnotify — I/O is injected at the store seam
// (internal/store/verbosity.go) and the ingest graft, never here. It also
// must not reach into codeintel: the language table is OWNED here (plan
// §4), deliberately broader than codeintel's parseable-language set.
func TestNoForbiddenImports(t *testing.T) {
	forbidden := []string{
		"database/sql",
		"net/http",
		"github.com/fsnotify/fsnotify",
		"github.com/marmutapp/superbased-observer/internal/codeintel",
		"github.com/marmutapp/superbased-observer/internal/codeintel/parse",
	}
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
					t.Errorf("%s imports forbidden %q — internal/verbosity must stay pure", f, bad)
				}
			}
		}
	}
}
