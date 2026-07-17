package termfeed

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoForbiddenImports pins the module-boundary discipline (CLAUDE.md §1):
// internal/termfeed is a pure in-process primitive. It must not import
// database/sql, net/http, or fsnotify — and (F4 requirement) must not depend on
// the terminal packages, so it stays a generic feed the application service
// wires producers into.
func TestNoForbiddenImports(t *testing.T) {
	forbidden := []string{
		"database/sql",
		"net/http",
		"github.com/fsnotify/fsnotify",
		"github.com/marmutapp/superbased-observer/internal/termsession",
		"github.com/marmutapp/superbased-observer/internal/termrun",
		"github.com/marmutapp/superbased-observer/internal/termoob",
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
					t.Errorf("%s imports forbidden %q — internal/termfeed must stay pure & terminal-package-free", f, bad)
				}
			}
		}
	}
}
