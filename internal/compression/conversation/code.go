package conversation

import (
	"fmt"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/compression/conversation/types"
)

// CodeCompressor produces a content-preserving skeleton of source code.
// Every non-comment, non-blank line of the input appears bytewise in the
// output. The compressor only removes invisible bytes (trailing
// whitespace, padding-only blank-line runs), collapses runs of identical
// consecutive non-blank lines into a single line with a "[×N]" marker
// (mirroring [LogsCompressor.dedupConsecutive]), and elides a leading
// license/banner block when the file opens with a long contiguous run of
// comment-only lines.
//
// Forbidden — these are the regression class that v1.4.38 / v1.4.39 spent
// two releases fixing:
//   - body elision
//   - mid-file comment stripping
//   - signature-only skeleton (drop function bodies, keep declarations)
//
// Yields are modest (~5-15% on typical source) but preservation is total,
// which is why `code` ships in the default `compress_types` allow-list.
type CodeCompressor struct{}

// NewCodeCompressor constructs a CodeCompressor.
func NewCodeCompressor() *CodeCompressor { return &CodeCompressor{} }

// Type implements Compressor.
func (CodeCompressor) Type() types.ContentType { return types.Code }

// Compress applies four strict-content-preserving transforms in order:
//  1. trim trailing whitespace per line
//  2. elide a leading license/banner block (file-top, ≥5 lines, ≥3 comment lines)
//  3. dedup consecutive identical non-blank lines into "<line> [×N]"
//  4. cap consecutive blank-line runs at 2
//
// (3) and (4) run in a single pass so blank-run capping does not interact
// with run-length markers. Returns the original body unchanged when the
// compressed form would not be shorter (the standard short-circuit
// honoured by every compressor in this package).
func (c CodeCompressor) Compress(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	lines := splitLines(body)
	if len(lines) == 0 {
		return body
	}

	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}

	lines, _ = elideLicenseHeader(lines)

	lines = dedupCodeAndCapBlanks(lines)

	out := joinLines(lines, endsWithNewline(body))
	if len(out) >= len(body) {
		return body
	}
	return out
}

// elideLicenseHeader replaces a leading license/banner block with a
// single marker line when the file opens with a long contiguous run of
// comment-only (or blank) lines. Returns the (possibly rewritten) line
// slice and the count of lines that were elided (0 when nothing fired).
//
// Rule: from line 0, count a run of lines where each is either a comment
// (`//`, `/* … */`, or `#` excluding shebangs) or blank. The run ends at
// the first line that is neither. The run is replaced when its length is
// ≥ 5 lines AND it contains ≥ 3 actual comment lines (not just blanks),
// guarding against stripping a small file with one TODO at the top.
//
// Python `"""` docstrings are NOT recognised as comments here; they are
// string literals with module-level semantics (they become `__doc__`)
// and stripping them would change behaviour for any reader (or static
// analyser) that treats the body as runnable code.
func elideLicenseHeader(lines []string) ([]string, int) {
	if len(lines) < 5 {
		return lines, 0
	}
	inBlock := false
	runEnd := -1
	commentCount := 0
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if inBlock {
			commentCount++
			runEnd = i
			if strings.Contains(trimmed, "*/") {
				inBlock = false
			}
			continue
		}
		if trimmed == "" {
			runEnd = i
			continue
		}
		if strings.HasPrefix(trimmed, "//") {
			commentCount++
			runEnd = i
			continue
		}
		if strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(trimmed, "#!") {
			commentCount++
			runEnd = i
			continue
		}
		if strings.HasPrefix(trimmed, "/*") {
			commentCount++
			runEnd = i
			rest := trimmed[2:]
			if !strings.Contains(rest, "*/") {
				inBlock = true
			}
			continue
		}
		break
	}
	// Trim trailing blank lines from the run so the marker keeps the
	// visual break before the first code line. Anything trimmed off here
	// stays in the output as-is.
	for runEnd >= 0 && strings.TrimSpace(lines[runEnd]) == "" {
		runEnd--
	}
	runLen := runEnd + 1
	if runLen < 5 || commentCount < 3 {
		return lines, 0
	}
	out := make([]string, 0, len(lines)-runLen+1)
	out = append(out, fmt.Sprintf("// [%d-line license header elided]", runLen))
	out = append(out, lines[runLen:]...)
	return out, runLen
}

// dedupCodeAndCapBlanks runs the combined "dedup consecutive identical
// non-blank lines" + "cap consecutive blank-line runs at 2" pass. Done in
// one walk so blank-run handling and run-length handling don't fight.
//
// Run-length markers use the same `[×N]` form as
// [LogsCompressor.dedupConsecutive] for consistency across compressors.
func dedupCodeAndCapBlanks(lines []string) []string {
	out := make([]string, 0, len(lines))
	var prev string
	runCount := 0
	blankRun := 0
	flush := func() {
		switch {
		case runCount == 1:
			out = append(out, prev)
		case runCount > 1:
			out = append(out, fmt.Sprintf("%s [×%d]", prev, runCount))
		}
		runCount = 0
	}
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			flush()
			blankRun++
			if blankRun <= 2 {
				out = append(out, line)
			}
			continue
		}
		blankRun = 0
		if runCount > 0 && line == prev {
			runCount++
			continue
		}
		flush()
		prev = line
		runCount = 1
	}
	flush()
	return out
}
