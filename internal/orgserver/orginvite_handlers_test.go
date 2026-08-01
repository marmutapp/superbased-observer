package orgserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/orgserver/api"
	"github.com/marmutapp/superbased-observer/internal/orgserver/auth"
	orgdb "github.com/marmutapp/superbased-observer/internal/orgserver/db"
)

func newInviteTestDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := orgdb.Open(context.Background(), orgdb.Options{Path: t.TempDir() + "/server.db"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func seedInviteMember(t *testing.T, d *sql.DB, userID, email string, active int) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := d.Exec(
		`INSERT INTO org_members (user_id, user_name, email, active, created_at, updated_at) VALUES (?,?,?,?,?,?)`,
		userID, email, email, active, now, now,
	); err != nil {
		t.Fatal(err)
	}
}

// TestActiveMemberChecker pins the DELEGATED half of the invite gate: only a
// currently-active member qualifies. A deprovisioned user with a live session
// cookie must not be able to hand out enrolment tokens.
func TestActiveMemberChecker(t *testing.T) {
	d := newInviteTestDB(t)
	seedInviteMember(t, d, "member-1", "member@acme.example", 1)
	seedInviteMember(t, d, "gone-1", "gone@acme.example", 0)
	check := activeMemberCheckerFor(d)
	ctx := context.Background()

	for _, tc := range []struct {
		id   string
		want bool
	}{
		{"member-1", true},
		{"gone-1", false},
		{"never-existed", false},
		{"", false},
	} {
		got, err := check(ctx, tc.id)
		if err != nil {
			t.Fatalf("check(%q): %v", tc.id, err)
		}
		if got != tc.want {
			t.Errorf("activeMemberCheckerFor(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

// TestAdminCheckerRequiresActive is the CHECKER-level half of codex finding 2
// (HIGH): org-admin authority is "is in the allow-list AND is still an active
// member", not just the former. A SAML session outlives the SCIM row it was
// issued against, so without the `active = 1` clause a deprovisioned admin
// keeps every RequireAdminSAML capability — policy publish, announcements,
// the token list, the spend export — plus the UNCAPPED admin invite branch,
// for the whole session lifetime.
//
// Deleting `AND active = 1` from adminCheckerFor's query fires this test.
func TestAdminCheckerRequiresActive(t *testing.T) {
	d := newInviteTestDB(t)
	seedInviteMember(t, d, "admin-1", "admin@acme.example", 1)
	seedInviteMember(t, d, "admin-gone", "gone-admin@acme.example", 0)
	seedInviteMember(t, d, "member-1", "member@acme.example", 1)
	check := adminCheckerFor(d, []string{"admin@acme.example", "gone-admin@acme.example"})
	ctx := context.Background()

	for _, tc := range []struct {
		id   string
		want bool
	}{
		{"admin-1", true},
		{"admin-gone", false}, // allow-listed but SCIM-deactivated
		{"member-1", false},
		{"never-existed", false},
		{"", false},
	} {
		got, err := check(ctx, tc.id)
		if err != nil {
			t.Fatalf("check(%q): %v", tc.id, err)
		}
		if got != tc.want {
			t.Errorf("adminCheckerFor(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

// TestDeactivatedAdminCannotMintThroughMux is the ROUTE-level proof of codex
// finding 2: through the assembled mux, with a genuinely valid SAML session
// cookie, an allow-listed but SCIM-deactivated admin gets 403 and mints
// nothing — on the default (invites-off) posture and the delegated one alike.
func TestDeactivatedAdminCannotMintThroughMux(t *testing.T) {
	for _, memberInvites := range []bool{false, true} {
		t.Run(fmt.Sprintf("member_invites=%v", memberInvites), func(t *testing.T) {
			d := newInviteTestDB(t)
			seedInviteMember(t, d, "admin-1", "admin@acme.example", 0) // deprovisioned
			seedInviteMember(t, d, "user-1", "dev@acme.example", 1)
			mux, sessions := newInviteMux(t, d, memberInvites, 10)

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, sessionRequest(t, sessions, "admin-1", http.MethodPost,
				"/api/org/enrolment-tokens", `{"user_id":"user-1"}`))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("deactivated admin mint: code=%d body=%s, want 403", rec.Code, rec.Body.String())
			}
			var n int
			if err := d.QueryRow(`SELECT COUNT(*) FROM enrolment_tokens`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Errorf("deactivated admin minted %d tokens, want 0", n)
			}

			// The admin READ is gated by the same checker.
			rec = httptest.NewRecorder()
			mux.ServeHTTP(rec, sessionRequest(t, sessions, "admin-1", http.MethodGet,
				"/api/org/enrolment-tokens", ""))
			if rec.Code != http.StatusForbidden {
				t.Errorf("deactivated admin token list: code=%d, want 403", rec.Code)
			}
		})
	}
}

// TestEnrolmentTokensListHandler covers the admin tokens rail — including the
// invite→enrolment CONVERSION fields (minted_by_email + redeemed), which are
// the plan's item-4 growth measurement computed entirely inside the org's own
// DB, on no wire.
func TestEnrolmentTokensListHandler(t *testing.T) {
	d := newInviteTestDB(t)
	ctx := context.Background()
	seedInviteMember(t, d, "member-1", "member@acme.example", 1)
	seedInviteMember(t, d, "user-1", "dev@acme.example", 1)

	// One attributed mint (a delegated invite) and one unattributed (CLI).
	if _, _, _, err := api.MintEnrolmentTokenForUserBy(ctx, d, "user-1", "member-1", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := api.MintEnrolmentTokenForUser(ctx, d, "user-1", time.Hour); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	enrolmentTokensListHandler(d)(rec, httptest.NewRequest(http.MethodGet, "/api/org/enrolment-tokens", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Tokens []enrolmentTokenDTO `json:"tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Tokens) != 2 {
		t.Fatalf("tokens = %d, want 2", len(resp.Tokens))
	}
	var attributed, unattributed int
	for _, tok := range resp.Tokens {
		if tok.UserEmail != "dev@acme.example" {
			t.Errorf("user_email = %q", tok.UserEmail)
		}
		if tok.Redeemed {
			t.Error("a freshly minted token reports redeemed=true")
		}
		switch tok.MintedByEmail {
		case "member@acme.example":
			attributed++
		case "":
			unattributed++
		default:
			t.Errorf("unexpected minted_by_email %q", tok.MintedByEmail)
		}
		// The secret half is never stored, so it can never be listed.
		if bytes.Contains(rec.Body.Bytes(), []byte(`"token"`)) {
			t.Fatal("the tokens list leaked a token field — only token_id is non-secret")
		}
	}
	if attributed != 1 || unattributed != 1 {
		t.Errorf("attributed=%d unattributed=%d, want 1/1", attributed, unattributed)
	}
}

// TestTokenListKeepsInviterAfterDeprovision pins codex finding 6 (MEDIUM):
// org_members rows cascade-delete, and the tokens list joined the inviter's
// email with a two-argument COALESCE that fell back to the empty string —
// so deleting the inviter blanked the
// attribution on every token they had minted, exactly when an admin most
// needs to know who handed out the outstanding credentials.
//
// enrolment_tokens.minted_by is deliberately NOT a foreign key so the id
// survives (migration 023); the read now falls back to it. Reverting the
// COALESCE to two arguments fires this test.
func TestTokenListKeepsInviterAfterDeprovision(t *testing.T) {
	d := newInviteTestDB(t)
	ctx := context.Background()
	seedInviteMember(t, d, "member-1", "member@acme.example", 1)
	seedInviteMember(t, d, "user-1", "dev@acme.example", 1)
	if _, _, _, err := api.MintEnrolmentTokenForUserBy(ctx, d, "user-1", "member-1", time.Hour); err != nil {
		t.Fatal(err)
	}

	// SCIM deletes the inviter. The token they minted is still outstanding.
	if _, err := d.Exec(`DELETE FROM org_members WHERE user_id = 'member-1'`); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	enrolmentTokensListHandler(d)(rec, httptest.NewRequest(http.MethodGet, "/api/org/enrolment-tokens", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Tokens []enrolmentTokenDTO `json:"tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Tokens) != 1 {
		t.Fatalf("tokens = %d, want 1", len(resp.Tokens))
	}
	if got := resp.Tokens[0].MintedByEmail; got != "member-1" {
		t.Errorf("minted_by_email after inviter deletion = %q, want the raw id \"member-1\" — attribution must survive a deprovision", got)
	}
}

// newInviteMux assembles the real route table with a real DB, session
// manager, and handlers so the gates under test are the MOUNTED ones. The
// SAML/SCIM/dashboard deps stay nil: no route this test exercises calls them.
func newInviteMux(t *testing.T, d *sql.DB, memberInvites bool, cap int) (http.Handler, *auth.SessionManager) {
	t.Helper()
	org, err := orgdb.EnsureOrg(context.Background(), d, "https://org.example")
	if err != nil {
		t.Fatal(err)
	}
	priv, _, err := auth.GenerateSigningKeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := auth.NewIssuer(priv, org.ExternalURL, 90*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := auth.NewSessionManager(bytes.Repeat([]byte("k"), 32), auth.DefaultSessionTTL, false)
	if err != nil {
		t.Fatal(err)
	}
	handlers := api.New(d, issuer, org, 7*24*time.Hour, nil)
	handlers.SetInviteOptions(api.InviteOptions{MemberInvites: memberInvites, MonthlyCap: cap})
	return buildMux(buildDeps{
		handlers:      handlers,
		sessions:      sessions,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		db:            d,
		adminEmails:   []string{"admin@acme.example"},
		memberInvites: memberInvites,
	}), sessions
}

// sessionRequest issues a session cookie for userID and attaches it to req.
func sessionRequest(t *testing.T, sessions *auth.SessionManager, userID, method, path, body string) *http.Request {
	t.Helper()
	rec := httptest.NewRecorder()
	if err := sessions.Issue(rec, userID); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	for _, c := range rec.Result().Cookies() {
		req.AddCookie(c)
	}
	return req
}

// TestMintRouteGateThroughMux is the ROUTE-LEVEL proof of the default
// posture: through the assembled mux, a non-admin member's mint is 403 when
// [server].member_invites is false and 201 when it is true, while the admin
// path is 201 in both. This is the mutation-proof anchor for "flag-off member
// mint accepted": removing either the middleware branch or the handler's
// defence-in-depth check flips the first case to 201 and fires here.
func TestMintRouteGateThroughMux(t *testing.T) {
	t.Run("invites off", func(t *testing.T) {
		d := newInviteTestDB(t)
		seedInviteMember(t, d, "admin-1", "admin@acme.example", 1)
		seedInviteMember(t, d, "member-1", "member@acme.example", 1)
		seedInviteMember(t, d, "user-1", "dev@acme.example", 1)
		mux, sessions := newInviteMux(t, d, false, 10)

		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, sessionRequest(t, sessions, "member-1", http.MethodPost,
			"/api/org/enrolment-tokens", `{"user_id":"user-1"}`))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("member mint (invites off): code=%d body=%s, want 403", rec.Code, rec.Body.String())
		}

		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, sessionRequest(t, sessions, "admin-1", http.MethodPost,
			"/api/org/enrolment-tokens", `{"user_id":"user-1"}`))
		if rec.Code != http.StatusCreated {
			t.Fatalf("admin mint (invites off): code=%d body=%s, want 201", rec.Code, rec.Body.String())
		}

		// No session at all is still 401, not 403.
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/org/enrolment-tokens",
			bytes.NewReader([]byte(`{"user_id":"user-1"}`))))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous mint: code=%d, want 401", rec.Code)
		}
	})

	t.Run("invites on", func(t *testing.T) {
		d := newInviteTestDB(t)
		seedInviteMember(t, d, "admin-1", "admin@acme.example", 1)
		seedInviteMember(t, d, "member-1", "member@acme.example", 1)
		seedInviteMember(t, d, "gone-1", "gone@acme.example", 0)
		seedInviteMember(t, d, "user-1", "dev@acme.example", 1)
		mux, sessions := newInviteMux(t, d, true, 10)

		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, sessionRequest(t, sessions, "member-1", http.MethodPost,
			"/api/org/enrolment-tokens", `{"email":"dev@acme.example"}`))
		if rec.Code != http.StatusCreated {
			t.Fatalf("member mint (invites on): code=%d body=%s, want 201", rec.Code, rec.Body.String())
		}

		// A deprovisioned member is still refused.
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, sessionRequest(t, sessions, "gone-1", http.MethodPost,
			"/api/org/enrolment-tokens", `{"email":"dev@acme.example"}`))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("deprovisioned member mint: code=%d, want 403", rec.Code)
		}
	})
}

// TestAgentInviteRouteRequiresBearer pins that the node-facing invite
// endpoint is mounted behind the enrolment bearer — an unauthenticated POST
// is 401, never a mint. It is the mutation-proof anchor for the endpoint's
// auth mount.
func TestAgentInviteRouteRequiresBearer(t *testing.T) {
	d := newInviteTestDB(t)
	seedInviteMember(t, d, "user-1", "dev@acme.example", 1)
	mux, _ := newInviteMux(t, d, true, 10)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/agent/invite-token",
		bytes.NewReader([]byte(`{"email":"dev@acme.example"}`))))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("agent invite without bearer: code=%d body=%s, want 401", rec.Code, rec.Body.String())
	}
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM enrolment_tokens`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("unauthenticated agent invite minted %d tokens", n)
	}
}
