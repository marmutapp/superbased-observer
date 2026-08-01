package statusline

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoForbiddenImports pins internal/statusline's purity (CLAUDE.md
// "Module Boundaries" #1): no database/sql, net/http, os/exec, fsnotify,
// and no internal/store, internal/db, or internal/intelligence/cost. All
// I/O (stdin read, the bounded daemon HTTP call, the lockfile pre-check)
// lives at the cmd/observer/statusline.go boundary, never here. Modeled
// on internal/oneshot/imports_test.go.
func TestNoForbiddenImports(t *testing.T) {
	forbidden := []string{
		"database/sql",
		"net/http",
		"os/exec",
		"github.com/fsnotify/fsnotify",
		"github.com/marmutapp/superbased-observer/internal/store",
		"github.com/marmutapp/superbased-observer/internal/db",
		"github.com/marmutapp/superbased-observer/internal/intelligence/cost",
	}

	fset := token.NewFileSet()
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("no .go files found in internal/statusline — glob broken?")
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
					t.Errorf("%s imports forbidden package %q", f, path)
				}
			}
		}
	}
}
