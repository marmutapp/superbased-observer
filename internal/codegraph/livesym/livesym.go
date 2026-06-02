package livesym

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Symbol is a name+kind+start match from a single-line regex pass.
// EndLine is -1 when the parser can't reliably determine it (always
// in v1.7.16; reserved for future tree-sitter accuracy).
type Symbol struct {
	Name      string
	Kind      string // "function" | "method" | "class" | "interface" | "type"
	StartLine int
	EndLine   int // -1 = regex can't determine
	Language  string
}

// Parse scans the file at path and returns symbols matched by the
// language-specific regex set. Language is chosen by extension.
//
// Returns (nil, true, nil) when the extension isn't in the supported
// set — callers surface this as the regex_fallback_language_unsupported
// warning to the agent. Other I/O errors propagate.
func Parse(path string) (syms []Symbol, langUnsupported bool, err error) {
	if path == "" {
		return nil, false, errors.New("livesym.Parse: path is required")
	}
	lang, ok := languageOf(path)
	if !ok {
		return nil, true, nil
	}
	patterns, ok := patternsFor(lang)
	if !ok {
		return nil, true, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, false, fmt.Errorf("livesym.Parse: open: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		for _, p := range patterns {
			if m := p.re.FindStringSubmatch(line); m != nil {
				syms = append(syms, Symbol{
					Name:      m[p.nameGroup],
					Kind:      p.kind,
					StartLine: lineNum,
					EndLine:   -1,
					Language:  lang,
				})
				break
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, false, fmt.Errorf("livesym.Parse: scan: %w", err)
	}
	return syms, false, nil
}

// languageOf maps a file's extension to the language identifier used
// in Symbol.Language and elsewhere in the V7-12 surface. Returns
// ok=false for unsupported extensions.
func languageOf(path string) (string, bool) {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".go":
		return "go", true
	case ".ts":
		return "typescript", true
	case ".tsx":
		return "typescript", true
	case ".js", ".jsx", ".mjs", ".cjs":
		return "javascript", true
	case ".py":
		return "python", true
	}
	return "", false
}

type pattern struct {
	re        *regexp.Regexp
	kind      string
	nameGroup int // submatch index for the name; named groups would be cleaner but the index is stable + cheap
}

var patternCache = map[string][]pattern{
	"go": {
		// Method: matches `func (recv *T) Name(...` BEFORE the generic
		// function pattern (the function pattern also matches method
		// receivers via greedy regex otherwise).
		{re: regexp.MustCompile(`^func\s+\([^)]+\)\s+(\w+)\s*\(`), kind: "method", nameGroup: 1},
		{re: regexp.MustCompile(`^func\s+(\w+)\s*\(`), kind: "function", nameGroup: 1},
		{re: regexp.MustCompile(`^type\s+(\w+)\s+struct\b`), kind: "type", nameGroup: 1},
		{re: regexp.MustCompile(`^type\s+(\w+)\s+interface\b`), kind: "interface", nameGroup: 1},
		{re: regexp.MustCompile(`^type\s+(\w+)\s+[\w=]`), kind: "type", nameGroup: 1},
	},
	"typescript": {
		{re: regexp.MustCompile(`^(?:export\s+)?(?:async\s+)?function\s+(\w+)\s*[<(]`), kind: "function", nameGroup: 1},
		{re: regexp.MustCompile(`^(?:export\s+)?class\s+(\w+)\b`), kind: "class", nameGroup: 1},
		{re: regexp.MustCompile(`^(?:export\s+)?interface\s+(\w+)\b`), kind: "interface", nameGroup: 1},
		{re: regexp.MustCompile(`^(?:export\s+)?type\s+(\w+)\s*=`), kind: "type", nameGroup: 1},
	},
	"javascript": {
		{re: regexp.MustCompile(`^(?:export\s+)?(?:async\s+)?function\s+(\w+)\s*\(`), kind: "function", nameGroup: 1},
		{re: regexp.MustCompile(`^(?:export\s+)?class\s+(\w+)\b`), kind: "class", nameGroup: 1},
	},
	"python": {
		{re: regexp.MustCompile(`^(?:async\s+)?def\s+(\w+)\s*\(`), kind: "function", nameGroup: 1},
		{re: regexp.MustCompile(`^class\s+(\w+)\s*[:\(]`), kind: "class", nameGroup: 1},
	},
}

func patternsFor(lang string) ([]pattern, bool) {
	p, ok := patternCache[lang]
	return p, ok
}
