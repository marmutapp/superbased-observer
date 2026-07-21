package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/guard"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// writeGuardTestConfig writes a minimal config.toml whose DB path
// lives in the same temp dir, returning (configPath, dbPath).
func writeGuardTestConfig(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "observer.db")
	cfgPath := filepath.Join(dir, "config.toml")
	// Forward slashes keep the TOML free of the Windows \U escaping
	// trap (the documented Windows-host fixture failure class).
	body := "[observer]\ndb_path = \"" + filepath.ToSlash(dbPath) + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return cfgPath, dbPath
}

// runGuardCmd executes `observer guard <args...>` capturing stdout.
func runGuardCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newGuardCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

// TestGuardRulesCmd smoke-tests the catalog listing in both shapes.
func TestGuardRulesCmd(t *testing.T) {
	t.Parallel()
	out, err := runGuardCmd(t, "rules")
	if err != nil {
		t.Fatalf("guard rules: %v", err)
	}
	for _, want := range []string{"R-101", "R-152", "T-504", "destructive", "boundary", "taint"} {
		if !strings.Contains(out, want) {
			t.Errorf("catalog output missing %q", want)
		}
	}
	out, err = runGuardCmd(t, "rules", "--json")
	if err != nil {
		t.Fatalf("guard rules --json: %v", err)
	}
	if !strings.Contains(out, `"id": "R-101"`) || !strings.Contains(out, `"observe": "flag"`) {
		t.Errorf("json output shape: %s", out[:min(200, len(out))])
	}
}

// TestGuardTestCmd dry-runs a destructive command and a benign one
// through a real temp config.
func TestGuardTestCmd(t *testing.T) {
	t.Parallel()
	cfgPath, _ := writeGuardTestConfig(t)

	out, err := runGuardCmd(t, "test", "--config", cfgPath, "git push --force origin main")
	if err != nil {
		t.Fatalf("guard test: %v", err)
	}
	if !strings.Contains(out, "R-110") || !strings.Contains(out, "flag") {
		t.Errorf("force-push dry-run output: %s", out)
	}
	// The pre-enforce confidence line: observe flags, enforce denies.
	if !strings.Contains(out, "In enforce:   deny") {
		t.Errorf("missing enforce-mode projection: %s", out)
	}

	out, err = runGuardCmd(t, "test", "--config", cfgPath, "go test ./...")
	if err != nil {
		t.Fatalf("guard test benign: %v", err)
	}
	if !strings.Contains(out, "allow — no rule matched") {
		t.Errorf("benign dry-run output: %s", out)
	}

	// --file form classifies as a write.
	out, err = runGuardCmd(t, "test", "--config", cfgPath, "--file", "~/.ssh/authorized_keys")
	if err != nil {
		t.Fatalf("guard test --file: %v", err)
	}
	if !strings.Contains(out, "R-152") {
		t.Errorf("sensitive write dry-run output: %s", out)
	}

	// Exactly one input form is required.
	if _, err := runGuardCmd(t, "test", "--config", cfgPath); err == nil {
		t.Error("no input form accepted")
	}
}

// TestGuardLintCmd lints a clean and a broken explicit policy file.
func TestGuardLintCmd(t *testing.T) {
	t.Parallel()
	cfgPath, _ := writeGuardTestConfig(t)
	dir := t.TempDir()

	good := filepath.Join(dir, "good.toml")
	if err := os.WriteFile(good, []byte(
		"[[rule]]\nid='U-1'\ncategory='boundary'\ndecision='flag'\nmatch.command_base='make'\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runGuardCmd(t, "lint", "--config", cfgPath, good)
	if err != nil {
		t.Fatalf("lint clean file: %v (%s)", err, out)
	}
	if !strings.Contains(out, "OK") {
		t.Errorf("lint output: %s", out)
	}

	bad := filepath.Join(dir, "bad.toml")
	if err := os.WriteFile(bad, []byte(
		"[[rule]]\nid='R-101'\ncategory='destructive'\ndecision='deny'\nmatch.command_base='rm'\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = runGuardCmd(t, "lint", "--config", cfgPath, bad)
	if err == nil {
		t.Fatalf("lint accepted a builtin-ID collision: %s", out)
	}
	if !strings.Contains(out, "collides with built-in") {
		t.Errorf("lint problem output: %s", out)
	}
}

// TestGuardVerifyAuditCmd runs the chain walk against a real temp DB:
// clean chain passes; a tampered row fails with exit-error.
func TestGuardVerifyAuditCmd(t *testing.T) {
	t.Parallel()
	cfgPath, dbPath := writeGuardTestConfig(t)

	// Seed two chained rows through the one-owner helper.
	database, err := db.Open(context.Background(), db.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	s := store.New(database)
	if _, err := s.InsertGuardEvents(context.Background(), []store.GuardEventRow{
		{TS: time.Now().UTC(), SessionID: "s1", RuleID: "R-101", Decision: "flag", Severity: "critical"},
		{TS: time.Now().UTC(), SessionID: "s1", RuleID: "R-110", Decision: "deny", Severity: "critical"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	out, err := runGuardCmd(t, "verify-audit", "--config", cfgPath)
	if err != nil {
		t.Fatalf("verify-audit clean: %v (%s)", err, out)
	}
	if !strings.Contains(out, "audit chain OK — 2 row(s)") {
		t.Errorf("clean output: %s", out)
	}

	if _, err := database.ExecContext(context.Background(),
		`UPDATE guard_events SET decision = 'allow' WHERE rule_id = 'R-110'`); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	_ = database.Close()

	out, err = runGuardCmd(t, "verify-audit", "--config", cfgPath)
	if err == nil {
		t.Fatalf("verify-audit passed a tampered chain: %s", out)
	}
	if !strings.Contains(out, "BROKEN") {
		t.Errorf("tampered output: %s", out)
	}
}

// TestGuardStatusCmd smoke-tests status against a real temp DB with
// one verdict row.
func TestGuardStatusCmd(t *testing.T) {
	t.Parallel()
	cfgPath, dbPath := writeGuardTestConfig(t)
	database, err := db.Open(context.Background(), db.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	s := store.New(database)
	if _, err := s.PersistGuardVerdicts(context.Background(), []guard.ActionVerdict{}); err != nil {
		t.Fatalf("empty persist: %v", err)
	}
	if _, err := s.InsertGuardEvents(context.Background(), []store.GuardEventRow{
		{
			TS: time.Now().UTC(), SessionID: "s1", RuleID: "R-101", Decision: "flag",
			Severity: "critical", Category: "destructive",
		},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = database.Close()

	out, err := runGuardCmd(t, "status", "--config", cfgPath)
	if err != nil {
		t.Fatalf("guard status: %v (%s)", err, out)
	}
	for _, want := range []string{
		"Guard:        on", "Mode:         observe",
		"Verdicts (24h): 1 total", "by decision: flag=1",
		"Audit chain:  OK (1 rows verified)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}
}
