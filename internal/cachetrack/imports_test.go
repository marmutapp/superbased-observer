package cachetrack

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPackageImports_Bounded enforces the module-boundary
// discipline (CLAUDE.md "Module Boundaries & Anti-Spaghetti
// Discipline" + spec §24.1). The cachetrack package contains pure
// logic; non-test source files must not reach for infrastructure
// imports (database/sql, net/http, fsnotify) or other observer
// subsystems (internal/store, internal/proxy, internal/db,
// internal/adapter, internal/watcher, internal/hook,
// internal/intelligence). Failing this test is a defect — the
// failure message names the file and the offending import so the
// offender is named at compile time.
//
// Tests may import broader stdlib infra (go/parser, httptest,
// etc.) without violating the boundary; only non-test files are
// scanned.
func TestPackageImports_Bounded(t *testing.T) {
	t.Parallel()

	forbidden := []string{
		"database/sql",
		"net/http",
		"github.com/fsnotify/fsnotify",
		"github.com/marmutapp/superbased-observer/internal/store",
		"github.com/marmutapp/superbased-observer/internal/proxy",
		"github.com/marmutapp/superbased-observer/internal/db",
		"github.com/marmutapp/superbased-observer/internal/adapter",
		"github.com/marmutapp/superbased-observer/internal/watcher",
		"github.com/marmutapp/superbased-observer/internal/hook",
		"github.com/marmutapp/superbased-observer/internal/intelligence",
		"github.com/marmutapp/superbased-observer/internal/freshness",
		"github.com/marmutapp/superbased-observer/internal/scoring",
		"github.com/marmutapp/superbased-observer/internal/compression",
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
