package announce

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoForbiddenImports pins the module-boundary discipline (CLAUDE.md
// §1): internal/announce is pure data + logic. It must not import
// database/sql, net/http, or fsnotify.
//
// net/http matters more here than in a typical pure package: the whole
// point of the approved design (plan §0/§6) is that announcements ride
// channels that already exist and NEVER add an unsolicited outbound
// call. An http import in this package would be the first symptom of
// re-inventing the rejected phone-home rail, so it fails loudly here.
func TestNoForbiddenImports(t *testing.T) {
	forbidden := []string{
		"database/sql",
		"net/http",
		"net",
		"github.com/fsnotify/fsnotify",
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
					t.Errorf("%s imports forbidden %q — internal/announce must stay pure (no transport)", f, bad)
				}
			}
		}
	}
}
