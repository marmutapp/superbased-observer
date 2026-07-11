package admission

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoForbiddenImports pins admission-core purity (CLAUDE.md §1, obs plan
// §11, admission spec §2): internal/obs/admission is the pipeline + prefilter +
// judge-prompt + lint and must not import database/sql, net/http, or fsnotify.
// Persistence lives in internal/obs/store; the judge's network call is the
// host's, reached only through the injected JudgeClient interface; config is
// translated into a PolicySpec at the boundary (Compile), so this package
// never imports internal/config either.
func TestNoForbiddenImports(t *testing.T) {
	forbidden := []string{
		"database/sql",
		"net/http",
		"github.com/fsnotify/fsnotify",
		"github.com/marmutapp/superbased-observer/internal/config",
		"github.com/marmutapp/superbased-observer/internal/obs/eval",
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
					t.Errorf("%s imports forbidden %q — internal/obs/admission must stay pure", f, bad)
				}
			}
		}
	}
}
