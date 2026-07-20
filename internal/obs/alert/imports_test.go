package alert

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoForbiddenImports pins alert-core purity (CLAUDE.md rule #1, obs plan
// §11): internal/obs/alert is the pure evaluation logic — rules + metric
// snapshots in, fired-alert values out — and must not import database/sql,
// net/http, or fsnotify, nor internal/config. The metric snapshot is computed
// in internal/obs/store, the webhook POST + scheduler live in cmd/observer, and
// config is translated into Rule values at the boundary. Mirrors
// internal/obs/admission/imports_test.go.
func TestNoForbiddenImports(t *testing.T) {
	forbidden := []string{
		"database/sql",
		"net/http",
		"github.com/fsnotify/fsnotify",
		"github.com/marmutapp/superbased-observer/internal/config",
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
				if path == bad {
					t.Errorf("%s imports forbidden %q — internal/obs/alert must stay pure", f, bad)
				}
			}
		}
	}
}
