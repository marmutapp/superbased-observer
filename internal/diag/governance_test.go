package diag

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/govern/sidecar"
)

func TestCheckGovernance_NotEnrolledNoSidecar(t *testing.T) {
	cfg, database, home, _ := newTestEnv(t)

	c := checkGovernance(context.Background(), database, cfg, home)
	t.Logf("message=%q", c.Message)
	for _, d := range c.Details {
		t.Logf("  detail: %s", d)
	}
	if c.Status != StatusOK {
		t.Fatalf("status=%s want OK — %q %v", c.Status, c.Message, c.Details)
	}
	if c.Message != "not governed; no sidecar (correct)" {
		t.Errorf("message=%q", c.Message)
	}
}

func TestCheckGovernance_GovernedLiveSidecar(t *testing.T) {
	cfg, database, home, _ := newTestEnv(t)
	expiry := time.Now().UTC().Add(90 * 24 * time.Hour)
	writeGrantRow(t, database, expiry)
	path := writeSidecarFile(t, home, sidecar.File{
		Schema:         sidecar.MaxSchema,
		State:          sidecar.StateApplied,
		GrantExpiresAt: sidecar.FormatTime(expiry),
		Pinned:         map[string]any{"guard.enabled": true},
	})

	c := checkGovernance(context.Background(), database, cfg, home)
	t.Logf("message=%q", c.Message)
	for _, d := range c.Details {
		t.Logf("  detail: %s", d)
	}
	if c.Status != StatusOK {
		t.Fatalf("status=%s want OK — %q %v", c.Status, c.Message, c.Details)
	}
	if !containsAll(c.Message, "sidecar live", "pins applied=true") {
		t.Errorf("message=%q", c.Message)
	}
	if !detailsContain(c.Details, path) {
		t.Errorf("details missing sidecar path %q: %v", path, c.Details)
	}
}

func TestCheckGovernance_OrphanedSidecarNoGrant(t *testing.T) {
	cfg, database, home, _ := newTestEnv(t)
	// No grant row at all — a sidecar surviving unenroll.
	path := writeSidecarFile(t, home, sidecar.File{
		Schema: sidecar.MaxSchema,
		State:  sidecar.StateApplied,
		Pinned: map[string]any{"guard.enabled": true},
	})

	c := checkGovernance(context.Background(), database, cfg, home)
	t.Logf("message=%q", c.Message)
	for _, d := range c.Details {
		t.Logf("  detail: %s", d)
	}
	if c.Status != StatusWarn {
		t.Fatalf("status=%s want WARN — %q %v", c.Status, c.Message, c.Details)
	}
	if c.Message != "orphaned/stale sidecar at "+path {
		t.Errorf("message=%q", c.Message)
	}
	if !detailsContain(c.Details, "observer unenroll") {
		t.Errorf("details missing remedy: %v", c.Details)
	}
}

func TestCheckGovernance_OrphanedSidecarExpiredGrant(t *testing.T) {
	cfg, database, home, _ := newTestEnv(t)
	expiry := time.Now().UTC().Add(-48 * time.Hour)
	writeGrantRow(t, database, expiry)
	path := writeSidecarFile(t, home, sidecar.File{
		Schema:         sidecar.MaxSchema,
		State:          sidecar.StateApplied,
		GrantExpiresAt: sidecar.FormatTime(expiry),
		Pinned:         map[string]any{"guard.enabled": true},
	})

	c := checkGovernance(context.Background(), database, cfg, home)
	t.Logf("message=%q", c.Message)
	for _, d := range c.Details {
		t.Logf("  detail: %s", d)
	}
	if c.Status != StatusWarn {
		t.Fatalf("status=%s want WARN — %q %v", c.Status, c.Message, c.Details)
	}
	if c.Message != "orphaned/stale sidecar at "+path {
		t.Errorf("message=%q", c.Message)
	}
}

func TestCheckGovernance_UnwritableSidecarDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based read-only directory probe is POSIX-specific")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permission checks")
	}
	cfg, database, home, _ := newTestEnv(t)
	expiry := time.Now().UTC().Add(90 * 24 * time.Hour)
	writeGrantRow(t, database, expiry)

	dir := filepath.Dir(filepath.Join(home, ".observer", "governance-effective.json"))
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	c := checkGovernance(context.Background(), database, cfg, home)
	t.Logf("message=%q", c.Message)
	for _, d := range c.Details {
		t.Logf("  detail: %s", d)
	}
	if c.Status != StatusFail {
		t.Fatalf("status=%s want FAIL — %q %v", c.Status, c.Message, c.Details)
	}
	if !containsAll(c.Message, "not writable", "errno") {
		t.Errorf("message=%q — expected the errno to be named", c.Message)
	}
}

func TestCheckGovernance_ExpiringWithinSevenDays(t *testing.T) {
	cfg, database, home, _ := newTestEnv(t)
	expiry := time.Now().UTC().Add(3 * 24 * time.Hour)
	writeGrantRow(t, database, expiry)
	writeSidecarFile(t, home, sidecar.File{
		Schema:         sidecar.MaxSchema,
		State:          sidecar.StateApplied,
		GrantExpiresAt: sidecar.FormatTime(expiry),
		Pinned:         map[string]any{"guard.enabled": true},
	})

	c := checkGovernance(context.Background(), database, cfg, home)
	t.Logf("message=%q", c.Message)
	for _, d := range c.Details {
		t.Logf("  detail: %s", d)
	}
	if c.Status != StatusWarn {
		t.Fatalf("status=%s want WARN — %q %v", c.Status, c.Message, c.Details)
	}
	if !containsAll(c.Message, "grant expires in", "day(s)") {
		t.Errorf("message=%q", c.Message)
	}
}

func TestCheckGovernance_FilterBySubstring(t *testing.T) {
	cfg, database, home, binary := newTestEnv(t)
	report := Run(context.Background(), DoctorOptions{
		Config: cfg, DB: database, HomeDir: home, BinaryPath: binary,
	})
	filtered := report.Filter("governance")
	if len(filtered.Checks) != 1 || filtered.Checks[0].Name != "governance" {
		t.Fatalf("Filter(\"governance\") = %+v", filtered.Checks)
	}
}

// --- test helpers -----------------------------------------------------

// writeGrantRow inserts a minimal org_enrolment_grant row directly via SQL,
// mirroring store.WriteEnrolmentGrant's column set. This package
// deliberately avoids importing internal/store (see checkOrgEnrolment's
// "keep diag standalone" comment), so tests write the row the same way the
// production check reads it: raw SQL against the schema.
func writeGrantRow(t *testing.T, database *sql.DB, expiresAt time.Time) {
	t.Helper()
	_, err := database.ExecContext(context.Background(), `
		INSERT INTO org_enrolment_grant
		  (org_key, generation, org_id, org_name, org_server_url, key_pin_sha256,
		   authority_json, consent_mode, consent_actor, granted_at, expires_at,
		   signed_expires_at, last_renewed_at, signature, receipt_hash)
		VALUES (?, 1, 'org-1', 'Acme', 'https://org.example', 'deadbeef',
		        '[]', 'admin_managed', 'admin@example.com', ?, ?, ?, '', 'sig', 'hash')`,
		"test-org-key", time.Now().UTC().Format(time.RFC3339), expiresAt.Format(time.RFC3339), expiresAt.Format(time.RFC3339))
	if err != nil {
		t.Fatalf("insert org_enrolment_grant: %v", err)
	}
}

// writeSidecarFile writes f to the sidecar path this test's home would
// resolve (beside the DB) and returns that path.
func writeSidecarFile(t *testing.T, home string, f sidecar.File) string {
	t.Helper()
	dir := filepath.Join(home, ".observer")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "governance-effective.json")
	body, err := sidecar.Encode(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

func detailsContain(details []string, sub string) bool {
	for _, d := range details {
		if strings.Contains(d, sub) {
			return true
		}
	}
	return false
}
