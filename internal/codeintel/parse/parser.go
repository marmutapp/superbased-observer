package parse

import (
	"context"
	"sync"

	"github.com/marmutapp/superbased-observer/internal/codeintel"
)

// Parser is the single extension point for adding a LANGUAGE. A backend
// declares which languages it serves, the capability flags per language,
// and a pure bytes -> ParseResult function. The go/ast backend, the
// heuristic fallback, and the wasm tree-sitter host all satisfy it.
//
// Parse MUST be pure and panic-free (CLAUDE.md: no panic in library
// code) — malformed input returns a best-effort ParseResult (possibly
// empty), never a panic. Callers fail open on a non-nil error.
type Parser interface {
	// Languages reports the languages this backend can parse.
	Languages() []codeintel.Language
	// Capabilities reports the per-language capability flags. A backend
	// that cannot serve lang should return the zero LanguageCapability.
	Capabilities(lang codeintel.Language) codeintel.LanguageCapability
	// Parse turns src into a ParseResult. filename is advisory (used for
	// FQN construction and language-specific heuristics); it is not read
	// from disk.
	Parse(ctx context.Context, src []byte, lang codeintel.Language, filename string) (codeintel.ParseResult, error)
}

// Registry maps a Language to the Parser that serves it. It is the
// table the indexer consults to pick a backend per file. Registration
// order matters: a later Register for the same language WINS, so a
// precise backend (go/ast, tree-sitter) can override the heuristic
// fallback as it lands — the "swap a language from heuristic to
// tree-sitter with zero consumer change" guarantee.
//
// Safe for concurrent use after construction.
type Registry struct {
	mu     sync.RWMutex
	byLang map[codeintel.Language]Parser
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{byLang: make(map[codeintel.Language]Parser)}
}

// Register adds p for every language it serves. Later registrations
// override earlier ones for the same language (precise > fallback).
func (r *Registry) Register(p Parser) {
	if p == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, lang := range p.Languages() {
		r.byLang[lang] = p
	}
}

// For returns the Parser registered for lang, and whether one exists.
func (r *Registry) For(lang codeintel.Language) (Parser, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.byLang[lang]
	return p, ok
}

// Capability returns the capability flags for lang, or the zero value
// when no backend serves it.
func (r *Registry) Capability(lang codeintel.Language) codeintel.LanguageCapability {
	if p, ok := r.For(lang); ok {
		return p.Capabilities(lang)
	}
	return codeintel.LanguageCapability{}
}

// Languages returns the set of languages with a registered backend,
// in no particular order.
func (r *Registry) Languages() []codeintel.Language {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]codeintel.Language, 0, len(r.byLang))
	for lang := range r.byLang {
		out = append(out, lang)
	}
	return out
}
