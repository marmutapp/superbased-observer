package index

import (
	"github.com/marmutapp/superbased-observer/internal/codeintel/parse"
	"github.com/marmutapp/superbased-observer/internal/codeintel/parse/goast"
	"github.com/marmutapp/superbased-observer/internal/codeintel/parse/heuristic"
	"github.com/marmutapp/superbased-observer/internal/codeintel/parse/treesitter"
)

// DefaultRegistry assembles the standard parse backend set. Registration
// order encodes precedence: the heuristic fallback is registered FIRST,
// then precise backends override it per language (go/ast wins for Go;
// tree-sitter wins for its grammars — the "swap a language from heuristic
// to precise with zero consumer change" guarantee).
//
// Assembled here (the orchestrator) rather than in parse, because the
// backends import parse — registering them inside parse would be an
// import cycle.
func DefaultRegistry() *parse.Registry {
	r, _ := DefaultRegistryWithWarnings()
	return r
}

// DefaultRegistryWithWarnings is DefaultRegistry plus any non-fatal
// warnings from compiling the tree-sitter grammar modules (a module that
// fails to compile is dropped and that language stays on the heuristic
// fallback). The index path can surface these; most callers use
// DefaultRegistry and ignore them.
func DefaultRegistryWithWarnings() (*parse.Registry, []string) {
	r := parse.NewRegistry()
	r.Register(heuristic.New()) // approximate spans, broad language coverage
	r.Register(goast.New())     // exact Go spans — overrides heuristic for Go
	ts, warnings := treesitter.New()
	r.Register(ts) // exact spans for its grammars — overrides heuristic
	return r, warnings
}
