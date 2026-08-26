package git

import "testing"

// TestNormalizeRemote is the table-driven suite covering every rule in the
// Team Project Identity Mapping plan's (2026-08-21, §1 L1) normalization
// table, plus the empty-input and unparseable-fail-open cases.
func TestNormalizeRemote(t *testing.T) {
	// Credential-bearing test inputs are built via string concatenation,
	// never as a single literal with a password-shaped word glued to a
	// colon — the session's write filter can corrupt credential-shaped
	// literals on disk (see the task's write-filter hazard note).
	credentialedHTTPS := "https://alice:" + "hunter" + "2@github.com/org/repo.git"

	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "scp_style_ssh",
			raw:  "git@github.com:org/repo.git",
			want: "github.com/org/repo",
		},
		{
			name: "ssh_url",
			raw:  "ssh://git@github.com/org/repo.git",
			want: "github.com/org/repo",
		},
		{
			name: "https_url",
			raw:  "https://github.com/org/repo.git",
			want: "github.com/org/repo",
		},
		{
			name: "http_url",
			raw:  "http://github.com/org/repo.git",
			want: "github.com/org/repo",
		},
		{
			name: "credentials_stripped",
			raw:  credentialedHTTPS,
			want: "github.com/org/repo",
		},
		{
			name: "scp_userinfo_stripped",
			raw:  "git@github.com:org/repo.git",
			want: "github.com/org/repo",
		},
		{
			name: "host_case_lowered",
			raw:  "https://GitHub.com/org/repo.git",
			want: "github.com/org/repo",
		},
		{
			name: "path_case_preserved",
			raw:  "https://github.com/Org/Repo.git",
			want: "github.com/Org/Repo",
		},
		{
			name: "scp_path_case_preserved",
			raw:  "git@github.com:Org/Repo.git",
			want: "github.com/Org/Repo",
		},
		{
			name: "default_ssh_port_stripped",
			raw:  "ssh://git@github.com:22/org/repo",
			want: "github.com/org/repo",
		},
		{
			name: "default_https_port_stripped",
			raw:  "https://github.com:443/org/repo.git",
			want: "github.com/org/repo",
		},
		{
			name: "default_http_port_stripped",
			raw:  "http://github.com:80/org/repo.git",
			want: "github.com/org/repo",
		},
		{
			name: "non_default_ssh_port_preserved",
			raw:  "ssh://git@gitlab.example.com:2222/group/repo",
			want: "gitlab.example.com:2222/group/repo",
		},
		{
			name: "non_default_https_port_preserved",
			raw:  "https://gitlab.example.com:8443/group/repo.git",
			want: "gitlab.example.com:8443/group/repo",
		},
		{
			name: "trailing_git_stripped",
			raw:  "https://github.com/org/repo.git",
			want: "github.com/org/repo",
		},
		{
			name: "trailing_slash_stripped",
			raw:  "https://github.com/org/repo/",
			want: "github.com/org/repo",
		},
		{
			name: "scp_trailing_slash_stripped",
			raw:  "git@github.com:org/repo/",
			want: "github.com/org/repo",
		},
		{
			name: "unparseable_local_unix_path_unchanged",
			raw:  "/home/alice/bare-repo.git",
			want: "/home/alice/bare-repo.git",
		},
		{
			name: "unparseable_windows_drive_path_unchanged",
			raw:  `C:\Users\alice\repo.git`,
			want: `C:\Users\alice\repo.git`,
		},
		{
			name: "unparseable_garbage_unchanged",
			raw:  "not a valid remote $$$",
			want: "not a valid remote $$$",
		},
		{
			name: "whitespace_trimmed_on_unparseable",
			raw:  "  /home/alice/bare-repo.git  ",
			want: "/home/alice/bare-repo.git",
		},
		{
			name: "empty_input",
			raw:  "",
			want: "",
		},
		{
			name: "whitespace_only_input",
			raw:  "   ",
			want: "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := NormalizeRemote(c.raw)
			if got != c.want {
				t.Errorf("NormalizeRemote(%q) = %q, want %q", c.raw, got, c.want)
			}
		})
	}
}

// TestNormalizeRemote_CrossTransportEquivalence asserts the plan's hard
// requirement (§1 L1 RULING, former §5 OQ3): scp, ssh://, https://, and
// http:// forms of the identical repository must all normalize to the
// identical string. Transport is not identity.
func TestNormalizeRemote_CrossTransportEquivalence(t *testing.T) {
	forms := []string{
		"git@github.com:org/repo.git",
		"ssh://git@github.com/org/repo.git",
		"ssh://git@github.com/org/repo",
		"https://github.com/org/repo.git",
		"https://github.com/org/repo",
		"http://github.com/org/repo.git",
	}

	want := "github.com/org/repo"
	for _, raw := range forms {
		if got := NormalizeRemote(raw); got != want {
			t.Errorf("NormalizeRemote(%q) = %q, want %q (cross-transport equivalence)", raw, got, want)
		}
	}
}

// TestNormalizeRemote_Idempotent asserts NormalizeRemote(NormalizeRemote(x))
// == NormalizeRemote(x) for a representative set of inputs, including the
// non-default-port canonical shape that round-trips through the scp-style
// parser (the case the port/path heuristic in parseSCPLike exists for).
func TestNormalizeRemote_Idempotent(t *testing.T) {
	inputs := []string{
		"git@github.com:org/repo.git",
		"ssh://git@github.com/org/repo.git",
		"https://github.com/org/repo.git",
		"http://github.com/org/repo.git",
		"https://GitHub.com/Org/Repo.git",
		"ssh://git@gitlab.example.com:2222/group/repo",
		"https://gitlab.example.com:8443/group/repo.git",
		"https://github.com/org/repo/",
		"",
		"/home/alice/bare-repo.git",
		`C:\Users\alice\repo.git`,
	}

	for _, raw := range inputs {
		once := NormalizeRemote(raw)
		twice := NormalizeRemote(once)
		if once != twice {
			t.Errorf("NormalizeRemote not idempotent for %q: first=%q second=%q", raw, once, twice)
		}
	}
}
