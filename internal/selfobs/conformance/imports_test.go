package conformance

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPackageImports_Bounded pins conformance as a pure-ish registry package: it
// may import internal/selfobs/emit + internal/selfobs/run + internal/provenance
// (plus context), but never internal/obs (the reverse-import boundary) nor the
// infrastructure imports database/sql / fsnotify.
//
// It checks DIRECT imports ONLY (parser.ImportsOnly over this package's own
// files). net/http is deliberately NOT forbidden: emit transitively pulls it in
// via the OTLP exporter, so a transitive net/http is legitimate — but this
// package never imports net/http directly, and a direct import would be caught
// only if listed, which it is not, on purpose.
func TestPackageImports_Bounded(t *testing.T) {
	t.Parallel()

	forbidden := []string{
		"github.com/marmutapp/superbased-observer/internal/obs",
		"database/sql",
		"github.com/fsnotify/fsnotify",
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(cwd, "*.go"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if len(matches) == 0 {
		t.Fatalf("no source files in %s", cwd)
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
			for _, bad := range forbidden {
				if p == bad || strings.HasPrefix(p, bad+"/") {
					t.Errorf("%s: forbidden direct import %q (conformance must not import internal/obs, database/sql, or fsnotify)", filepath.Base(path), p)
				}
			}
		}
	}
}
