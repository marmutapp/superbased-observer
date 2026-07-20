package handoff

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoForbiddenImports pins the module-boundary discipline (CLAUDE.md
// §1): internal/handoff is pure logic. Transcripts and action facts arrive
// pre-loaded from the store seam / adapter readers via internal/handoffsvc;
// pricing arrives as a plain PriceFunc. No SQL, no HTTP, no fsnotify, no
// file I/O, no cost/routing/store imports — the same posture
// internal/cachewarm and internal/predict keep.
func TestNoForbiddenImports(t *testing.T) {
	forbidden := []string{
		"database/sql",
		"net/http",
		"os",
		"io/ioutil",
		"github.com/fsnotify/fsnotify",
		"github.com/marmutapp/superbased-observer/internal/intelligence/cost",
		"github.com/marmutapp/superbased-observer/internal/routing",
		"github.com/marmutapp/superbased-observer/internal/store",
		"github.com/marmutapp/superbased-observer/internal/adapter",
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
					t.Errorf("%s imports forbidden %q — internal/handoff must stay pure (CLAUDE.md §1)", f, path)
				}
			}
		}
	}
}
