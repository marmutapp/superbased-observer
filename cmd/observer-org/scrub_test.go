package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	orgdb "github.com/marmutapp/superbased-observer/internal/orgserver/db"
)

// seedLeakedContent inserts one row per content-bearing table with a
// distinctive raw value, simulating a pre-v1.8.0 push that landed
// before the seam was fixed.
func seedLeakedContent(t *testing.T, dbPath string) {
	t.Helper()
	d, err := orgdb.Open(context.Background(), orgdb.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	mustExec := func(q string, args ...any) {
		if _, err := d.Exec(q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	mustExec(`INSERT INTO sessions (id, user_id, org_id, user_email, project_root, project_root_hash, git_remote, git_remote_hash, tool, started_at, pushed_at, pushed_by_user_id)
		VALUES ('s1','u1','o1','dev@x','/home/me/secret-proj','aaa','git@github.com:me/private.git','bbb','claude-code',?,?,'u1')`, now, now)
	mustExec(`INSERT INTO actions (source_file, source_event_id, user_id, session_id, org_id, user_email, timestamp, tool, action_type, target, target_hash, source_file_hash, pushed_at, pushed_by_user_id)
		VALUES ('/home/me/.claude/cc.jsonl','e1','u1','s1','o1','dev@x',?,'claude-code','run_command','rm -rf /tmp/secret','ccc','ddd',?,'u1')`, now, now)
	mustExec(`INSERT INTO api_turns (user_id, org_id, user_email, session_id, project_root, project_root_hash, timestamp, provider, model, request_id, input_tokens, output_tokens, pushed_at, pushed_by_user_id)
		VALUES ('u1','o1','dev@x','s1','/home/me/secret-proj','aaa',?,'anthropic','claude-opus-4-7','req1',100,50,?,'u1')`, now, now)
	mustExec(`INSERT INTO token_usage (source_file, source_event_id, user_id, org_id, user_email, session_id, project_root, project_root_hash, source_file_hash, timestamp, tool, model, input_tokens, output_tokens, source, pushed_at, pushed_by_user_id)
		VALUES ('/home/me/.claude/cc.jsonl','tu1','u1','o1','dev@x','s1','/home/me/secret-proj','aaa','ddd',?,'claude-code','claude-opus-4-7',100,50,'proxy',?,'u1')`, now, now)
}

func TestScrubContent_DryRunDoesNothing(t *testing.T) {
	cfgPath, dbPath := writeConfig(t)
	if _, err := runCmd(t, "migrate", "--config", cfgPath); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	seedLeakedContent(t, dbPath)

	// Default: --confirm not passed → dry run. Should report counts and
	// leave the DB untouched.
	out, err := runCmd(t, "scrub-content", "--config", cfgPath, "--all")
	if err != nil {
		t.Fatalf("scrub-content --all dry: %v", err)
	}
	if !strings.Contains(out, "would scrub") || !strings.Contains(out, "DRY RUN") {
		t.Errorf("dry-run output missing markers:\n%s", out)
	}
	if !strings.Contains(out, "actions.target") {
		t.Errorf("dry-run did not include actions.target:\n%s", out)
	}

	// Verify the row is still there with raw content intact.
	d, err := orgdb.Open(context.Background(), orgdb.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var target string
	if err := d.QueryRow(`SELECT target FROM actions WHERE source_event_id = 'e1'`).Scan(&target); err != nil {
		t.Fatal(err)
	}
	if target != "rm -rf /tmp/secret" {
		t.Errorf("dry-run modified the DB: target = %q", target)
	}
}

func TestScrubContent_ConfirmNullsColumnsAndPreservesHashes(t *testing.T) {
	cfgPath, dbPath := writeConfig(t)
	if _, err := runCmd(t, "migrate", "--config", cfgPath); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	seedLeakedContent(t, dbPath)

	out, err := runCmd(t, "scrub-content", "--config", cfgPath, "--all", "--confirm", "--skip-backup")
	if err != nil {
		t.Fatalf("scrub-content --confirm: %v", err)
	}
	if !strings.Contains(out, "scrub applied") {
		t.Errorf("confirm output missing marker:\n%s", out)
	}

	d, err := orgdb.Open(context.Background(), orgdb.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	// Every raw column should be NULL; every *_hash should remain.
	for _, q := range []string{
		`SELECT COUNT(*) FROM actions WHERE target IS NOT NULL AND target != ''`,
		`SELECT COUNT(*) FROM actions WHERE source_file IS NOT NULL AND source_file != ''`,
		`SELECT COUNT(*) FROM sessions WHERE project_root IS NOT NULL AND project_root != ''`,
		`SELECT COUNT(*) FROM sessions WHERE git_remote IS NOT NULL AND git_remote != ''`,
		`SELECT COUNT(*) FROM api_turns WHERE project_root IS NOT NULL AND project_root != ''`,
		`SELECT COUNT(*) FROM token_usage WHERE project_root IS NOT NULL AND project_root != ''`,
		`SELECT COUNT(*) FROM token_usage WHERE source_file IS NOT NULL AND source_file != ''`,
	} {
		var n int
		if err := d.QueryRow(q).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 0 {
			t.Errorf("expected 0 leaked rows for %q, got %d", q, n)
		}
	}
	// Hashes survived.
	for _, q := range []string{
		`SELECT COUNT(*) FROM actions WHERE target_hash = 'ccc'`,
		`SELECT COUNT(*) FROM sessions WHERE project_root_hash = 'aaa'`,
	} {
		var n int
		if err := d.QueryRow(q).Scan(&n); err != nil {
			t.Fatalf("hash count: %v", err)
		}
		if n != 1 {
			t.Errorf("expected 1 hash-preserved row for %q, got %d", q, n)
		}
	}
}

func TestScrubContent_BackupWritten(t *testing.T) {
	cfgPath, dbPath := writeConfig(t)
	if _, err := runCmd(t, "migrate", "--config", cfgPath); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	seedLeakedContent(t, dbPath)

	out, err := runCmd(t, "scrub-content", "--config", cfgPath, "--all", "--confirm")
	if err != nil {
		t.Fatalf("scrub-content --confirm: %v", err)
	}
	if !strings.Contains(out, "backup written:") {
		t.Errorf("backup line missing:\n%s", out)
	}
	// Look for the .bak alongside the original DB.
	matches, _ := os.ReadDir(filepathDir(dbPath))
	var found bool
	for _, m := range matches {
		if strings.HasPrefix(m.Name(), "server.db.scrubbed-") && strings.HasSuffix(m.Name(), ".bak") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a .scrubbed-<ts>.bak file alongside %s", dbPath)
	}
}

func TestScrubContent_RequiresColumnFlag(t *testing.T) {
	cfgPath, _ := writeConfig(t)
	if _, err := runCmd(t, "migrate", "--config", cfgPath); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := runCmd(t, "scrub-content", "--config", cfgPath); err == nil {
		t.Error("expected error when no column flag (or --all) passed")
	}
}

// filepathDir returns path's parent directory using filepath rules; kept
// inline to avoid pulling another import for one call.
func filepathDir(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			return path[:i]
		}
	}
	return "."
}
