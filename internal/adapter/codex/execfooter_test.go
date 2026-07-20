package codex

import "testing"

// TestParseExecFooter pins extraction of the exec_command output footer that
// carries the command's true wall time + numeric exit code (the accurate
// source the action-derived process rows and the back-skew correlation use,
// in preference to the call->output gap estimate).
func TestParseExecFooter(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ms   int64
		code int
		ok   bool
	}{
		{"full footer", "Chunk ID: 230457\nWall time: 0.6788 seconds\nProcess exited with code 0\nOutput:\nx", 678, 0, true},
		{"nonzero exit", "Wall time: 2 seconds\nProcess exited with code 1", 2000, 1, true},
		{"trailing period", "Process exited with code 127.", 0, 127, true},
		{"no footer", "Total output lines: 220\n# Heading", 0, 0, false},
		{"empty", "", 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ms, code, ok := parseExecFooter(c.in)
			if ms != c.ms || code != c.code || ok != c.ok {
				t.Errorf("parseExecFooter() = (%d,%d,%v), want (%d,%d,%v)", ms, code, ok, c.ms, c.code, c.ok)
			}
		})
	}
}
