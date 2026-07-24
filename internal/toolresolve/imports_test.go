package toolresolve

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPackageImports_Bounded enforces the module-boundary discipline
// (CLAUDE.md "Module Boundaries" #1): toolresolve is PURE — all I/O is
// injected through Env. Non-test source files must not reach for
// infrastructure (database/sql, net/http, os/exec, fsnotify) NOR even `os`
// itself: the package must not touch the real filesystem or environment. The
// only observer dependency allowed is internal/integration (the DATA the
// resolver dispatches on). A failure names the file and the offending import.
func TestPackageImports_Bounded(t *testing.T) {
	t.Parallel()

	forbidden := []string{
		"database/sql",
		"net/http",
		"os",
		"os/exec",
		"github.com/fsnotify/fsnotify",
	}
	// Any observer-internal import other than integration is a boundary
	// violation for this pure package.
	const allowedInternal = "github.com/marmutapp/superbased-observer/internal/integration"

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
					t.Errorf("%s: forbidden import %q (toolresolve is pure — inject I/O via Env)", filepath.Base(path), p)
				}
			}
			if strings.HasPrefix(p, "github.com/marmutapp/superbased-observer/internal/") && p != allowedInternal {
				t.Errorf("%s: forbidden observer-internal import %q (only internal/integration allowed)", filepath.Base(path), p)
			}
		}
	}
}
