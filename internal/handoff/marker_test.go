package handoff

import (
	"reflect"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
)

func msg(role models.TranscriptRole, text string, calls ...models.ToolCallRef) models.TranscriptMessage {
	return models.TranscriptMessage{Role: role, Text: text, ToolCalls: calls}
}

func TestScanMarkers(t *testing.T) {
	tests := []struct {
		name string
		msgs []models.TranscriptMessage
		want []string
	}{
		{
			name: "none",
			msgs: []models.TranscriptMessage{msg(models.TranscriptUser, "hello world")},
			want: nil,
		},
		{
			name: "first user message carries the marker (prompt lane)",
			msgs: []models.TranscriptMessage{
				msg(models.TranscriptUser, "<!-- superbased-handoff abcd1234 -->\nlet's continue"),
			},
			want: []string{"abcd1234"},
		},
		{
			name: "marker in a file-read tool_result excerpt (file lane)",
			msgs: []models.TranscriptMessage{
				msg(models.TranscriptUser, "read HANDOFF.md"),
				msg(models.TranscriptAssistant, "reading", models.ToolCallRef{
					Name:          "Read",
					ResultExcerpt: "<!-- superbased-handoff deadbeef -->\n# Handover",
				}),
			},
			want: []string{"deadbeef"},
		},
		{
			name: "marker inside a tool call input excerpt",
			msgs: []models.TranscriptMessage{
				msg(models.TranscriptAssistant, "", models.ToolCallRef{
					Name:         "Bash",
					InputExcerpt: "grep superbased-handoff cafe0001 file",
				}),
			},
			want: []string{"cafe0001"},
		},
		{
			name: "no trailing space still captures to end of blob",
			msgs: []models.TranscriptMessage{msg(models.TranscriptUser, "superbased-handoff feedface")},
			want: []string{"feedface"},
		},
		{
			name: "duplicates collapse, order preserved",
			msgs: []models.TranscriptMessage{
				msg(models.TranscriptUser, "superbased-handoff aaaa1111 -->"),
				msg(models.TranscriptAssistant, "superbased-handoff bbbb2222 -->"),
				msg(models.TranscriptUser, "superbased-handoff aaaa1111 again"),
			},
			want: []string{"aaaa1111", "bbbb2222"},
		},
		{
			name: "prefix with no token yields nothing",
			msgs: []models.TranscriptMessage{msg(models.TranscriptUser, "superbased-handoff ")},
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ScanMarkers(tt.msgs)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ScanMarkers() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestContainsMarker(t *testing.T) {
	msgs := []models.TranscriptMessage{
		msg(models.TranscriptUser, "read the file"),
		msg(models.TranscriptAssistant, "ok", models.ToolCallRef{
			Name:          "Read",
			ResultExcerpt: "<!-- superbased-handoff 1a2b3c4d -->\nheader",
		}),
	}
	tests := []struct {
		name    string
		shortID string
		want    bool
	}{
		{"present in tool_result", "1a2b3c4d", true},
		{"absent", "99999999", false},
		{"empty never matches", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ContainsMarker(msgs, tt.shortID); got != tt.want {
				t.Errorf("ContainsMarker(%q) = %v, want %v", tt.shortID, got, tt.want)
			}
		})
	}
	if ContainsMarker(nil, "1a2b3c4d") {
		t.Error("nil transcript must not match")
	}
}
