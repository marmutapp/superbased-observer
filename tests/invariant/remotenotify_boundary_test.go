package invariant

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

const remotenotifyImportPrefix = "github.com/marmutapp/superbased-observer/internal/remotenotify"

// forbiddenRemotenotifyImporters are the capture-path packages that must NEVER
// reach the outbound notification rail — the "no network calls in the
// observer/watcher" invariant (CLAUDE.md; remote-dashboard-access plan §2).
// remotenotify is invoked ONLY from the dashboard/lifecycle layer (the
// termsession session-exit seam wired in cmd); a leaked import here would let
// the watcher or proxy phone out, which this test refuses at build time.
var forbiddenRemotenotifyImporters = []string{
	filepath.Join("..", "..", "internal", "watcher"),
	filepath.Join("..", "..", "internal", "proxy"),
}

// TestRemotenotifyUnreachableFromCapturePaths enforces the boundary: no file
// under internal/watcher or internal/proxy may import internal/remotenotify.
// Textual import scan (build-tag agnostic), mirroring
// TestObsReverseImportBoundary.
func TestRemotenotifyUnreachableFromCapturePaths(t *testing.T) {
	fset := token.NewFileSet()
	for _, root := range forbiddenRemotenotifyImporters {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			af, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if perr != nil {
				t.Fatalf("parse %s: %v", path, perr)
			}
			for _, imp := range af.Imports {
				p := strings.Trim(imp.Path.Value, `"`)
				if p == remotenotifyImportPrefix || strings.HasPrefix(p, remotenotifyImportPrefix+"/") {
					t.Errorf("%s imports %q — the outbound notification rail must never be reachable from the capture path (watcher/proxy); invoke it from the dashboard/lifecycle layer only", filepath.ToSlash(path), p)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
}
