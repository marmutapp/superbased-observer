package brackets

import "testing"

// This test is CORRECT and complete. The task is to implement IsBalanced
// so every case passes. Do not weaken, delete, or edit this file.
func TestIsBalanced(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", true},
		{"()", true},
		{"()[]{}", true},
		{"([{}])", true},
		{"{[()]}", true},
		{"(]", false},
		{"([)]", false},
		{"(", false},
		{"}", false},
		{"(()", false},
	}
	for _, c := range cases {
		if got := IsBalanced(c.in); got != c.want {
			t.Errorf("IsBalanced(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
