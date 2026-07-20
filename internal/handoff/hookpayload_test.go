package handoff

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestHookPayload(t *testing.T) {
	marker := "<!-- superbased-handoff abcd1234 -->\n"
	// A realistic multi-line doc, larger than the pointer footer so the
	// truncate-with-pointer path leaves room for both a head and the
	// pointer (the real hook budget is 8KB; docs are 5–30KB).
	body := strings.Repeat("this is a line of the handover body\n", 80) // ~2.9KB
	doc := marker + "# Session handoff\n\n" + body

	tests := []struct {
		name     string
		doc      string
		docPath  string
		maxBytes int
		want     func(t *testing.T, out string)
	}{
		{
			name:     "fits verbatim at exact boundary",
			doc:      doc,
			docPath:  "/proj/HANDOFF-abcd1234.md",
			maxBytes: len(doc),
			want: func(t *testing.T, out string) {
				if out != doc {
					t.Errorf("exact-fit doc must pass through verbatim")
				}
			},
		},
		{
			name:     "one byte over → truncates with pointer, keeps marker + path",
			doc:      doc,
			docPath:  "/proj/HANDOFF-abcd1234.md",
			maxBytes: len(doc) - 1,
			want: func(t *testing.T, out string) {
				if len(out) > len(doc)-1 {
					t.Errorf("output %d bytes exceeds budget %d", len(out), len(doc)-1)
				}
				if !strings.Contains(out, "/proj/HANDOFF-abcd1234.md") {
					t.Error("truncated payload must point at the doc path")
				}
				if !strings.HasPrefix(out, marker) {
					t.Error("truncated payload must keep the marker line")
				}
			},
		},
		{
			name:     "moderate budget respects the cap and points to disk",
			doc:      doc,
			docPath:  "/proj/HANDOFF-abcd1234.md",
			maxBytes: 400,
			want: func(t *testing.T, out string) {
				if len(out) > 400 {
					t.Errorf("output %d bytes exceeds budget 400", len(out))
				}
				if !strings.Contains(out, "HANDOFF-abcd1234.md") {
					t.Error("payload must still point at the doc path")
				}
			},
		},
		{
			name:     "tiny budget smaller than the pointer still caps",
			doc:      doc,
			docPath:  "/proj/HANDOFF-abcd1234.md",
			maxBytes: 10,
			want: func(t *testing.T, out string) {
				if len(out) > 10 {
					t.Errorf("output %d bytes exceeds budget 10", len(out))
				}
			},
		},
		{
			name:     "zero budget yields empty",
			doc:      doc,
			docPath:  "/proj/HANDOFF-abcd1234.md",
			maxBytes: 0,
			want: func(t *testing.T, out string) {
				if out != "" {
					t.Errorf("zero budget must yield empty, got %q", out)
				}
			},
		},
		{
			name:     "empty doc path degrades to the read-the-file hint",
			doc:      doc,
			docPath:  "",
			maxBytes: len(doc) - 5,
			want: func(t *testing.T, out string) {
				if len(out) > len(doc)-5 {
					t.Errorf("output %d bytes exceeds budget", len(out))
				}
				if !strings.Contains(out, "HANDOFF-*.md") {
					t.Error("path-less pointer must still tell the model to read the file")
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := HookPayload(tc.doc, tc.docPath, tc.maxBytes)
			tc.want(t, out)
		})
	}
}

func TestHookPayloadNeverSplitsRune(t *testing.T) {
	// A doc with no newline, all multi-byte runes: truncation lands on a
	// non-boundary byte offset. Output must stay valid UTF-8 and within
	// budget for every budget across the head-truncation and
	// pointer-truncation paths.
	doc := strings.Repeat("é", 100) // 200 bytes, no newline
	for _, maxBytes := range []int{5, 40, 61, 120, 150, 199} {
		out := HookPayload(doc, "/p", maxBytes)
		if len(out) > maxBytes {
			t.Errorf("budget %d: output %d bytes exceeds budget", maxBytes, len(out))
		}
		if !utf8.ValidString(out) {
			t.Errorf("budget %d: output must remain valid UTF-8: %q", maxBytes, out)
		}
	}
}
