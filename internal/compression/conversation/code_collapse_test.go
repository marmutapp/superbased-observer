package conversation

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// collapseSample builds a Go-ish source with a LARGE Big() body (so the
// verbose retrieval marker is a net win) and a tiny Small(). Big spans
// lines 3..bigEnd; Small is the last line.
func collapseSampleBody() (body []byte, bigEnd, smallLine int) {
	var b strings.Builder
	b.WriteString("package x\n\n")
	b.WriteString("func Big() {\n") // line 3
	for i := range 30 {
		fmt.Fprintf(&b, "\tstatement%d := computeSomething(argumentOne, argumentTwo, %d)\n", i, i)
	}
	b.WriteString("}\n") // closes Big
	bigEnd = 3 + 30 + 1  // decl + 30 body lines + closing brace
	b.WriteString("\n")
	b.WriteString("func Small() { return }\n")
	smallLine = bigEnd + 2
	return []byte(b.String()), bigEnd, smallLine
}

func bigSymFor(bigEnd int, exact bool) CompressorSymbol {
	return CompressorSymbol{Name: "Big", Kind: "function", StartLine: 3, EndLine: bigEnd, Exact: exact}
}

func bigSym(exact bool) CompressorSymbol {
	_, bigEnd, _ := collapseSampleBody()
	return bigSymFor(bigEnd, exact)
}

var collapseSample = func() string { b, _, _ := collapseSampleBody(); return string(b) }()

func TestPlanCodeCollapse_OnlyExactSpans(t *testing.T) {
	body, bigEnd, _ := collapseSampleBody()

	// ADR-0005: a heuristic (Exact=false) span must NEVER be planned.
	if plans := planCodeCollapse(body, []CompressorSymbol{bigSymFor(bigEnd, false)}, collapseOptions{}); len(plans) != 0 {
		t.Fatalf("heuristic span was planned for collapse — ADR-0005 violation: %+v", plans)
	}

	// An exact span IS planned.
	plans := planCodeCollapse(body, []CompressorSymbol{bigSymFor(bigEnd, true)}, collapseOptions{})
	if len(plans) != 1 {
		t.Fatalf("exact span: want 1 plan, got %d", len(plans))
	}
	if plans[0].StartLine != 3 || plans[0].EndLine != bigEnd {
		t.Fatalf("plan span = %d-%d, want 3-%d", plans[0].StartLine, plans[0].EndLine, bigEnd)
	}
	if !strings.Contains(plans[0].DeclLine, "func Big()") {
		t.Fatalf("decl line = %q, want the func declaration", plans[0].DeclLine)
	}
}

func TestPlanCodeCollapse_MinLines(t *testing.T) {
	body, bigEnd, smallLine := collapseSampleBody()
	// Big is ~32 lines; a min of 100 excludes it.
	if plans := planCodeCollapse(body, []CompressorSymbol{bigSymFor(bigEnd, true)}, collapseOptions{MinLines: 100}); len(plans) != 0 {
		t.Fatalf("span below MinLines was planned: %+v", plans)
	}
	// A single-line symbol (EndLine == StartLine) is never planned.
	single := CompressorSymbol{Name: "Small", Kind: "function", StartLine: smallLine, EndLine: smallLine, Exact: true}
	if plans := planCodeCollapse(body, []CompressorSymbol{single}, collapseOptions{MinLines: 1}); len(plans) != 0 {
		t.Fatalf("single-line span was planned: %+v", plans)
	}
}

func TestPlanCodeCollapse_NonOverlapping(t *testing.T) {
	body, bigEnd, _ := collapseSampleBody()
	outer := bigSymFor(bigEnd, true)
	nested := CompressorSymbol{Name: "inner", Kind: "function", StartLine: 6, EndLine: 12, Exact: true}
	plans := planCodeCollapse(body, []CompressorSymbol{outer, nested}, collapseOptions{MinLines: 3})
	if len(plans) != 1 || plans[0].StartLine != 3 {
		t.Fatalf("expected only the outermost span, got %+v", plans)
	}
}

func TestApplyCodeCollapse_RoundTripAndDeterministic(t *testing.T) {
	body := []byte(collapseSample)
	plans := planCodeCollapse(body, []CompressorSymbol{bigSym(true)}, collapseOptions{})
	if len(plans) != 1 {
		t.Fatalf("setup: want 1 plan, got %d", len(plans))
	}

	var stashed [][]byte
	write := func(b []byte) (string, error) {
		stashed = append(stashed, append([]byte(nil), b...))
		// deterministic fake sha from content length so the marker is stable
		return "deadbeefcafe", nil
	}

	out, events, fired := applyCodeCollapse(body, plans, 0, write)
	if !fired {
		t.Fatal("collapse did not fire")
	}
	if len(out) >= len(body) {
		t.Fatalf("collapsed body (%d) not smaller than original (%d)", len(out), len(body))
	}
	s := string(out)
	if !strings.Contains(s, "func Big() {") {
		t.Error("decl line dropped — the model loses the signature")
	}
	if !strings.Contains(s, "mcp__observer__retrieve_stashed") || !strings.Contains(s, "deadbeefcafe") {
		t.Errorf("collapse marker missing/!directive:\n%s", s)
	}
	if !strings.Contains(s, "func Small()") {
		t.Error("untouched sibling symbol was lost")
	}
	if strings.Contains(s, "statement29 :=") {
		t.Error("collapsed body interior leaked into output")
	}
	// The stashed original must be the full span (decl through close).
	if len(stashed) != 1 || !strings.Contains(string(stashed[0]), "func Big()") || !strings.Contains(string(stashed[0]), "statement29 :=") {
		t.Fatalf("stashed original is not the full span: %q", stashed)
	}
	if len(events) != 1 || events[0].Mechanism != "code_collapse" {
		t.Fatalf("events = %+v, want one code_collapse", events)
	}

	// Determinism: same inputs -> byte-identical output (prefix cache).
	out2, _, _ := applyCodeCollapse(body, planCodeCollapse(body, []CompressorSymbol{bigSym(true)}, collapseOptions{}), 0, write)
	if !reflect.DeepEqual(out, out2) {
		t.Error("collapse output is not deterministic across runs")
	}
}

// TestApplyCodeCollapse_NoStashMarker covers the decoupled-from-stash
// path: when the writer returns an empty sha (no stash wired, e.g. the
// codex-safe profile), the collapse still fires but emits the re-read
// marker instead of a dangling retrieve_stashed pointer.
func TestApplyCodeCollapse_NoStashMarker(t *testing.T) {
	body := []byte(collapseSample)
	plans := planCodeCollapse(body, []CompressorSymbol{bigSym(true)}, collapseOptions{})
	if len(plans) != 1 {
		t.Fatalf("setup: want 1 plan, got %d", len(plans))
	}
	// No-op writer: stash-less collapse returns an empty sha, no error.
	noStash := func([]byte) (string, error) { return "", nil }

	out, events, fired := applyCodeCollapse(body, plans, 0, noStash)
	if !fired {
		t.Fatal("collapse did not fire without a stash")
	}
	s := string(out)
	if len(out) >= len(body) {
		t.Fatalf("collapsed body (%d) not smaller than original (%d)", len(out), len(body))
	}
	if !strings.Contains(s, "func Big() {") {
		t.Error("decl line dropped — the model loses the signature")
	}
	if !strings.Contains(s, "re-read this file") {
		t.Errorf("expected the stash-less re-read marker:\n%s", s)
	}
	if strings.Contains(s, "retrieve_stashed") || strings.Contains(s, "observer://stash/") {
		t.Errorf("stash-less marker must NOT reference the stash:\n%s", s)
	}
	if len(events) != 1 || events[0].Mechanism != "code_collapse" {
		t.Fatalf("events = %+v, want one code_collapse", events)
	}
	// Determinism: byte-identical across runs (prefix cache safety).
	out2, _, _ := applyCodeCollapse(body, plans, 0, noStash)
	if !reflect.DeepEqual(out, out2) {
		t.Error("stash-less collapse output is not deterministic")
	}
}

func TestPlanCodeCollapse_EmptyInputs(t *testing.T) {
	if plans := planCodeCollapse(nil, []CompressorSymbol{bigSym(true)}, collapseOptions{}); plans != nil {
		t.Error("nil body should yield no plans")
	}
	if plans := planCodeCollapse([]byte(collapseSample), nil, collapseOptions{}); plans != nil {
		t.Error("no symbols should yield no plans")
	}
}
