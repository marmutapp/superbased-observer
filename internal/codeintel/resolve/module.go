package resolve

import (
	"path"
	"sort"
	"strings"
)

// This file holds the PURE module-import scoped resolver — W2 §4.2
// (docs/codeintel/resolution.md "Scoped resolution"). It is the TS/Python
// counterpart to the Go package-dir resolver in scoped.go: those languages
// use EXPLICIT per-file imports (not Go's same-directory = same package),
// so a bare call binds via the file's imports, and a namespace-qualified
// call binds via the imported module. It imports nothing but stdlib
// strings/path (no SQL/HTTP/fsnotify); the store seam feeds it plain data.
//
// Like the Go resolver it is unambiguous-only: it upgrades a name-matched
// edge only when the import binds the callee name AND the module resolves
// to exactly one indexed file defining exactly one such symbol. Every
// other shape (default imports, wildcard imports, tsconfig path aliases,
// external modules, multi-line imports whose names fell off the captured
// line) stays name-matched — precision can only improve.

// ParsedImport is the structured form of one import statement, parsed from
// its source-line excerpt by a ModuleRules implementation.
type ParsedImport struct {
	// Module is the module path as written (e.g. "./x", "utils", ".sub.m").
	Module string
	// Members maps a LOCAL name bound into the importing file to the
	// ORIGINAL name in the target module — `import {a, b as c}` yields
	// {a:a, c:b}; `from m import a, b as c` the same. A bare call to a
	// local name resolves to its original name in the module's file.
	Members map[string]string
	// Namespaces are local names that bind the WHOLE module as a qualifier
	// (TS `import * as ns`, Python `import m` / `import m as ns`): a call
	// `ns.foo()` resolves to `foo` in the module's file.
	Namespaces []string
}

// ModuleRules is the per-language rule set for the module-import scoped
// model. The resolver branches on this capability (one rule set per
// language via ModuleRulesFor), never on a language name (CLAUDE.md 3/5).
type ModuleRules interface {
	// ParseImport parses an import statement excerpt (rawText, the full
	// statement line) plus its already-extracted module path into a
	// ParsedImport. ok is false for shapes that bind nothing resolvable
	// (side-effect / wildcard / default-only imports, or a truncated line).
	ParseImport(rawText, modulePath string) (ParsedImport, bool)
	// SplitCall classifies a callee: a bare call foo() (qualified=false) vs
	// a qualified call ns.foo() (qualifier="ns"). A complex/unparseable
	// callee returns ("", false) AND should not be treated as bare — the
	// resolver only binds a qualified call through a known namespace.
	SplitCall(callee, rawText string) (qualifier string, qualified bool)
	// ResolveModule resolves a module path imported from importerFile to the
	// indexed files it could denote. fileSet is the set of normalised
	// (forward-slash) indexed file paths for the language; allFiles is the
	// same as a slice (for suffix matching of absolute module paths). It
	// returns the matches; the resolver binds only when there is exactly
	// one (unambiguous-only). An external/unresolvable module returns nil.
	ResolveModule(modulePath, importerFile string, fileSet map[string]bool, allFiles []string) []string
}

// moduleRulesByLang is the module-model rule-set registry. A language with
// no entry uses no module resolver — additive onboarding, same as the Go
// package-model registry.
var moduleRulesByLang = map[string]ModuleRules{
	"typescript": tsModuleRules{},
	"tsx":        tsModuleRules{},
	"python":     pyModuleRules{},
}

// ModuleRulesFor returns the module-model rule set for lang, or
// (nil, false) when the language has no module resolver.
func ModuleRulesFor(lang string) (ModuleRules, bool) {
	r, ok := moduleRulesByLang[lang]
	return r, ok
}

// ModuleScopedLangs returns, sorted, the languages that use the module-
// import scoped resolver (TS/TSX/Python today).
func ModuleScopedLangs() []string { return sortedKeys(moduleRulesByLang) }

// sortedKeys returns the keys of a string-keyed map in sorted order, for
// deterministic language iteration in the store.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// RawImport is one import as persisted for the module resolver: its module
// path (the site target_name) and the statement-line excerpt (raw_text,
// which carries the imported names).
type RawImport struct {
	Path    string
	RawText string
}

// memberTarget is a resolved import binding: the target file scope and the
// original symbol name in that file.
type memberTarget struct {
	file string
	orig string
}

// ModuleResolve upgrades name-matched edges for a module-model language.
// nodes carry Pkg = their FILE path (the scope key); calls carry
// CallerPkg = the caller's file path; imports is per-caller-file; files is
// the language's indexed file set. It returns only the edges that gained a
// confident import-bound binding (deterministic input order preserved).
func ModuleResolve(rules ModuleRules, nodes []ScopedNodeRef, imports map[int64][]RawImport, calls []ScopedCall, files []string) []Binding {
	if rules == nil {
		return nil
	}
	ix := buildScopeIndex(nodes)
	fileSet := make(map[string]bool, len(files))
	norm := make([]string, len(files))
	for i, f := range files {
		nf := normSlash(f)
		norm[i] = nf
		fileSet[nf] = true
	}

	// Per-caller-file binding maps, built lazily: a local name / namespace
	// -> its unique target file (+ original name). A name that two imports
	// bind to different files is dropped (ambiguous -> name-matched).
	type fileBinds struct {
		member map[string]memberTarget
		ns     map[string]string
	}
	cache := make(map[int64]*fileBinds)
	bindsFor := func(fileID int64, importerPath string) *fileBinds {
		if fb, ok := cache[fileID]; ok {
			return fb
		}
		fb := &fileBinds{member: map[string]memberTarget{}, ns: map[string]string{}}
		dropped := map[string]bool{}
		for _, ri := range imports[fileID] {
			pi, ok := rules.ParseImport(ri.RawText, ri.Path)
			if !ok {
				continue
			}
			targets := rules.ResolveModule(pi.Module, importerPath, fileSet, norm)
			if len(targets) != 1 {
				continue // unambiguous-only
			}
			t := targets[0]
			for local, orig := range pi.Members {
				if dropped[local] {
					continue
				}
				if prev, ok := fb.member[local]; ok && (prev.file != t || prev.orig != orig) {
					delete(fb.member, local)
					dropped[local] = true
					continue
				}
				fb.member[local] = memberTarget{file: t, orig: orig}
			}
			for _, n := range pi.Namespaces {
				if prev, ok := fb.ns[n]; ok && prev != t {
					delete(fb.ns, n)
					continue
				}
				fb.ns[n] = t
			}
		}
		cache[fileID] = fb
		return fb
	}

	var out []Binding
	for _, c := range calls {
		fb := bindsFor(c.CallerFile, normSlash(c.CallerPkg))
		qual, qualified := rules.SplitCall(c.Callee, c.RawText)
		if qualified {
			// ns.foo() — bind only through a known namespace import.
			t, ok := fb.ns[qual]
			if !ok {
				continue
			}
			if b, ok := uniqueInFile(ix, c, c.Callee, t); ok {
				out = append(out, b)
			}
			continue
		}
		if qual != "" {
			continue // a complex callee the rules declined to classify
		}
		// bare foo() — bind only through an imported member.
		mt, ok := fb.member[c.Callee]
		if !ok {
			continue
		}
		if b, ok := uniqueInFile(ix, c, mt.orig, mt.file); ok {
			out = append(out, b)
		}
	}
	return out
}

// uniqueInFile binds the call to name `name` defined in target file scope
// `file` when that file defines exactly one such symbol.
func uniqueInFile(ix *scopeIndex, c ScopedCall, name, file string) (Binding, bool) {
	byScope := ix.byNamePkg[name]
	if byScope == nil {
		return Binding{}, false
	}
	ids := byScope[file]
	if len(ids) == 1 {
		return Binding{EdgeID: c.EdgeID, DstID: ids[0], Confidence: 0.9}, true
	}
	return Binding{}, false
}

// normSlash normalises a path to forward slashes (separator-agnostic so a
// Windows-stored '\' path and a WSL/Unix '/' path compare equal).
func normSlash(p string) string { return strings.ReplaceAll(p, `\`, "/") }

// --- TypeScript / TSX -------------------------------------------------

type tsModuleRules struct{}

// ParseImport handles `import { a, b as c } from 'm'` and
// `import * as ns from 'm'`. Default imports and side-effect imports bind
// nothing resolvable (the default export's symbol name is unknown), so
// they return ok=false.
func (tsModuleRules) ParseImport(rawText, modulePath string) (ParsedImport, bool) {
	line := stripLineComment(rawText, "//")
	fromIdx := strings.LastIndex(line, " from ")
	if fromIdx < 0 {
		return ParsedImport{}, false // side-effect import: nothing to bind
	}
	clause := line[:fromIdx]
	if i := strings.Index(clause, "import"); i >= 0 {
		clause = clause[i+len("import"):]
	}
	clause = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(clause), "type"))

	pi := ParsedImport{Module: modulePath, Members: map[string]string{}}
	// namespace: * as ns
	if star := strings.Index(clause, "*"); star >= 0 {
		rest := strings.TrimSpace(clause[star+1:])
		if strings.HasPrefix(rest, "as ") {
			if ns := firstIdent(strings.TrimSpace(rest[len("as "):])); ns != "" {
				pi.Namespaces = append(pi.Namespaces, ns)
			}
		}
	}
	// named members: { a, b as c, type D }
	if open := strings.IndexByte(clause, '{'); open >= 0 {
		if close := strings.IndexByte(clause[open:], '}'); close >= 0 {
			for _, part := range strings.Split(clause[open+1:open+close], ",") {
				local, orig := parseAlias(part, "as")
				if local != "" && orig != "" {
					pi.Members[local] = orig
				}
			}
		}
	}
	if len(pi.Members) == 0 && len(pi.Namespaces) == 0 {
		return ParsedImport{}, false
	}
	return pi, true
}

// SplitCall classifies a TS callee from the call-expression excerpt. The
// qualifier is a single leading identifier (a member chain a.b.foo is
// complex — only a single-identifier namespace binds).
func (tsModuleRules) SplitCall(callee, rawText string) (string, bool) {
	return splitCallGeneric(callee, rawText)
}

// ResolveModule resolves a relative TS import to indexed files, trying the
// standard extension + index resolution order. Bare specifiers (npm /
// node:) and tsconfig path aliases (no leading ./ or ../) are external —
// nil, left name-matched.
func (tsModuleRules) ResolveModule(modulePath, importerFile string, fileSet map[string]bool, _ []string) []string {
	if !strings.HasPrefix(modulePath, "./") && !strings.HasPrefix(modulePath, "../") {
		return nil
	}
	base := path.Clean(path.Dir(normSlash(importerFile)) + "/" + modulePath)
	cands := []string{
		base + ".ts", base + ".tsx", base + ".d.ts", base + ".js", base + ".jsx", base + ".mjs", base + ".cjs",
		base + "/index.ts", base + "/index.tsx", base + "/index.js", base + "/index.jsx",
	}
	var out []string
	for _, c := range cands {
		if fileSet[c] {
			out = append(out, c)
		}
	}
	return out
}

// --- Python -----------------------------------------------------------

type pyModuleRules struct{}

// ParseImport handles `from m import a, b as c` (members) and
// `import m` / `import m as ns` (namespace). `from m import *` binds an
// unknown name set (ok=false); `from . import x` (module-as-name) is not
// attempted here.
func (pyModuleRules) ParseImport(rawText, modulePath string) (ParsedImport, bool) {
	line := stripLineComment(rawText, "#")
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return ParsedImport{}, false
	}
	pi := ParsedImport{Module: modulePath, Members: map[string]string{}}
	switch fields[0] {
	case "from":
		imp := strings.Index(line, " import ")
		if imp < 0 {
			return ParsedImport{}, false
		}
		clause := strings.TrimSpace(line[imp+len(" import "):])
		clause = strings.Trim(clause, "()")
		if strings.Contains(clause, "*") {
			return ParsedImport{}, false // wildcard: unknown names
		}
		for _, part := range strings.Split(clause, ",") {
			local, orig := parseAlias(part, "as")
			if local != "" && orig != "" {
				pi.Members[local] = orig
			}
		}
	case "import":
		// `import m` / `import m as ns` — modulePath is the captured dotted
		// name; the alias (if any) comes from the line.
		ns := modulePath
		if as := strings.Index(line, " as "); as >= 0 {
			if a := firstIdent(strings.TrimSpace(line[as+len(" as "):])); a != "" {
				ns = a
			}
		}
		if ns != "" {
			pi.Namespaces = append(pi.Namespaces, ns)
		}
	default:
		return ParsedImport{}, false
	}
	if len(pi.Members) == 0 && len(pi.Namespaces) == 0 {
		return ParsedImport{}, false
	}
	return pi, true
}

// SplitCall classifies a Python callee. A dotted qualifier (m.sub.foo) is
// kept whole so `import m.sub` namespaces match.
func (pyModuleRules) SplitCall(callee, rawText string) (string, bool) {
	return splitCallGeneric(callee, rawText)
}

// ResolveModule resolves a Python module to indexed files. Relative
// imports (leading dots) anchor on the importer's directory; absolute
// dotted modules are matched by unique path suffix. External / stdlib
// modules (no matching indexed file) return nil.
func (pyModuleRules) ResolveModule(modulePath, importerFile string, fileSet map[string]bool, allFiles []string) []string {
	if strings.HasPrefix(modulePath, ".") {
		dots := 0
		for dots < len(modulePath) && modulePath[dots] == '.' {
			dots++
		}
		dir := path.Dir(normSlash(importerFile))
		for i := 1; i < dots; i++ {
			dir = path.Dir(dir)
		}
		sub := strings.ReplaceAll(modulePath[dots:], ".", "/")
		base := dir
		if sub != "" {
			base = path.Clean(dir + "/" + sub)
		}
		var out []string
		for _, c := range []string{base + ".py", base + "/__init__.py"} {
			if fileSet[c] {
				out = append(out, c)
			}
		}
		return out
	}
	sub := strings.ReplaceAll(modulePath, ".", "/")
	var out []string
	for _, f := range allFiles {
		if strings.HasSuffix(f, "/"+sub+".py") || f == sub+".py" ||
			strings.HasSuffix(f, "/"+sub+"/__init__.py") || f == sub+"/__init__.py" {
			out = append(out, f)
		}
	}
	return out
}

// --- shared helpers ---------------------------------------------------

// splitCallGeneric classifies a call from its raw excerpt: foo() -> bare;
// q.foo() -> qualified(q) where q is a single (possibly dotted) identifier
// path; anything else (a()..foo(), arr[i].foo()) -> complex (no bind).
func splitCallGeneric(callee, rawText string) (string, bool) {
	prefix := calleePrefix(rawText)
	if callee != "" && prefix == callee {
		return "", false // bare: qualifier empty, qualified=false
	}
	// qualified q.callee where q is a single/dotted identifier path.
	if dot := strings.LastIndexByte(prefix, '.'); dot >= 0 &&
		prefix[dot+1:] == callee && isDottedIdent(prefix[:dot]) {
		return prefix[:dot], true
	}
	// Anything else (a().foo(), arr[i].foo(), empty) is complex — signalled
	// by a non-empty qualifier with qualified=false so the resolver binds
	// neither as bare nor as a namespace.
	return "_complex", false
}

// parseAlias splits "name" / "name as alias" into (localName, originalName).
// For `a as b` the LOCAL name is b and the ORIGINAL is a.
func parseAlias(part, asKw string) (local, orig string) {
	part = strings.TrimSpace(part)
	if part == "" {
		return "", ""
	}
	fields := strings.Fields(part)
	switch {
	case len(fields) == 1:
		id := firstIdent(fields[0])
		return id, id
	case len(fields) == 3 && fields[1] == asKw:
		return firstIdent(fields[2]), firstIdent(fields[0])
	default:
		// e.g. "type a" (TS) — last token is the name.
		id := firstIdent(fields[len(fields)-1])
		return id, id
	}
}

// firstIdent returns the leading identifier of s (letters/digits/_/$),
// dropping any trailing punctuation; "" when s does not start with one.
func firstIdent(s string) string {
	s = strings.TrimSpace(s)
	end := 0
	for end < len(s) {
		c := s[end]
		if c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			end++
			continue
		}
		break
	}
	return s[:end]
}

// isDottedIdent reports whether s is one or more identifiers joined by dots
// (a Python/TS namespace path like "m" or "a.b"). Empty segments reject.
func isDottedIdent(s string) bool {
	if s == "" {
		return false
	}
	for _, seg := range strings.Split(s, ".") {
		if seg == "" || firstIdent(seg) != seg {
			return false
		}
	}
	return true
}

// stripLineComment trims a trailing line comment (// or #) from an import
// excerpt so it does not pollute name parsing. It is excerpt-grade (no
// string-literal awareness needed — import statements have no // or # in
// their binding clause before the module string).
func stripLineComment(line, marker string) string {
	if i := strings.Index(line, marker); i >= 0 {
		return strings.TrimSpace(line[:i])
	}
	return strings.TrimSpace(line)
}
