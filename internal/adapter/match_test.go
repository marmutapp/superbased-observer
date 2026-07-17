package adapter

import (
	"path/filepath"
	"testing"
)

func TestHasPathPrefix(t *testing.T) {
	t.Parallel()
	// Use absolute paths to keep the test independent of CWD.
	root := filepath.Join("/", "tmp", "observer-test", "root")
	cases := []struct {
		name   string
		path   string
		prefix string
		want   bool
	}{
		{"exact match", root, root, true},
		{"strict ancestor", filepath.Join(root, "a", "b.txt"), root, true},
		{"prefix string but no separator boundary", root + "extra", root, false},
		{"different roots", filepath.Join("/", "etc"), root, false},
		{"empty path", "", root, false},
		{"empty prefix", root, "", false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := HasPathPrefix(c.path, c.prefix)
			if got != c.want {
				t.Fatalf("HasPathPrefix(%q, %q) = %v, want %v", c.path, c.prefix, got, c.want)
			}
		})
	}
}

func TestHasPathPrefix_CaseInsensitive(t *testing.T) {
	// Force case-insensitive comparison regardless of host OS so this
	// branch runs on Linux CI.
	orig := caseInsensitiveFS
	caseInsensitiveFS = true
	t.Cleanup(func() { caseInsensitiveFS = orig })

	root := filepath.Join("/", "tmp", "Mixed", "Case")
	probe := filepath.Join("/", "tmp", "mixed", "case", "file.jsonl")
	if !HasPathPrefix(probe, root) {
		t.Fatalf("HasPathPrefix case-fold mismatch: want true, root=%q probe=%q", root, probe)
	}
}

func TestUnderAnyWatchRoot(t *testing.T) {
	t.Parallel()
	rootA := filepath.Join("/", "tmp", "observer-test", "a")
	rootB := filepath.Join("/", "tmp", "observer-test", "b")

	cases := []struct {
		name  string
		path  string
		roots []string
		want  bool
	}{
		{"under first root", filepath.Join(rootA, "x.jsonl"), []string{rootA, rootB}, true},
		{"under second root", filepath.Join(rootB, "x.jsonl"), []string{rootA, rootB}, true},
		{"under no root", filepath.Join("/", "other", "x.jsonl"), []string{rootA, rootB}, false},
		{"empty roots", filepath.Join(rootA, "x.jsonl"), nil, false},
		{"nil-safe empty entry skipped", filepath.Join(rootA, "x.jsonl"), []string{"", rootA}, true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := UnderAnyWatchRoot(c.path, c.roots)
			if got != c.want {
				t.Fatalf("UnderAnyWatchRoot(%q, %v) = %v, want %v", c.path, c.roots, got, c.want)
			}
		})
	}
}
