package codeintel

// This file defines the plain-data output of a parse pass — the
// contract between the parse backends (parse/goast, parse/heuristic,
// parse/treesitter) and the indexer that persists it. It carries no
// behaviour and no dependency, so the pure backends produce it and the
// I/O layer consumes it without either importing the other.

// Node is a parsed symbol definition with full span detail. It is the
// richer in-flight form of [Symbol]: the indexer persists it to
// codeintel_nodes, and a span without an exact end (heuristic backend)
// leaves EndLine/EndByte zero — which marks the node non-collapsible
// downstream (ADR-0005).
type Node struct {
	Kind      string // function | method | class | interface | type | enum | …
	Name      string
	FQN       string
	StartLine int
	EndLine   int // 0 when the backend produced only an approximate span
	StartByte int
	EndByte   int
	// Signature is a bounded excerpt (the declaration line), never a
	// body. Persisted as codeintel_nodes.signature.
	Signature string
}

// Import is an import/dependency specifier extracted from a file. Path
// is the imported module/package as written; RawText is a bounded
// excerpt of the import statement (never a file body).
type Import struct {
	Path      string
	Alias     string
	StartLine int
	StartByte int
	RawText   string
}

// CallSite is a call expression extracted from a file — the raw input
// to name-matched CALLS resolution (resolve package). Name is the
// callee identifier as written (the trailing segment of a dotted call).
// Enclosing is the index into [ParseResult.Nodes] of the symbol the
// call sits inside, or -1 when it is top-level / unresolved.
type CallSite struct {
	Name      string
	Enclosing int
	StartLine int
	StartByte int
	// RawText is a bounded excerpt of the call expression, never a file
	// body.
	RawText string
	// RecvType is the inferred receiver TYPE NAME for a method call on a
	// local variable of a known same-package type (Go: x := NewFoo();
	// x.Bar() -> "Foo"), so the scoped resolver can bind x.Bar() to Foo.Bar
	// (W2 §4.1). It is empty for every other call shape and for parsers that
	// do not infer it (additive — leaving it zero changes nothing). It does
	// NOT alter Name/target_name: the edge stays name-matched until the
	// scoped pass upgrades it in place.
	RecvType string
}

// ParseResult is the complete, pure output of a [parse.Parser] for one
// file. Parser is the backend identity (e.g. "goast", "treesitter:py",
// "heuristic") recorded on every persisted row so an edge/span can be
// traced to its producer and later upgraded.
type ParseResult struct {
	Lang       Language
	Capability LanguageCapability
	Parser     string
	Nodes      []Node
	Imports    []Import
	Calls      []CallSite
}
