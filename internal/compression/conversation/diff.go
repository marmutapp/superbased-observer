package conversation

import (
	"strings"

	"github.com/marmutapp/superbased-observer/internal/compression/conversation/types"
)

// DiffCompressor strips unified-diff context beyond ±1 line of each change.
// Keeps: git/index/`---`/`+++`/`@@` headers, every `+` and `-` line, and
// one leading + one trailing context line per change run. Drops everything
// else with a single "… N context lines elided" marker between runs.
type DiffCompressor struct{}

// NewDiffCompressor constructs a DiffCompressor.
func NewDiffCompressor() *DiffCompressor { return &DiffCompressor{} }

// Type implements Compressor.
func (DiffCompressor) Type() types.ContentType { return types.Diff }

// Compress implements Compressor.
func (c DiffCompressor) Compress(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	lines := splitLines(body)
	keep := make([]bool, len(lines))
	for i, line := range lines {
		if isDiffHeader(line) || isChangeLine(line) {
			keep[i] = true
		}
	}
	// Extend keep ±1 around each change line, but never across a hunk
	// header — keeping context that's actually adjacent to the change.
	for i, line := range lines {
		if !isChangeLine(line) {
			continue
		}
		if i > 0 && !isHunkHeader(lines[i-1]) {
			keep[i-1] = true
		}
		if i+1 < len(lines) && !isHunkHeader(lines[i+1]) {
			keep[i+1] = true
		}
	}

	out := make([]string, 0, len(lines))
	elided := 0
	for i, line := range lines {
		if keep[i] {
			if elided > 0 {
				out = append(out, elisionMarker(elided))
				elided = 0
			}
			out = append(out, line)
			continue
		}
		elided++
	}
	if elided > 0 {
		out = append(out, elisionMarker(elided))
	}
	compact := joinLines(out, endsWithNewline(body))
	if len(compact) >= len(body) {
		return body
	}
	return compact
}

func isDiffHeader(line string) bool {
	switch {
	case strings.HasPrefix(line, "diff --git "):
		return true
	case strings.HasPrefix(line, "index "):
		return true
	case strings.HasPrefix(line, "--- "):
		return true
	case strings.HasPrefix(line, "+++ "):
		return true
	case strings.HasPrefix(line, "new file mode"),
		strings.HasPrefix(line, "deleted file mode"),
		strings.HasPrefix(line, "rename from "),
		strings.HasPrefix(line, "rename to "),
		strings.HasPrefix(line, "similarity index "):
		return true
	case isHunkHeader(line):
		return true
	}
	return false
}

func isHunkHeader(line string) bool {
	return strings.HasPrefix(line, "@@ ")
}

// isChangeLine reports whether line is a change (`+` or `-`) inside a hunk
// body. The `---` / `+++` file-header lines and `@@` hunk headers are
// excluded via a prefix check in [isDiffHeader] which runs first.
func isChangeLine(line string) bool {
	if strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") {
		return false
	}
	if line == "" {
		return false
	}
	switch line[0] {
	case '+', '-':
		return true
	}
	return false
}

func elisionMarker(n int) string {
	if n == 1 {
		return "… 1 context line elided"
	}
	return formatElision(n)
}

// formatElision is factored out so callers can replace the phrasing
// without chasing Sprintf formatting into multiple spots.
func formatElision(n int) string {
	return "… " + itoa(n) + " context lines elided"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
