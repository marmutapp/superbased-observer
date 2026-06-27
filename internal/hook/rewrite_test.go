package hook

import "testing"

func TestRewriteBash(t *testing.T) {
	bin := "/usr/local/bin/observer"
	excl := []string{"curl", "playwright"}

	cases := []struct {
		name    string
		in      string
		want    string
		changed bool
	}{
		{
			name:    "simple command rewrites",
			in:      "git status",
			want:    bin + " run -- git status",
			changed: true,
		},
		{
			name:    "path-prefixed command rewrites",
			in:      "/usr/bin/git log -n5",
			want:    bin + " run -- /usr/bin/git log -n5",
			changed: true,
		},
		{
			name:    "pipe disqualifies",
			in:      "git status | head -5",
			changed: false,
		},
		{
			name:    "redirect disqualifies",
			in:      "ls > out.txt",
			changed: false,
		},
		{
			name:    "subshell disqualifies",
			in:      "echo $(date)",
			changed: false,
		},
		{
			name:    "backtick disqualifies",
			in:      "echo `date`",
			changed: false,
		},
		{
			name:    "env-var interpolation disqualifies",
			in:      "echo $HOME",
			changed: false,
		},
		{
			name:    "quoted metachar allowed",
			in:      "grep '$foo' file",
			want:    bin + " run -- grep '$foo' file",
			changed: true,
		},
		{
			name:    "double-quoted metachar still unsafe",
			in:      `echo "$HOME"`,
			changed: false, // double quotes don't suppress $
		},
		{
			name:    "excluded command skipped",
			in:      "curl example.com",
			changed: false,
		},
		{
			name:    "excluded command with leading path skipped",
			in:      "/usr/bin/curl example.com",
			changed: false,
		},
		{
			name:    "double-wrap guard",
			in:      "observer run -- git status",
			changed: false,
		},
		{
			name:    "empty string",
			in:      "",
			changed: false,
		},
		{
			name:    "whitespace-only",
			in:      "   \t  ",
			changed: false,
		},
		{
			name:    "trailing newline ok",
			in:      "git log\n",
			want:    bin + " run -- git log",
			changed: true,
		},
		{
			name:    "unbalanced quote disqualifies",
			in:      "echo 'unterminated",
			changed: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := RewriteBash(bin, tc.in, excl)
			if changed != tc.changed {
				t.Fatalf("changed: got %v want %v (got=%q)", changed, tc.changed, got)
			}
			if changed && got != tc.want {
				t.Fatalf("rewritten: got %q want %q", got, tc.want)
			}
			if !changed && got != tc.in {
				t.Fatalf("unchanged should return original, got %q want %q", got, tc.in)
			}
		})
	}
}

func TestRewriteBashEmptyBinary(t *testing.T) {
	got, changed := RewriteBash("", "git status", nil)
	if changed {
		t.Fatalf("want no rewrite with empty binary, got %q", got)
	}
}
