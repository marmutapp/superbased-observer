package nodefeatures

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoForbiddenImports pins policyfam/nodefeatures's purity (CLAUDE.md
// §1): this is the node.features family COMPILER + decision helper, reused
// by internal/orgserver and internal/orgclient/cmd/observer, so it must not
// import database/sql, net/http, fsnotify, internal/config, internal/obs,
// or internal/proxy (mirrors internal/policyfam/{admission,egress,
// providers,nodegov}).
func TestNoForbiddenImports(t *testing.T) {
	forbidden := []string{
		"database/sql",
		"net/http",
		"github.com/fsnotify/fsnotify",
		"github.com/marmutapp/superbased-observer/internal/config",
		"github.com/marmutapp/superbased-observer/internal/obs",
		"github.com/marmutapp/superbased-observer/internal/proxy",
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
					t.Errorf("%s imports forbidden %q — internal/policyfam/nodefeatures must stay pure", f, bad)
				}
			}
		}
	}
}
