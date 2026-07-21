package remoteauth

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// forbiddenImports pins remoteauth as a PURE-LOGIC package (CLAUDE.md module
// rule #1): it may not import net/http, database/sql, or fsnotify. The HTTP
// glue, SQL persistence, and file I/O live in the caller layers that inject
// these primitives.
var forbiddenImports = []string{
	"net/http",
	"database/sql",
	"github.com/fsnotify/fsnotify",
}

func TestNoIOImports(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		af, perr := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ImportsOnly)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		for _, imp := range af.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			for _, bad := range forbiddenImports {
				if p == bad || strings.HasPrefix(p, bad+"/") {
					t.Errorf("%s imports forbidden %q — remoteauth must stay pure (no net/http, database/sql, fsnotify)", name, p)
				}
			}
		}
	}
}
