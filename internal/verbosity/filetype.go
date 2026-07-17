package verbosity

import "strings"

// This file owns the language resolvers — the feature's comprehensive
// "name any file/tag" table (plan §4). It is deliberately BROADER than
// codeintel's parseable-language set: codeintel maps only languages it can
// PARSE, this maps anything we want to NAME for reporting. Unresolved
// extensions/tags are surfaced (false) so the caller can feed the §4
// unknown-ext / unknown-tag ledger and we extend these maps from real
// misses, never guesses.

// extLang maps a lowercased file extension (leading dot) to a canonical
// language name (a key of langCategory).
var extLang = map[string]string{
	".go": "go",
	".rs": "rust",
	".ts": "typescript", ".mts": "typescript", ".cts": "typescript",
	".tsx": "tsx",
	".js":  "javascript", ".mjs": "javascript", ".cjs": "javascript",
	".jsx": "jsx",
	".py":  "python", ".pyi": "python",
	".java": "java",
	".kt":   "kotlin", ".kts": "kotlin",
	".c": "c", ".h": "c",
	".cc": "cpp", ".cpp": "cpp", ".cxx": "cpp", ".hpp": "cpp", ".hxx": "cpp", ".hh": "cpp",
	".cs": "csharp",
	".rb": "ruby", ".rake": "ruby",
	".php":   "php",
	".swift": "swift",
	".scala": "scala", ".sc": "scala",
	".lua": "lua",
	".pl":  "perl", ".pm": "perl",
	".r":    "r",
	".dart": "dart",
	".ex":   "elixir", ".exs": "elixir",
	".erl": "erlang", ".hrl": "erlang",
	".clj": "clojure", ".cljs": "clojure", ".cljc": "clojure",
	".hs": "haskell",
	".ml": "ocaml", ".mli": "ocaml",
	".fs": "fsharp", ".fsx": "fsharp", ".fsi": "fsharp",
	".vb":     "vbnet",
	".groovy": "groovy",
	".gradle": "gradle",
	".m":      "objc", ".mm": "objc",
	".proto": "protobuf",
	".sol":   "solidity",
	".zig":   "zig",
	".nim":   "nim",
	".jl":    "julia",
	".tf":    "terraform", ".tfvars": "terraform",
	".hcl":     "hcl",
	".sql":     "sql",
	".graphql": "graphql", ".gql": "graphql",
	".diff": "diff", ".patch": "diff",
	".scm": "scheme", ".ss": "scheme", ".rkt": "scheme",
	".ipynb": "jupyter",
	// shell / CLI
	".sh": "bash", ".bash": "bash", ".zsh": "bash", ".ksh": "bash",
	".fish": "fish",
	".ps1":  "powershell", ".psm1": "powershell", ".psd1": "powershell",
	".bat": "batch", ".cmd": "batch",
	".mk": "make", ".mak": "make",
	".cmake": "cmake",
	// docs
	".md": "markdown", ".mdx": "markdown", ".markdown": "markdown",
	".rst":  "rst",
	".adoc": "asciidoc", ".asciidoc": "asciidoc",
	".org": "org",
	".tex": "latex",
	".txt": "text",
	// config
	".toml": "toml",
	".yaml": "yaml", ".yml": "yaml",
	".json": "json", ".jsonc": "json", ".json5": "json", ".jsonl": "json",
	".ndjson": "json", ".tsbuildinfo": "json",
	".ini": "ini", ".cfg": "ini", ".conf": "ini",
	".xml":          "xml",
	".properties":   "properties",
	".env":          "dotenv",
	".plist":        "plist",
	".editorconfig": "editorconfig",
	// data / markup
	".html": "html", ".htm": "html",
	".css":  "css",
	".scss": "scss",
	".sass": "sass",
	".less": "less",
	".csv":  "csv",
	".tsv":  "tsv",
	".svg":  "svg",
	".log":  "log",
	".exe":  "binary", ".bin": "binary", ".so": "binary", ".dll": "binary",
	".dylib": "binary", ".o": "binary", ".a": "binary", ".class": "binary",
	".wasm": "binary",
}

// filenameLang maps a lowercased exact basename (extensionless files and
// well-known dotfiles) to a canonical language name. Checked before the
// extension fallback so go.mod / Dockerfile / .gitignore resolve cleanly.
var filenameLang = map[string]string{
	"makefile":       "make",
	"dockerfile":     "dockerfile",
	"containerfile":  "dockerfile",
	"jenkinsfile":    "groovy",
	"vagrantfile":    "ruby",
	"rakefile":       "ruby",
	"gemfile":        "ruby",
	"procfile":       "yaml",
	"cmakelists.txt": "cmake",
	"go.mod":         "gomod",
	"go.sum":         "gosum",
	"license":        "license",
	"codeowners":     "codeowners",
	".gitignore":     "gitignore",
	".gitattributes": "gitattributes",
	".dockerignore":  "dockerignore",
	".editorconfig":  "editorconfig",
	".bashrc":        "bash", ".zshrc": "bash", ".bash_profile": "bash", ".profile": "bash",
	".env": "dotenv",
}

// tagAlias maps a lowercased fence info-string token OR shell hint to a
// canonical language name. A tag that is already canonical (a key of
// langCategory) needs no alias entry — LangForTag falls through to it.
var tagAlias = map[string]string{
	// shell / CLI aliases — all canonicalize to bash (category=code)
	"sh": "bash", "shell": "bash", "shell-session": "bash", "shellsession": "bash",
	"console": "bash", "terminal": "bash", "zsh": "bash", "ksh": "bash", "sh-session": "bash",
	"ps": "powershell", "ps1": "powershell", "pwsh": "powershell",
	"bat": "batch", "cmd": "batch", "dosbatch": "batch",
	// language aliases
	"ts": "typescript", "js": "javascript", "node": "javascript",
	"py": "python", "python3": "python", "rb": "ruby",
	"golang": "go", "rs": "rust",
	"c++": "cpp", "cxx": "cpp", "cplusplus": "cpp",
	"c#": "csharp", "cs": "csharp",
	"objective-c": "objc", "objectivec": "objc",
	"yml":   "yaml",
	"md":    "markdown",
	"jsonc": "json", "json5": "json",
	"txt": "text", "plaintext": "text", "plain": "text",
	"patch":  "diff",
	"gql":    "graphql",
	"proto":  "protobuf",
	"docker": "dockerfile",
	"tf":     "terraform",
	"htm":    "html",
}

// FileType resolves a file path to its canonical language + category by
// exact basename first (Makefile, go.mod, .gitignore), then by extension.
// Returns (Lang{}, false) for an unknown/extensionless file so the caller
// can record it in the unknown-ext ledger.
func FileType(path string) (Lang, bool) {
	p := strings.ToLower(strings.ReplaceAll(path, `\`, "/"))
	base := p
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		base = p[i+1:]
	}
	if base == "" {
		return Lang{}, false
	}
	if lang, ok := filenameLang[base]; ok {
		return mkLang(lang), true
	}
	dot := strings.LastIndexByte(base, '.')
	if dot <= 0 { // no extension, or a dotfile already handled above
		return Lang{}, false
	}
	if lang, ok := extLang[base[dot:]]; ok {
		return mkLang(lang), true
	}
	return Lang{}, false
}

// LangForTag resolves a fence info-string token or a shell hint (e.g. a
// command channel's dialect) to a canonical language + category. Returns
// (Lang{}, false) for an unrecognized tag so the caller can record it in
// the unknown-tag ledger.
func LangForTag(tag string) (Lang, bool) {
	t := strings.ToLower(strings.TrimSpace(tag))
	if t == "" {
		return Lang{}, false
	}
	if canon, ok := tagAlias[t]; ok {
		t = canon
	}
	if _, ok := langCategory[t]; ok {
		return mkLang(t), true
	}
	return Lang{}, false
}

func mkLang(name string) Lang {
	return Lang{Name: name, Category: CategoryOf(name)}
}
