package store

import (
	"testing"

	"github.com/marmutapp/superbased-observer/internal/codeintel"
)

// TestContainmentParents_ByteSpans covers the exact-backend path: a class
// whose byte span encloses two methods, with one method nested deeper.
func TestContainmentParents_ByteSpans(t *testing.T) {
	nodes := []codeintel.Node{
		{Name: "Outer", Kind: "class", StartByte: 0, EndByte: 200},     // 0
		{Name: "m1", Kind: "method", StartByte: 10, EndByte: 60},       // 1 -> Outer
		{Name: "m2", Kind: "method", StartByte: 70, EndByte: 190},      // 2 -> Outer
		{Name: "inner", Kind: "function", StartByte: 90, EndByte: 150}, // 3 -> m2 (smallest container)
	}
	parents := containmentParents(nodes)
	if parents[0] != -1 {
		t.Errorf("Outer parent = %d, want -1", parents[0])
	}
	if parents[1] != 0 || parents[2] != 0 {
		t.Errorf("m1/m2 parents = %d/%d, want 0/0", parents[1], parents[2])
	}
	if parents[3] != 2 {
		t.Errorf("inner parent = %d, want 2 (m2, the smallest container)", parents[3])
	}
}

// TestContainmentParents_LineSpans covers the heuristic-backend path
// (no byte ends; line spans only) and equal-span non-nesting.
func TestContainmentParents_LineSpans(t *testing.T) {
	nodes := []codeintel.Node{
		{Name: "Mod", Kind: "module", StartLine: 1, EndLine: 100},
		{Name: "C", Kind: "class", StartLine: 10, EndLine: 50},
		{Name: "meth", Kind: "method", StartLine: 20, EndLine: 30},
		{Name: "dup", Kind: "type", StartLine: 10, EndLine: 50}, // equal span to C — must NOT nest under C
	}
	parents := containmentParents(nodes)
	if parents[0] != -1 {
		t.Errorf("Mod parent = %d, want -1", parents[0])
	}
	if parents[1] != 0 {
		t.Errorf("C parent = %d, want 0 (Mod)", parents[1])
	}
	if parents[2] != 1 {
		t.Errorf("meth parent = %d, want 1 (C)", parents[2])
	}
	// dup shares C's span; strict containment forbids equal spans, so its
	// parent is the next-larger container (Mod), never C.
	if parents[3] != 0 {
		t.Errorf("dup parent = %d, want 0 (Mod, not the equal-span C)", parents[3])
	}
}

// TestStrictlyContains_NoEndInfo: a node without a usable end span never
// contains or is contained.
func TestStrictlyContains_NoEndInfo(t *testing.T) {
	outer := codeintel.Node{StartLine: 1, EndLine: 0} // EndLine 0 = unknown
	inner := codeintel.Node{StartLine: 5, EndLine: 6}
	if ok, _ := strictlyContains(outer, inner); ok {
		t.Error("a node with no end span must not contain another")
	}
}
