package policy

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestPackageImports_Bounded enforces the module-boundary discipline
// (CLAUDE.md "Module Boundaries & Anti-Spaghetti Discipline" + guard
// spec §17.1, the cachetrack precedent). internal/policy is PURE:
// non-test source files must not import I/O infrastructure
// (database/sql, net/http, os, os/exec, fsnotify), any other observer
// subsystem (the package imports ZERO observer packages — the
// strongest form of the invariant), or path/filepath (whose semantics
// depend on the daemon's runtime GOOS; this package's path handling
// is deliberately OS-agnostic so the same Event yields the same
// Verdict on every daemon OS).
//
// Tests may import broader stdlib infra (go/parser, os, etc.) without
// violating the boundary; only non-test files are scanned.
func TestPackageImports_Bounded(t *testing.T) {
	t.Parallel()

	forbiddenExact := []string{
		"os",
		"os/exec",
		"io",
		"io/ioutil",
		"path/filepath",
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
				t.Errorf("%s: forbidden observer import %q (policy imports ZERO observer packages, §17.1)", filepath.Base(path), p)
			}
			for _, bad := range forbiddenExact {
				if p == bad {
					t.Errorf("%s: forbidden import %q (module boundary §17.1)", filepath.Base(path), p)
				}
			}
		}
	}
}

// TestNoToolIdentityBranching enforces guard spec §17.3: no logic in
// this package may branch on tool/client identity. Event.Tool exists
// for reporting only; client differences resolve into Capabilities
// and Dialect at the boundary. The scan rejects equality comparisons
// and switches on anything named *tool/*Tool in non-test source.
func TestNoToolIdentityBranching(t *testing.T) {
	t.Parallel()

	patterns := []*regexp.Regexp{
		regexp.MustCompile(`\b[Tt]ool\s*[!=]=`),
		regexp.MustCompile(`switch\s+[\w.]*\.?Tool\b`),
	}
	for _, path := range nonTestSourceFiles(t) {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, re := range patterns {
			if loc := re.Find(src); loc != nil {
				t.Errorf("%s: tool-identity branching %q (spec §17.3 — resolve differences into Capabilities at the boundary)",
					filepath.Base(path), string(loc))
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
