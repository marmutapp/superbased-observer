package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// roleCapture is an okHandler that records the InviteRole it was reached with.
func roleCapture(got *InviteRole) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = InviteRoleFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
}

func inviteCheckers() (isAdmin, isActiveMember AdminChecker) {
	admins := map[string]bool{"admin-1": true}
	active := map[string]bool{"admin-1": true, "member-1": true}
	return func(_ context.Context, id string) (bool, error) { return admins[id], nil },
		func(_ context.Context, id string) (bool, error) { return active[id], nil }
}

// TestRequireInviterSAML_DefaultIsAdminOnly pins the default posture: with
// memberInvites false the gate is byte-for-byte RequireAdminSAML — an admin
// passes, a valid non-admin session is 403, no session is 401.
//
// This is the MUTATION-PROOF anchor for the flag: deleting the
// `if !memberInvites { onForbidden }` branch in RequireInviterSAML makes the
// "member, invites off" case pass through and this test fires.
func TestRequireInviterSAML_DefaultIsAdminOnly(t *testing.T) {
	sessions := newTestSessions(t)
	isAdmin, isMember := inviteCheckers()
	var got InviteRole
	guarded := RequireInviterSAML(sessions, isAdmin, isMember, false,
		JSONUnauthorized(), JSONForbidden())(roleCapture(&got))

	got = InviteRoleNone
	rec := httptest.NewRecorder()
	guarded.ServeHTTP(rec, roundTrip(t, sessions, "admin-1"))
	if rec.Code != http.StatusOK || got != InviteRoleAdmin {
		t.Errorf("admin (invites off): code=%d role=%v, want 200/admin", rec.Code, got)
	}

	got = InviteRoleNone
	rec = httptest.NewRecorder()
	guarded.ServeHTTP(rec, roundTrip(t, sessions, "member-1"))
	if rec.Code != http.StatusForbidden {
		t.Errorf("member (invites off): code=%d, want 403", rec.Code)
	}
	if got != InviteRoleNone {
		t.Errorf("refused member still reached the handler with role %v", got)
	}

	rec = httptest.NewRecorder()
	guarded.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/org/enrolment-tokens", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no session: code=%d, want 401", rec.Code)
	}
}

// TestRequireInviterSAML_MemberAdmittedWhenEnabled pins the single relaxation
// and its bounds: an active member passes AS A MEMBER (so the handler applies
// the cap), an admin still passes as an admin, and a session user who is not
// an active member is still refused.
func TestRequireInviterSAML_MemberAdmittedWhenEnabled(t *testing.T) {
	sessions := newTestSessions(t)
	isAdmin, isMember := inviteCheckers()
	var got InviteRole
	guarded := RequireInviterSAML(sessions, isAdmin, isMember, true,
		JSONUnauthorized(), JSONForbidden())(roleCapture(&got))

	got = InviteRoleNone
	rec := httptest.NewRecorder()
	guarded.ServeHTTP(rec, roundTrip(t, sessions, "member-1"))
	if rec.Code != http.StatusOK || got != InviteRoleMember {
		t.Errorf("member (invites on): code=%d role=%v, want 200/member", rec.Code, got)
	}

	got = InviteRoleNone
	rec = httptest.NewRecorder()
	guarded.ServeHTTP(rec, roundTrip(t, sessions, "admin-1"))
	if rec.Code != http.StatusOK || got != InviteRoleAdmin {
		t.Errorf("admin (invites on): code=%d role=%v, want 200/admin — the admin path must not be downgraded", rec.Code, got)
	}

	// A SAML-JIT'd but DEPROVISIONED user: has a session, is not an active
	// member → refused even with the flag on.
	got = InviteRoleNone
	rec = httptest.NewRecorder()
	guarded.ServeHTTP(rec, roundTrip(t, sessions, "ghost-9"))
	if rec.Code != http.StatusForbidden {
		t.Errorf("inactive member (invites on): code=%d, want 403", rec.Code)
	}
}

// TestRequireInviterSAML_DeactivatedAdminRefused is the MUTATION-PROOF anchor
// for the deactivated-admin hole (codex finding 2, HIGH).
//
// The admin branch used to return BEFORE any liveness lookup, so a SCIM-
// deactivated admin holding a still-valid SAML cookie kept minting enrolment
// tokens — uncapped, because admins are exempt from the monthly cap. That is
// the exact user an off-boarding exists to disarm, and the session outlives
// the SCIM row by design.
//
// Deleting the `active` check at the top of RequireInviterSAML (or moving it
// back below the admin branch) flips this to 200/admin and fires here.
func TestRequireInviterSAML_DeactivatedAdminRefused(t *testing.T) {
	sessions := newTestSessions(t)
	// admin-1 is allow-listed as an admin but SCIM has deactivated them.
	isAdmin := func(_ context.Context, id string) (bool, error) { return id == "admin-1", nil }
	isActiveMember := func(_ context.Context, id string) (bool, error) { return id == "member-1", nil }

	// Both postures must refuse: the flag governs the MEMBER relaxation, it
	// has nothing to do with whether a dead admin still holds authority.
	for _, memberInvites := range []bool{false, true} {
		var got InviteRole
		guarded := RequireInviterSAML(sessions, isAdmin, isActiveMember, memberInvites,
			JSONUnauthorized(), JSONForbidden())(roleCapture(&got))

		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, roundTrip(t, sessions, "admin-1"))
		if rec.Code != http.StatusForbidden {
			t.Errorf("deactivated admin (member_invites=%v): code=%d, want 403", memberInvites, rec.Code)
		}
		if got != InviteRoleNone {
			t.Errorf("deactivated admin (member_invites=%v) reached the handler with role %v", memberInvites, got)
		}
	}

	// The live admin is unaffected — the fix must not cost the admin path.
	var got InviteRole
	guarded := RequireInviterSAML(sessions,
		func(_ context.Context, id string) (bool, error) { return id == "member-1", nil },
		isActiveMember, false, JSONUnauthorized(), JSONForbidden())(roleCapture(&got))
	rec := httptest.NewRecorder()
	guarded.ServeHTTP(rec, roundTrip(t, sessions, "member-1"))
	if rec.Code != http.StatusOK || got != InviteRoleAdmin {
		t.Errorf("active admin: code=%d role=%v, want 200/admin", rec.Code, got)
	}
}

// TestRequireInviterSAML_CheckerErrorsFailClosed pins that a DB failure in
// either checker is a 403, never a pass.
func TestRequireInviterSAML_CheckerErrorsFailClosed(t *testing.T) {
	sessions := newTestSessions(t)
	boom := func(_ context.Context, _ string) (bool, error) { return false, errors.New("db down") }
	_, isMember := inviteCheckers()

	rec := httptest.NewRecorder()
	RequireInviterSAML(sessions, boom, isMember, true, JSONUnauthorized(), JSONForbidden())(okHandler()).
		ServeHTTP(rec, roundTrip(t, sessions, "admin-1"))
	if rec.Code != http.StatusForbidden {
		t.Errorf("admin-checker error: code=%d, want 403 (fail-closed)", rec.Code)
	}

	isAdmin, _ := inviteCheckers()
	rec = httptest.NewRecorder()
	RequireInviterSAML(sessions, isAdmin, boom, true, JSONUnauthorized(), JSONForbidden())(okHandler()).
		ServeHTTP(rec, roundTrip(t, sessions, "member-1"))
	if rec.Code != http.StatusForbidden {
		t.Errorf("member-checker error: code=%d, want 403 (fail-closed)", rec.Code)
	}
}

// TestInviteRoleContextRoundTrip pins the accessor's zero behaviour: a context
// with no role reports InviteRoleNone, which the mint handler treats as
// "no authority established".
func TestInviteRoleContextRoundTrip(t *testing.T) {
	if got := InviteRoleFromContext(context.Background()); got != InviteRoleNone {
		t.Errorf("bare context role = %v, want none", got)
	}
	for _, r := range []InviteRole{InviteRoleAdmin, InviteRoleMember} {
		if got := InviteRoleFromContext(ContextWithInviteRole(context.Background(), r)); got != r {
			t.Errorf("round trip %v = %v", r, got)
		}
	}
	if InviteRoleAdmin.String() != "admin" || InviteRoleMember.String() != "member" || InviteRoleNone.String() != "none" {
		t.Error("InviteRole.String() vocabulary drifted — it lands in audit/log lines")
	}
}
