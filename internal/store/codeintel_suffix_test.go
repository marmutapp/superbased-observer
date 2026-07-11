package store

import "testing"

// TestUniqueLongestSuffixMatch covers the tolerant path reconciliation
// that lets codex shell-read paths (relative or Windows-form) resolve to
// the absolute, OS-native paths the codeintel index keys on — while
// refusing to guess when two files could match.
func TestUniqueLongestSuffixMatch(t *testing.T) {
	idx := []string{
		"/mnt/d/programsx/superbased-observer/internal/mcp/tools_get_relations.go",
		"/mnt/d/programsx/superbased-observer/internal/store/codeintel.go",
		"/mnt/d/programsx/other-proj/internal/store/codeintel.go",
	}
	cases := []struct {
		name   string
		target string
		cands  []string
		want   string
	}{
		{
			name:   "relative path resolves to the unique indexed file",
			target: "internal/mcp/tools_get_relations.go",
			cands:  []string{idx[0]},
			want:   idx[0],
		},
		{
			name:   "windows absolute resolves across the OS path-form gap",
			target: `D:\programsx\superbased-observer\internal\mcp\tools_get_relations.go`,
			cands:  []string{idx[0]},
			want:   idx[0],
		},
		{
			name:   "longer shared suffix wins over a same-basename file in another project",
			target: "superbased-observer/internal/store/codeintel.go",
			cands:  []string{idx[1], idx[2]},
			want:   idx[1],
		},
		{
			name:   "ambiguous basename-only target with equal suffixes is skipped",
			target: "codeintel.go",
			cands:  []string{idx[1], idx[2]},
			want:   "",
		},
		{
			name:   "no candidates yields no match",
			target: "internal/mcp/tools_get_relations.go",
			cands:  nil,
			want:   "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := uniqueLongestSuffixMatch(tc.target, tc.cands); got != tc.want {
				t.Errorf("uniqueLongestSuffixMatch(%q) = %q, want %q", tc.target, got, tc.want)
			}
		})
	}
}

func TestCommonSuffixComponents(t *testing.T) {
	a := splitPathComponents("/mnt/d/proj/internal/store/codeintel.go")
	b := splitPathComponents(`D:\other\internal\store\codeintel.go`) //nolint:misspell // "other" is a path component, not a typo
	if n := commonSuffixComponents(a, b); n != 3 {
		t.Errorf("commonSuffixComponents = %d, want 3 (internal/store/codeintel.go; proj!=other)", n)
	}
	if n := commonSuffixComponents(a, nil); n != 0 {
		t.Errorf("commonSuffixComponents with empty = %d, want 0", n)
	}
}
