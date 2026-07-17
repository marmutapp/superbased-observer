package remotecfg

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestImportsRestrictedToConfigAndRemoteauth pins the module boundary (plan
// §B): internal/remotecfg is the lower-level arm/disarm/rotate transaction
// owner. It may import ONLY internal/config + internal/remoteauth from the
// project (plus stdlib), and NEVER net/http, the dashboard, or cmd — moving
// BuildController here would cycle. A drift is build-breaking.
func TestImportsRestrictedToConfigAndRemoteauth(t *testing.T) {
	const prefix = "github.com/marmutapp/superbased-observer/"
	allowedInternal := map[string]struct{}{
		prefix + "internal/config":     {},
		prefix + "internal/remoteauth": {},
	}
	forbidden := []string{
		"net/http",
		"github.com/fsnotify/fsnotify",
		prefix + "internal/intelligence/dashboard",
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
					t.Errorf("%s imports forbidden %q — remotecfg must stay config+remoteauth-only", f, bad)
				}
			}
			if strings.HasPrefix(path, prefix) {
				if _, ok := allowedInternal[path]; !ok {
					t.Errorf("%s imports project package %q — remotecfg may import ONLY internal/config + internal/remoteauth", f, path)
				}
			}
		}
	}
}
