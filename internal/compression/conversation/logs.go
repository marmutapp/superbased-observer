package conversation

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/compression/conversation/types"
)

// LogsCompressor collapses runs of identical consecutive lines into a single
// line with a " [×N]" suffix. When the deduped output is still long (>
// [LogsOptions.MaxLines]), a final head+tail truncation clamps it with a
// single elision marker.
//
// The shell layer has a similar rule (shell.DedupConsecutive); this one is
// kept self-contained because it operates on whole bodies rather than a
// streaming [io.Writer] pipeline.
type LogsCompressor struct {
	opts LogsOptions
}

// LogsOptions tunes LogsCompressor.
type LogsOptions struct {
	// MaxLines is the ceiling on the post-dedup line count; zero disables
	// the truncation pass. Default 200.
	MaxLines int
	// Head and Tail control the line budgets when truncation fires. Both
	// default to half of (MaxLines / 2) so the elision lands in the middle.
	Head, Tail int
}

// NewLogsCompressor constructs a LogsCompressor with default options.
func NewLogsCompressor() *LogsCompressor {
	return &LogsCompressor{opts: LogsOptions{MaxLines: 200, Head: 100, Tail: 100}}
}

// Type implements Compressor.
func (LogsCompressor) Type() types.ContentType { return types.Logs }

// Compress implements Compressor.
func (c LogsCompressor) Compress(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	lines := splitLines(body)
	deduped := dedupConsecutive(lines)
	clipped := headTail(deduped, c.opts.MaxLines, c.opts.Head, c.opts.Tail, "… [%d lines elided]")
	out := joinLines(clipped, endsWithNewline(body))
	if len(out) >= len(body) {
		return body
	}
	return out
}

func dedupConsecutive(lines []string) []string {
	out := make([]string, 0, len(lines))
	var prev string
	count := 0
	for _, line := range lines {
		if count > 0 && line == prev {
			count++
			continue
		}
		if count == 1 {
			out = append(out, prev)
		} else if count > 1 {
			out = append(out, fmt.Sprintf("%s [×%d]", prev, count))
		}
		prev = line
		count = 1
	}
	if count == 1 {
		out = append(out, prev)
	} else if count > 1 {
		out = append(out, fmt.Sprintf("%s [×%d]", prev, count))
	}
	return out
}

// headTail trims lines to head+marker+tail when the count exceeds max. When
// max <= 0 or head/tail are zero, the input is returned unchanged.
func headTail(lines []string, max, head, tail int, marker string) []string {
	if max <= 0 || len(lines) <= max {
		return lines
	}
	if head <= 0 {
		head = max / 2
	}
	if tail <= 0 {
		tail = max / 2
	}
	if head+tail >= len(lines) {
		return lines
	}
	dropped := len(lines) - head - tail
	out := make([]string, 0, head+1+tail)
	out = append(out, lines[:head]...)
	out = append(out, fmt.Sprintf(marker, dropped))
	out = append(out, lines[len(lines)-tail:]...)
	return out
}

func splitLines(body []byte) []string {
	s := string(body)
	// Strip one trailing newline so "foo\n" → ["foo"] rather than ["foo", ""].
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func joinLines(lines []string, trailingNewline bool) []byte {
	if len(lines) == 0 {
		return nil
	}
	out := strings.Join(lines, "\n")
	if trailingNewline {
		out += "\n"
	}
	return []byte(out)
}

func endsWithNewline(body []byte) bool {
	return len(body) > 0 && bytes.HasSuffix(body, []byte("\n"))
}
