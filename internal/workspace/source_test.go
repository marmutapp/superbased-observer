package workspace

import "testing"

func TestParseSource(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    Source
		wantErr bool
	}{
		{"live", "live", SourceLive, false},
		{"clone-local", "clone-local", SourceCloneLocal, false},
		{"clone-remote", "clone-remote", SourceCloneRemote, false},
		{"worktree", "worktree", SourceWorktree, false},
		{"empty-rejected", "", "", true},
		{"unknown-rejected", "bogus", "", true},
		{"case-sensitive-rejected", "Live", "", true},
		{"trailing-space-rejected", "live ", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseSource(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseSource(%q): want error, got nil", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSource(%q): unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ParseSource(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
