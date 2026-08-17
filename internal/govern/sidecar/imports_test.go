package sidecar

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestPackageIsStdlibOnly pins the sidecar reader's purity (§6.3).
//
// It is read from config.Load, which nine cmd/observer/hook.go call sites
// run on the developer's critical path. A dependency on the store, on HTTP,
// on fsnotify, or on internal/config (which would also be an import cycle)
// would put that cost — or that failure surface — inside every tool call.
func TestPackageIsStdlibOnly(t *testing.T) {
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
			if strings.Contains(path, ".") {
				t.Errorf("%s imports %q — internal/govern/sidecar must stay stdlib-only", f, path)
			}
		}
	}
}
