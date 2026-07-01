package parse

import (
	"path/filepath"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/codeintel"
)

// Canonical Language values for the v1 set. Spelled once here so every
// backend and the registry agree. The capability tier (Core vs
// Extended) is declared by each backend, not by this list — this is
// purely the identity + extension map (CLAUDE.md rule 3: identity at the
// boundary, capabilities downstream).
const (
	LangGo         codeintel.Language = "go"
	LangPython     codeintel.Language = "python"
	LangTypeScript codeintel.Language = "typescript"
	LangTSX        codeintel.Language = "tsx"
	LangJavaScript codeintel.Language = "javascript"
	LangJSX        codeintel.Language = "jsx"
	LangRust       codeintel.Language = "rust"
	LangJava       codeintel.Language = "java"
	LangC          codeintel.Language = "c"
	LangCPP        codeintel.Language = "cpp"
	LangCSharp     codeintel.Language = "csharp"
	LangRuby       codeintel.Language = "ruby"
	LangPHP        codeintel.Language = "php"
	LangKotlin     codeintel.Language = "kotlin"
	LangSwift      codeintel.Language = "swift"
	LangScala      codeintel.Language = "scala"
	LangBash       codeintel.Language = "bash"
	LangLua        codeintel.Language = "lua"
)

// extLang maps a lowercased file extension (with leading dot) to its
// Language. This is the single source of truth for path -> language
// resolution; adding a language adds rows here (data, not code). Where
// an extension is ambiguous (.h could be C or C++), the conservative
// choice is made and documented in docs/codeintel/languages.
var extLang = map[string]codeintel.Language{
	".go":    LangGo,
	".py":    LangPython,
	".pyi":   LangPython,
	".ts":    LangTypeScript,
	".mts":   LangTypeScript,
	".cts":   LangTypeScript,
	".tsx":   LangTSX,
	".js":    LangJavaScript,
	".mjs":   LangJavaScript,
	".cjs":   LangJavaScript,
	".jsx":   LangJSX,
	".rs":    LangRust,
	".java":  LangJava,
	".c":     LangC,
	".h":     LangC,
	".cc":    LangCPP,
	".cpp":   LangCPP,
	".cxx":   LangCPP,
	".hpp":   LangCPP,
	".hh":    LangCPP,
	".hxx":   LangCPP,
	".cs":    LangCSharp,
	".rb":    LangRuby,
	".php":   LangPHP,
	".kt":    LangKotlin,
	".kts":   LangKotlin,
	".swift": LangSwift,
	".scala": LangScala,
	".sc":    LangScala,
	".sh":    LangBash,
	".bash":  LangBash,
	".lua":   LangLua,
}

// LanguageForPath resolves a file path to its Language by extension.
// Returns ("", false) for an unknown/unsupported extension so the
// indexer can skip the file cleanly.
func LanguageForPath(path string) (codeintel.Language, bool) {
	ext := strings.ToLower(filepath.Ext(path))
	if ext == "" {
		return "", false
	}
	lang, ok := extLang[ext]
	return lang, ok
}

// Extensions returns every extension mapped to lang (leading dot,
// lowercased), in no particular order. Used by docs/diagnostics.
func Extensions(lang codeintel.Language) []string {
	var out []string
	for ext, l := range extLang {
		if l == lang {
			out = append(out, ext)
		}
	}
	return out
}
