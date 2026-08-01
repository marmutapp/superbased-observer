package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/orgcontract"
	"github.com/marmutapp/superbased-observer/internal/orgserver/auth"
	"github.com/marmutapp/superbased-observer/internal/orgserver/rollup"
)

// agentInviteRequestFor builds a POST /api/agent/invite-token request carrying
// the bearer claims auth.RequireBearer would have injected.
func agentInviteRequestFor(sub, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/agent/invite-token", bytes.NewReader([]byte(body)))
	return r.WithContext(auth.ContextWithClaims(r.Context(), orgcontract.BearerClaims{Sub: sub}))
}

// countAudit returns how many invite_minted audit rows the given actor has.
func countAudit(t *testing.T, h *Handlers, actor string) int {
	t.Helper()
	var n int
	if err := h.store.db.QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE action = ? AND actor_user_id = ?`,
		rollup.ActionInviteMinted, actor,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestMintAdminPathUnchangedWhenInvitesOff pins that the delegated-invite work
// did not move the admin surface: with member_invites OFF (the zero value of
// InviteOptions — nothing configured at all) an admin mint still succeeds.
func TestMintAdminPathUnchangedWhenInvitesOff(t *testing.T) {
	h, d := newTestHandlers(t)
	seedMember(t, d, "admin-1", "admin@acme.example")
	seedMember(t, d, "user-1", "dev@acme.example")

	rec := httptest.NewRecorder()
	h.MintEnrolmentToken(rec, mintRequest(auth.InviteRoleAdmin, "admin-1", `{"user_id":"user-1"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin mint with invites off: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp mintTokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Token == "" || resp.UserID != "user-1" {
		t.Fatalf("admin mint response = %+v", resp)
	}
	// An admin mint is uncapped, so the cap counters stay absent.
	if resp.MonthlyCap != 0 || resp.MintedMonth != 0 {
		t.Errorf("admin mint reported cap counters %+v — admin mints are uncapped", resp)
	}
	// ...but it IS attributed and audited.
	var mintedBy string
	if err := d.QueryRow(`SELECT COALESCE(minted_by,'') FROM enrolment_tokens WHERE id = ?`, resp.TokenID).Scan(&mintedBy); err != nil {
		t.Fatal(err)
	}
	if mintedBy != "admin-1" {
		t.Errorf("minted_by = %q, want admin-1", mintedBy)
	}
	if n := countAudit(t, h, "admin-1"); n != 1 {
		t.Errorf("invite_minted audit rows = %d, want 1", n)
	}
}

// TestMemberMintRefusedWhenFlagOff is the mutation-proof anchor for the
// server-side flag: a MEMBER-role request must be refused when
// [server].member_invites is off. The gate that stamps the role
// (auth.RequireInviterSAML) is the primary guard; this pins the handler side
// so a member role can never be minted against a delegation-off server.
func TestMemberMintRefusedWhenFlagOff(t *testing.T) {
	h, d := newTestHandlers(t)
	seedMember(t, d, "member-1", "member@acme.example")
	seedMember(t, d, "user-1", "dev@acme.example")
	// InviteOptions deliberately NOT set — the default posture.

	rec := httptest.NewRecorder()
	h.MintEnrolmentToken(rec, mintRequest(auth.InviteRoleMember, "member-1", `{"email":"dev@acme.example"}`))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member mint with invites off: code=%d body=%s, want 403", rec.Code, rec.Body.String())
	}
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM enrolment_tokens`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("refused member mint left %d token rows, want 0", n)
	}
}

// TestMemberMintAllowedWhenFlagOn is the positive half: with the flag on, a
// member may mint for an existing member, addressed by email, and the result
// is attributed + audited + counted.
func TestMemberMintAllowedWhenFlagOn(t *testing.T) {
	h, d := newTestHandlers(t)
	h.SetInviteOptions(InviteOptions{MemberInvites: true, MonthlyCap: 10})
	seedMember(t, d, "member-1", "member@acme.example")
	seedMember(t, d, "user-1", "Dev@Acme.Example") // stored with different case

	rec := httptest.NewRecorder()
	h.MintEnrolmentToken(rec, mintRequest(auth.InviteRoleMember, "member-1", `{"email":"dev@acme.example"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("member mint: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp mintTokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.UserID != "user-1" {
		t.Errorf("resolved user_id = %q, want user-1 (case-insensitive email match)", resp.UserID)
	}
	if resp.MonthlyCap != 10 || resp.MintedMonth != 1 {
		t.Errorf("cap counters = %d/%d, want 1/10", resp.MintedMonth, resp.MonthlyCap)
	}
	var mintedBy string
	if err := d.QueryRow(`SELECT COALESCE(minted_by,'') FROM enrolment_tokens WHERE id = ?`, resp.TokenID).Scan(&mintedBy); err != nil {
		t.Fatal(err)
	}
	if mintedBy != "member-1" {
		t.Errorf("minted_by = %q, want member-1", mintedBy)
	}
	if n := countAudit(t, h, "member-1"); n != 1 {
		t.Errorf("invite_minted audit rows = %d, want 1", n)
	}
	// The audit row names the invited user and the surface used.
	var detail string
	if err := d.QueryRow(`SELECT target_detail FROM audit_log WHERE action = ?`, rollup.ActionInviteMinted).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("invited=user-1 token_id=%s via=member", resp.TokenID)
	if detail != want {
		t.Errorf("audit target_detail = %q, want %q", detail, want)
	}
}

// TestMemberMintRefusesUnknownEmail pins the SCIM-first boundary: an invite is
// a token handoff, not account creation.
func TestMemberMintRefusesUnknownEmail(t *testing.T) {
	h, d := newTestHandlers(t)
	h.SetInviteOptions(InviteOptions{MemberInvites: true, MonthlyCap: 10})
	seedMember(t, d, "member-1", "member@acme.example")

	rec := httptest.NewRecorder()
	h.MintEnrolmentToken(rec, mintRequest(auth.InviteRoleMember, "member-1", `{"email":"stranger@acme.example"}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown email: code=%d body=%s, want 404", rec.Code, rec.Body.String())
	}
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM org_members WHERE email='stranger@acme.example'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("a refused invite created a member — invites must never provision")
	}
}

// TestMemberMintCapEnforced is the mutation-proof anchor for the cap: the
// (cap+1)th delegated mint in a month is refused with 429, and it leaves no
// token row.
func TestMemberMintCapEnforced(t *testing.T) {
	h, d := newTestHandlers(t)
	h.SetInviteOptions(InviteOptions{MemberInvites: true, MonthlyCap: 2})
	seedMember(t, d, "member-1", "member@acme.example")
	seedMember(t, d, "user-1", "dev@acme.example")

	for i := 1; i <= 2; i++ {
		rec := httptest.NewRecorder()
		h.MintEnrolmentToken(rec, mintRequest(auth.InviteRoleMember, "member-1", `{"user_id":"user-1"}`))
		if rec.Code != http.StatusCreated {
			t.Fatalf("mint %d: code=%d body=%s", i, rec.Code, rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	h.MintEnrolmentToken(rec, mintRequest(auth.InviteRoleMember, "member-1", `{"user_id":"user-1"}`))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over-cap mint: code=%d body=%s, want 429", rec.Code, rec.Body.String())
	}
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM enrolment_tokens WHERE minted_by='member-1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("token rows for member-1 = %d, want 2 (the over-cap mint must not land)", n)
	}

	// A DIFFERENT member has their own budget — the cap is per-actor.
	seedMember(t, d, "member-2", "member2@acme.example")
	rec = httptest.NewRecorder()
	h.MintEnrolmentToken(rec, mintRequest(auth.InviteRoleMember, "member-2", `{"user_id":"user-1"}`))
	if rec.Code != http.StatusCreated {
		t.Errorf("second member's first mint: code=%d, want 201 (the cap is per-actor)", rec.Code)
	}

	// An ADMIN is not capped even after the member ceiling is hit.
	seedMember(t, d, "admin-1", "admin@acme.example")
	for i := 0; i < 3; i++ {
		rec = httptest.NewRecorder()
		h.MintEnrolmentToken(rec, mintRequest(auth.InviteRoleAdmin, "admin-1", `{"user_id":"user-1"}`))
		if rec.Code != http.StatusCreated {
			t.Fatalf("admin mint %d: code=%d, want 201 (admins are uncapped)", i, rec.Code)
		}
	}
}

// TestCapCountsOnlyCurrentMonth pins the window: mints from a previous month
// do not consume this month's budget.
func TestCapCountsOnlyCurrentMonth(t *testing.T) {
	h, d := newTestHandlers(t)
	h.SetInviteOptions(InviteOptions{MemberInvites: true, MonthlyCap: 1})
	seedMember(t, d, "member-1", "member@acme.example")
	seedMember(t, d, "user-1", "dev@acme.example")

	// Backdate a mint into last month. NOT AddDate(0,-1,0) on "now": Go
	// normalises 31 June to 1 July, so on a 31st that lands back INSIDE the
	// current month and the test would assert nothing. Anchor on the last
	// day of the previous month instead.
	now := time.Now().UTC()
	last := time.Date(now.Year(), now.Month(), 1, 12, 0, 0, 0, time.UTC).
		AddDate(0, 0, -1).Format(time.RFC3339Nano)
	if _, err := d.Exec(
		`INSERT INTO enrolment_tokens (id, token_hash, user_id, minted_by, created_at, expires_at)
		 VALUES ('old','h','user-1','member-1',?,?)`, last, last,
	); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.MintEnrolmentToken(rec, mintRequest(auth.InviteRoleMember, "member-1", `{"user_id":"user-1"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint after last month's token: code=%d body=%s, want 201", rec.Code, rec.Body.String())
	}
}

// TestAgentInviteRefusedWhenFlagOff is the mutation-proof anchor for the
// bearer endpoint: it is unreachable unless the org opted in, even for a
// perfectly valid enrolment bearer.
func TestAgentInviteRefusedWhenFlagOff(t *testing.T) {
	h, d := newTestHandlers(t)
	seedMember(t, d, "member-1", "member@acme.example")
	seedMember(t, d, "user-1", "dev@acme.example")

	rec := httptest.NewRecorder()
	h.MintInviteToken(rec, agentInviteRequestFor("member-1", `{"email":"dev@acme.example"}`))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("agent invite with invites off: code=%d body=%s, want 403", rec.Code, rec.Body.String())
	}
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM enrolment_tokens`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("refused agent invite left %d token rows, want 0", n)
	}
	if n := countAudit(t, h, "member-1"); n != 0 {
		t.Errorf("refused agent invite wrote %d audit rows, want 0", n)
	}
}

// TestAgentInviteFlow covers the enabled path end to end: mint, attribution,
// audit, cap counters, and the cap ceiling — the agent surface is ALWAYS
// capped, with no admin exemption (a node bearer is a machine credential).
func TestAgentInviteFlow(t *testing.T) {
	h, d := newTestHandlers(t)
	h.SetInviteOptions(InviteOptions{MemberInvites: true, MonthlyCap: 1})
	seedMember(t, d, "member-1", "member@acme.example")
	seedMember(t, d, "user-1", "dev@acme.example")

	rec := httptest.NewRecorder()
	h.MintInviteToken(rec, agentInviteRequestFor("member-1", `{"email":"dev@acme.example"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("agent invite: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp agentInviteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Token == "" || resp.UserID != "user-1" || resp.UserEmail != "dev@acme.example" {
		t.Fatalf("agent invite response = %+v", resp)
	}
	if resp.MintedMonth != 1 || resp.MonthlyCap != 1 {
		t.Errorf("cap counters = %d/%d, want 1/1", resp.MintedMonth, resp.MonthlyCap)
	}
	if n := countAudit(t, h, "member-1"); n != 1 {
		t.Errorf("invite_minted audit rows = %d, want 1", n)
	}
	var detail string
	if err := d.QueryRow(`SELECT target_detail FROM audit_log WHERE action = ?`, rollup.ActionInviteMinted).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("invited=user-1 token_id=%s via=agent", resp.TokenID); detail != want {
		t.Errorf("audit target_detail = %q, want %q", detail, want)
	}

	// Over cap.
	rec = httptest.NewRecorder()
	h.MintInviteToken(rec, agentInviteRequestFor("member-1", `{"email":"dev@acme.example"}`))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over-cap agent invite: code=%d, want 429", rec.Code)
	}
}

// TestAgentInviteRequiresKnownEmail pins the SCIM-first boundary on the agent
// surface too, and TestAgentInviteRequiresClaims pins the fail-closed backstop
// for a missing bearer identity.
func TestAgentInviteRequiresKnownEmail(t *testing.T) {
	h, d := newTestHandlers(t)
	h.SetInviteOptions(InviteOptions{MemberInvites: true, MonthlyCap: 5})
	seedMember(t, d, "member-1", "member@acme.example")

	rec := httptest.NewRecorder()
	h.MintInviteToken(rec, agentInviteRequestFor("member-1", `{"email":"stranger@acme.example"}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown email: code=%d, want 404", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.MintInviteToken(rec, agentInviteRequestFor("member-1", `{}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing email: code=%d, want 400", rec.Code)
	}
}

func TestAgentInviteRequiresClaims(t *testing.T) {
	h, d := newTestHandlers(t)
	h.SetInviteOptions(InviteOptions{MemberInvites: true, MonthlyCap: 5})
	seedMember(t, d, "user-1", "dev@acme.example")

	rec := httptest.NewRecorder()
	h.MintInviteToken(rec, httptest.NewRequest(http.MethodPost, "/api/agent/invite-token",
		bytes.NewReader([]byte(`{"email":"dev@acme.example"}`))))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("agent invite without claims: code=%d, want 401", rec.Code)
	}
}

// TestInviteOptionsCapNeverUnlimited pins that a mis-wired assembly (cap 0 or
// negative) falls back to the built-in default rather than meaning "no limit".
func TestInviteOptionsCapNeverUnlimited(t *testing.T) {
	for _, in := range []int{0, -1, -100} {
		if got := (InviteOptions{MonthlyCap: in}).cap(); got != defaultInviteMonthlyCap {
			t.Errorf("InviteOptions{MonthlyCap:%d}.cap() = %d, want %d", in, got, defaultInviteMonthlyCap)
		}
	}
	if got := (InviteOptions{MonthlyCap: 3}).cap(); got != 3 {
		t.Errorf("explicit cap = %d, want 3", got)
	}
}

// TestMintTargetAddressingIsExclusive pins that user_id and email cannot both
// be supplied (an ambiguous request is a bug, not a preference order).
func TestMintTargetAddressingIsExclusive(t *testing.T) {
	h, d := newTestHandlers(t)
	seedMember(t, d, "user-1", "dev@acme.example")
	rec := httptest.NewRecorder()
	h.MintEnrolmentToken(rec, mintRequest(auth.InviteRoleAdmin, "admin-1",
		`{"user_id":"user-1","email":"dev@acme.example"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("both addressing modes: code=%d, want 400", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.MintEnrolmentToken(rec, mintRequest(auth.InviteRoleAdmin, "admin-1", `{}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("neither addressing mode: code=%d, want 400", rec.Code)
	}
}

// TestMintRefusesInactiveMember pins that a deprovisioned target cannot be
// invited — the token would enrol an agent as a user SCIM has switched off.
func TestMintRefusesInactiveMember(t *testing.T) {
	h, d := newTestHandlers(t)
	seedMember(t, d, "user-1", "dev@acme.example")
	if _, err := d.Exec(`UPDATE org_members SET active = 0 WHERE user_id = 'user-1'`); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.MintEnrolmentToken(rec, mintRequest(auth.InviteRoleAdmin, "admin-1", `{"user_id":"user-1"}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("inactive target: code=%d body=%s, want 404", rec.Code, rec.Body.String())
	}
}
