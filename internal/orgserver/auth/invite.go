package auth

import (
	"context"
	"net/http"
)

// InviteRole records HOW the caller of the enrolment-token mint endpoint was
// authorized. It is set by RequireInviterSAML and read by the mint handler,
// which needs the distinction to decide whether the per-member monthly cap
// applies. Delegated invites are Arc 2 of
// docs/plans/tier3-local-contract-and-teams-invite-plan-2026-07-31.md.
//
// The zero value is InviteRoleNone and means "no invite authorization was
// established". The mint handler REFUSES that case: authority for this route
// is granted in exactly one place (the middleware), so a handler reached
// without it — a mis-mount, a future route added behind the weaker
// RequireSAMLSession — fails closed rather than minting for anyone with an
// SSO session.
type InviteRole int

const (
	// InviteRoleNone means no invite authorization was established.
	InviteRoleNone InviteRole = iota
	// InviteRoleAdmin is an org admin per the AdminChecker: the pre-existing
	// authority, unchanged by the delegated-invite work and uncapped.
	InviteRoleAdmin
	// InviteRoleMember is a non-admin SAML member admitted only because
	// [server].member_invites is on. Capped and audited.
	InviteRoleMember
)

// String renders the role for audit/log lines.
func (r InviteRole) String() string {
	switch r {
	case InviteRoleAdmin:
		return "admin"
	case InviteRoleMember:
		return "member"
	default:
		return "none"
	}
}

// ctxKeyInviteRole is the context key carrying the InviteRole. It is a
// distinct unexported key type value from middleware.go's ctxKey set, so
// nothing outside this package can forge it.
type inviteCtxKey int

const ctxKeyInviteRole inviteCtxKey = 0

// ContextWithInviteRole returns ctx carrying role. RequireInviterSAML uses it
// after authorizing; it is also the seam handler tests use to construct an
// authorized context without standing up the middleware chain.
func ContextWithInviteRole(ctx context.Context, role InviteRole) context.Context {
	return context.WithValue(ctx, ctxKeyInviteRole, role)
}

// InviteRoleFromContext returns the role placed by RequireInviterSAML.
// It returns InviteRoleNone when no role was established.
func InviteRoleFromContext(ctx context.Context) InviteRole {
	v, ok := ctx.Value(ctxKeyInviteRole).(InviteRole)
	if !ok {
		return InviteRoleNone
	}
	return v
}

// RequireInviterSAML guards POST /api/org/enrolment-tokens. It is
// RequireAdminSAML plus ONE relaxation, taken only when the org has opted in:
//
//   - no valid SAML session            → onUnauthorized (401)
//   - session user is NOT active       → onForbidden (403)   [any role]
//   - session user is an org admin     → pass, InviteRoleAdmin
//   - memberInvites is false           → onForbidden (403)   [default posture]
//   - session user is an ACTIVE member → pass, InviteRoleMember
//   - otherwise                        → onForbidden (403)
//
// memberInvites is a plain bool read once at assembly from
// [server].member_invites: the switch is server-config, never a request
// parameter and never remotely togglable.
//
// THE ACTIVE CHECK IS FIRST, AND IT APPLIES TO ADMINS TOO. It used to guard
// only the delegated (member) branch, which left a real hole: SAML sessions
// outlive SCIM, so an allow-listed admin who had been DEACTIVATED — the exact
// user whose authority an off-boarding is meant to remove — could keep
// minting enrolment tokens from a still-valid cookie, and uncapped, because
// the admin branch returned before any liveness lookup. Deactivation must
// revoke authority, not just the ability to log in again; an admin's
// membership row is checked exactly like a member's.
//
// isActiveMember is checked separately from the session because SAML
// JIT-creates a member row on first login and SCIM may later deactivate it;
// a deprovisioned-but-still-cookied user must not be able to hand out
// enrolment tokens.
func RequireInviterSAML(sessions *SessionManager, isAdmin, isActiveMember AdminChecker, memberInvites bool, onUnauthorized, onForbidden http.HandlerFunc) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			userID, err := sessions.UserID(r)
			if err != nil {
				onUnauthorized(w, r)
				return
			}
			ctx := ContextWithUserID(r.Context(), userID)
			active, err := isActiveMember(ctx, userID)
			if err != nil || !active {
				onForbidden(w, r)
				return
			}
			admin, err := isAdmin(ctx, userID)
			if err != nil {
				onForbidden(w, r)
				return
			}
			if admin {
				next.ServeHTTP(w, r.WithContext(ContextWithInviteRole(ctx, InviteRoleAdmin)))
				return
			}
			if !memberInvites {
				onForbidden(w, r)
				return
			}
			next.ServeHTTP(w, r.WithContext(ContextWithInviteRole(ctx, InviteRoleMember)))
		})
	}
}
