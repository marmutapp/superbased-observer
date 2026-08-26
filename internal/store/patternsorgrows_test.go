package store

import (
	"context"
	"testing"
)

// TestSelectProjectPatternRows proves the org wire rows are read correctly
// from a seeded project_patterns table: value extraction per kind (hot_file
// → file_path, common_command → command, other kinds → raw JSON blob), the
// project identity join (root_path_hash always, root_path riding alongside),
// and the per-(project,kind) cap.
func TestSelectProjectPatternRows(t *testing.T) {
	t.Parallel()
	s, d := newTestStore(t)
	ctx := context.Background()

	pid, err := s.UpsertProject(ctx, "/repo/patterns-x", "")
	if err != nil {
		t.Fatal(err)
	}

	insert := func(patternType, data string, obs int, confidence float64, sourceTools, lastSeen string) {
		t.Helper()
		if _, err := d.ExecContext(ctx,
			`INSERT INTO project_patterns (project_id, pattern_type, pattern_data, confidence, last_reinforced_at, observation_count, source_tools)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			pid, patternType, data, confidence, lastSeen, obs, sourceTools); err != nil {
			t.Fatalf("seed insert: %v", err)
		}
	}

	insert("hot_file", `{"file_path":"internal/x/foo.go","reads":10,"edits":3,"writes":1,"total_touch":14}`,
		14, 0.9, "claude-code,codex", "2026-08-20T10:00:00Z")
	insert("common_command", `{"command":"go test ./...","command_hash":"abc","run_count":20,"success_rate":0.95}`,
		20, 0.8, "claude-code", "2026-08-21T10:00:00Z")
	insert("knowledge_snippet", `{"span":"remember to run migrations before tests","source_count":2}`,
		2, 0.5, "claude-code", "2026-08-22T10:00:00Z")

	// Over-fill one (project, kind) group past the cap to prove capping.
	for i := 0; i < projectPatternCapPerGroup+5; i++ {
		insert("hot_file", `{"file_path":"internal/y/bar.go"}`, 1, 0.1, "claude-code", "2026-08-19T10:00:00Z")
	}

	rows, err := s.SelectProjectPatternRows(ctx)
	if err != nil {
		t.Fatalf("SelectProjectPatternRows: %v", err)
	}

	var hotFile, commonCmd, snippet bool
	hotFileCount := 0
	for _, r := range rows {
		if r.ProjectRootHash == "" {
			t.Fatalf("row missing ProjectRootHash: %+v", r)
		}
		if r.ProjectRoot != "/repo/patterns-x" {
			t.Errorf("ProjectRoot = %q, want /repo/patterns-x", r.ProjectRoot)
		}
		if r.Kind == "hot_file" {
			hotFileCount++
		}
		switch {
		case r.Kind == "hot_file" && r.Value == "internal/x/foo.go":
			hotFile = true
			if r.ObservationCount != 14 {
				t.Errorf("hot_file ObservationCount = %d, want 14", r.ObservationCount)
			}
			if r.SourceTools != "claude-code,codex" {
				t.Errorf("hot_file SourceTools = %q, want claude-code,codex", r.SourceTools)
			}
			if r.LastSeen != "2026-08-20T10:00:00Z" {
				t.Errorf("hot_file LastSeen = %q, unexpected", r.LastSeen)
			}
		case r.Kind == "common_command" && r.Value == "go test ./...":
			commonCmd = true
		case r.Kind == "knowledge_snippet":
			snippet = true
			if r.Value != `{"span":"remember to run migrations before tests","source_count":2}` {
				t.Errorf("knowledge_snippet Value = %q, want raw JSON passthrough", r.Value)
			}
		}
	}
	if !hotFile {
		t.Error("expected a hot_file row with Value=internal/x/foo.go")
	}
	if !commonCmd {
		t.Error("expected a common_command row with Value='go test ./...'")
	}
	if !snippet {
		t.Error("expected a knowledge_snippet row carrying the raw pattern_data JSON")
	}
	// The hot_file group had 1 + (cap+5) rows seeded; only the cap should
	// survive.
	if hotFileCount != projectPatternCapPerGroup {
		t.Errorf("hot_file row count = %d, want cap %d", hotFileCount, projectPatternCapPerGroup)
	}
}

// TestSelectProjectPatternRows_SkipsUnhashedProjects proves a project with no
// root_path_hash (pre-migration-034 legacy row, or a corrupt project) is
// excluded rather than shipped with an empty identity.
func TestSelectProjectPatternRows_SkipsUnhashedProjects(t *testing.T) {
	t.Parallel()
	s, d := newTestStore(t)
	ctx := context.Background()

	if _, err := d.ExecContext(ctx,
		`INSERT INTO projects (root_path, created_at, root_path_hash) VALUES ('/repo/no-hash', '2026-01-01T00:00:00Z', '')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	var pid int64
	if err := d.QueryRowContext(ctx, `SELECT id FROM projects WHERE root_path = '/repo/no-hash'`).Scan(&pid); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if _, err := d.ExecContext(ctx,
		`INSERT INTO project_patterns (project_id, pattern_type, pattern_data, confidence, observation_count) VALUES (?, 'hot_file', '{"file_path":"x.go"}', 0.5, 1)`,
		pid); err != nil {
		t.Fatalf("seed pattern: %v", err)
	}

	rows, err := s.SelectProjectPatternRows(ctx)
	if err != nil {
		t.Fatalf("SelectProjectPatternRows: %v", err)
	}
	for _, r := range rows {
		if r.ProjectRoot == "/repo/no-hash" {
			t.Fatalf("unhashed project must be excluded, got row: %+v", r)
		}
	}
}
