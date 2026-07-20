package resolve

import "strings"

// This file holds the PURE scoped (import/package-bound) CALLS resolver —
// W2 (docs/codeintel/resolution.md "Scoped resolution"). It upgrades the
// name-matched edges produced by Resolve for the dominant *unambiguous*
// cases, leaving everything else name-matched (no regression). It imports
// nothing (no SQL/HTTP/fsnotify); the store seam feeds it plain data and
// persists the bindings.
//
// It is NOT a type checker. It resolves the two call shapes that can be
// bound from in-project structure alone:
//   - a BARE call foo() binds within the caller's own package (Go: one
//     package per directory) — never to a same-named symbol in another
//     package (the #1 over-link bound, limitations.md §1);
//   - a QUALIFIED call pkg.Foo() binds to the package the file imports as
//     `pkg`.
// Receiver calls x.Foo() (x a local var) and complex receivers
// (a.b().Foo()) need type inference and are left name-matched.

// CallShape classifies a callee's syntax for the scoped resolver.
type CallShape int

const (
	// ShapeBare is an unqualified call: foo().
	ShapeBare CallShape = iota
	// ShapeQualified is a single-identifier-qualified call: q.foo().
	ShapeQualified
	// ShapeComplex is anything else (method on an expression, chained
	// calls) — not scope-resolvable here.
	ShapeComplex
)

// ScopeRules is the per-language classifier for the scoped resolver. The
// resolver branches on this capability (one rule set per language), never
// on a language name (CLAUDE.md rule 3 / 5). Resolve registers rule sets
// via ScopeRulesFor.
type ScopeRules interface {
	// Classify derives the call shape (and, for ShapeQualified, the
	// qualifier identifier) from the callee name and the call's raw source
	// excerpt. It must be conservative: anything it is unsure about is
	// ShapeComplex so the edge stays name-matched.
	Classify(callee, rawText string) (shape CallShape, qualifier string)
	// ReceiverType resolves a receiver variable to its type using the
	// ENCLOSING definition's signature — e.g. recvVar "r" against
	// "func (r *Foo) M(...)" yields "Foo", so a self-receiver call r.Bar()
	// can bind to Foo.Bar. ok is false when the signature does not bind
	// recvVar (the call then stays name-matched). Languages without method
	// receivers always return false. This is the reliable, declared-type
	// case only; local-var constructor inference (x := NewFoo()) is not
	// attempted here.
	ReceiverType(enclosingSig, recvVar string) (typeName string, ok bool)
}

// scopeRulesByLang is the rule-set registry. A language with no entry gets
// no scoped pass (its edges stay name-matched) — additive onboarding.
var scopeRulesByLang = map[string]ScopeRules{
	"go": goScopeRules{},
}

// ScopeRulesFor returns the scoped rule set for lang, or (nil, false) when
// the language has no scoped resolver yet.
func ScopeRulesFor(lang string) (ScopeRules, bool) {
	r, ok := scopeRulesByLang[lang]
	return r, ok
}

// PackageScopedLangs returns, sorted, the languages that use the Go-style
// package-dir scoped resolver (the store dispatches on this — capability,
// not a hardcoded list).
func PackageScopedLangs() []string { return sortedKeys(scopeRulesByLang) }

// ScopedNodeRef is a defined symbol the scoped resolver can target: a
// NodeRef plus its package key (Go: the file's directory) and FQN (for
// receiver-method binding, e.g. "Foo.Bar").
type ScopedNodeRef struct {
	ID     int64
	Name   string
	FQN    string
	FileID int64
	Pkg    string
}

// ImportBinding maps a qualifier as written in source (Local) to the
// package key (Pkg) a target node must carry to satisfy it. For Go the
// store supplies basename(import path) for both (unaliased imports);
// aliased imports it cannot reconstruct are simply absent, so those
// qualified calls stay name-matched.
type ImportBinding struct {
	Local string
	Pkg   string
}

// ScopedCall is one CALLS edge to (re)resolve with package scope.
// CallerSig is the signature of the call's enclosing definition (the src
// node), used to bind a self-receiver method call to its receiver type.
type ScopedCall struct {
	EdgeID     int64
	CallerFile int64
	CallerPkg  string
	Callee     string
	RawText    string
	CallerSig  string
	// RecvType is the receiver type inferred at parse time for a method
	// call on a local variable (W2 §4.1; x := NewFoo(); x.Bar() -> "Foo").
	// When set, the call binds directly to RecvType.Callee in the caller's
	// package (same machinery as a self-receiver), bypassing shape
	// classification. Empty for every other call.
	RecvType string
}

// Binding is a scoped resolution result for one edge: the chosen target
// and the confidence to stamp (resolver_backend "scoped" is applied by the
// store). Only edges the resolver is confident about are returned; the rest
// keep their name-matched binding.
type Binding struct {
	EdgeID     int64
	DstID      int64
	Confidence float64
}

// scopeIndex indexes target nodes for scoped lookup.
type scopeIndex struct {
	// byNamePkg[name][pkg] = ids defined with that name in that package.
	byNamePkg map[string]map[string][]int64
	// byNamePkgBase[name][base] = ids defined with that name in a package
	// whose basename is base (for qualified import matching).
	byNamePkgBase map[string]map[string][]int64
	// byFQNPkg[fqn][pkg] = ids with that FQN in that package (for
	// receiver-method binding, e.g. "Foo.Bar").
	byFQNPkg map[string]map[string][]int64
	fileOf   map[int64]int64
}

func buildScopeIndex(nodes []ScopedNodeRef) *scopeIndex {
	ix := &scopeIndex{
		byNamePkg:     make(map[string]map[string][]int64),
		byNamePkgBase: make(map[string]map[string][]int64),
		byFQNPkg:      make(map[string]map[string][]int64),
		fileOf:        make(map[int64]int64, len(nodes)),
	}
	for _, n := range nodes {
		if n.Name == "" {
			continue
		}
		if ix.byNamePkg[n.Name] == nil {
			ix.byNamePkg[n.Name] = make(map[string][]int64)
			ix.byNamePkgBase[n.Name] = make(map[string][]int64)
		}
		ix.byNamePkg[n.Name][n.Pkg] = append(ix.byNamePkg[n.Name][n.Pkg], n.ID)
		base := pkgBase(n.Pkg)
		ix.byNamePkgBase[n.Name][base] = append(ix.byNamePkgBase[n.Name][base], n.ID)
		if n.FQN != "" && n.FQN != n.Name {
			if ix.byFQNPkg[n.FQN] == nil {
				ix.byFQNPkg[n.FQN] = make(map[string][]int64)
			}
			ix.byFQNPkg[n.FQN][n.Pkg] = append(ix.byFQNPkg[n.FQN][n.Pkg], n.ID)
		}
		ix.fileOf[n.ID] = n.FileID
	}
	return ix
}

// ScopedResolve upgrades name-matched edges to scoped bindings where the
// call shape resolves unambiguously within the project. rules is the
// caller-language rule set; imports maps a caller file id to its import
// bindings. The result holds only the edges that gained a confident scoped
// binding (deterministic input order preserved).
func ScopedResolve(rules ScopeRules, nodes []ScopedNodeRef, imports map[int64][]ImportBinding, calls []ScopedCall) []Binding {
	if rules == nil {
		return nil
	}
	ix := buildScopeIndex(nodes)
	var out []Binding
	for _, c := range calls {
		// A parse-time-inferred local-var receiver (x := NewFoo(); x.Bar())
		// binds directly to RecvType.Callee — it cannot be any other shape
		// (x is a variable, not a package), so it bypasses classification.
		// An unresolved bind leaves the edge name-matched (no regression).
		if c.RecvType != "" {
			if b, ok := ix.bindMethod(c, c.RecvType); ok {
				out = append(out, b)
			}
			continue
		}
		shape, qual := rules.Classify(c.Callee, c.RawText)
		switch shape {
		case ShapeBare:
			if b, ok := ix.resolveBare(c); ok {
				out = append(out, b)
			}
		case ShapeQualified:
			if b, ok := ix.resolveQualified(c, qual, imports[c.CallerFile]); ok {
				out = append(out, b)
			} else if b, ok := ix.resolveReceiver(rules, c, qual); ok {
				out = append(out, b)
			}
		}
	}
	return out
}

// resolveBare binds a bare call to a same-package definition. None in the
// caller's package -> no binding (the call is left name-matched rather than
// over-linked to another package).
func (ix *scopeIndex) resolveBare(c ScopedCall) (Binding, bool) {
	ids := ix.byNamePkg[c.Callee][c.CallerPkg]
	switch len(ids) {
	case 0:
		return Binding{}, false
	case 1:
		return Binding{EdgeID: c.EdgeID, DstID: ids[0], Confidence: 0.9}, true
	}
	// Several same-package definitions of the name: prefer the same file,
	// else the first (deterministic). Still scoped (in-package), but lower
	// confidence to reflect the residual ambiguity.
	for _, id := range ids {
		if ix.fileOf[id] == c.CallerFile {
			return Binding{EdgeID: c.EdgeID, DstID: id, Confidence: 0.8}, true
		}
	}
	return Binding{EdgeID: c.EdgeID, DstID: ids[0], Confidence: 0.7}, true
}

// resolveQualified binds q.Foo() to the package the file imports as q. Only
// a unique target across the matching package(s) is bound; otherwise the
// edge stays name-matched.
func (ix *scopeIndex) resolveQualified(c ScopedCall, qual string, binds []ImportBinding) (Binding, bool) {
	if qual == "" {
		return Binding{}, false
	}
	byBase := ix.byNamePkgBase[c.Callee]
	if byBase == nil {
		return Binding{}, false
	}
	var cand []int64
	seen := map[string]bool{}
	for _, b := range binds {
		if b.Local != qual || seen[b.Pkg] {
			continue
		}
		seen[b.Pkg] = true
		cand = append(cand, byBase[b.Pkg]...)
	}
	if len(cand) == 1 {
		return Binding{EdgeID: c.EdgeID, DstID: cand[0], Confidence: 0.9}, true
	}
	return Binding{}, false
}

// resolveReceiver binds a self-receiver method call x.Foo() to
// RecvType.Foo when x is the enclosing method's receiver (its type read
// from the enclosing signature). Restricted to the caller's own package
// (a method's type lives in the same package), and only a unique match
// binds; otherwise the edge stays name-matched.
func (ix *scopeIndex) resolveReceiver(rules ScopeRules, c ScopedCall, recvVar string) (Binding, bool) {
	if c.CallerSig == "" {
		return Binding{}, false
	}
	typ, ok := rules.ReceiverType(c.CallerSig, recvVar)
	if !ok || typ == "" {
		return Binding{}, false
	}
	return ix.bindMethod(c, typ)
}

// bindMethod binds a method call to typeName.Callee in the caller's own
// package (a method lives in its type's package). Only a unique match
// binds; otherwise the edge stays name-matched. Shared by the
// self-receiver (type from signature) and local-var (type inferred at
// parse time) paths.
func (ix *scopeIndex) bindMethod(c ScopedCall, typeName string) (Binding, bool) {
	byPkg := ix.byFQNPkg[typeName+"."+c.Callee]
	if byPkg == nil {
		return Binding{}, false
	}
	ids := byPkg[c.CallerPkg]
	if len(ids) == 1 {
		return Binding{EdgeID: c.EdgeID, DstID: ids[0], Confidence: 0.9}, true
	}
	return Binding{}, false
}

// pkgBase returns the last path segment of a package key (its directory
// basename), matching how an unaliased import's local name relates to its
// path. Handles both '/' and '\' separators.
func pkgBase(pkg string) string {
	if i := strings.LastIndexAny(pkg, `/\`); i >= 0 {
		return pkg[i+1:]
	}
	return pkg
}

// goScopeRules classifies Go callees from the call's raw excerpt. Go is the
// first (and currently only) scoped language; another language adds a rule
// set + a scopeRulesByLang row (data, not core edits).
type goScopeRules struct{}

// Classify derives the shape from the source prefix up to the first call or
// index bracket. "Foo" -> bare; "pkg.Foo" -> qualified(pkg); anything else
// (method on an expression, chained calls, a mismatch) -> complex.
func (goScopeRules) Classify(callee, rawText string) (CallShape, string) {
	prefix := calleePrefix(rawText)
	if prefix == "" || callee == "" {
		return ShapeComplex, ""
	}
	if prefix == callee {
		return ShapeBare, ""
	}
	// qualified: q.callee where q is a single identifier.
	if dot := strings.LastIndexByte(prefix, '.'); dot >= 0 {
		q := prefix[:dot]
		name := prefix[dot+1:]
		if name == callee && isIdent(q) {
			return ShapeQualified, q
		}
	}
	return ShapeComplex, ""
}

// ReceiverType parses a Go method signature's receiver clause and returns
// the receiver type when recvVar matches the declared receiver name —
// e.g. ("func (r *Foo) M(x int)", "r") -> ("Foo", true). It returns false
// for a plain function signature, a mismatched receiver var, or anything it
// can't parse confidently (the call then stays name-matched).
func (goScopeRules) ReceiverType(sig, recvVar string) (string, bool) {
	if recvVar == "" {
		return "", false
	}
	// Expect: func ( <recv> <type> ) <name> ...
	rest := strings.TrimSpace(sig)
	if !strings.HasPrefix(rest, "func") {
		return "", false
	}
	rest = strings.TrimSpace(rest[len("func"):])
	if len(rest) == 0 || rest[0] != '(' {
		return "", false // no receiver clause -> plain function
	}
	close := strings.IndexByte(rest, ')')
	if close < 0 {
		return "", false
	}
	recv := strings.TrimSpace(rest[1:close]) // e.g. "r *Foo" or "r Foo[T]"
	fields := strings.Fields(recv)
	if len(fields) != 2 || fields[0] != recvVar {
		return "", false // unnamed receiver, mismatch, or odd shape
	}
	typ := strings.TrimLeft(fields[1], "*") // drop pointer
	if i := strings.IndexAny(typ, "[("); i >= 0 {
		typ = typ[:i] // drop generic params
	}
	if !isIdent(typ) {
		return "", false
	}
	return typ, true
}

// calleePrefix returns the callee path text: the raw excerpt up to the
// first '(' or '[' (a type-argument or call-argument opener), trimmed.
func calleePrefix(rawText string) string {
	end := len(rawText)
	for i := 0; i < len(rawText); i++ {
		if rawText[i] == '(' || rawText[i] == '[' {
			end = i
			break
		}
	}
	return strings.TrimSpace(rawText[:end])
}

// isIdent reports whether s is a single Go-style identifier (so a qualifier
// like "pkg" is accepted but "a.b" or "a()" is not).
func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '_':
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
