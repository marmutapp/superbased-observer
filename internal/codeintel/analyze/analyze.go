package analyze

// Fidelity bound (see docs/codeintel/{analysis,limitations}.md): the
// CALLS graph is NAME-MATCHED, not type-resolved (D1), so dead-code and
// impact are heuristic — an over-linked edge can hide a dead symbol; an
// under-linked one can miss an impacted caller.

import (
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/codeintel"
)

// --- Architecture overview -------------------------------------------

// DirSummary aggregates the symbols and call coupling of one directory.
type DirSummary struct {
	Dir      string // directory (project-relative-ish; the file's parent)
	Symbols  int    // nodes whose file is in this dir
	Internal int    // CALLS edges whose src+dst are both in this dir
	Outbound int    // CALLS edges from this dir to another dir
	Inbound  int    // CALLS edges from another dir into this dir
}

// Architecture is the structural overview of a project.
type Architecture struct {
	Project     string
	TotalNodes  int
	TotalEdges  int // resolved CALLS edges
	Dirs        []DirSummary
	Communities []Community
}

// ArchitectureOf computes the directory-level overview + communities.
func ArchitectureOf(g codeintel.Graph) Architecture {
	a := Architecture{Project: g.Project, TotalNodes: len(g.Nodes)}
	dirOf := map[int64]string{}
	for _, n := range g.Nodes {
		dirOf[n.ID] = path.Dir(strings.ReplaceAll(n.File, "\\", "/"))
	}
	sums := map[string]*DirSummary{}
	get := func(d string) *DirSummary {
		s := sums[d]
		if s == nil {
			s = &DirSummary{Dir: d}
			sums[d] = s
		}
		return s
	}
	for _, n := range g.Nodes {
		get(dirOf[n.ID]).Symbols++
	}
	for _, e := range g.Edges {
		if e.Kind != "CALLS" {
			continue
		}
		a.TotalEdges++
		sd, dd := dirOf[e.Src], dirOf[e.Dst]
		if sd == "" || dd == "" {
			continue
		}
		if sd == dd {
			get(sd).Internal++
		} else {
			get(sd).Outbound++
			get(dd).Inbound++
		}
	}
	a.Dirs = make([]DirSummary, 0, len(sums))
	for _, s := range sums {
		a.Dirs = append(a.Dirs, *s)
	}
	sort.Slice(a.Dirs, func(i, j int) bool {
		if a.Dirs[i].Symbols != a.Dirs[j].Symbols {
			return a.Dirs[i].Symbols > a.Dirs[j].Symbols
		}
		return a.Dirs[i].Dir < a.Dirs[j].Dir
	})
	a.Communities = Louvain(g)
	return a
}

// --- Dead-code candidates --------------------------------------------

// DeadSymbol is a function/method with no inbound CALLS edge that is not
// an obvious entrypoint — a candidate for removal (heuristic).
type DeadSymbol struct {
	ID        int64
	Name      string
	FQN       string
	Kind      string
	File      string
	StartLine int
	Reason    string
}

// entrypointNames are symbol names never flagged dead — they are called
// by a runtime/framework/test harness, not by another indexed symbol.
var entrypointNames = map[string]struct{}{
	"main": {}, "init": {}, "setup": {}, "teardown": {},
	"setUp": {}, "tearDown": {}, "handler": {}, "Handler": {},
}

// DeadCode returns function/method nodes with zero inbound CALLS edges,
// excluding entrypoint-named symbols and (when onlyUnexported) exported
// API surface. The result is a candidate list, not a verdict — the
// name-matched call graph can miss dynamic/reflective/cross-language
// callers (documented bound).
func DeadCode(g codeintel.Graph, onlyUnexported bool) []DeadSymbol {
	inbound := map[int64]int{}
	for _, e := range g.Edges {
		if e.Kind == "CALLS" {
			inbound[e.Dst]++
		}
	}
	var out []DeadSymbol
	for _, n := range g.Nodes {
		if n.Kind != "function" && n.Kind != "method" {
			continue
		}
		if inbound[n.ID] > 0 {
			continue
		}
		if _, ok := entrypointNames[n.Name]; ok {
			continue
		}
		if isTestName(n.Name) {
			continue
		}
		exported := isExported(n.Name)
		if onlyUnexported && exported {
			continue
		}
		reason := "no inbound CALLS edge"
		if exported {
			reason += " (exported — may be public API or an external entrypoint)"
		}
		out = append(out, DeadSymbol{
			ID: n.ID, Name: n.Name, FQN: n.FQN, Kind: n.Kind,
			File: n.File, StartLine: n.StartLine, Reason: reason,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].StartLine < out[j].StartLine
	})
	return out
}

func isExported(name string) bool {
	if name == "" {
		return false
	}
	r := rune(name[0])
	return r >= 'A' && r <= 'Z'
}

func isTestName(name string) bool {
	return strings.HasPrefix(name, "Test") || strings.HasPrefix(name, "Benchmark") ||
		strings.HasPrefix(name, "Example") || strings.HasPrefix(name, "Fuzz") ||
		strings.HasPrefix(name, "test_") || strings.HasPrefix(name, "test")
}

// --- Impact analysis --------------------------------------------------

// Impact returns the transitive set of symbols that (directly or
// indirectly) CALL any seed symbol — i.e. what could break if the seeds
// change. Reverse-CALLS BFS, cycle-safe, excluding the seeds themselves.
func Impact(g codeintel.Graph, seed []int64) []int64 {
	callers := map[int64][]int64{} // dst -> srcs
	for _, e := range g.Edges {
		if e.Kind == "CALLS" {
			callers[e.Dst] = append(callers[e.Dst], e.Src)
		}
	}
	seen := map[int64]bool{}
	for _, s := range seed {
		seen[s] = true
	}
	queue := append([]int64(nil), seed...)
	impacted := map[int64]bool{}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, src := range callers[cur] {
			if seen[src] {
				continue
			}
			seen[src] = true
			impacted[src] = true
			queue = append(queue, src)
		}
	}
	out := make([]int64, 0, len(impacted))
	for id := range impacted {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// --- Louvain community detection -------------------------------------

// Community is a set of node ids the Louvain pass grouped together.
type Community struct {
	ID      int
	Members []int64
	Size    int
}

// Louvain runs one level of Louvain modularity local-moving over the
// UNDIRECTED CALLS graph (edge weight = number of resolved CALLS between
// two symbols, either direction). It returns the discovered communities
// sorted by size desc. Multi-level aggregation is a documented
// enhancement (single level already separates well-connected clusters).
func Louvain(g codeintel.Graph) []Community {
	// Build undirected adjacency with weights over CALLS only.
	idx := map[int64]int{}
	var ids []int64
	for _, n := range g.Nodes {
		if _, ok := idx[n.ID]; !ok {
			idx[n.ID] = len(ids)
			ids = append(ids, n.ID)
		}
	}
	n := len(ids)
	if n == 0 {
		return nil
	}
	adj := make([]map[int]float64, n)
	for i := range adj {
		adj[i] = map[int]float64{}
	}
	var m2 float64 // 2 * total weight
	deg := make([]float64, n)
	for _, e := range g.Edges {
		if e.Kind != "CALLS" {
			continue
		}
		si, sok := idx[e.Src]
		di, dok := idx[e.Dst]
		if !sok || !dok || si == di {
			continue
		}
		adj[si][di]++
		adj[di][si]++
		deg[si]++
		deg[di]++
		m2 += 2
	}
	if m2 == 0 {
		return nil // no edges → no meaningful communities
	}

	comm := make([]int, n)
	for i := range comm {
		comm[i] = i
	}
	sigmaTot := make([]float64, n)
	copy(sigmaTot, deg)

	// Local moving: repeatedly move each node to the neighbour community
	// that yields the greatest modularity gain, until stable (or capped).
	for range 20 {
		improved := false
		for v := range n {
			cur := comm[v]
			// weights from v to each neighbouring community
			kin := map[int]float64{}
			for u, w := range adj[v] {
				kin[comm[u]] += w
			}
			// remove v from its community
			sigmaTot[cur] -= deg[v]
			best, bestGain := cur, 0.0
			for c, wIn := range kin {
				gain := wIn - sigmaTot[c]*deg[v]/m2
				if gain > bestGain {
					bestGain, best = gain, c
				}
			}
			// staying gain baseline is 0 (we removed v); if no positive
			// move, fall back to original community.
			if bestGain <= 0 {
				best = cur
			}
			sigmaTot[best] += deg[v]
			if best != cur {
				comm[v] = best
				improved = true
			}
		}
		if !improved {
			break
		}
	}

	// Collect communities by label.
	members := map[int][]int64{}
	for i, c := range comm {
		members[c] = append(members[c], ids[i])
	}
	out := make([]Community, 0, len(members))
	cid := 0
	// deterministic order: by smallest member id
	labels := make([]int, 0, len(members))
	for c := range members {
		labels = append(labels, c)
	}
	sort.Slice(labels, func(i, j int) bool {
		return members[labels[i]][0] < members[labels[j]][0]
	})
	for _, c := range labels {
		mem := members[c]
		slices.Sort(mem)
		out = append(out, Community{ID: cid, Members: mem, Size: len(mem)})
		cid++
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Size != out[j].Size {
			return out[i].Size > out[j].Size
		}
		return out[i].Members[0] < out[j].Members[0]
	})
	return out
}
