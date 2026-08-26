package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// seedCodeintelFanout writes a fixture whose whole point is MULTI-node AND
// MULTI-edge files. A fixture with at most one node and one edge per file
// (which is what codeinteldevorgrows_test.go originally had) cannot tell the
// Cartesian shape from the pre-aggregated one: 1x1 == 1, so both spellings
// agree. Only nodes_f x edges_f > nodes_f + edges_f exposes a fan-out bug.
//
// Layout, per (project, lang) bucket:
//
//	/home/dev/repo-a  go : a.go (4 nodes, 3 edges)  -> 4x3 = 12 product rows
//	                       b.go (2 nodes, 0 edges)
//	                       c.go (0 nodes, 5 edges)
//	                  ts : d.ts (3 nodes, 2 edges)  -> 3x2 = 6 product rows
//	/home/dev/repo-b  go : e.go (1 node,  1 edge)
//
// Expected: repo-a/go = 3 files, 6 symbols, 8 edges; repo-a/ts = 1/3/2;
// repo-b/go = 1/1/1. Under a Cartesian COUNT() (no DISTINCT) a.go alone
// would report 12 of each.
func seedCodeintelFanout(t *testing.T, s *Store) (repoA, repoB string) {
	t.Helper()
	ctx := context.Background()
	repoA, repoB = "/home/dev/repo-a", "/home/dev/repo-b"

	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seedCodeintelFanout exec %q: %v", q, err)
		}
	}
	addFile := func(id int, project, path, lang string, indexedAt int, nodes, edges int) {
		t.Helper()
		mustExec(`INSERT INTO codeintel_files (id, project, path, lang, indexed_at, status)
			VALUES (?, ?, ?, ?, ?, 'indexed')`, id, project, path, lang, indexedAt)
		for i := 0; i < nodes; i++ {
			mustExec(`INSERT INTO codeintel_nodes (project, file_id, kind, name, lang)
				VALUES (?, ?, 'function', ?, ?)`, project, id, fmt.Sprintf("sym%d_%d", id, i), lang)
		}
		for i := 0; i < edges; i++ {
			mustExec(`INSERT INTO codeintel_edges (project, file_id, kind)
				VALUES (?, ?, 'CALLS')`, project, id)
		}
	}

	addFile(1, repoA, "/home/dev/repo-a/a.go", "go", 1000, 4, 3)
	addFile(2, repoA, "/home/dev/repo-a/b.go", "go", 2000, 2, 0)
	addFile(3, repoA, "/home/dev/repo-a/c.go", "go", 1500, 0, 5)
	addFile(4, repoA, "/home/dev/repo-a/d.ts", "ts", 500, 3, 2)
	addFile(5, repoB, "/home/dev/repo-b/e.go", "go", 900, 1, 1)
	return repoA, repoB
}

// codeintelBucket is the (files, symbols, edges) triple both org reads
// produce per (project, lang).
type codeintelBucket struct{ Files, Symbols, Edges int64 }

// wantCodeintelBuckets is the arithmetic truth for seedCodeintelFanout,
// shared by the summary and dev-row tests so the two reads are pinned to the
// SAME numbers — they are documented as producing identical counts, and a
// divergence between them is exactly the regression worth catching.
func wantCodeintelBuckets(repoA, repoB string) map[string]codeintelBucket {
	return map[string]codeintelBucket{
		repoA + "|go": {Files: 3, Symbols: 6, Edges: 8},
		repoA + "|ts": {Files: 1, Symbols: 3, Edges: 2},
		repoB + "|go": {Files: 1, Symbols: 1, Edges: 1},
	}
}

// TestSelectCodeintelSummaries_FanOutCorrectedCounts pins the counts across
// multi-node AND multi-edge files. Before the 2026-08-26 (F4) rewrite this
// query built a files x nodes x edges product and leaned on COUNT(DISTINCT)
// to undo it; the rewrite pre-aggregates each child by file_id instead. The
// numbers below must be identical under either spelling — that is the point
// of the change (cost, not output), so this test is what proves the rewrite
// did not quietly alter the wire.
func TestSelectCodeintelSummaries_FanOutCorrectedCounts(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	repoA, repoB := seedCodeintelFanout(t, s)

	got, err := s.SelectCodeintelSummaries(ctx)
	if err != nil {
		t.Fatalf("SelectCodeintelSummaries: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3 buckets (repo-a/go, repo-a/ts, repo-b/go); got %+v", len(got), got)
	}

	// The wire carries only the HASH, so index expectations by hash.
	want := map[string]codeintelBucket{}
	for k, v := range wantCodeintelBuckets(repoA, repoB) {
		parts := strings.SplitN(k, "|", 2)
		want[hashCodeintelProject(parts[0])+"|"+parts[1]] = v
	}

	for _, r := range got {
		key := r.ProjectHash + "|" + r.Lang
		w, ok := want[key]
		if !ok {
			t.Errorf("unexpected bucket %q (%+v)", key, r)
			continue
		}
		if r.Files != w.Files || r.Symbols != w.Symbols || r.Edges != w.Edges {
			t.Errorf("bucket %s: files/symbols/edges = %d/%d/%d, want %d/%d/%d",
				key, r.Files, r.Symbols, r.Edges, w.Files, w.Symbols, w.Edges)
		}
		delete(want, key)
	}
	for key := range want {
		t.Errorf("missing bucket %s", key)
	}
}

// TestSelectCodeintelSummaries_NeverShipsRawProject pins the privacy posture
// this file's header claims: the grouping key is a one-way domain-separated
// hash, and the raw git-root path never appears on the summary wire. (The
// per-dev row DOES carry the raw path — that is a different, explicitly
// shipsRawContent()-gated wire — so the distinction is worth a test.)
func TestSelectCodeintelSummaries_NeverShipsRawProject(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	repoA, _ := seedCodeintelFanout(t, s)

	got, err := s.SelectCodeintelSummaries(ctx)
	if err != nil {
		t.Fatalf("SelectCodeintelSummaries: %v", err)
	}
	for _, r := range got {
		if strings.Contains(r.ProjectHash, repoA) || r.ProjectHash == repoA {
			t.Errorf("ProjectHash %q leaks the raw project path", r.ProjectHash)
		}
		if r.ProjectHash == "" {
			t.Errorf("empty ProjectHash for a seeded project (%+v)", r)
		}
	}
}

// TestSelectCodeintelSummaries_Empty proves an empty index yields no rows at
// all (no phantom zero bucket), matching the dev-row sibling.
func TestSelectCodeintelSummaries_Empty(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	got, err := s.SelectCodeintelSummaries(context.Background())
	if err != nil {
		t.Fatalf("SelectCodeintelSummaries: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no rows for an empty index, got %+v", got)
	}
}

// TestSelectCodeintelSummaries_MatchesDevRows pins the two reads against each
// other. codeinteldevorgrows.go documents its aggregation as "a superset of
// SelectCodeintelSummaries: same COUNT(DISTINCT ...) triple-join"; now that
// both have been rewritten, this asserts the shared half still agrees rather
// than trusting the comment.
func TestSelectCodeintelSummaries_MatchesDevRows(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	seedCodeintelFanout(t, s)

	summaries, err := s.SelectCodeintelSummaries(ctx)
	if err != nil {
		t.Fatalf("SelectCodeintelSummaries: %v", err)
	}
	devRows, err := s.SelectCodeintelDevRows(ctx)
	if err != nil {
		t.Fatalf("SelectCodeintelDevRows: %v", err)
	}
	if len(summaries) != len(devRows) {
		t.Fatalf("bucket count differs: summaries=%d devRows=%d", len(summaries), len(devRows))
	}

	dev := map[string]codeintelBucket{}
	for _, r := range devRows {
		dev[r.ProjectHash+"|"+r.Language] = codeintelBucket{Files: r.Files, Symbols: r.Symbols, Edges: r.Edges}
	}
	for _, r := range summaries {
		key := r.ProjectHash + "|" + r.Lang
		d, ok := dev[key]
		if !ok {
			t.Errorf("bucket %s present in summaries but not dev rows", key)
			continue
		}
		if d.Files != r.Files || d.Symbols != r.Symbols || d.Edges != r.Edges {
			t.Errorf("bucket %s diverges: summary %d/%d/%d vs dev %d/%d/%d",
				key, r.Files, r.Symbols, r.Edges, d.Files, d.Symbols, d.Edges)
		}
	}
}

// TestSelectCodeintelSummaries_QueryPlan pins the PLAN. The F4 rewrite does
// not change a single returned number — its entire value is the shape of the
// join — so no correctness test can detect a regression back to the
// Cartesian spelling. This can.
//
// Asserting "no MATERIALIZE" would be wrong here: the fixed query
// deliberately materializes the two per-file child aggregates (O(files) rows
// each). The marker that actually separates the two shapes is
// count(DISTINCT) — present only when a fan-out is being masked after the
// fact.
func TestSelectCodeintelSummaries_QueryPlan(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	seedCodeintelFanout(t, s)

	rows, err := s.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+codeintelSummaryQuery)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	joined := strings.Join(plan, "\n")

	// THE marker of the Cartesian shape: a distinct-count undoing a fan-out
	// the join should never have produced.
	if strings.Contains(joined, "count(DISTINCT)") {
		t.Errorf("plan uses count(DISTINCT) — the Cartesian shape is back\nplan:\n%s", joined)
	}
	// Both children must be pre-aggregated before reaching the parent join.
	for _, want := range []string{"MATERIALIZE nc", "MATERIALIZE ec"} {
		if !strings.Contains(joined, want) {
			t.Errorf("plan is missing %q — a child is not pre-aggregated by file_id\nplan:\n%s", want, joined)
		}
	}
	// Each pre-aggregate must ride a covering index, never a bare table scan
	// (these are the widest tables in the node-local index).
	for _, want := range []string{
		"COVERING INDEX idx_codeintel_nodes_file",
		"COVERING INDEX idx_codeintel_edges_file",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("plan does not use %s\nplan:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "SCAN codeintel_nodes\n") || strings.HasSuffix(joined, "SCAN codeintel_nodes") {
		t.Errorf("plan does a bare (non-covering) scan of codeintel_nodes\nplan:\n%s", joined)
	}

	t.Logf("query plan:\n%s", joined)
}
