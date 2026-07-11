package modelvalue

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPackageImports_Bounded enforces the module boundary the package
// doc declares: modelvalue is PURE aggregation — SQL rows are injected
// by the store seam (internal/store/modelvalue.go), and this package
// must never import database/sql or any infrastructure subsystem.
// Stricter than the advisor (whose loader holds its own SQL); mirrors
// internal/cachetrack/imports_test.go and internal/routing.
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
		"github.com/marmutapp/superbased-observer/internal/mcp",
		"github.com/marmutapp/superbased-observer/internal/config",
		"github.com/marmutapp/superbased-observer/internal/intelligence/advisor",
		"github.com/marmutapp/superbased-observer/internal/intelligence/dashboard",
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
				if p == bad || strings.HasPrefix(p, bad+"/") {
					t.Errorf("%s: forbidden import %q (pure-aggregation boundary)", filepath.Base(path), p)
				}
			}
		}
	}
}
