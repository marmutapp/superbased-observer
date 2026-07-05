package types

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
)

// ContentType is the classified shape of a tool_result body. The enum values
// double as the string keys in [config.ConversationConfig.CompressTypes], so
// a user can opt a type in/out by name.
type ContentType string

// Enumeration of classified tool-result shapes. See spec §10 Layer 3.
const (
	JSON ContentType = "json"
	Code ContentType = "code"
	Logs ContentType = "logs"
	Text ContentType = "text"
	Diff ContentType = "diff"
	HTML ContentType = "html"
	// Tools gates the envelope-level tool-definition trim (V7-21).
	// Unlike the other content types — which classify a tool_result
	// BODY (e.g. Bash output → Logs) — Tools is a sentinel: no
	// detector returns it. It only appears as a key in the
	// buildAllow map consulted by pipeline.go before calling
	// compressToolDefinitions / compressOpenAI*ToolDefinitions on
	// the envelope's `tools` field. NOT in the default
	// CompressTypes list (operators opt in explicitly via
	// `compress_types = [..., "tools"]`) — see V7-21 plan-doc.
	Tools ContentType = "tools"

	Unknown ContentType = "unknown"
)

// All returns the set of classifiable content types in stable order.
func All() []ContentType {
	return []ContentType{JSON, Code, Logs, Text, Diff, HTML}
}

// Detect classifies body using filename as an optional hint. The filename
// comes from the originating action's target (e.g. a Read tool's file_path).
// When the filename extension gives a strong signal, it wins; otherwise body
// sniffing decides. Empty / whitespace-only bodies return Unknown.
func Detect(body []byte, filename string) ContentType {
	if len(bytes.TrimSpace(body)) == 0 {
		return Unknown
	}
	// V7-3: codex's `shell` tool always wraps its output in
	// `Exit code: N\nWall time: X.Xs\nOutput:\n<inner>`. Without
	// stripping this envelope the inner stream (which may be Code,
	// JSON, Markdown, …) is misclassified as Logs based on the
	// envelope's leading bytes. Strip first, classify the inner on
	// its own merits. Non-codex bodies pass through unchanged
	// because codexShellEnvelopeRe is anchored to \A and requires
	// the exact three-line prefix.
	if inner, stripped := stripCodexShellEnvelope(body); stripped {
		body = inner
		if len(bytes.TrimSpace(body)) == 0 {
			return Unknown
		}
	}
	if IsLineNumbered(body) {
		body = StripLineNumbers(body)
	}

	if ft := fromFilename(filename); ft != Unknown {
		// Even with a filename hint, a tool_result body that starts with `{`
		// or `[` is almost always JSON (the filename might be the file the
		// tool operated on, but the tool emitted a structured summary).
		if sniff := sniffStructural(body); sniff == JSON && ft != JSON {
			return JSON
		}
		return ft
	}

	if sniff := sniffStructural(body); sniff != Unknown {
		return sniff
	}
	if looksLikeLogs(body) {
		return Logs
	}
	return Text
}

// codexShellEnvelopeRe matches the literal "Exit code: N\nWall time:
// X.Xs[s|seconds]?\nOutput:\n" prefix codex's shell tool always emits.
// Anchored with \A so non-codex outputs that merely *contain* the
// phrase are not stripped. Negative exit codes recognized too
// (codex emits them on signal-terminated processes).
//
// Variants observed in real codex 0.130+ traffic:
//   - `Wall time: 0.123s\n`
//   - `Wall time: 0.123 seconds\n`
//   - `Wall time: 0.123\n` (no unit suffix; older codex builds)
var codexShellEnvelopeRe = regexp.MustCompile(
	`(?s)\AExit code: -?\d+\nWall time: [\d.]+ ?(?:seconds|s)?\nOutput:\n`,
)

// stripCodexShellEnvelope returns body with the leading envelope
// removed, plus true when the envelope was recognized. Otherwise
// returns body unchanged and false.
//
// V7-3 / V7-11 — the envelope normally pushes Detect to Logs based on
// its leading bytes; the inner Output: payload classifies on its own
// merits (TSX is Code, JSON is JSON, etc.) which materially changes
// what per-type compressor (if any) runs.
func stripCodexShellEnvelope(body []byte) ([]byte, bool) {
	if loc := codexShellEnvelopeRe.FindIndex(body); loc != nil {
		return body[loc[1]:], true
	}
	return body, false
}

// IsLineNumbered reports whether body looks like the cat -n style Read
// tool output: every (or nearly every) non-empty line begins with a
// run of digits, then a tab, then content. Threshold 80% of the first
// 50 non-empty lines so an occasional unprefixed continuation line
// doesn't break detection.
func IsLineNumbered(body []byte) bool {
	const sampleLines = 50
	const threshold = 0.80
	var matched, total int
	for _, raw := range bytes.SplitN(body, []byte("\n"), sampleLines+2) {
		line := bytes.TrimRight(raw, "\r")
		if len(line) == 0 {
			continue
		}
		total++
		if hasLineNumberPrefix(line) {
			matched++
		}
		if total >= sampleLines {
			break
		}
	}
	if total < 3 {
		// Too short to call — guard against spurious matches on small
		// bodies that happen to start with a digit.
		return false
	}
	return float64(matched)/float64(total) >= threshold
}

// StripLineNumbers strips the leading `<digits>\t` prefix from every
// line that has one, returning the underlying content. Lines without a
// prefix pass through unchanged.
func StripLineNumbers(body []byte) []byte {
	lines := bytes.Split(body, []byte("\n"))
	for i, line := range lines {
		j := 0
		for j < len(line) && line[j] >= '0' && line[j] <= '9' {
			j++
		}
		if j > 0 && j < len(line) && line[j] == '\t' {
			lines[i] = line[j+1:]
		}
	}
	return bytes.Join(lines, []byte("\n"))
}

func hasLineNumberPrefix(line []byte) bool {
	j := 0
	for j < len(line) && line[j] >= '0' && line[j] <= '9' {
		j++
	}
	return j > 0 && j < len(line) && line[j] == '\t'
}

// fromFilename maps a filename or path to a content type by extension.
// Returns Unknown when the extension is unrecognized or the filename is empty.
func fromFilename(filename string) ContentType {
	if filename == "" {
		return Unknown
	}
	base := filepath.Base(filename)
	ext := strings.ToLower(filepath.Ext(base))
	switch ext {
	case ".json", ".jsonl", ".ndjson":
		return JSON
	case ".go", ".py", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs",
		".java", ".kt", ".swift", ".rs", ".rb", ".php", ".c", ".h",
		".cc", ".cpp", ".hpp", ".cs", ".scala", ".sh", ".bash", ".zsh":
		return Code
	case ".log":
		return Logs
	case ".diff", ".patch":
		return Diff
	case ".html", ".htm":
		return HTML
	case ".md", ".txt", ".rst":
		return Text
	}
	return Unknown
}

// sniffStructural returns JSON, Diff, or HTML when the leading bytes form a
// recognizable structural signal. Returns Unknown when nothing structural
// matches.
func sniffStructural(body []byte) ContentType {
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) == 0 {
		return Unknown
	}

	// HTML — cheapest to check first because the markers are long-prefix.
	lower := asciiLowerPrefix(trimmed, 64)
	if bytes.HasPrefix(lower, []byte("<!doctype html")) ||
		bytes.HasPrefix(lower, []byte("<html")) {
		return HTML
	}

	// Diff — unified-diff markers. Require at least one `@@` hunk header in
	// the first 4KB so plain prose starting with "---" (Markdown rules) or
	// a single stray `+++` doesn't misclassify.
	head := trimmed
	if len(head) > 4096 {
		head = head[:4096]
	}
	if bytes.Contains(head, []byte("\n@@ ")) || bytes.HasPrefix(head, []byte("@@ ")) {
		if bytes.Contains(head, []byte("\n--- ")) || bytes.HasPrefix(head, []byte("--- ")) {
			return Diff
		}
		if bytes.Contains(head, []byte("\n+++ ")) || bytes.HasPrefix(head, []byte("+++ ")) {
			return Diff
		}
	}
	if bytes.HasPrefix(head, []byte("diff --git ")) {
		return Diff
	}

	// JSON — leading brace/bracket AND parses as JSON. Plain-text bodies
	// that happen to start with `{` (template markers, prose) will fail
	// the parse and fall through.
	if trimmed[0] == '{' || trimmed[0] == '[' {
		if json.Valid(bytes.TrimRight(trimmed, " \t\r\n")) {
			return JSON
		}
	}

	return Unknown
}

// logLinePattern matches lines that start with a timestamp or a leveled
// bracketed tag — the two universal log signatures. Compiled once.
var logLinePattern = regexp.MustCompile(
	`^(?:\[\d{4}-\d{2}-\d{2}|\d{4}-\d{2}-\d{2}[T ]|\d{2}:\d{2}:\d{2}|\[(?:DEBUG|INFO|WARN|WARNING|ERROR|FATAL|TRACE)\b)`,
)

// shellOutputPattern matches lines from common Bash tool output shapes
// observed in Claude Code workloads:
//   - `ls -la` / `ls -l`: permissions prefix `drwxr-xr-x`, `-rw-r--r--`
//   - grep with -n: `path:lineno:content`
//   - find: `/abs/path/to/file` or `./relative/path`
//   - wc -l: `      631 lib/application.js`
//
// Routed to the Logs ContentType; LogsCompressor's dedupe + head/tail
// strategy compresses repetitive shell output well without
// destructively eliding mid-content.
var shellOutputPattern = regexp.MustCompile(
	`^(?:[dlcbps-][rwx-]{9}\s|[\w./_~-]+:\d+:|[./~]?[\w./_-]+/[\w./_-]+\s*$|\s*\d+\s+\S+)`,
)

// looksLikeLogs returns true when enough lines in body match a log-line
// signature (timestamp prefix or leveled bracketed tag) OR a bash-output
// shape (perms-prefix, file:line:, path lines, wc -l). The threshold is
// deliberately modest — 30% of the first 50 non-empty lines — because real
// logs are often interleaved with bare continuation lines, and bash output
// often has header rows ("total 192") that don't match.
func looksLikeLogs(body []byte) bool {
	const sampleLines = 50
	const threshold = 0.30

	var matched, total int
	scanner := bytes.Split(body, []byte("\n"))
	for _, raw := range scanner {
		line := bytes.TrimRight(raw, "\r")
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		total++
		if logLinePattern.Match(line) || shellOutputPattern.Match(line) {
			matched++
		}
		if total >= sampleLines {
			break
		}
	}
	if total == 0 {
		return false
	}
	return float64(matched)/float64(total) >= threshold
}

// asciiLowerPrefix returns a lowercased copy of the first n bytes of b,
// assuming ASCII. Used for cheap case-insensitive prefix checks.
func asciiLowerPrefix(b []byte, n int) []byte {
	if n > len(b) {
		n = len(b)
	}
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		c := b[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return out
}
