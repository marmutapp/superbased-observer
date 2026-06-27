package api

import (
	"context"
	"database/sql"
	"testing"
	"time"

	orgdb "github.com/marmutapp/superbased-observer/internal/orgserver/db"
)

func newAdminTestDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := orgdb.Open(context.Background(), orgdb.Options{Path: t.TempDir() + "/admin.db"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestProvisionUser(t *testing.T) {
	ctx := context.Background()
	d := newAdminTestDB(t)

	id1, created1, err := ProvisionUser(ctx, d, "dev@acme.example", "Dev")
	if err != nil {
		t.Fatal(err)
	}
	if !created1 {
		t.Error("first provision should report created=true")
	}
	if id1 == "" {
		t.Error("empty user_id")
	}

	id2, created2, err := ProvisionUser(ctx, d, "dev@acme.example", "")
	if err != nil {
		t.Fatal(err)
	}
	if created2 {
		t.Error("second provision of same email should be idempotent (created=false)")
	}
	if id2 != id1 {
		t.Errorf("idempotent provision returned a different id: %s vs %s", id2, id1)
	}

	if _, _, err := ProvisionUser(ctx, d, "   ", ""); err == nil {
		t.Error("empty email should error")
	}
}

func TestListEnrolmentTokens(t *testing.T) {
	ctx := context.Background()
	d := newAdminTestDB(t)

	if toks, err := ListEnrolmentTokens(ctx, d); err != nil || len(toks) != 0 {
		t.Fatalf("empty list: err=%v n=%d", err, len(toks))
	}

	id, _, err := ProvisionUser(ctx, d, "dev@acme.example", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := MintEnrolmentTokenForUser(ctx, d, id, time.Hour); err != nil {
		t.Fatal(err)
	}

	toks, err := ListEnrolmentTokens(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if len(toks) != 1 {
		t.Fatalf("expected 1 token, got %d", len(toks))
	}
	tk := toks[0]
	if tk.UserEmail != "dev@acme.example" {
		t.Errorf("email = %q", tk.UserEmail)
	}
	if tk.TokenID == "" {
		t.Error("empty token_id")
	}
	if tk.Redeemed() {
		t.Error("fresh token should not be redeemed")
	}
	if tk.Expired(time.Now()) {
		t.Error("a 1h token should not be expired now")
	}
}
