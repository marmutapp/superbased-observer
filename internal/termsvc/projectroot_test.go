package termsvc

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestValidateProjectRoot(t *testing.T) {
	// A real allowed root + a descendant, plus an outside dir and a symlink
	// that escapes the allowed root.
	base := t.TempDir()
	allowed := filepath.Join(base, "allowed")
	child := filepath.Join(allowed, "sub", "deep")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{allowed, child, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	// Canonicalize the allow-list entry the way the operator would configure it
	// (EvalSymlinks handles macOS /var → /private/var etc.).
	allowedCanon, err := filepath.EvalSymlinks(allowed)
	if err != nil {
		t.Fatalf("evalsymlinks allowed: %v", err)
	}

	// A symlink inside the allowed tree pointing OUTSIDE must be rejected.
	escape := filepath.Join(allowed, "escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Skipf("symlink unsupported: %v", err) // Windows without privilege
	}

	fileInAllowed := filepath.Join(allowed, "afile")
	if err := os.WriteFile(fileInAllowed, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	tests := []struct {
		name      string
		requested string
		roots     []string
		wantErr   bool
		wantCanon string
	}{
		{"empty is allowed (default cwd)", "", []string{allowed}, false, ""},
		{"exact allowed root", allowed, []string{allowed}, false, allowedCanon},
		{"descendant of allowed root", child, []string{allowed}, false, ""},
		{"outside allowed root", outside, []string{allowed}, true, ""},
		{"empty allow-list denies", allowed, nil, true, ""},
		{"nonexistent dir", filepath.Join(base, "nope"), []string{allowed}, true, ""},
		{"file not dir", fileInAllowed, []string{allowed}, true, ""},
		{"symlink escaping allowed root", escape, []string{allowed}, true, ""},
		{"relative path", "relative/path", []string{allowed}, true, ""},
		{"UNC path", `\\server\share`, []string{allowed}, true, ""},
		{"forward-slash UNC", "//server/share", []string{allowed}, true, ""},
		{"NUL byte", allowed + "\x00evil", []string{allowed}, true, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateProjectRoot(tc.requested, tc.roots)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got canonical %q", got)
				}
				if !errors.Is(err, ErrProjectRootDenied) {
					t.Fatalf("error %v is not ErrProjectRootDenied", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantCanon != "" && got != tc.wantCanon {
				t.Fatalf("canonical = %q, want %q", got, tc.wantCanon)
			}
		})
	}
}

func TestIsUnderOrEqual(t *testing.T) {
	sep := string(filepath.Separator)
	root := sep + "a" + sep + "b"
	tests := []struct {
		child, parent string
		want          bool
	}{
		{root, root, true},
		{root + sep + "c", root, true},
		{sep + "a", root, false},
		{sep + "a" + sep + "bb", root, false}, // sibling prefix, not a child
	}
	for _, tc := range tests {
		if got := isUnderOrEqual(tc.child, tc.parent); got != tc.want {
			t.Errorf("isUnderOrEqual(%q,%q) = %v, want %v", tc.child, tc.parent, got, tc.want)
		}
	}
	_ = runtime.GOOS
}
