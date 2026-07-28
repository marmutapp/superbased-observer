package etw

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUntaggedFilesStayPure pins the module-boundary discipline (CLAUDE.md §1)
// for the portable half of this package, the same way internal/cachewarm,
// internal/predict and internal/routing pin theirs.
//
// Until now the split was enforced only by build tags, which is a different
// property: a build tag says "this does not compile off Windows", not "this is
// pure decode logic". parse.go, errors.go, session.go and handles.go are the
// package's testable surface — CI has no Windows runner, so anything that
// migrates out of them stops being covered at all. They must therefore never
// grow a database, an HTTP client, a file watcher, or a syscall/unsafe
// dependency that would drag them back under a build tag.
//
// syscall is the one deliberate exception, allowed in errors.go only:
// errnoFromCall normalises (*windows.LazyProc).Call's boxed syscall.Errno, and
// syscall.Errno is a plain integer type available on every platform. That is
// what lets the Call idiom be unit-tested on Linux. It is NOT a licence to make
// a syscall.
func TestUntaggedFilesStayPure(t *testing.T) {
	t.Parallel()

	forbidden := []string{
		"database/sql",
		"net/http",
		"unsafe",
		"golang.org/x/sys/windows",
		"github.com/fsnotify/fsnotify",
	}
	// path -> the only file allowed to import it.
	exceptions := map[string]string{
		"syscall": "errors.go",
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	var checked int
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		tagged, err := hasBuildConstraint(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if tagged {
			continue
		}
		checked++

		af, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for _, imp := range af.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if only, ok := exceptions[path]; ok {
				if f != only {
					t.Errorf("%s imports %q, which is allowed only in %s", f, path, only)
				}
				continue
			}
			for _, bad := range forbidden {
				if path == bad || strings.HasPrefix(path, bad+"/") {
					t.Errorf("%s imports forbidden %q — the untagged half of internal/processobs/etw must stay portable and testable off Windows", f, bad)
				}
			}
		}
	}

	// A guard that silently checked nothing would be worse than none.
	if checked < 4 {
		t.Fatalf("only %d untagged files found; expected at least parse.go, errors.go, session.go and handles.go", checked)
	}
}

// hasBuildConstraint reports whether a file carries a //go:build line ahead of
// its package clause, i.e. whether it is platform-tagged.
func hasBuildConstraint(path string) (bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "package ") {
			return false, nil
		}
		if strings.HasPrefix(line, "//go:build") {
			return true, nil
		}
	}
	return false, nil
}
