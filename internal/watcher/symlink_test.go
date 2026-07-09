package watcher

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSymlinkLeafEscapes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privilege on Windows")
	}
	root := t.TempDir()
	outside := t.TempDir()

	// A regular file under the watch root — allowed.
	regular := filepath.Join(root, "session.jsonl")
	if err := os.WriteFile(regular, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A secret outside the watch root, and a symlink to it planted under root.
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("password"), 0o600); err != nil {
		t.Fatal(err)
	}
	escaping := filepath.Join(root, "escape.jsonl")
	if err := os.Symlink(secret, escaping); err != nil {
		t.Fatal(err)
	}

	// A symlink under root that points back inside root — allowed.
	inside := filepath.Join(root, "inside.jsonl")
	if err := os.Symlink(regular, inside); err != nil {
		t.Fatal(err)
	}

	// A dangling symlink — refused.
	dangling := filepath.Join(root, "dangling.jsonl")
	if err := os.Symlink(filepath.Join(outside, "gone"), dangling); err != nil {
		t.Fatal(err)
	}

	roots := []string{root}
	cases := []struct {
		name string
		path string
		want bool
	}{
		{"regular file", regular, false},
		{"symlink escaping root", escaping, true},
		{"symlink inside root", inside, false},
		{"dangling symlink", dangling, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := symlinkLeafEscapes(tc.path, roots)
			if got != tc.want {
				t.Errorf("symlinkLeafEscapes(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}
