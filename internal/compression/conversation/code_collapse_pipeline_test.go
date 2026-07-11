package conversation

import (
	"context"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/stash"
)

// fakeLookup is a SymbolLookup that returns a fixed symbol set.
type fakeLookup struct {
	available bool
	stale     bool
	syms      []CompressorSymbol
}

func (f fakeLookup) Available() bool   { return f.available }
func (f fakeLookup) Stale(string) bool { return f.stale }
func (f fakeLookup) SymbolsInFile(context.Context, string) ([]CompressorSymbol, error) {
	return f.syms, nil
}

func newCollapsePipeline(t *testing.T, lookup SymbolLookup, exts map[string]bool, preview bool) *Pipeline {
	t.Helper()
	st, err := stash.New(stash.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("stash.New: %v", err)
	}
	return NewPipeline(PipelineConfig{}, NewRegistry(), nil).
		WithStash(st, 1).
		WithSymbolLookup(lookup).
		WithAggressiveCode(true, exts, preview, 0)
}

// TestBuildCodeCollapser_FiresOnExactSpan proves the wired collapser
// collapses an exact-span Go body, stashes the original, and leaves a
// retrieval marker.
func TestBuildCodeCollapser_FiresOnExactSpan(t *testing.T) {
	body, bigEnd, _ := collapseSampleBody()
	lookup := fakeLookup{available: true, syms: []CompressorSymbol{bigSymFor(bigEnd, true)}}
	p := newCollapsePipeline(t, lookup, map[string]bool{".go": true}, false)

	collapse := p.buildCodeCollapser(context.Background())
	if collapse == nil {
		t.Fatal("collapser nil despite enabled + stash + exts")
	}
	out, evs, fired := collapse("/proj/x.go", body, 0)
	if !fired {
		t.Fatal("collapser did not fire on an exact span")
	}
	if !strings.Contains(string(out), "mcp__observer__retrieve_stashed") {
		t.Errorf("no retrieval marker in collapsed body:\n%s", out)
	}
	if len(evs) != 1 || evs[0].Mechanism != "code_collapse" {
		t.Fatalf("events = %+v", evs)
	}
}

// TestBuildCodeCollapser_RefusesHeuristicSpan is the ADR-0005 wiring
// guard: a heuristic span (Exact=false) is never collapsed.
func TestBuildCodeCollapser_RefusesHeuristicSpan(t *testing.T) {
	body, bigEnd, _ := collapseSampleBody()
	lookup := fakeLookup{available: true, syms: []CompressorSymbol{bigSymFor(bigEnd, false)}}
	p := newCollapsePipeline(t, lookup, map[string]bool{".go": true}, false)

	_, _, fired := p.buildCodeCollapser(context.Background())("/proj/x.go", body, 0)
	if fired {
		t.Fatal("collapser fired on a heuristic span — ADR-0005 violation")
	}
}

// TestBuildCodeCollapser_StaleSkips proves a stale index degrades to
// content-preserving (no collapse).
func TestBuildCodeCollapser_StaleSkips(t *testing.T) {
	body, bigEnd, _ := collapseSampleBody()
	lookup := fakeLookup{available: true, stale: true, syms: []CompressorSymbol{bigSymFor(bigEnd, true)}}
	p := newCollapsePipeline(t, lookup, map[string]bool{".go": true}, false)

	if _, _, fired := p.buildCodeCollapser(context.Background())("/proj/x.go", body, 0); fired {
		t.Fatal("collapser fired on a stale index — must degrade to content-preserving")
	}
}

// TestBuildCodeCollapser_LanguageGate proves a file whose extension is
// not opted in is never collapsed.
func TestBuildCodeCollapser_LanguageGate(t *testing.T) {
	body, bigEnd, _ := collapseSampleBody()
	lookup := fakeLookup{available: true, syms: []CompressorSymbol{bigSymFor(bigEnd, true)}}
	p := newCollapsePipeline(t, lookup, map[string]bool{".go": true}, false)

	if _, _, fired := p.buildCodeCollapser(context.Background())("/proj/x.py", body, 0); fired {
		t.Fatal("collapser fired on a non-opted-in extension")
	}
}

// TestBuildCodeCollapser_PreviewOnly proves preview mode records events
// but does NOT change the body or stash anything.
func TestBuildCodeCollapser_PreviewOnly(t *testing.T) {
	body, bigEnd, _ := collapseSampleBody()
	lookup := fakeLookup{available: true, syms: []CompressorSymbol{bigSymFor(bigEnd, true)}}
	p := newCollapsePipeline(t, lookup, map[string]bool{".go": true}, true)

	out, evs, fired := p.buildCodeCollapser(context.Background())("/proj/x.go", body, 0)
	if fired {
		t.Fatal("preview mode must not fire (change the body)")
	}
	if string(out) != string(body) {
		t.Fatal("preview mode changed the body")
	}
	if len(evs) != 1 || evs[0].Mechanism != "code_collapse_preview" {
		t.Fatalf("preview events = %+v, want one code_collapse_preview", evs)
	}
}

// TestBuildCodeCollapser_FiresWithoutStash proves the decoupling: with no
// stash wired the collapser is still built and fires (so codex-safe
// traffic, which pins stash off, benefits from collapse), emitting the
// stash-less re-read marker instead of a retrieve_stashed pointer.
func TestBuildCodeCollapser_FiresWithoutStash(t *testing.T) {
	body, bigEnd, _ := collapseSampleBody()
	lookup := fakeLookup{available: true, syms: []CompressorSymbol{bigSymFor(bigEnd, true)}}
	p := NewPipeline(PipelineConfig{}, NewRegistry(), nil).
		WithSymbolLookup(lookup).
		WithAggressiveCode(true, map[string]bool{".go": true}, false, 0)

	collapse := p.buildCodeCollapser(context.Background())
	if collapse == nil {
		t.Fatal("collapser must be wired even without a stash (decoupled)")
	}
	out, evs, fired := collapse("/proj/x.go", body, 0)
	if !fired {
		t.Fatal("collapser did not fire without a stash")
	}
	if !strings.Contains(string(out), "re-read this file") {
		t.Errorf("expected the stash-less re-read marker:\n%s", out)
	}
	if strings.Contains(string(out), "retrieve_stashed") {
		t.Error("stash-less collapse must not reference retrieve_stashed")
	}
	if len(evs) != 1 || evs[0].Mechanism != "code_collapse" {
		t.Fatalf("events = %+v", evs)
	}
}
