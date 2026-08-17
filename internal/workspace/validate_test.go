package workspace

import "testing"

func TestValidateManagedWorkspace(t *testing.T) {
	cases := []struct {
		name        string
		path        string
		managedRoot string
		wantErr     bool
	}{
		{"under-root", "/home/u/.observer/workspaces/abc/repo", "/home/u/.observer/workspaces", false},
		{"under-root-deeper", "/home/u/.observer/workspaces/abc/repo/sub", "/home/u/.observer/workspaces", false},
		{"equals-root-rejected", "/home/u/.observer/workspaces", "/home/u/.observer/workspaces", true},
		{"dotdot-segment-rejected", "/home/u/.observer/workspaces/../secrets", "/home/u/.observer/workspaces", true},
		{"sibling-dir-escapes", "/home/u/.observer/other/abc", "/home/u/.observer/workspaces", true},
		{"prefix-collision-escapes", "/home/u/.observer/workspaces-evil/abc", "/home/u/.observer/workspaces", true},
		{"relative-path-rejected", "relative/abc", "/home/u/.observer/workspaces", true},
		{"relative-root-rejected", "/home/u/.observer/workspaces/abc", "relative", true},
		{"empty-path-rejected", "", "/home/u/.observer/workspaces", true},
		{"empty-root-rejected", "/home/u/.observer/workspaces/abc", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateManagedWorkspace(tc.path, tc.managedRoot)
			if tc.wantErr && err == nil {
				t.Fatalf("ValidateManagedWorkspace(%q, %q): want error, got nil", tc.path, tc.managedRoot)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ValidateManagedWorkspace(%q, %q): unexpected error: %v", tc.path, tc.managedRoot, err)
			}
		})
	}
}
