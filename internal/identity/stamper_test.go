package identity

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// fakeRow is a test OrgRow that records the SetOrg call.
type fakeRow struct {
	org, email string
	called     bool
}

func (f *fakeRow) SetOrg(orgID, userEmail string) {
	f.org, f.email, f.called = orgID, userEmail, true
}

// openDB returns a fresh temp-file SQLite DB. A temp file (not :memory:)
// avoids the modernc connection-pool gotcha where serial INSERT/SELECT on
// a :memory: DSN can land on different, independent in-memory databases.
func openDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "id.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// withEnrolmentTable creates the org_enrolment table (mirroring migration
// 028's columns that NewStamper reads) and optionally seeds the singleton
// enrolment row.
func withEnrolmentTable(t *testing.T, db *sql.DB, seed bool) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE org_enrolment (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		org_id TEXT NOT NULL, org_name TEXT NOT NULL, org_server_url TEXT NOT NULL,
		user_id TEXT NOT NULL, user_email TEXT NOT NULL,
		enrolled_at TEXT NOT NULL, bearer_key_id TEXT NOT NULL)`); err != nil {
		t.Fatalf("create org_enrolment: %v", err)
	}
	if seed {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO org_enrolment (id, org_id, org_name, org_server_url, user_id, user_email, enrolled_at, bearer_key_id)
			 VALUES (1, 'org-acme', 'Acme', 'https://org.acme.example', 'scim-9', 'dev@acme.example', '2026-05-25T00:00:00Z', 'kc-1')`); err != nil {
			t.Fatalf("seed enrolment: %v", err)
		}
	}
}

func TestNewStamper_Enrolled(t *testing.T) {
	t.Parallel()
	db := openDB(t)
	withEnrolmentTable(t, db, true)

	s, err := NewStamper(context.Background(), db)
	if err != nil {
		t.Fatalf("NewStamper: %v", err)
	}
	if !s.IsEnrolled() {
		t.Fatal("IsEnrolled() = false, want true")
	}
	if s.OrgID() != "org-acme" {
		t.Errorf("OrgID() = %q, want org-acme", s.OrgID())
	}
	if s.UserEmail() != "dev@acme.example" {
		t.Errorf("UserEmail() = %q, want dev@acme.example", s.UserEmail())
	}

	row := &fakeRow{}
	s.Stamp(row)
	if !row.called {
		t.Fatal("Stamp did not call SetOrg on an enrolled stamper")
	}
	if row.org != "org-acme" || row.email != "dev@acme.example" {
		t.Errorf("Stamp set (%q, %q), want (org-acme, dev@acme.example)", row.org, row.email)
	}
}

func TestNewStamper_NotEnrolled_EmptyTable(t *testing.T) {
	t.Parallel()
	db := openDB(t)
	withEnrolmentTable(t, db, false)

	s, err := NewStamper(context.Background(), db)
	if err != nil {
		t.Fatalf("NewStamper: %v", err)
	}
	assertNoOp(t, s)
}

func TestNewStamper_MissingTable(t *testing.T) {
	t.Parallel()
	db := openDB(t) // no org_enrolment table (simulates a pre-028 schema)

	s, err := NewStamper(context.Background(), db)
	if err != nil {
		t.Fatalf("NewStamper must not error on missing table: %v", err)
	}
	assertNoOp(t, s)
}

func TestNewStamper_NilDB(t *testing.T) {
	t.Parallel()
	s, err := NewStamper(context.Background(), nil)
	if err != nil {
		t.Fatalf("NewStamper(nil): %v", err)
	}
	assertNoOp(t, s)
}

func TestNilStamper_IsSafe(t *testing.T) {
	t.Parallel()
	var s *Stamper // the solo-local default the store may hold
	assertNoOp(t, s)
}

// assertNoOp checks that a stamper is unenrolled and that Stamp does not
// touch the row (and does not panic, exercised by the nil-stamper case).
func assertNoOp(t *testing.T, s *Stamper) {
	t.Helper()
	if s.IsEnrolled() {
		t.Error("IsEnrolled() = true, want false")
	}
	if got := s.OrgID(); got != "" {
		t.Errorf("OrgID() = %q, want empty", got)
	}
	if got := s.UserEmail(); got != "" {
		t.Errorf("UserEmail() = %q, want empty", got)
	}
	row := &fakeRow{}
	s.Stamp(row)
	if row.called {
		t.Error("Stamp called SetOrg on an unenrolled/nil stamper")
	}
}
