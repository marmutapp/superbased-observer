package aggregate

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPackageImports_Bounded pins the pure boundary the package doc declares
// (design §6.1, mirroring internal/intelligence/modelvalue/imports_test.go):
// internal/aggregate is pure assembly — the SQL read that feeds Build lives in
// internal/aggregatesource and the network egress in internal/aggregateclient.
// This package must never import database/sql, net/http, or any of the
// infrastructure subsystems that could give a wire type an I/O side channel.
func TestPackageImports_Bounded(t *testing.T) {
	t.Parallel()

	forbidden := []string{
		"database/sql",
		"net/http",
		"github.com/fsnotify/fsnotify",
		"github.com/marmutapp/superbased-observer/internal/store",
		"github.com/marmutapp/superbased-observer/internal/orgclient",
		"github.com/marmutapp/superbased-observer/internal/proxy",
		"github.com/marmutapp/superbased-observer/internal/intelligence/cost",
		"github.com/marmutapp/superbased-observer/internal/aggregateclient",
		"github.com/marmutapp/superbased-observer/internal/aggregatesource",
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
					t.Errorf("%s: forbidden import %q (pure-aggregation boundary)", filepath.Base(path), p)
				}
			}
		}
	}
}
