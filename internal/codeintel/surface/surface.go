package surface

// surface is the catalog + orchestration layer for codeintel's named
// read capabilities. It depends on the codeintel.Provider (store-backed
// reads + LoadGraph) and on the PURE analyze/ + query/ packages — it
// hosts the analyze/query orchestration the root engine cannot (the
// engine would form an import cycle with analyze/query, which import the
// root types). Nothing in the root package imports surface. (Package doc
// lives in doc.go.)

import (
	"context"

	"github.com/marmutapp/superbased-observer/internal/codeintel"
	"github.com/marmutapp/superbased-observer/internal/codeintel/analyze"
	"github.com/marmutapp/superbased-observer/internal/codeintel/query"
)

// Service runs the Tier-C surfaces over a codeintel.Provider.
type Service struct {
	p codeintel.Provider
}

// New builds a Service over a provider. A nil provider yields a Service
// whose surfaces all return empty (the provider's fail-open contract).
func New(p codeintel.Provider) *Service {
	if p == nil {
		p = codeintel.Unavailable()
	}
	return &Service{p: p}
}

// Available reports whether the backing index is usable.
func (s *Service) Available() bool { return s.p.Available() }

// Search runs full-text symbol search.
func (s *Service) Search(ctx context.Context, project, q string, limit int) ([]codeintel.SymbolMatch, error) {
	return s.p.Search(ctx, project, q, limit)
}

// SemanticNeighbors returns the symbols most related to nodeID.
func (s *Service) SemanticNeighbors(ctx context.Context, nodeID int64, limit int) ([]codeintel.SymbolMatch, error) {
	return s.p.SemanticNeighbors(ctx, nodeID, limit)
}

// Similar returns near-clone candidates for nodeID.
func (s *Service) Similar(ctx context.Context, nodeID int64, limit int) ([]codeintel.SymbolMatch, error) {
	return s.p.SimilarTo(ctx, nodeID, limit)
}

// Architecture computes the directory-level overview + communities for a
// project (loads the graph once, analyzes in memory).
func (s *Service) Architecture(ctx context.Context, project string) (analyze.Architecture, error) {
	g, err := s.p.LoadGraph(ctx, project)
	if err != nil {
		return analyze.Architecture{}, err
	}
	return analyze.ArchitectureOf(g), nil
}

// DeadCode returns dead-symbol candidates for a project.
func (s *Service) DeadCode(ctx context.Context, project string, onlyUnexported bool) ([]analyze.DeadSymbol, error) {
	g, err := s.p.LoadGraph(ctx, project)
	if err != nil {
		return nil, err
	}
	return analyze.DeadCode(g, onlyUnexported), nil
}

// Impact returns the symbols transitively affected by changes to the seed
// symbols (resolved by name within the project) — what could break.
func (s *Service) Impact(ctx context.Context, project string, seedNames []string) ([]codeintel.SymbolMatch, error) {
	g, err := s.p.LoadGraph(ctx, project)
	if err != nil {
		return nil, err
	}
	byName := map[string][]int64{}
	byID := map[int64]codeintel.GraphNode{}
	for _, n := range g.Nodes {
		byName[n.Name] = append(byName[n.Name], n.ID)
		byID[n.ID] = n
	}
	var seed []int64
	for _, name := range seedNames {
		seed = append(seed, byName[name]...)
	}
	impacted := analyze.Impact(g, seed)
	out := make([]codeintel.SymbolMatch, 0, len(impacted))
	for _, id := range impacted {
		n := byID[id]
		out = append(out, codeintel.SymbolMatch{
			ID: n.ID, Name: n.Name, FQN: n.FQN, Kind: n.Kind,
			File: n.File, Language: n.Language, StartLine: n.StartLine, EndLine: n.EndLine,
		})
	}
	return out, nil
}

// Query runs a Cypher-subset query over a project's graph.
func (s *Service) Query(ctx context.Context, project, cypher string) (codeintel.ResultSet, error) {
	g, err := s.p.LoadGraph(ctx, project)
	if err != nil {
		return codeintel.ResultSet{}, err
	}
	return query.Run(g, cypher)
}

// Surface is one entry in the discoverable catalog: a name + one-line
// description. Adding a surface = a Service method + a Catalog row.
type Surface struct {
	Name        string
	Description string
}

// Catalog lists the named surfaces (for `observer codeintel surfaces` and
// help text). The handler is the matching Service method.
func Catalog() []Surface {
	return []Surface{
		{"search", "Full-text symbol search (name/fqn/signature, camel+snake aware)."},
		{"similar", "Near-clone candidates for a symbol (MinHash/LSH)."},
		{"related", "Semantically related symbols (embedding cosine)."},
		{"architecture", "Directory-level overview + Louvain communities."},
		{"deadcode", "Functions/methods with no inbound calls (heuristic)."},
		{"impact", "Symbols transitively affected by changing given symbols."},
		{"query", "Read-only Cypher subset over the symbol graph."},
	}
}
