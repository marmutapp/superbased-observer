package routing

import "testing"

// TestResolveCommandClass exercises the keyword table — one case per
// rule row plus the precedence pins and the none/other floors.
func TestResolveCommandClass(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		command string
		want    CommandClass
	}{
		{"test_go", "go test -race ./...", CommandTest},
		{"test_beats_build_make", "make test", CommandTest},
		{"test_beats_vcs_substring", "npm test && git status", CommandTest},
		{"lint_go_vet", "go vet ./...", CommandLint},
		{"lint_beats_build_tsc", "tsc --noEmit", CommandLint},
		{"build_go", "go build -o bin/observer ./cmd/observer", CommandBuild},
		{"build_bare_make", "make", CommandBuild},
		{"vcs_git_commit", "git commit -m 'feat: thing'", CommandVCS},
		{"vcs_gh_pr", "gh pr view 42", CommandVCS},
		{"other_unknown", "curl -s https://example.com", CommandOther},
		{"none_empty", "", CommandNone},
		{"none_whitespace", "   ", CommandNone},
		{"case_insensitive", "GO TEST ./...", CommandTest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ResolveCommandClass(tc.command); got != tc.want {
				t.Errorf("ResolveCommandClass(%q) = %q, want %q", tc.command, got, tc.want)
			}
		})
	}
}
