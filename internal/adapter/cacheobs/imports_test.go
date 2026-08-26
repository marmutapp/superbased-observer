package cacheobs

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoForbiddenImports pins the module-boundary discipline
// (CLAUDE.md "Module Boundaries & Anti-Spaghetti Discipline" / spec
// §24.1): internal/adapter/cacheobs is pure logic shared by every
// Tier-2 producer. It must never import database/sql, net/http, or
// fsnotify — I/O and per-tool parsing stay in the adapter package
// that calls into this one.
func TestNoForbiddenImports(t *testing.T) {
	forbidden := []string{
		"database/sql",
		"net/http",
		"github.com/fsnotify/fsnotify",
		"github.com/marmutapp/superbased-observer/internal/store",
		"github.com/marmutapp/superbased-observer/internal/proxy",
		"github.com/marmutapp/superbased-observer/internal/db",
		"github.com/marmutapp/superbased-observer/internal/watcher",
		"github.com/marmutapp/superbased-observer/internal/hook",
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(cwd, "*.go"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if len(matches) == 0 {
		t.Fatalf("no source files in %s", cwd)
	}

	fset := token.NewFileSet()
	for _, path := range matches {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, imp := range file.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			for _, bad := range forbidden {
				if p == bad {
					t.Errorf("%s: forbidden import %q (module boundary §24.1)", filepath.Base(path), p)
				}
			}
		}
	}
}
