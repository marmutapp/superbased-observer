package egress

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoForbiddenImports pins policyfam/egress's purity (CLAUDE.md §1,
// design §3.1, Plane-A P0-5 Phase F): beyond the admission family's forbid
// list (database/sql, net/http, fsnotify, internal/obs), egress ALSO must
// not import internal/routing (the vocabulary is re-expressed as Plane-A
// enums, finding 16) or internal/obs/admission (no admission type crosses
// this seam — the verdict enters as plain strings).
func TestNoForbiddenImports(t *testing.T) {
	forbidden := []string{
		"database/sql",
		"net/http",
		"github.com/fsnotify/fsnotify",
		"github.com/marmutapp/superbased-observer/internal/config",
		"github.com/marmutapp/superbased-observer/internal/obs",
		"github.com/marmutapp/superbased-observer/internal/routing",
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
			p := strings.Trim(imp.Path.Value, `"`)
			for _, bad := range forbidden {
				if p == bad || strings.HasPrefix(p, bad+"/") {
					t.Errorf("%s imports forbidden %q — internal/policyfam/egress must stay pure and routing/obs-free", f, bad)
				}
			}
		}
	}
}
