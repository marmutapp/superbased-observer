package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	orgdb "github.com/marmutapp/superbased-observer/internal/orgserver/db"
)

// writeConfig writes a minimal config pointing the DB at a temp path and
// returns the config path + db path.
func writeConfig(t *testing.T) (cfgPath, dbPath string) {
	t.Helper()
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "server.db")
	cfgPath = filepath.Join(dir, "config.toml")
	body := "[server]\n" +
		"external_url = \"https://org.example\"\n" +
		"db_path = \"" + filepath.ToSlash(dbPath) + "\"\n" +
		"session_key_path = \"/x/session.key\"\n" +
		"[bearer]\nsigning_key_path = \"/x/bearer.key\"\n" +
		"[scim]\nauth_token_path = \"/x/scim.token\"\n" +
		"[saml]\nsp_entity_id = \"https://org.example/saml/metadata\"\n" +
		"sp_cert_path = \"/x/sp.crt\"\nsp_key_path = \"/x/sp.key\"\n" +
		"idp_metadata_url = \"https://idp.example/metadata\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, dbPath
}

func runCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestMigrateAndDumpConfig(t *testing.T) {
	cfgPath, _ := writeConfig(t)

	out, err := runCmd(t, "migrate", "--config", cfgPath)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Schema 16 = the guard arc's paired server migrations (004 policy
	// bundles at G13, 005 guard_events at G14), the model-routing arc's
	// 006_routing, the native-console servers (007_otel_content Phase 2b
	// Layer B, 008_cc_analytics_daily Claude Code Analytics poller,
	// 009_codex_analytics_daily Codex instance #2, 010_copilot_analytics_daily
	// Copilot instance #3), 011_otel_content_session_idx (Phase 7
	// message-content viewer index; server-only, no agent pair), plus the
	// generalized-observability org-tier servers (012_obs_summaries,
	// 013_obs_traces, 014_obs_content, 015_obs_eval, 016_obs_alerts).
	if !strings.Contains(out, "schema version = 16") {
		t.Errorf("migrate output = %q", out)
	}

	out, err = runCmd(t, "dump-config", "--config", cfgPath)
	if err != nil {
		t.Fatalf("dump-config: %v", err)
	}
	if !strings.Contains(out, "https://org.example") || !strings.Contains(out, "default_lifetime_days") {
		t.Errorf("dump-config output missing expected keys:\n%s", out)
	}
}

func TestNewEnrolmentTokenCmd(t *testing.T) {
	cfgPath, dbPath := writeConfig(t)

	// Apply migrations + seed a member.
	d, err := orgdb.Open(context.Background(), orgdb.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := d.Exec(`INSERT INTO org_members (user_id, user_name, email, active, created_at, updated_at) VALUES ('u1','dev','dev@x',1,?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	_ = d.Close()

	// Happy path.
	out, err := runCmd(t, "new-enrolment-token", "--config", cfgPath, "--user-id", "u1")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !strings.Contains(out, "token:") || !strings.Contains(out, "expires_at:") {
		t.Errorf("mint output = %q", out)
	}

	// Missing --user-id.
	if _, err := runCmd(t, "new-enrolment-token", "--config", cfgPath); err == nil {
		t.Error("expected error when --user-id missing")
	}

	// Unknown user.
	if _, err := runCmd(t, "new-enrolment-token", "--config", cfgPath, "--user-id", "ghost"); err == nil {
		t.Error("expected error for unknown user")
	}
}
