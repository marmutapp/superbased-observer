package contentcap

import (
	"strings"
	"testing"
)

func TestCap(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		max      int
		wantBody string
		wantMark bool
	}{
		{
			name:     "empty stays empty",
			input:    "",
			max:      10,
			wantBody: "",
			wantMark: false,
		},
		{
			name:     "fits exactly",
			input:    "0123456789",
			max:      10,
			wantBody: "0123456789",
			wantMark: false,
		},
		{
			name:     "under cap",
			input:    "hello",
			max:      100,
			wantBody: "hello",
			wantMark: false,
		},
		{
			name:     "over cap appends marker",
			input:    strings.Repeat("x", 100),
			max:      10,
			wantBody: strings.Repeat("x", 10),
			wantMark: true,
		},
		{
			name:     "zero max uses default",
			input:    strings.Repeat("y", DefaultMaxBytes+5),
			max:      0,
			wantBody: strings.Repeat("y", DefaultMaxBytes),
			wantMark: true,
		},
		{
			name:     "negative max uses default",
			input:    strings.Repeat("z", DefaultMaxBytes+1),
			max:      -1,
			wantBody: strings.Repeat("z", DefaultMaxBytes),
			wantMark: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Cap(tc.input, tc.max)
			if !strings.HasPrefix(got, tc.wantBody) {
				t.Fatalf("body prefix mismatch: got %q, want prefix %q", got[:min(len(got), 40)], tc.wantBody[:min(len(tc.wantBody), 40)])
			}
			hasMark := strings.Contains(got, "content truncated at")
			if hasMark != tc.wantMark {
				t.Fatalf("marker presence: got=%v want=%v (output len=%d)", hasMark, tc.wantMark, len(got))
			}
			if !tc.wantMark && got != tc.input {
				t.Fatalf("untruncated should equal input: got %q want %q", got, tc.input)
			}
		})
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
