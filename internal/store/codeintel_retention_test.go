package store

import (
	"context"
	"testing"
	"time"
)

// seedCodeIntelProject inserts one file (with dependent node/edge/site/
// minhash/embedding/fts rows) for project, stamping the file's indexed_at
// (unix seconds; 0 = never successfully indexed).
func seedCodeIntelProject(ctx context.Context, t *testing.T, s *Store, project string, indexedAt int64) {
	t.Helper()
	database := s.db
	rf, err := database.ExecContext(ctx,
		`INSERT INTO codeintel_files(project, path, lang, status, indexed_at)
		 VALUES(?, ?, 'go', 'indexed', ?)`,
		project, project+"/a.go", indexedAt)
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

// TestCodeIntelPruneStaleProjects is the Ticket-B codeintel retention
// sweep test (docs/plans/claude-code-hook-stall-ticket-and-db-prune-plan-
// 2026-07-12.md): projects whose last index pass is past the horizon are
// deleted wholesale (all codeintel_* tables); fresh, never-indexed, and
// boundary projects survive; ≤ 0 disables; a second run is a no-op.
func TestCodeIntelPruneStaleProjects(t *testing.T) {
	now := time.Now().Unix()
	const day = int64(24 * 60 * 60)

	// One seed catalog shared by every case: project → indexed_at.
	catalog := map[string]int64{
		"/p/stale":   now - 200*day, // past any tested horizon
		"/p/fresh":   now - 5*day,   // recently indexed
		"/p/border":  now - 80*day,  // inside a 90d horizon, outside 60d
		"/p/pending": 0,             // never successfully indexed
	}

	tests := []struct {
		name          string
		retentionDays int
		wantDeleted   []string
	}{
		{
			name:          "default 90d horizon prunes only the stale project",
			retentionDays: 90,
			wantDeleted:   []string{"/p/stale"},
		},
		{
			name:          "tighter 60d horizon also takes the border project",
			retentionDays: 60,
			wantDeleted:   []string{"/p/border", "/p/stale"},
		},
		{
			name:          "zero disables the sweep entirely",
			retentionDays: 0,
			wantDeleted:   nil,
		},
		{
			name:          "negative disables the sweep entirely",
			retentionDays: -1,
			wantDeleted:   nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, database := newTestStore(t)
			ctx := context.Background()
			for p, ts := range catalog {
				seedCodeIntelProject(ctx, t, s, p, ts)
			}

			deleted, err := s.CodeIntelPruneStaleProjects(ctx, tc.retentionDays)
			if err != nil {
				t.Fatalf("CodeIntelPruneStaleProjects: %v", err)
			}
			if len(deleted) != len(tc.wantDeleted) {
				t.Fatalf("deleted %v, want %v", deleted, tc.wantDeleted)
			}
			for i, p := range tc.wantDeleted {
				if deleted[i] != p {
					t.Errorf("deleted[%d] = %q, want %q (full: %v)", i, deleted[i], p, deleted)
				}
			}

			// Every codeintel_* table must be empty for deleted projects
			// and intact for survivors.
			isDeleted := map[string]bool{}
			for _, p := range tc.wantDeleted {
				isDeleted[p] = true
			}
			tables := []string{
				"codeintel_files", "codeintel_nodes", "codeintel_edges",
				"codeintel_sites", "codeintel_minhash", "codeintel_embeddings",
				"codeintel_fts",
			}
			for p := range catalog {
				want := 1
				if isDeleted[p] {
					want = 0
				}
				for _, tbl := range tables {
					var n int
					if err := database.QueryRowContext(ctx,
						"SELECT COUNT(*) FROM "+tbl+" WHERE project = ?", p).Scan(&n); err != nil {
						t.Fatalf("count %s: %v", tbl, err)
					}
					if n != want {
						t.Errorf("%s: project %q has %d row(s), want %d", tbl, p, n, want)
					}
				}
			}

			// Idempotence: a second run within the same horizon deletes
			// nothing more.
			again, err := s.CodeIntelPruneStaleProjects(ctx, tc.retentionDays)
			if err != nil {
				t.Fatalf("CodeIntelPruneStaleProjects (2nd): %v", err)
			}
			if len(again) != 0 {
				t.Errorf("second run deleted %v, want none (idempotence)", again)
			}
		})
	}
}
