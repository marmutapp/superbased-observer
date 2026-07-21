package conversation

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/compression/conversation/types"
)

// logLevelCount tracks how many lines in an elided range carry each
// log severity. Empty when no recognisable level appeared. Used by the
// G32 marker enrichment so the model can decide whether to retrieve
// the elided range based on what's in it.
type logLevelCount struct {
	errors int
	warns  int
	infos  int
	debugs int
}

// logLevelPattern matches a log level word that appears either at the
// very start of a line (`ERROR something`) or in a bracketed prefix
// (`[ERROR] something`, `[2026-05-07T10:00 ERROR] something`). Three
// canonical levels plus DEBUG covered — INFO is the implicit-floor
// level so non-matching lines don't inflate any one bucket.
var logLevelPattern = regexp.MustCompile(`(?:^|[\[ ])(ERROR|FATAL|WARN(?:ING)?|INFO|DEBUG|TRACE)\b`)

func countLogLevels(lines []string) logLevelCount {
	var c logLevelCount
	for _, line := range lines {
		m := logLevelPattern.FindStringSubmatch(line)
		if len(m) < 2 {
			continue
		}
		switch m[1] {
		case "ERROR", "FATAL":
			c.errors++
		case "WARN", "WARNING":
			c.warns++
		case "INFO":
			c.infos++
		case "DEBUG", "TRACE":
			c.debugs++
		}
	}
	return c
}

// maxSymbolsInMarker caps the symbol enrichment list so a file with
// 50+ functions doesn't blow up marker length. The top-10 by
// start_line covers the file's structural shape without overwhelming
// the model. Files with more than this surface a `; +M more` suffix.
const maxSymbolsInMarker = 10

// maxMarkerBytes caps the rendered marker length defensively. ~400
// bytes is well under the head+tail content the marker is replacing
// (200+ lines × ~60 chars/line = 12 KB) and stays informative when
// the symbol list is non-trivial. Beyond the cap the marker truncates
// the symbol list with `(+M more)` rather than discarding the rest of
// the structure (level counts, anomaly preservation, etc).
const maxMarkerBytes = 400

// formatLogElisionMarker renders the elision marker. Four forms exist
// (orthogonal axes — each enrichment is opportunistic, none load-
// bearing):
//
//   - `… [N lines elided]` — neither levels nor anomalies present.
//   - `… [N lines elided: 3 ERRORs, 12 WARNs]` — G32 level breakdown.
//   - `… [N lines elided: 3 ERRORs; preserved 5 anomalies]` — E27 form
//     when anomaly lines are lifted out of the elided middle.
//   - `… [N lines elided from src/foo.tsx: 3 ERRORs, 12 WARNs;
//     preserved 5 anomalies; file contains fn handleClick, class Editor
//     (codeintel)]` — v1.7.7 / V7-11 mitigation (e). Filename surfaces
//     when the caller's [CompressHints.Filename] is set; symbol list
//     surfaces when [CompressHints.Symbols] is non-empty (caller pre-
//     fetched via the codeintel index + staleness check).
//
// The anomalies-preserved suffix is the E27 enrichment from the
// v1.4.43+ / Tier 3 bundle: it tells the model how many lines after
// the marker were promoted out of the elided range so it can read
// them inline rather than re-running the producing tool.
//
// The filename + symbols enrichments (V7-11 mitigation (e)) give
// codex-variant models the structural context they need to trust the
// marker, instead of re-deriving via shell tools to recover the
// elided middle.
func formatLogElisionMarker(dropped int, c logLevelCount, preserved int, hints CompressHints) string {
	parts := []string{}
	if c.errors > 0 {
		parts = append(parts, fmt.Sprintf("%d ERRORs", c.errors))
	}
	if c.warns > 0 {
		parts = append(parts, fmt.Sprintf("%d WARNs", c.warns))
	}
	if c.infos > 0 {
		parts = append(parts, fmt.Sprintf("%d INFO", c.infos))
	}
	if c.debugs > 0 {
		parts = append(parts, fmt.Sprintf("%d DEBUG", c.debugs))
	}
	base := fmt.Sprintf("… [%d lines elided", dropped)
	if hints.Filename != "" {
		base += " from " + hints.Filename
	}
	if len(parts) > 0 {
		base += ": " + strings.Join(parts, ", ")
	}
	if preserved > 0 {
		base += fmt.Sprintf("; preserved %d anomalies", preserved)
	}
	if len(hints.Symbols) > 0 {
		base += "; file contains " + symbolSuffix(hints.Symbols, maxSymbolsInMarker)
	}
	marker := base + "]"
	if len(marker) > maxMarkerBytes && len(hints.Symbols) > 0 {
		// Symbol list pushed past the byte budget even after the
		// per-count cap — re-render with progressively fewer symbols
		// until we fit. Caller may legitimately pass long symbol names
		// (TypeScript / Rust monomorphisation, fully-qualified
		// generics) that consume marker space faster than the count
		// cap anticipates.
		caretPos := strings.LastIndex(base, "; file contains ")
		for cap := maxSymbolsInMarker - 1; cap > 0; cap-- {
			candidate := base[:caretPos] +
				"; file contains " + symbolSuffix(hints.Symbols, cap) + "]"
			if len(candidate) <= maxMarkerBytes {
				return candidate
			}
		}
	}
	return marker
}

// symbolSuffix renders the symbol list portion of the elision marker
// with a `(+M more)` tail when the input exceeds cap. Cap == 0 returns
// the empty string — defensive for callers that pre-empty their list.
func symbolSuffix(syms []CompressorSymbol, cap int) string {
	if len(syms) == 0 || cap <= 0 {
		return ""
	}
	if len(syms) <= cap {
		return formatSymbolsForMarker(syms)
	}
	// Render the top-cap symbols, then append `(+M more)` BEFORE the
	// closing `(codeintel)` tag. The agent sees both signals: which
	// symbols we surfaced + that more exist but didn't fit.
	rendered := formatSymbolsForMarker(syms[:cap])
	const indexSuffix = " (codeintel)"
	rendered = strings.TrimSuffix(rendered, indexSuffix)
	return fmt.Sprintf("%s (+%d more)%s", rendered, len(syms)-cap, indexSuffix)
}

// formatSymbolsForMarker renders the symbol list portion of the
// elision marker. Prefixes each name with a short kind tag (`fn`,
// `class`, `interface`, `type`, `method`) — gives the model a
// glanceable structural shape without committing marker bytes to
// full prose. Closes with a parenthetical `(codeintel)` so the
// model knows the list came from a code index, not a guess.
//
// Caller is responsible for capping the symbol list at
// [maxSymbolsInMarker] before invoking — this function trusts the
// input and doesn't re-cap.
func formatSymbolsForMarker(syms []CompressorSymbol) string {
	if len(syms) == 0 {
		return ""
	}
	parts := make([]string, 0, len(syms))
	for _, s := range syms {
		prefix := ""
		switch s.Kind {
		case "function":
			prefix = "fn "
		case "method":
			prefix = "method "
		case "class":
			prefix = "class "
		case "interface":
			prefix = "interface "
		case "type":
			prefix = "type "
		}
		parts = append(parts, prefix+s.Name)
	}
	return strings.Join(parts, ", ") + " (codeintel)"
}

// anomalyPattern matches lines that warrant lift-out preservation
// through head+tail truncation, even when they fall in the elided
// middle range. Severities (FATAL/ERROR/PANIC) cover the load-bearing
// failure signals; `Caused by:` / `panic:` / `exit status N` (N > 0)
// catch process-level failures and Go/Java stack-frame headers.
//
// Anchors: severities match either at line start or after a bracket
// or space (so `[ERROR]` and `2026-05-07 ERROR` both match) so the
// shape is consistent with logLevelPattern.
var anomalyPattern = regexp.MustCompile(`(?:^|[\[ ])(FATAL|ERROR|PANIC)\b|^Caused by:|^panic:|exit (?:status|code) [1-9]`)

// maxPreservedAnomalies caps the lift-out so a pathological middle
// (e.g. 1000 retry-storm ERRORs) doesn't blow up the post-truncation
// body. 20 captures a typical FATAL+stack-frame cluster + a few
// supporting ERRORs without making the elided range pointless.
const maxPreservedAnomalies = 20

// extractAnomalies returns the indices in `lines` whose content matches
// [anomalyPattern], capped at [maxPreservedAnomalies]. Order preserved.
func extractAnomalies(lines []string) []int {
	var idxs []int
	for i, line := range lines {
		if anomalyPattern.MatchString(line) {
			idxs = append(idxs, i)
			if len(idxs) >= maxPreservedAnomalies {
				break
			}
		}
	}
	return idxs
}

// LogsCompressor strips invisible / repetitive bytes from log-shaped
// tool_result bodies (Bash, npm, cargo, pytest, kubectl, PowerShell
// Get-* tables, etc.). Every visible content line of the input still
// appears in the output; only ANSI escapes, carriage-return overwrites,
// trailing whitespace, padding-only blank-line runs, progress-bar
// padding, PowerShell-table column gutters, and run-length-encoded
// repeats are touched.
//
// Pipeline order (each step is independently content-preserving):
//  1. ANSI CSI escape strip
//  2. Carriage-return overwrite collapse (per line)
//  3. Trailing-whitespace trim per line
//  4. PowerShell-table wide-padding squash (only fires on lines that
//     look like Get-* tabular output)
//  5. Progress-bar line drop (lines that are only `=`/`-`/`>`/`%`/`#`/
//     `*`/`.` characters, optionally with whitespace)
//  6. Cap consecutive blank-line runs at 2
//  7. Run-length dedup of consecutive identical non-blank lines into
//     `<line> [×N]`
//  8. Final head+tail truncation pass when the post-dedup line count
//     still exceeds [LogsOptions.MaxLines]
//
// When all transforms together do not shrink the body, the original is
// returned (the standard `len(out) >= len(body)` short-circuit).
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

// NewLogsCompressor constructs a LogsCompressor with default options
// (MaxLines=200, Head=100, Tail=100). For a config-driven instance use
// [NewLogsCompressorWithOptions].
func NewLogsCompressor() *LogsCompressor {
	return &LogsCompressor{opts: LogsOptions{MaxLines: 200, Head: 100, Tail: 100}}
}

// NewLogsCompressorWithOptions constructs a LogsCompressor with the
// supplied options. Used by [BuildRegistryFromConfig] to honour the
// `[compression.conversation.logs]` TOML block. Zero-valued fields
// retain the per-field default behaviour described on [LogsOptions]
// (MaxLines=0 disables truncation; Head/Tail=0 fall back to
// MaxLines/2).
func NewLogsCompressorWithOptions(opts LogsOptions) *LogsCompressor {
	return &LogsCompressor{opts: opts}
}

// Type implements Compressor.
func (LogsCompressor) Type() types.ContentType { return types.Logs }

// Compress implements [Compressor]. Equivalent to [CompressHinted]
// with an empty [CompressHints] — the marker emitted in this path
// carries no filename or symbol enrichment. See the type doc for the
// per-step pipeline order and content-preservation guarantees.
func (c LogsCompressor) Compress(body []byte) []byte {
	return c.CompressHinted(body, CompressHints{})
}

// CompressHinted implements [HintedCompressor]. Identical pipeline to
// [Compress], but when [CompressHints.Filename] is non-empty and/or
// [CompressHints.Symbols] is non-nil, the elision marker emitted on
// over-budget bodies surfaces that context. Each hint is opportunistic
// — an empty zero-value hint produces byte-identical output to
// [Compress]. (V7-11 mitigation (e).)
func (c LogsCompressor) CompressHinted(body []byte, hints CompressHints) []byte {
	if len(body) == 0 {
		return body
	}

	stripped := stripANSI(body)

	lines := splitLines(stripped)
	if len(lines) == 0 {
		return body
	}

	for i, line := range lines {
		lines[i] = collapseCarriageReturns(line)
	}
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	for i, line := range lines {
		lines[i] = squashWidePadding(line)
	}
	lines = dropProgressBarLines(lines)
	lines = capBlankRunsAt(lines, 2)
	lines = dedupConsecutive(lines)

	clipped := headTailWithLevelStats(lines, c.opts.MaxLines, c.opts.Head, c.opts.Tail, hints)
	out := joinLines(clipped, endsWithNewline(body))
	if len(out) >= len(body) {
		return body
	}
	return out
}

// headTailWithLevelStats is the Logs-specific cousin of [headTail]. It
// performs the same head + marker + tail clip, but two enrichments
// fire on the elided middle:
//   - G32: the marker carries [logLevelCount] so the model has a
//     shape signal for the dropped content.
//   - E27: anomaly lines (FATAL/ERROR/PANIC/`Caused by:`/`panic:`/
//     non-zero exit) are lifted out of the middle and inserted right
//     after the marker, so the load-bearing failure signal survives
//     even when it would have landed in the elided range.
func headTailWithLevelStats(lines []string, max, head, tail int, hints CompressHints) []string {
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
	mid := lines[head : len(lines)-tail]
	stats := countLogLevels(mid)
	anomalyIdxs := extractAnomalies(mid)
	out := make([]string, 0, head+1+len(anomalyIdxs)+tail)
	out = append(out, lines[:head]...)
	out = append(out, formatLogElisionMarker(dropped, stats, len(anomalyIdxs), hints))
	for _, i := range anomalyIdxs {
		out = append(out, mid[i])
	}
	out = append(out, lines[len(lines)-tail:]...)
	return out
}

// ansiCSIPattern matches CSI (Control Sequence Introducer) escape
// sequences: ESC `[` then any params/intermediates then a final byte in
// [a-zA-Z]. Covers 95 % of real-world tool output (SGR colour codes,
// cursor moves, line erase). OSC and other ESC families are left
// untouched — they're rare enough that a conservative pass is safer
// than a fuzzy one.
var ansiCSIPattern = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

func stripANSI(body []byte) []byte {
	if !bytes.ContainsRune(body, 0x1b) {
		return body
	}
	return ansiCSIPattern.ReplaceAll(body, nil)
}

// collapseCarriageReturns replaces a line containing `\r` overwrites
// with just the final segment. Progress bars and spinners write
// `█\rdone` / `25 %\r50 %\r75 %\r100 %`-style sequences; only the last
// segment is the visible final state.
func collapseCarriageReturns(line string) string {
	if !strings.Contains(line, "\r") {
		return line
	}
	idx := strings.LastIndex(line, "\r")
	return line[idx+1:]
}

// progressBarPattern matches lines whose visible content is only
// padding/progress glyphs. Length ≥ 6 to avoid swallowing benign short
// dividers like `---` or `===` that often introduce a section in
// real prose.
var progressBarPattern = regexp.MustCompile(`^[\s=\->%#*.[\]<>█▓▒░]+$`)

func dropProgressBarLines(lines []string) []string {
	out := lines[:0]
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if len(trimmed) >= 6 && progressBarPattern.MatchString(trimmed) {
			continue
		}
		out = append(out, line)
	}
	return out
}

// widePaddingRun matches 4+ consecutive spaces — the gutter character
// PowerShell `Get-*` cmdlets emit between columns and that some
// build/test runners use for hand-aligned table output.
var widePaddingRun = regexp.MustCompile(`    +`)

// squashWidePadding collapses each run of ≥ 4 spaces to a single space,
// but only when the line contains at least 3 such runs. The 3-run
// threshold targets multi-column tabular output (header + ≥ 2 data
// columns) and avoids corrupting ls -l / wc -l style output, which
// typically has at most 1 such run per line. Tabs are left alone — they
// are semantically meaningful in many tool outputs.
func squashWidePadding(line string) string {
	if len(widePaddingRun.FindAllString(line, 4)) < 3 {
		return line
	}
	return widePaddingRun.ReplaceAllString(line, " ")
}

// capBlankRunsAt limits any run of consecutive blank (whitespace-only)
// lines to at most `cap` lines. Padding-only blank runs add no signal
// for the model and break dedup-by-blanks.
func capBlankRunsAt(lines []string, capLines int) []string {
	out := make([]string, 0, len(lines))
	blankRun := 0
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			blankRun++
			if blankRun <= capLines {
				out = append(out, line)
			}
			continue
		}
		blankRun = 0
		out = append(out, line)
	}
	return out
}

// dedupConsecutive collapses runs of identical consecutive non-blank
// lines into a single line with a `[×N]` suffix when N > 1. Blank
// lines act as run boundaries and are emitted as-is — collapsing
// blank runs is the job of [capBlankRunsAt], not this pass.
func dedupConsecutive(lines []string) []string {
	out := make([]string, 0, len(lines))
	var prev string
	count := 0
	flush := func() {
		switch {
		case count == 1:
			out = append(out, prev)
		case count > 1:
			out = append(out, fmt.Sprintf("%s [×%d]", prev, count))
		}
		count = 0
	}
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			flush()
			out = append(out, line)
			continue
		}
		if count > 0 && line == prev {
			count++
			continue
		}
		flush()
		prev = line
		count = 1
	}
	flush()
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
