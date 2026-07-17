package heuristic

import (
	"context"
	"regexp"
	"sort"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/codeintel"
	"github.com/marmutapp/superbased-observer/internal/codeintel/parse"
)

// parserName is the backend identity stamped on every ParseResult so a
// persisted row can be traced to (and later upgraded from) this fallback.
const parserName = "heuristic"

// maxExcerpt bounds every excerpt this backend emits (signatures, import
// raw text, call raw text). Heuristic never stores a body — only a
// declaration-line-sized slice.
const maxExcerpt = 500

// symRule is one symbol-definition pattern: a compiled regex, the kind it
// maps onto ("function" | "method" | "class" | "interface" | "type"), and
// the submatch index that holds the name. Rules are tried top-down per
// line; the first match wins, so more-specific rules come first.
type symRule struct {
	re   *regexp.Regexp
	kind string
	name int // submatch index of the captured name
}

// impRule is one import/use/require/include pattern. path is the submatch
// index of the imported specifier; alias is the submatch index of an
// easily captured alias (0 when the language form carries none).
type impRule struct {
	re    *regexp.Regexp
	path  int
	alias int
}

// langRules is the complete rule set for a language: its symbol and
// import patterns plus whether it uses braces (balanced-brace end scan)
// or indentation (dedent end scan) to bound a definition. The parse loop
// resolves to one langRules at the top and runs a single generic engine
// over it — there is no per-language branch inside the loop (CLAUDE.md
// anti-spaghetti rules 3 and 5).
type langRules struct {
	symbols   []symRule
	imports   []impRule
	braceLang bool // true: brace-delimited; false: indentation (python)
}

// servedCapability is the capability every served language reports.
// ExactSpans is ALWAYS false — heuristic spans are non-destructive.
var servedCapability = codeintel.LanguageCapability{
	Symbols:    true,
	ExactSpans: false,
	Imports:    true,
	Calls:      true,
}

// callRe captures a bare `identifier(` call. The leading negative
// lookbehind a regexp can't do is approximated by requiring the identifier
// to start at a word boundary and by filtering keywords via callStopwords.
var callRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\s*\(`)

// callStopwords are identifiers that look like calls (`if (`, `for (`,
// `return (` …) but are control-flow keywords, not callees. Over- and
// under-capture is acceptable for this best-effort pass; the stopword set
// removes the highest-frequency false positives across all served langs.
var callStopwords = map[string]bool{
	"if": true, "for": true, "while": true, "switch": true, "return": true,
	"def": true, "function": true, "func": true, "catch": true, "with": true,
	"do": true, "else": true, "elif": true, "case": true, "when": true,
	"and": true, "or": true, "not": true, "in": true, "match": true,
	"foreach": true, "fn": true, "let": true, "var": true, "const": true,
	"new": true, "await": true, "yield": true, "throw": true, "use": true,
	"using": true, "lock": true, "sizeof": true, "typeof": true, "where": true,
	"defer": true, "go": true, "select": true, "until": true, "unless": true,
}

// rules is the table-driven heart of the backend: Language -> langRules.
// Built once at package init. The engine never branches on the Language
// value; it looks the value up here and runs the generic loop.
var rules = buildRules()

// New returns the heuristic parse.Parser.
func New() parse.Parser { return parser{} }

// parser is the stateless backend value. All state lives in the
// package-level rule table.
type parser struct{}

// Languages reports the languages this backend serves (every key in the
// rule table), in a stable sorted order.
func (parser) Languages() []codeintel.Language {
	out := make([]codeintel.Language, 0, len(rules))
	for lang := range rules {
		out = append(out, lang)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Capabilities reports the per-language flags: servedCapability for a
// language in the rule table, the zero value otherwise.
func (parser) Capabilities(lang codeintel.Language) codeintel.LanguageCapability {
	if _, ok := rules[lang]; ok {
		return servedCapability
	}
	return codeintel.LanguageCapability{}
}

// Parse line-scans src against the rules for lang and returns a
// best-effort ParseResult. It never panics and never errors: an
// unsupported language or empty input yields an (essentially empty)
// result with the correct capability flags. filename is currently unused
// (heuristic FQN == Name).
func (parser) Parse(_ context.Context, src []byte, lang codeintel.Language, _ string) (codeintel.ParseResult, error) {
	res := codeintel.ParseResult{
		Lang:       lang,
		Capability: codeintel.LanguageCapability{}, // zero for unserved langs
		Parser:     parserName,
	}
	lr, ok := rules[lang]
	if !ok {
		return res, nil
	}
	res.Capability = servedCapability

	text := string(src)
	lines := splitLines(text)
	offsets := lineByteOffsets(text, len(lines))

	res.Nodes = extractNodes(lines, offsets, lr)
	res.Imports = extractImports(lines, offsets, lr)
	res.Calls = extractCalls(lines, offsets, res.Nodes)

	sortResult(&res)
	return res, nil
}

// extractNodes runs the symbol rule set over every line and resolves a
// best-effort end span. The engine is generic over braceLang vs indent.
func extractNodes(lines []string, offsets []int, lr langRules) []codeintel.Node {
	var nodes []codeintel.Node
	for i, line := range lines {
		for _, r := range lr.symbols {
			m := r.re.FindStringSubmatch(line)
			if m == nil || r.name >= len(m) || m[r.name] == "" {
				continue
			}
			name := m[r.name]
			start := i + 1 // 1-based
			end := 0
			if lr.braceLang {
				end = braceEndLine(lines, i)
			} else {
				end = indentEndLine(lines, i)
			}
			nodes = append(nodes, codeintel.Node{
				Kind:      r.kind,
				Name:      name,
				FQN:       name, // heuristic can't build reliable qualified names
				StartLine: start,
				EndLine:   end,
				StartByte: offsets[i],
				EndByte:   0, // advisory end; byte end intentionally left 0
				Signature: clip(strings.TrimSpace(line), maxExcerpt),
			})
			break // first matching rule wins for this line
		}
	}
	return nodes
}

// extractImports runs the import rule set over every line.
func extractImports(lines []string, offsets []int, lr langRules) []codeintel.Import {
	var imps []codeintel.Import
	for i, line := range lines {
		for _, r := range lr.imports {
			m := r.re.FindStringSubmatch(line)
			if m == nil || r.path >= len(m) || m[r.path] == "" {
				continue
			}
			alias := ""
			if r.alias > 0 && r.alias < len(m) {
				alias = m[r.alias]
			}
			imps = append(imps, codeintel.Import{
				Path:      m[r.path],
				Alias:     alias,
				StartLine: i + 1,
				StartByte: offsets[i],
				RawText:   clip(strings.TrimSpace(line), maxExcerpt),
			})
			break
		}
	}
	return imps
}

// extractCalls runs the generic call regex over every line, filtering
// keywords, and attaches each call to its enclosing node (by line
// containment) or -1.
func extractCalls(lines []string, offsets []int, nodes []codeintel.Node) []codeintel.CallSite {
	var calls []codeintel.CallSite
	for i, line := range lines {
		for _, loc := range callRe.FindAllStringSubmatchIndex(line, -1) {
			name := line[loc[2]:loc[3]]
			if callStopwords[name] {
				continue
			}
			calls = append(calls, codeintel.CallSite{
				Name:      name,
				Enclosing: enclosingNode(nodes, i+1),
				StartLine: i + 1,
				StartByte: offsets[i] + loc[2],
				RawText:   clip(strings.TrimSpace(line), maxExcerpt),
			})
		}
	}
	return calls
}

// enclosingNode returns the index of the node whose [StartLine, EndLine]
// contains line, or -1. When several nodes contain the line (nested), the
// innermost (latest-starting) wins. Nodes with EndLine==0 contribute only
// their start line.
func enclosingNode(nodes []codeintel.Node, line int) int {
	best := -1
	for idx, n := range nodes {
		end := n.EndLine
		if end == 0 {
			end = n.StartLine
		}
		if line >= n.StartLine && line <= end {
			if best == -1 || n.StartLine > nodes[best].StartLine {
				best = idx
			}
		}
	}
	return best
}

// braceEndLine returns the 1-based line holding the balanced closer of the
// first `{` at or after startIdx, or 0 if none is found. The scanner skips
// braces inside double/single/backtick strings and after a line-comment
// marker. It is deliberately simple — the end is advisory only.
func braceEndLine(lines []string, startIdx int) int {
	depth := 0
	opened := false
	for i := startIdx; i < len(lines); i++ {
		d, sawOpen := scanBraces(lines[i], &depth)
		if sawOpen {
			opened = true
		}
		_ = d
		if opened && depth <= 0 {
			return i + 1
		}
	}
	return 0
}

// scanBraces walks one line, adjusting *depth for `{`/`}` outside strings
// and comments. It returns the net delta and whether it saw an opening
// brace. A lightweight state machine handles "…", '…', `…` and the common
// line-comment markers (//, #, --).
func scanBraces(line string, depth *int) (int, bool) {
	const (
		normal = iota
		inDouble
		inSingle
		inBacktick
	)
	state := normal
	sawOpen := false
	delta := 0
	runes := []rune(line)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch state {
		case normal:
			switch c {
			case '"':
				state = inDouble
			case '\'':
				state = inSingle
			case '`':
				state = inBacktick
			case '/':
				if i+1 < len(runes) && runes[i+1] == '/' {
					return delta, sawOpen // rest is comment
				}
			case '#':
				return delta, sawOpen // shell/ruby/python-style comment
			case '-':
				if i+1 < len(runes) && runes[i+1] == '-' {
					return delta, sawOpen // lua/sql-style comment
				}
			case '{':
				*depth++
				delta++
				sawOpen = true
			case '}':
				*depth--
				delta--
			}
		case inDouble:
			if c == '\\' {
				i++ // skip escaped char
			} else if c == '"' {
				state = normal
			}
		case inSingle:
			if c == '\\' {
				i++
			} else if c == '\'' {
				state = normal
			}
		case inBacktick:
			if c == '`' {
				state = normal
			}
		}
	}
	return delta, sawOpen
}

// indentEndLine returns the 1-based line where an indentation-scoped
// definition (python) ends: the line just before the next non-blank line
// whose indentation is <= the definition's. When the body runs to EOF
// without a dedent it returns the last non-blank line. Returns 0 only when
// no body line exists (a bare definition with nothing under it). The end
// is advisory either way (ExactSpans stays false).
func indentEndLine(lines []string, startIdx int) int {
	defIndent := leadingIndent(lines[startIdx])
	end := 0
	for i := startIdx + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "" {
			continue // blank lines don't end a block
		}
		if leadingIndent(lines[i]) <= defIndent {
			return i // the dedented line is NOT part of the body; end is the line before it
		}
		end = i + 1 // 1-based; track the deepest body line for an EOF-bounded block
	}
	return end
}

// leadingIndent counts leading-whitespace columns, treating a tab as one
// column. The absolute value doesn't matter — only relative comparison
// against the definition line does.
func leadingIndent(line string) int {
	n := 0
	for _, c := range line {
		if c == ' ' || c == '\t' {
			n++
			continue
		}
		break
	}
	return n
}

// splitLines splits text on '\n', stripping a trailing '\r' per line so
// CRLF inputs scan identically to LF. An empty string yields no lines.
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	raw := strings.Split(text, "\n")
	for i := range raw {
		raw[i] = strings.TrimSuffix(raw[i], "\r")
	}
	return raw
}

// lineByteOffsets returns the byte offset of the start of each line in the
// ORIGINAL text (before \r stripping), so StartByte points at the real
// line start. n is len(splitLines(text)).
func lineByteOffsets(text string, n int) []int {
	offsets := make([]int, n)
	if n == 0 {
		return offsets
	}
	off := 0
	idx := 0
	for idx < n {
		offsets[idx] = off
		nl := strings.IndexByte(text[off:], '\n')
		if nl < 0 {
			break
		}
		off += nl + 1
		idx++
	}
	return offsets
}

// clip bounds s to max bytes without splitting a UTF-8 rune.
func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !validBoundary(s, cut) {
		cut--
	}
	return s[:cut]
}

// validBoundary reports whether i is a UTF-8 rune boundary in s.
func validBoundary(s string, i int) bool {
	if i <= 0 || i >= len(s) {
		return true
	}
	return s[i]&0xC0 != 0x80 // not a continuation byte
}

// sortResult makes the output deterministic: Nodes by StartLine then Name;
// Imports by StartLine then Path; Calls by StartLine then StartByte.
// Sorting Nodes can re-order them, so call site Enclosing indices are
// recomputed afterward against the sorted slice.
func sortResult(res *codeintel.ParseResult) {
	sort.SliceStable(res.Nodes, func(i, j int) bool {
		if res.Nodes[i].StartLine != res.Nodes[j].StartLine {
			return res.Nodes[i].StartLine < res.Nodes[j].StartLine
		}
		return res.Nodes[i].Name < res.Nodes[j].Name
	})
	sort.SliceStable(res.Imports, func(i, j int) bool {
		if res.Imports[i].StartLine != res.Imports[j].StartLine {
			return res.Imports[i].StartLine < res.Imports[j].StartLine
		}
		return res.Imports[i].Path < res.Imports[j].Path
	})
	// Recompute enclosing against the (now sorted) node slice so the index
	// is correct regardless of the order extraction produced.
	for i := range res.Calls {
		res.Calls[i].Enclosing = enclosingNode(res.Nodes, res.Calls[i].StartLine)
	}
	sort.SliceStable(res.Calls, func(i, j int) bool {
		if res.Calls[i].StartLine != res.Calls[j].StartLine {
			return res.Calls[i].StartLine < res.Calls[j].StartLine
		}
		return res.Calls[i].StartByte < res.Calls[j].StartByte
	})
}
