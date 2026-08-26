package store

import (
	"context"
	"testing"
)

// TestSelectCodeintelDevRows_Aggregates proves the W2.4 per-developer wire
// row groups files/symbols/edges by (project, lang) exactly like
// SelectCodeintelSummaries, plus carries MAX(indexed_at) as LastIndexed.
// Seeded via direct SQL against the migration-050/051 schema (no
// codeintel.FileResult plumbing needed for a pure count/aggregate test).
func TestSelectCodeintelDevRows_Aggregates(t *testing.T) {
	t.Parallel()
	s, db := newTestStore(t)
	ctx := context.Background()

	const project = "/home/dev/repo-a"

	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed exec %q: %v", q, err)
		}
	}

	// Two Go files (one with an edge), one TS file — three files total,
	// two languages.
	mustExec(`INSERT INTO codeintel_files (id, project, path, lang, indexed_at, status) VALUES
		(1, ?, ?, 'go', 1000, 'indexed'),
		(2, ?, ?, 'go', 2000, 'indexed'),
		(3, ?, ?, 'ts', 500, 'indexed')`,
		project, "/home/dev/repo-a/a.go",
		project, "/home/dev/repo-a/b.go",
		project, "/home/dev/repo-a/c.ts")

	mustExec(`INSERT INTO codeintel_nodes (id, project, file_id, kind, name, lang) VALUES
		(1, ?, 1, 'function', 'Foo', 'go'),
		(2, ?, 2, 'function', 'Bar', 'go'),
		(3, ?, 3, 'function', 'baz', 'ts')`,
		project, project, project)

	mustExec(`INSERT INTO codeintel_edges (id, project, file_id, kind) VALUES
		(1, ?, 1, 'CALLS')`,
		project)

	got, err := s.SelectCodeintelDevRows(ctx)
	if err != nil {
		t.Fatalf("SelectCodeintelDevRows: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2 buckets (go + ts), got %+v", len(got), got)
	}

	wantHash := hashCodeintelProject(project)
	var goRow, tsRow *codeintelDevRowRef
	for i := range got {
		r := got[i]
		if r.ProjectHash != wantHash {
			t.Errorf("row %d ProjectHash = %q, want %q (stable join key)", i, r.ProjectHash, wantHash)
		}
		// Enterprise ruling (plan §0.1, 2026-08-24): the RAW git-root path
		// rides alongside the hash on this shipsRawContent()-gated wire.
		if r.ProjectRoot != project {
			t.Errorf("row %d ProjectRoot = %q, want %q (raw path must be carried)", i, r.ProjectRoot, project)
		}
		switch r.Language {
		case "go":
			goRow = &codeintelDevRowRef{Files: r.Files, Symbols: r.Symbols, Edges: r.Edges, LastIndexed: r.LastIndexed}
		case "ts":
			tsRow = &codeintelDevRowRef{Files: r.Files, Symbols: r.Symbols, Edges: r.Edges, LastIndexed: r.LastIndexed}
		}
	}
	if goRow == nil || tsRow == nil {
		t.Fatalf("missing expected language buckets: %+v", got)
	}

	if goRow.Files != 2 || goRow.Symbols != 2 || goRow.Edges != 1 {
		t.Errorf("go bucket = files:%d symbols:%d edges:%d, want 2/2/1", goRow.Files, goRow.Symbols, goRow.Edges)
	}
	if goRow.LastIndexed != 2000 {
		t.Errorf("go bucket LastIndexed = %d, want 2000 (MAX of 1000,2000)", goRow.LastIndexed)
	}
	if tsRow.Files != 1 || tsRow.Symbols != 1 || tsRow.Edges != 0 {
		t.Errorf("ts bucket = files:%d symbols:%d edges:%d, want 1/1/0", tsRow.Files, tsRow.Symbols, tsRow.Edges)
	}
	if tsRow.LastIndexed != 500 {
		t.Errorf("ts bucket LastIndexed = %d, want 500", tsRow.LastIndexed)
	}
}

// codeintelDevRowRef is a small local projection used to compare bucket
// fields without importing orgcontract twice for one test file.
type codeintelDevRowRef struct {
	Files, Symbols, Edges, LastIndexed int64
}

// TestSelectCodeintelDevRows_FanOutCorrectedCounts is the coverage
// TestSelectCodeintelDevRows_Aggregates above cannot provide: its fixture
// gives every file at most ONE node and ONE edge, and 1x1 == 1, so the
// Cartesian spelling that a8d802421 removed and the pre-aggregated spelling
// that replaced it return identical numbers on it. Only a file with multiple
// nodes AND multiple edges separates them. Shares the fixture (and the
// arithmetic truth table) with the summary sibling so both reads are pinned
// to the same expectations.
func TestSelectCodeintelDevRows_FanOutCorrectedCounts(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	repoA, repoB := seedCodeintelFanout(t, s)

	got, err := s.SelectCodeintelDevRows(ctx)
	if err != nil {
		t.Fatalf("SelectCodeintelDevRows: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3 buckets; got %+v", len(got), got)
	}

	want := wantCodeintelBuckets(repoA, repoB)
	for _, r := range got {
		// The per-dev wire carries the RAW project root, so key on it.
		key := r.ProjectRoot + "|" + r.Language
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

// TestSelectCodeintelDevRows_Empty proves a project with no indexed files
// yields no rows at all (no phantom zero-bucket).
func TestSelectCodeintelDevRows_Empty(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()

	got, err := s.SelectCodeintelDevRows(ctx)
	if err != nil {
		t.Fatalf("SelectCodeintelDevRows: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no rows for an empty index, got %+v", got)
	}
}
