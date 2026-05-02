package conversation

import (
	"regexp"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/compression/conversation/types"
)

// CodeCompressor keeps top-of-file imports plus lines that look like a
// signature declaration (function/method/class/struct/interface/type) and
// drops the rest. This is a best-effort skeleton — without a language
// parser we can't strip bodies with full fidelity. In the default config
// ([compress_types = ["json", "logs", "text"]]) code is opt-in only, so
// the heuristic only runs when a user explicitly asks for it.
type CodeCompressor struct{}

// NewCodeCompressor constructs a CodeCompressor.
func NewCodeCompressor() *CodeCompressor { return &CodeCompressor{} }

// Type implements Compressor.
func (CodeCompressor) Type() types.ContentType { return types.Code }

// codeSignaturePattern matches common declaration keywords across popular
// languages. Ordered from language-specific (fn, def, func) to general
// (class, interface, type, struct).
var codeSignaturePattern = regexp.MustCompile(
	`^\s*(?:` +
		`export\s+(?:default\s+)?(?:async\s+)?function\b|` +
		`(?:async\s+)?function\b|` +
		`func\s+(?:\([^)]*\)\s+)?[A-Za-z_]|` +
		`fn\s+[A-Za-z_]|` +
		`def\s+[A-Za-z_]|` +
		`class\s+[A-Za-z_]|` +
		`interface\s+[A-Za-z_]|` +
		`struct\s+[A-Za-z_]|` +
		`trait\s+[A-Za-z_]|` +
		`type\s+[A-Za-z_]|` +
		`enum\s+[A-Za-z_]|` +
		`public\s+[A-Za-z_]|` +
		`private\s+[A-Za-z_]|` +
		`protected\s+[A-Za-z_]|` +
		`(?:impl|mod|namespace|package)\s+[A-Za-z_]` +
		`)`,
)

var codeImportPattern = regexp.MustCompile(
	`^\s*(?:import\b|from\s+[A-Za-z_.]+\s+import\b|require\b|use\b|#include\b)`,
)

// Compress implements Compressor.
func (c CodeCompressor) Compress(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	lines := splitLines(body)
	out := make([]string, 0, len(lines))
	elided := 0
	for _, line := range lines {
		if codeSignaturePattern.MatchString(line) || codeImportPattern.MatchString(line) {
			if elided > 0 {
				out = append(out, formatCodeElision(elided))
				elided = 0
			}
			out = append(out, line)
			continue
		}
		elided++
	}
	if elided > 0 {
		out = append(out, formatCodeElision(elided))
	}
	// If nothing matched, the file is likely not code (or is a language
	// without any matching keywords). Fall back to head+tail truncation so
	// we still emit something smaller than the original on large inputs.
	if len(out) == 1 && strings.HasPrefix(out[0], "… ") {
		trunc := headTail(lines, 60, 20, 20, "… [%d lines elided]")
		out = trunc
	}
	compact := joinLines(out, endsWithNewline(body))
	if len(compact) >= len(body) {
		return body
	}
	return compact
}

func formatCodeElision(n int) string {
	return "… " + itoa(n) + " non-signature lines elided"
}
