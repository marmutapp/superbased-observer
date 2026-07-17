package query

import (
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/codeintel"
)

// sampleGraph: Run -CALLS-> Helper -CALLS-> Format ; Editor CONTAINS render.
func sampleGraph() codeintel.Graph {
	return codeintel.Graph{
		Project: "p",
		Nodes: []codeintel.GraphNode{
			{ID: 1, Name: "Run", FQN: "Run", Kind: "function", File: "a.go", StartLine: 10},
			{ID: 2, Name: "Helper", FQN: "Helper", Kind: "function", File: "b.go", StartLine: 20},
			{ID: 3, Name: "Format", FQN: "Format", Kind: "function", File: "b.go", StartLine: 40},
			{ID: 4, Name: "Editor", FQN: "Editor", Kind: "class", File: "c.go", StartLine: 5},
			{ID: 5, Name: "render", FQN: "Editor.render", Kind: "method", File: "c.go", StartLine: 8},
		},
		Edges: []codeintel.GraphEdge{
			{Src: 1, Dst: 2, Kind: "CALLS", Confidence: 1},
			{Src: 2, Dst: 3, Kind: "CALLS", Confidence: 1},
			{Src: 4, Dst: 5, Kind: "CONTAINS", Confidence: 1},
		},
	}
}

func run(t *testing.T, q string) codeintel.ResultSet {
	t.Helper()
	rs, err := Run(sampleGraph(), q)
	if err != nil {
		t.Fatalf("Run(%q): %v", q, err)
	}
	return rs
}

func TestQuery_ForwardCalls(t *testing.T) {
	t.Parallel()
	rs := run(t, `MATCH (a)-[:CALLS]->(b) WHERE a.name = "Run" RETURN b.name`)
	if got := flatten(rs); !contains(got, "Helper") || len(got) != 1 {
		t.Errorf("got %v, want [Helper]", got)
	}
}

func TestQuery_BackwardCalls(t *testing.T) {
	t.Parallel()
	rs := run(t, `MATCH (a)<-[:CALLS]-(b) WHERE a.name = "Helper" RETURN b.name`)
	if got := flatten(rs); !contains(got, "Run") {
		t.Errorf("callers of Helper = %v, want Run", got)
	}
}

func TestQuery_TwoHop(t *testing.T) {
	t.Parallel()
	rs := run(t, `MATCH (a)-[:CALLS]->(b)-[:CALLS]->(c) WHERE a.name = "Run" RETURN c.name`)
	if got := flatten(rs); !contains(got, "Format") {
		t.Errorf("2-hop from Run = %v, want Format", got)
	}
}

func TestQuery_LabelAndInlineProps(t *testing.T) {
	t.Parallel()
	rs := run(t, `MATCH (a:function {name:"Helper"}) RETURN a.file, a.start_line`)
	if len(rs.Rows) != 1 || rs.Rows[0][0] != "b.go" || rs.Rows[0][1] != "20" {
		t.Errorf("rows = %v", rs.Rows)
	}
	if rs.Columns[0] != "a.file" || rs.Columns[1] != "a.start_line" {
		t.Errorf("cols = %v", rs.Columns)
	}
}

func TestQuery_WhereContainsAndNeqAndAlias(t *testing.T) {
	t.Parallel()
	rs := run(t, `MATCH (a) WHERE a.fqn CONTAINS "Editor" AND a.kind <> "class" RETURN a.name AS sym`)
	got := flatten(rs)
	if !contains(got, "render") || contains(got, "Editor") {
		t.Errorf("got %v, want only render (method under Editor, class excluded)", got)
	}
	if rs.Columns[0] != "sym" {
		t.Errorf("alias column = %v", rs.Columns)
	}
}

func TestQuery_UndirectedAndLimit(t *testing.T) {
	t.Parallel()
	rs := run(t, `MATCH (a)-[:CALLS]-(b) WHERE a.name = "Helper" RETURN b.name`)
	got := flatten(rs)
	// Undirected: Helper touches both Run (caller) and Format (callee).
	if !contains(got, "Run") || !contains(got, "Format") {
		t.Errorf("undirected neighbours of Helper = %v, want Run+Format", got)
	}
	rs2 := run(t, `MATCH (a)-[:CALLS]-(b) WHERE a.name = "Helper" RETURN b.name LIMIT 1`)
	if len(rs2.Rows) != 1 {
		t.Errorf("LIMIT 1 should cap rows, got %d", len(rs2.Rows))
	}
}

func TestQuery_AnyRelType(t *testing.T) {
	t.Parallel()
	rs := run(t, `MATCH (a)-[]->(b) WHERE a.name = "Editor" RETURN b.name`)
	if got := flatten(rs); !contains(got, "render") {
		t.Errorf("any-rel from Editor = %v, want render (CONTAINS)", got)
	}
}

func TestQuery_Errors(t *testing.T) {
	t.Parallel()
	bad := []string{
		``,                                      // empty
		`RETURN a`,                              // no MATCH
		`MATCH (a) RETURN`,                      // empty return
		`MATCH (a) WHERE a.name ~ "x" RETURN a`, // unsupported operator
		`MATCH (a) RETURN a DROP TABLE`,         // trailing junk
		`MATCH (a)-[:CALLS]->(b) DELETE b`,      // unsupported clause
		`MATCH (a) WHERE a.name = unquoted RETURN a`, // unquoted value
	}
	for _, q := range bad {
		if _, err := Run(sampleGraph(), q); err == nil {
			t.Errorf("expected error for %q", q)
		}
	}
}

func flatten(rs codeintel.ResultSet) []string {
	var out []string
	for _, row := range rs.Rows {
		out = append(out, row...)
	}
	return out
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if strings.EqualFold(x, want) {
			return true
		}
	}
	return false
}
