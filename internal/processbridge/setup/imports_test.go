package setup

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// pureFiles are the files that must carry NO I/O at all. plan.go holds the
// planner (PlanTask) and every helper it calls; the whole point of the E1
// refactor is that a Linux/WSL daemon — and, next, the dashboard — can plan a
// Windows command without touching a Windows host. The moment plan.go can exec
// or stat, the decision table stops being unit-testable without a real
// schtasks.exe and the seam has leaked.
var pureFiles = map[string]bool{"plan.go": true}

// pureFileAllowedImports is the CLOSED allow-list for those files. An addition
// here is a deliberate decision, not an accident: anything that can reach the
// filesystem, the process table or the network belongs behind the Env seam in
// env.go instead.
var pureFileAllowedImports = map[string]bool{
	"errors":  true,
	"fmt":     true,
	"net":     true, // net.SplitHostPort/JoinHostPort — string surgery, no dialling.
	"strings": true,
	"github.com/marmutapp/superbased-observer/internal/processobs/bridge": true, // DefaultListenAddr, a const.
}

// TestNoForbiddenImports pins the module-boundary discipline (CLAUDE.md §1):
// internal/processbridge/setup is a planner, not a subsystem. No file in it may
// import database/sql, net/http or fsnotify — the only I/O the package is
// allowed is the read-only `schtasks /Query` probe and the PATH/env lookups
// behind the injectable Env seam in env.go.
func TestNoForbiddenImports(t *testing.T) {
	forbidden := []string{
		"database/sql",
		"net/http",
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
					t.Errorf("%s imports forbidden %q — internal/processbridge/setup must stay a planner", f, bad)
				}
			}
		}
	}
}

// TestPlannerIsPure is the stronger gate: the planner file itself may import
// only the closed allow-list above. In particular NOT os, os/exec, os/user,
// path/filepath, net (dialling), runtime or context — every one of those is a
// way for a resolved input to be re-resolved inside the decision, which is
// exactly what the Inputs struct exists to prevent.
func TestPlannerIsPure(t *testing.T) {
	fset := token.NewFileSet()
	for f := range pureFiles {
		af, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for _, imp := range af.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if !pureFileAllowedImports[path] {
				t.Errorf("%s imports %q, which is not on the pure-planner allow-list — "+
					"I/O belongs behind the Env seam in env.go, never in the planner", f, path)
			}
		}
	}
}

// TestNoBuildTags pins the build-tag-free property, which is load-bearing
// rather than incidental: the WSL daemon that PLANS the elevated Windows
// command does not run on Windows. A `//go:build windows` anywhere in this
// package would compile the planner out of the exact binary that needs it.
func TestNoBuildTags(t *testing.T) {
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	for _, f := range matches {
		af, err := parser.ParseFile(fset, f, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for _, group := range af.Comments {
			for _, c := range group.List {
				if strings.HasPrefix(c.Text, "//go:build") || strings.HasPrefix(c.Text, "// +build") {
					t.Errorf("%s carries a build constraint (%s) — this package must build "+
						"everywhere, because the host that plans the Windows command is not Windows", f, c.Text)
				}
			}
		}
		// A GOOS/GOARCH filename suffix is the other way to constrain a file.
		base := strings.TrimSuffix(f, ".go")
		for _, suffix := range []string{"_windows", "_linux", "_darwin", "_js", "_wasm"} {
			if strings.HasSuffix(base, suffix) {
				t.Errorf("%s uses a GOOS filename constraint — this package must build everywhere", f)
			}
		}
	}
}
