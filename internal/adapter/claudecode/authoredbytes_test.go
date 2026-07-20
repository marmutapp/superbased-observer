package claudecode

import (
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// TestAuthoredBytes pins the Verbosity feature's per-action authored-code
// byte capture: it dispatches on the NORMALIZED action_type over the
// Anthropic-shaped tool input and counts only the bytes the model wrote.
func TestAuthoredBytes(t *testing.T) {
	cases := []struct {
		name       string
		actionType string
		input      string
		want       int64
	}{
		{
			name:       "write counts content only",
			actionType: models.ActionWriteFile,
			input:      `{"file_path":"a.go","content":"package main"}`,
			want:       int64(len("package main")),
		},
		{
			name:       "edit counts new_string only",
			actionType: models.ActionEditFile,
			input:      `{"file_path":"a.go","old_string":"x := 1 // long old","new_string":"y"}`,
			want:       1,
		},
		{
			name:       "multiedit sums new_string across edits",
			actionType: models.ActionEditFile,
			input:      `{"file_path":"a.go","edits":[{"old_string":"a","new_string":"AA"},{"old_string":"b","new_string":"BBB"}]}`,
			want:       5, // "AA" + "BBB"
		},
		{
			name:       "notebookedit counts new_source",
			actionType: models.ActionEditFile,
			input:      `{"notebook_path":"n.ipynb","new_source":"print(1)"}`,
			want:       int64(len("print(1)")),
		},
		{
			name:       "run_command counts command",
			actionType: models.ActionRunCommand,
			input:      `{"command":"go test ./..."}`,
			want:       int64(len("go test ./...")),
		},
		{
			name:       "read authors nothing",
			actionType: models.ActionReadFile,
			input:      `{"file_path":"a.go"}`,
			want:       0,
		},
		{
			name:       "search authors nothing",
			actionType: models.ActionSearchText,
			input:      `{"pattern":"TODO"}`,
			want:       0,
		},
		{
			name:       "malformed input is zero, not a panic",
			actionType: models.ActionWriteFile,
			input:      `{not json`,
			want:       0,
		},
		{
			name:       "empty input is zero",
			actionType: models.ActionWriteFile,
			input:      ``,
			want:       0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := authoredBytes(c.actionType, []byte(c.input)); got != c.want {
				t.Errorf("authoredBytes(%s) = %d, want %d", c.actionType, got, c.want)
			}
		})
	}
}
