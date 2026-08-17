package workspace

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoForbiddenImports pins the module-boundary discipline (CLAUDE.md
// "Module Boundaries & Anti-Spaghetti Discipline" #1): internal/workspace
// is pure logic. It decides git argv + destination paths but never runs
// a subprocess, touches a database, makes a network call, or watches the
// filesystem — I/O is injected at the caller (U4/U5).
func TestNoForbiddenImports(t *testing.T) {
	forbidden := []string{
		"os/exec",
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
		t.Fatalf("glob found no .go files — test is not actually checking anything")
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
					t.Errorf("%s imports forbidden %q — internal/workspace must stay pure (git argv/paths only, no subprocess exec)", f, bad)
				}
			}
		}
	}
}
