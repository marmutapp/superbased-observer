package mcp

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestSafeResolvePath_ContainmentCases covers the V7-13 Gap 4
// project-root containment defense including symlink-escape attempts.
func TestSafeResolvePath_ContainmentCases(t *testing.T) {
	root := t.TempDir()
	// File inside the project root.
	inside := filepath.Join(root, "src", "foo.ts")
	if err := os.MkdirAll(filepath.Dir(inside), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inside, []byte("contents"), 0o600); err != nil {
		t.Fatal(err)
	}

	// File OUTSIDE the project root, used as a symlink escape target.
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outside, []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Symlink under root → outside (escape attempt).
	escapeLink := filepath.Join(root, "escape.txt")
	if err := os.Symlink(outside, escapeLink); err != nil {
		// Some filesystems / CI environments forbid symlinks; skip.
		t.Skipf("symlink not supported here: %v", err)
	}

	// Symlink under root → inside root (legit case).
	innerLink := filepath.Join(root, "legit.ts")
	if err := os.Symlink(inside, innerLink); err != nil {
		t.Skipf("symlink not supported here: %v", err)
	}

	tests := []struct {
		name      string
		path      string
		wantErr   error
		wantAbs   bool // when true assert result is inside root
		errSubstr string
	}{
		{"relative-inside-root", "src/foo.ts", nil, true, ""},
		{"absolute-inside-root", inside, nil, true, ""},
		{"absolute-outside-root", outside, errPathOutsideRoot, false, ""},
		{"missing-file", "no/such/file.ts", errFileNotFound, false, ""},
		{"symlink-escape-denied", "escape.txt", errPathOutsideRoot, false, ""},
		{"symlink-inside-allowed", "legit.ts", nil, true, ""},
		// Note: dot-dot escape (e.g. "../../etc/hosts") cleans via
		// filepath.Abs to an absolute outside-root path before
		// EvalSymlinks. Behavior is functionally identical to the
		// absolute-outside-root case above; the deny reason depends on
		// whether the resolved target happens to exist on the host
		// (errFileNotFound vs errPathOutsideRoot). Either is a denial
		// — we don't pin which since the leak is benign.
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := safeResolvePath(root, tc.path)
			if tc.wantErr != nil {
				if err == nil {
					t.Fatalf("want error %v, got nil (resolved %q)", tc.wantErr, got)
				}
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("want errors.Is(err, %v), got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			rootResolved, _ := filepath.EvalSymlinks(root)
			if !isUnder(got, rootResolved) {
				t.Errorf("resolved %q should be under %q", got, rootResolved)
			}
		})
	}
}

func TestSafeResolvePath_EmptyArgs(t *testing.T) {
	if _, err := safeResolvePath("", "x"); err == nil {
		t.Error("empty root should error")
	}
	if _, err := safeResolvePath(t.TempDir(), ""); err == nil {
		t.Error("empty path should error")
	}
}

func TestSafeResolvePath_MissingRoot(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	_, err := safeResolvePath(missing, "x")
	if err == nil {
		t.Error("missing root should error")
	}
}

func TestExtensionAllowed(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		allowed []string
		want    bool
	}{
		{"empty-list-allows-everything", "foo.weird", nil, true},
		{"empty-list-allows-no-ext", "Makefile", []string{}, true},
		{"ts-in-allowed", "src/foo.ts", []string{"ts", "tsx"}, true},
		{"tsx-in-allowed", "src/Foo.tsx", []string{"ts", "tsx"}, true},
		{"case-insensitive", "FOO.TS", []string{"ts"}, true},
		{"dotted-allowed-entry", "x.go", []string{".go"}, true},
		{"py-not-in-allowed", "foo.py", []string{"ts", "tsx"}, false},
		{"no-ext-denied-when-list-set", "Makefile", []string{"ts"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := extensionAllowed(tc.path, tc.allowed); got != tc.want {
				t.Errorf("extensionAllowed(%q, %v) = %v, want %v",
					tc.path, tc.allowed, got, tc.want)
			}
		})
	}
}

func TestMatchesAny(t *testing.T) {
	patterns := []string{
		".env*", "*.key", "*.pem",
		".git/**", "node_modules/**",
		".ssh/**", ".aws/**",
		".npmrc",
	}
	tests := []struct {
		name string
		path string
		want bool
	}{
		// basename matches
		{"env-dotfile", ".env", true},
		{"env-with-suffix", ".env.local", true},
		{"env-in-subdir", "src/.env", true},
		{"key-extension", "secrets/foo.key", true},
		{"pem-extension", "certs/server.pem", true},
		{"exact-basename", ".npmrc", true},
		// directory prefix matches
		{"git-head", ".git/HEAD", true},
		{"git-nested", ".git/objects/00/abc", true},
		{"node-modules-shallow", "node_modules/pkg/index.js", true},
		{"node-modules-deep", "node_modules/a/b/c.js", true},
		{"ssh-config", ".ssh/config", true},
		// non-matches
		{"src-file", "src/foo.ts", false},
		{"git-substring", "mygit/foo", false},
		{"node-modules-substring", "snode_modules/x", false},
		{"missing-extension", "secrets/somefile", false},
		{"env-as-dir-allowed-by-our-rules", "env/foo.ts", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchesAny(tc.path, patterns); got != tc.want {
				t.Errorf("matchesAny(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

func TestMatchesAny_EmptyPatterns(t *testing.T) {
	if matchesAny("anything", nil) {
		t.Error("empty patterns should never match")
	}
	if matchesAny("anything", []string{""}) {
		t.Error("empty pattern entries should be skipped")
	}
}

func TestMatchesAny_WindowsSeparator(t *testing.T) {
	// On Windows the agent may pass backslash paths; matchesAny
	// normalises to forward slashes via filepath.ToSlash.
	if runtime.GOOS == "windows" {
		if !matchesAny(`.git\HEAD`, []string{".git/**"}) {
			t.Error("backslash path should match forward-slash pattern after normalise")
		}
	}
}

func TestHasUnsupportedGlobSyntax(t *testing.T) {
	tests := map[string]bool{
		"":            false,
		"*.ts":        false,
		".env*":       false,
		".git/**":     false,
		"src/?oo.ts":  false,
		"foo[abc].ts": true, // character class — unsupported
		"foo{a,b}.ts": true, // brace alternation — unsupported
		`foo\*bar.ts`: true, // escaped — unsupported
	}
	for pat, want := range tests {
		if got := hasUnsupportedGlobSyntax(pat); got != want {
			t.Errorf("hasUnsupportedGlobSyntax(%q) = %v, want %v", pat, got, want)
		}
	}
}
