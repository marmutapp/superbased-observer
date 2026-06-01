package failure

import (
	"strings"
	"testing"
)

func TestCommandHashNormalization(t *testing.T) {
	t.Parallel()
	cases := []struct {
		a, b string
		same bool
	}{
		{"ls -la", "ls  -la", true},    // internal whitespace collapse
		{"  ls -la  ", "ls -la", true}, // trim
		{"ls -la", "ls -la\n", true},   // trailing newline
		{"go test ./...", "go test .", false},
	}
	for _, tc := range cases {
		if (CommandHash(tc.a) == CommandHash(tc.b)) != tc.same {
			t.Errorf("CommandHash(%q) vs (%q): expected same=%v", tc.a, tc.b, tc.same)
		}
	}
	if CommandHash("") != "" {
		t.Error("empty command should hash to empty")
	}
}

func TestCategorize(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
	}{
		{"", CategoryUnknown},
		{"context deadline exceeded", CategoryTimeout},
		{"Error: operation timed out after 30s", CategoryTimeout},
		{"permission denied", CategoryPermission},
		{"bind: permission denied", CategoryPermission},
		{"403 Forbidden", CategoryPermission},
		{"./foo.go:12:5: undefined: bar", CategoryCompilation},
		{"error[E0382]: borrow of moved value", CategoryCompilation},
		{"syntax error near unexpected token", CategoryCompilation},
		{"--- FAIL: TestFoo", CategoryTestFailure},
		{"1 tests failed", CategoryTestFailure},
		{"panic: runtime error: nil pointer dereference", CategoryRuntime},
		{"no such file or directory", CategoryRuntime},
		{"bash: foo: command not found", CategoryRuntime},
		{"something weird happened", CategoryUnknown},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got := Categorize(tc.in)
			if got != tc.want {
				t.Errorf("Categorize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestTruncateErrorMessage(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", MaxErrorMessageLen+100)
	if got := TruncateErrorMessage(long); len(got) != MaxErrorMessageLen {
		t.Errorf("truncate len: %d want %d", len(got), MaxErrorMessageLen)
	}
	short := "small"
	if got := TruncateErrorMessage(short); got != short {
		t.Error("short message was modified")
	}
}

func TestCommandSummary(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", MaxCommandSummaryLen+50)
	if got := CommandSummary(long); len(got) != MaxCommandSummaryLen {
		t.Errorf("summary len: %d want %d", len(got), MaxCommandSummaryLen)
	}
	if got := CommandSummary("  hi  "); got != "hi" {
		t.Errorf("trim: %q", got)
	}
}
