package store

import (
	"context"
	"testing"
)

// TestCodeIntelDeleteProject_AllTablesAndIsolation verifies the explicit
// per-table delete removes EVERY codeintel_* row for the target project
// (including codeintel_fts, which has no FK and the old cascade-only delete
// leaked) while leaving other projects untouched, and that FK enforcement is
// restored on the pinned connection afterward.
func TestCodeIntelDeleteProject_AllTablesAndIsolation(t *testing.T) {
	s, database := newTestStore(t)
	// Pin to one connection so the delete's foreign_keys toggle and the
	// follow-up FK-restored assertion exercise the SAME connection.
	database.SetMaxOpenConns(1)
	ctx := context.Background()

	tables := []string{
		"codeintel_files", "codeintel_nodes", "codeintel_edges",
		"codeintel_sites", "codeintel_minhash", "codeintel_embeddings",
		"codeintel_fts",
	}

	seed := func(project string) {
		t.Helper()
		rf, err := database.ExecContext(ctx,
			`INSERT INTO codeintel_files(project, path, lang, status) VALUES(?, ?, 'go', 'indexed')`,
			project, project+"/a.go")
		if err != nil {
			t.Fatalf("seed file: %v", err)
		}
		fid, _ := rf.LastInsertId()
		rn, err := database.ExecContext(ctx,
			`INSERT INTO codeintel_nodes(project, file_id, kind, name) VALUES(?, ?, 'function', 'Foo')`,
			project, fid)
		if err != nil {
			t.Fatalf("seed node: %v", err)
		}
		nid, _ := rn.LastInsertId()
		re, err := database.ExecContext(ctx,
			`INSERT INTO codeintel_edges(project, file_id, src_id, dst_id, kind) VALUES(?, ?, ?, ?, 'CALLS')`,
			project, fid, nid, nid)
		if err != nil {
			t.Fatalf("seed edge: %v", err)
		}
		eid, _ := re.LastInsertId()
		if _, err := database.ExecContext(ctx,
			`INSERT INTO codeintel_sites(project, edge_id, file_id, target_name) VALUES(?, ?, ?, 'Foo')`,
			project, eid, fid); err != nil {
			t.Fatalf("seed site: %v", err)
		}
		if _, err := database.ExecContext(ctx,
			`INSERT INTO codeintel_minhash(node_id, project, band, hash) VALUES(?, ?, 0, 123)`,
			nid, project); err != nil {
			t.Fatalf("seed minhash: %v", err)
		}
		if _, err := database.ExecContext(ctx,
			`INSERT INTO codeintel_embeddings(node_id, project, dim, vec) VALUES(?, ?, 8, x'0000000000000000')`,
			nid, project); err != nil {
			t.Fatalf("seed embedding: %v", err)
		}
		if _, err := database.ExecContext(ctx,
			`INSERT INTO codeintel_fts(tokens, node_id, project, name) VALUES('foo', ?, ?, 'Foo')`,
			nid, project); err != nil {
			t.Fatalf("seed fts: %v", err)
		}
	}

	seed("/p/keep")
	seed("/p/drop")

	if err := s.CodeIntelDeleteProject(ctx, "/p/drop"); err != nil {
		t.Fatalf("CodeIntelDeleteProject: %v", err)
	}

	count := func(tbl, project string) int {
		var n int
		if err := database.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM "+tbl+" WHERE project = ?", project).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		return n
	}
	for _, tbl := range tables {
		if n := count(tbl, "/p/drop"); n != 0 {
			t.Errorf("%s: dropped project still has %d row(s)", tbl, n)
		}
		if n := count(tbl, "/p/keep"); n != 1 {
			t.Errorf("%s: kept project has %d row(s), want 1 (delete bled across projects)", tbl, n)
		}
	}

	// FK enforcement must be back on the (reused) connection: inserting a
	// node referencing a non-existent file_id must be rejected.
	if _, err := database.ExecContext(ctx,
		`INSERT INTO codeintel_nodes(project, file_id, name) VALUES('/p/x', 999999, 'Orphan')`); err == nil {
		t.Error("foreign_keys not restored after delete — orphan node insert succeeded")
	}
}
