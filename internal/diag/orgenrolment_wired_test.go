package diag

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// insertEnrolmentRow seeds the singleton org_enrolment row the way a real
// `observer enroll` would, via raw SQL (this package's standalone
// discipline — no internal/store import).
func insertEnrolmentRow(t *testing.T, database *sql.DB, orgURL string) {
	t.Helper()
	_, err := database.Exec(`
		INSERT INTO org_enrolment (id, org_id, org_name, org_server_url, user_id, user_email, enrolled_at, bearer_key_id)
		VALUES (1, 'org-1', 'Acme', ?, 'u-1', 'dev@acme.example', '2026-08-01T00:00:00Z', 'sbo-org-bearer-v1')`, orgURL)
	if err != nil {
		t.Fatalf("insert org_enrolment: %v", err)
	}
}

// TestOrgEnrolmentWarnsWhenEnrolledButDisabled is tracker #41: an enrolment
// row with [org_client] enabled = false means the node looks enrolled (to
// its operator AND to the org that minted the token) while pushing nothing
// and polling no policy — silently ungoverned. "Disabled" may only read as
// innocuous OK on a machine that never enrolled.
func TestOrgEnrolmentWarnsWhenEnrolledButDisabled(t *testing.T) {
	cfg, database, _, _ := newTestEnv(t)
	cfg.OrgClient.Enabled = false
	insertEnrolmentRow(t, database, "https://org.acme.example")

	c := checkOrgEnrolment(context.Background(), database, cfg)
	if c.Status != StatusWarn {
		t.Fatalf("status=%s want WARN — %q", c.Status, c.Message)
	}
	if !strings.Contains(c.Message, "enrolled but [org_client] enabled = false") {
		t.Errorf("message=%q", c.Message)
	}
}

// TestOrgEnrolmentDisabledNeverEnrolledStaysOK pins the innocuous half so
// the #41 warn cannot creep onto genuinely solo machines.
func TestOrgEnrolmentDisabledNeverEnrolledStaysOK(t *testing.T) {
	cfg, database, _, _ := newTestEnv(t)
	cfg.OrgClient.Enabled = false

	c := checkOrgEnrolment(context.Background(), database, cfg)
	if c.Status != StatusOK {
		t.Fatalf("status=%s want OK — %q", c.Status, c.Message)
	}
}

// TestOrgEnrolmentWarnsWhenServerURLUnset is #41's second half: the push
// loop reads the enrolment row's URL from the DB, but the guard
// policy-bundle runner reads [org_client].org_server_url from config — an
// enrolled node without the key runs its guard org-layer silently unwired.
func TestOrgEnrolmentWarnsWhenServerURLUnset(t *testing.T) {
	cfg, database, _, _ := newTestEnv(t)
	cfg.OrgClient.Enabled = true
	cfg.OrgClient.OrgServerURL = ""
	insertEnrolmentRow(t, database, "https://org.acme.example")

	c := checkOrgEnrolment(context.Background(), database, cfg)
	if c.Status != StatusWarn {
		t.Fatalf("status=%s want WARN — %q %v", c.Status, c.Message, c.Details)
	}
	found := false
	for _, d := range c.Details {
		if strings.Contains(d, "org_server_url is unset") && strings.Contains(d, "https://org.acme.example") {
			found = true
		}
	}
	if !found {
		t.Errorf("details missing the org_server_url config-gap line: %v", c.Details)
	}
}
