package admission

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoForbiddenImports pins policyfam/admission's purity (CLAUDE.md §1,
// Plane-A P0-5 Phase F): this is the extracted family COMPILER, reused by
// internal/orgserver and internal/orgclient, so it must not import
// database/sql, net/http, fsnotify, or internal/obs (the reverse-import
// boundary this package exists to enable — tests/invariant/obs_boundary_test.go
// pins the flip side: internal/obs itself must not leak into orgserver/
// orgclient's dependency graph).
func TestNoForbiddenImports(t *testing.T) {
	forbidden := []string{
		"database/sql",
		"net/http",
		"github.com/fsnotify/fsnotify",
		"github.com/marmutapp/superbased-observer/internal/config",
		"github.com/marmutapp/superbased-observer/internal/obs",
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
				if path == bad || strings.HasPrefix(path, bad+"/") {
					t.Errorf("%s imports forbidden %q — internal/policyfam/admission must stay pure", f, bad)
				}
			}
		}
	}
}
