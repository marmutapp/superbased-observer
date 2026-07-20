package invariant

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The external code-graph dependency (the `codebase-memory-mcp` companion
// behind internal/codegraph) was decommissioned in Phase 4 of the
// internal/codeintel build-out — replaced by the in-process, CGO-free
// code-intelligence engine. These guards fail loudly if it reappears, so a
// future change can't silently re-introduce a third-party binary download
// or a dependency on the deleted package (plan §12.5;
// docs/codeintel/migration-from-codegraph.md).
//
// The forbidden tokens are assembled from fragments so this guard file
// never matches itself.
var (
	forbiddenPkg    = "github.com/marmutapp/superbased-observer/internal/" + "codegraph"
	forbiddenBinary = "codebase-memory" + "-mcp"
)

// scanRoots are the source trees the guard walks, relative to the repo
// root (resolved from this test's working dir).
var scanRoots = []string{"internal", "cmd"}

// repoRoot walks up from the test's working directory until it finds the
// go.mod, so the guard works regardless of where `go test` is invoked.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("repoRoot: go.mod not found above %s", dir)
		}
		dir = parent
	}
}

// TestCodegraphPackageDeleted asserts the internal/codegraph directory no
// longer exists.
func TestCodegraphPackageDeleted(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	dir := filepath.Join(root, "internal", "codegraph")
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		t.Fatalf("internal/codegraph reappeared at %s — the external code-graph dependency is decommissioned (Phase 4); use internal/codeintel", dir)
	}
}

// TestNoCodegraphImport walks every .go file under the scan roots and
// fails if any imports the deleted package. AST import parsing keeps this
// precise — a string literal naming the path (e.g. a forbidden-imports
// allow-list) is intentionally not flagged.
func TestNoCodegraphImport(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	fset := token.NewFileSet()
	for _, sub := range scanRoots {
		base := filepath.Join(root, sub)
		walkErr := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if perr != nil {
				t.Errorf("parse %s: %v", path, perr)
				return nil
			}
			for _, imp := range f.Imports {
				p := strings.Trim(imp.Path.Value, `"`)
				if p == forbiddenPkg || strings.HasPrefix(p, forbiddenPkg+"/") {
					rel, _ := filepath.Rel(root, path)
					t.Errorf("%s imports the decommissioned package %q — use internal/codeintel (Phase 4)", filepath.ToSlash(rel), p)
				}
			}
			return nil
		})
		if walkErr != nil {
			t.Fatalf("walk %s: %v", base, walkErr)
		}
	}
}

// TestNoCodebaseMemoryMCPReference walks every .go file under the scan
// roots and fails if the external binary name reappears (the GitHub
// release download path that Phase 4 removed). This guard's own needle is
// assembled from fragments, so it does not match this file.
func TestNoCodebaseMemoryMCPReference(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	self := "codegraph_decommission_test.go"
	for _, sub := range scanRoots {
		base := filepath.Join(root, sub)
		walkErr := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || filepath.Base(path) == self {
				return nil
			}
			body, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			if strings.Contains(string(body), forbiddenBinary) {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s references %q — Observer no longer downloads a third-party code-graph binary (Phase 4)", filepath.ToSlash(rel), forbiddenBinary)
			}
			return nil
		})
		if walkErr != nil {
			t.Fatalf("walk %s: %v", base, walkErr)
		}
	}
}
