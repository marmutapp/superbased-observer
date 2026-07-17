package egress

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoForbiddenImports pins egress-core purity (CLAUDE.md §1, design §3.1).
// Beyond admission's forbid list (database/sql, net/http, fsnotify,
// internal/config, internal/obs/eval), egress ALSO must not import
// internal/routing (the vocabulary is re-expressed as Plane-A enums, finding
// 16) or internal/obs/admission (no admission type crosses the seam — the
// verdict enters as plain strings). Persistence lives in internal/obs/store;
// config is translated into a PolicyInput at the boundary (Compile).
func TestNoForbiddenImports(t *testing.T) {
	forbidden := []string{
		"database/sql",
		"net/http",
		"github.com/fsnotify/fsnotify",
		"github.com/marmutapp/superbased-observer/internal/config",
		"github.com/marmutapp/superbased-observer/internal/obs/eval",
		"github.com/marmutapp/superbased-observer/internal/routing",
		"github.com/marmutapp/superbased-observer/internal/obs/admission",
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
				if p == bad {
					t.Errorf("%s imports forbidden %q — internal/obs/egress must stay pure and routing/admission-free", f, bad)
				}
			}
		}
	}
}
