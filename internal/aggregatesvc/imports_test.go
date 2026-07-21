package aggregatesvc

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestPackageImports_Bounded pins the collector's impurity boundary (CLAUDE.md
// #1/#2, G25 design invariant): aggregatesvc orchestrates ONLY through seams —
// internal/store for the ledger + consent receipt, internal/aggregateclient for
// egress, and an injected Builder for the payload assembly. It must therefore
// write no raw I/O of its own: no database/sql, no net/http, no fsnotify
// directly. (Importing internal/store / internal/aggregateclient — which use
// those transitively — is allowed and expected; the guard is on DIRECT raw-I/O
// imports.)
func TestPackageImports_Bounded(t *testing.T) {
	t.Parallel()
	forbidden := map[string]bool{
		"database/sql":                 true,
		"net/http":                     true,
		"github.com/fsnotify/fsnotify": true,
	}
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
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
			if forbidden[p] {
				t.Errorf("%s imports forbidden %q — the collector must reach raw I/O only through its seams", filepath.Base(path), p)
			}
		}
	}
}
