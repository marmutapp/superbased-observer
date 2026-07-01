package verbosity

// Category classifies content by WHAT it is, independent of the channel it
// arrived through. It is a property of the content's language.
type Category string

const (
	// Prose is narrative explanation / plain text.
	Prose Category = "prose"
	// Code is source code AND shell/CLI commands (operator directive:
	// bash/powershell/sh/... are code wherever they appear).
	Code Category = "code"
	// Docs is documentation markup (markdown, rst, asciidoc, ...).
	Docs Category = "docs"
	// Config is configuration / serialization (toml, yaml, json, ini, ...).
	Config Category = "config"
	// Data is markup / style / tabular data (html, css, csv, sql, ...).
	Data Category = "data"
	// Unknown is an unresolved or unrecognized language.
	Unknown Category = "unknown"
)

// Lang is a canonical language name plus its category.
type Lang struct {
	Name     string
	Category Category
}

// langCategory is the single source of truth: canonical language name →
// category. Every value in extLang / filenameLang / tagAlias resolves to a
// key here, so FileType and LangForTag always yield a category. New
// languages are added HERE first (one owner), then wired into the
// resolver maps.
var langCategory = map[string]Category{
	// --- Code -----------------------------------------------------------
	"go": Code, "rust": Code, "typescript": Code, "tsx": Code,
	"javascript": Code, "jsx": Code, "python": Code, "java": Code,
	"kotlin": Code, "c": Code, "cpp": Code, "csharp": Code, "ruby": Code,
	"php": Code, "swift": Code, "scala": Code, "lua": Code, "perl": Code,
	"r": Code, "dart": Code, "elixir": Code, "erlang": Code, "clojure": Code,
	"haskell": Code, "ocaml": Code, "fsharp": Code, "vbnet": Code,
	"groovy": Code, "gradle": Code, "objc": Code, "protobuf": Code,
	"solidity": Code, "zig": Code, "nim": Code, "julia": Code,
	"terraform": Code, "sql": Code, "graphql": Code, "diff": Code,
	"scheme": Code, "jupyter": Code,
	// Shell / CLI — category=code by directive.
	"bash": Code, "powershell": Code, "batch": Code, "fish": Code,
	"make": Code, "cmake": Code, "dockerfile": Code,
	// --- Docs -----------------------------------------------------------
	"markdown": Docs, "rst": Docs, "asciidoc": Docs, "org": Docs,
	"latex": Docs, "license": Docs,
	// --- Config ---------------------------------------------------------
	"toml": Config, "yaml": Config, "json": Config, "ini": Config,
	"xml": Config, "properties": Config, "dotenv": Config, "plist": Config,
	"editorconfig": Config, "hcl": Config, "gitignore": Config,
	"gitattributes": Config, "dockerignore": Config, "codeowners": Config,
	"gomod": Config, "gosum": Config,
	// --- Data / markup --------------------------------------------------
	"html": Data, "css": Data, "scss": Data, "sass": Data, "less": Data,
	"csv": Data, "tsv": Data, "svg": Data, "log": Data, "binary": Data,
	// --- Prose ----------------------------------------------------------
	"text": Prose,
}

// CategoryOf returns the category of a canonical language name, or Unknown
// if the name is not registered.
func CategoryOf(lang string) Category {
	if c, ok := langCategory[lang]; ok {
		return c
	}
	return Unknown
}
