package handoff

import (
	"strings"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// MarkerPrefix is the literal that leads the marker embedded in a
// HandoverDoc's first line (RenderMarkdown emits
// "<!-- superbased-handoff <shortid> -->"). The marker rides into the
// TARGET session along every delivery lane — the injected first prompt
// (inject_prompt), the SessionStart additionalContext (inject_hook), or
// the model reading HANDOFF-<shortid>.md (inject_file / inject_mcp, where
// the file content enters the transcript as a tool_result). The
// best-effort P3 linker scans a candidate target session's transcript for
// this marker to stamp handoffs.target_session_id.
const MarkerPrefix = "superbased-handoff "

// ScanMarkers returns the distinct handoff short-ids embedded anywhere in
// a transcript — every message's flattened text AND every tool call's
// input/result excerpt, because the marker can land in a file-read
// tool_result or a hook-lane context block, not only the first user
// message. Order is first-seen; duplicates are collapsed.
func ScanMarkers(msgs []models.TranscriptMessage) []string {
	seen := map[string]bool{}
	var out []string
	add := func(text string) {
		for _, id := range markersIn(text) {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	for _, m := range msgs {
		add(m.Text)
		for _, tc := range m.ToolCalls {
			add(tc.InputExcerpt)
			add(tc.ResultExcerpt)
		}
	}
	return out
}

// ContainsMarker reports whether the transcript carries the marker for a
// specific handoff short-id anywhere in its text or tool-call excerpts.
// An empty shortID never matches.
func ContainsMarker(msgs []models.TranscriptMessage, shortID string) bool {
	if shortID == "" {
		return false
	}
	needle := MarkerPrefix + shortID
	for _, m := range msgs {
		if strings.Contains(m.Text, needle) {
			return true
		}
		for _, tc := range m.ToolCalls {
			if strings.Contains(tc.InputExcerpt, needle) || strings.Contains(tc.ResultExcerpt, needle) {
				return true
			}
		}
	}
	return false
}

// markersIn extracts every short-id token that follows the marker prefix
// in one text blob. A short-id runs to the next whitespace (the rendered
// marker is "<!-- superbased-handoff abcd1234 -->", so the token stops
// before the trailing " -->").
func markersIn(text string) []string {
	var ids []string
	rest := text
	for {
		i := strings.Index(rest, MarkerPrefix)
		if i < 0 {
			break
		}
		rest = rest[i+len(MarkerPrefix):]
		j := 0
		for j < len(rest) && !isMarkerSpace(rest[j]) {
			j++
		}
		if j > 0 {
			ids = append(ids, rest[:j])
		}
		rest = rest[j:]
	}
	return ids
}

func isMarkerSpace(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r':
		return true
	}
	return false
}
