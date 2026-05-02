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
	JSON    ContentType = "json"
	Code    ContentType = "code"
	Logs    ContentType = "logs"
	Text    ContentType = "text"
	Diff    ContentType = "diff"
	HTML    ContentType = "html"
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

// looksLikeLogs returns true when enough lines in body match a log-line
// signature (timestamp prefix or leveled bracketed tag). The threshold is
// deliberately modest — 30% of the first 50 non-empty lines — because real
// logs are often interleaved with bare continuation lines.
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
		if logLinePattern.Match(line) {
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
