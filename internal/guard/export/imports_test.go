package export

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPackageImports_Bounded enforces the module-boundary discipline
// (CLAUDE.md module rule 1 / guard spec §17.1, the internal/policy
// precedent): this package is PURE — non-test source files import no
// I/O infrastructure and ZERO observer packages. The §11.4
// no-network invariant for guard export is structural because of
// this: a formatter that cannot open a socket cannot exfiltrate.
func TestPackageImports_Bounded(t *testing.T) {
	t.Parallel()

	forbiddenExact := []string{
		"os",
		"os/exec",
		"io",
		"io/ioutil",
		"path/filepath",
		"net",
		"net/http",
		"database/sql",
		"github.com/fsnotify/fsnotify",
	}
	const forbiddenPrefix = "github.com/marmutapp/superbased-observer/"

	fset := token.NewFileSet()
	for _, path := range nonTestSourceFiles(t) {
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, imp := range file.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if strings.HasPrefix(p, forbiddenPrefix) {
				t.Errorf("%s: forbidden observer import %q (export imports ZERO observer packages, §17.1)", filepath.Base(path), p)
			}
			for _, bad := range forbiddenExact {
				if p == bad {
					t.Errorf("%s: forbidden import %q (module boundary §17.1)", filepath.Base(path), p)
				}
			}
		}
	}
}

// nonTestSourceFiles lists the package's non-test .go files.
func nonTestSourceFiles(t *testing.T) []string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(cwd, "*.go"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	var out []string
	for _, m := range matches {
		if !strings.HasSuffix(m, "_test.go") {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		t.Fatal("no non-test source files found")
	}
	return out
}
