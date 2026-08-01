// invite.go — delegated enrolment-token minting (Arc 2 of
// docs/plans/tier3-local-contract-and-teams-invite-plan-2026-07-31.md).
//
// Two callers reach the same core:
//
//   - a SAML member on POST /api/org/enrolment-tokens, admitted by
//     auth.RequireInviterSAML when [server].member_invites is on;
//   - an enrolled agent on POST /api/agent/invite-token, admitted by the
//     enrolment bearer, gated on the SAME flag.
//
// Neither creates an account. The target must already be an ACTIVE org
// member; an unknown email is a 404, and that is the deliberate v1 boundary
// (no email self-signup — see config.ServerConfig.MemberInvites).

package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/orgcontract"
	"github.com/marmutapp/superbased-observer/internal/orgserver/auth"
	"github.com/marmutapp/superbased-observer/internal/orgserver/rollup"
)

// maxInviteRequestBytes caps the invite request body. An invite is an email
// and a TTL; 64 KiB is three orders of magnitude of headroom.
const maxInviteRequestBytes = 64 << 10

// ErrInviteCapReached is returned when the actor has already minted
// [server].member_invite_monthly_cap tokens in the current UTC calendar
// month.
var ErrInviteCapReached = errors.New("api: monthly invite cap reached")

// ErrInviteAttemptBudget is returned when the actor has burned
// inviteAttemptBudget FAILED target resolutions inside inviteAttemptWindow.
// See migration 024 for why the oracle's RATE is bounded rather than its
// fidelity.
var ErrInviteAttemptBudget = errors.New("api: invite attempt budget exhausted")

// ErrInvitesDisabled is returned when a delegated mint is attempted while
// [server].member_invites is false.
var ErrInvitesDisabled = errors.New("api: delegated invites are disabled")

// Invite TTL bounds for an EXPLICIT ttl_days. An omitted ttl_days keeps the
// server's [enrolment].default_token_lifetime_days; anything supplied must
// land in [minInviteTTLDays, maxInviteTTLDays].
//
// The ceiling exists because ttl_days was unbounded: `{"ttl_days":100000}`
// minted a 273-year enrolment token, and a large enough value overflows the
// int64 nanoseconds of a time.Duration into a PAST expiry. 90 days is
// deliberately wider than the web2 form's 30 so an admin with a slow
// onboarding window has headroom without editing server config; anything
// longer is a standing credential, not an invite.
const (
	minInviteTTLDays = 1
	maxInviteTTLDays = 90
)

// Invite attempt budget: the per-inviter ceiling on FAILED target
// resolutions, and the rolling window it is measured over. Constants rather
// than config on purpose — this is a safety floor, not a policy knob, and a
// server operator has no legitimate reason to widen it (20 typos an hour is
// already an order of magnitude above human).
const (
	inviteAttemptBudget = 20
	inviteAttemptWindow = time.Hour
)

// InviteOptions is the server-config half of the delegated-invite feature,
// installed once at assembly via SetInviteOptions. The zero value is the
// safe default: delegation OFF.
//
// Deliberately a struct set at assembly rather than a per-request parameter:
// the switch is org policy read from [server], and there must be no request
// shape that can turn delegation on.
type InviteOptions struct {
	// MemberInvites mirrors [server].member_invites.
	MemberInvites bool
	// MonthlyCap mirrors [server].member_invite_monthly_cap. Values < 1 are
	// treated as the built-in default of 10 so a mis-wired assembly can
	// never mean "unlimited".
	MonthlyCap int
}

// defaultInviteMonthlyCap is the fallback when MonthlyCap is unset/invalid.
// It matches config.Default(); the duplication is intentional — this package
// must not import the server config, and a zero must never read as unlimited.
const defaultInviteMonthlyCap = 10

// cap returns the effective monthly cap.
func (o InviteOptions) cap() int {
	if o.MonthlyCap < 1 {
		return defaultInviteMonthlyCap
	}
	return o.MonthlyCap
}

// SetInviteOptions installs the delegated-invite policy. Call before the
// server starts serving; not safe to call concurrently with requests (the
// same contract as SetOrgPolicyPublicKey).
func (h *Handlers) SetInviteOptions(o InviteOptions) { h.invite = o }

// InviteOptions reports the installed delegated-invite policy (read by the
// route assembly to gate the agent endpoint's registration message and by
// tests).
func (h *Handlers) InviteOptions() InviteOptions { return h.invite }

// inviteResult is the outcome of a successful delegated mint.
type inviteResult struct {
	Token      string
	TokenID    string
	UserID     string
	UserEmail  string
	ExpiresAt  time.Time
	MintedThis int // mints by this actor in the current month, including this one
	MonthlyCap int
}

// inviteTarget is the VALIDATED addressing half of an invite request:
// exactly one of the two fields is non-empty. Producing it is pure (no DB) —
// the actual member lookup happens inside the mint transaction, because
// "does this address resolve" is a decision that must be serialized with the
// attempt-budget write that bounds it.
type inviteTarget struct {
	UserID string
	Email  string
}

// parseInviteTarget validates an invite request's user_id / email addressing.
// Exactly one is required; a request naming both is ambiguous and refused
// rather than given a preference order.
func parseInviteTarget(userID, email string) (inviteTarget, error) {
	userID = strings.TrimSpace(userID)
	email = strings.TrimSpace(email)
	switch {
	case userID != "" && email != "":
		return inviteTarget{}, errors.New("user_id and email are mutually exclusive")
	case userID != "":
		return inviteTarget{UserID: userID}, nil
	case email != "":
		return inviteTarget{Email: email}, nil
	default:
		return inviteTarget{}, errors.New("user_id or email is required")
	}
}

// inviteTTL resolves a request's optional ttl_days to a duration.
//
// days is a POINTER so "absent" and "explicitly zero" are distinguishable:
// absent keeps the server default, an explicit value must be in range. A
// plain int cannot tell `{}` from `{"ttl_days":0}`, and silently treating 0
// as "use the default" would let the bound be bypassed by naming it.
func inviteTTL(days *int, def time.Duration) (time.Duration, error) {
	if days == nil {
		return def, nil
	}
	if *days < minInviteTTLDays || *days > maxInviteTTLDays {
		return 0, fmt.Errorf("ttl_days must be between %d and %d (got %d)", minInviteTTLDays, maxInviteTTLDays, *days)
	}
	return time.Duration(*days) * 24 * time.Hour, nil
}

// mintDelegated is the ONE implementation of a delegated mint, shared by the
// SAML member path and the agent-bearer path.
//
// The expensive half (crypto/rand + argon2id, ~60ms) runs BEFORE the
// transaction; the decisions — attempt budget, target resolution, monthly
// cap, token insert — all run INSIDE one BEGIN IMMEDIATE transaction so a
// read can never be separated from the write it authorises. See
// store.mintInviteAtomically.
//
// The audit row is written AFTER the commit so it can carry the token id. A
// mint whose audit write fails is still reported as an error to the caller —
// an unattributable delegated token is not an outcome worth returning
// successfully — but the token row stays (it is single-use, expiring, and
// visible in the admin tokens list with its minted_by set).
func (h *Handlers) mintDelegated(ctx context.Context, actorUserID string, target inviteTarget, ttl time.Duration, capped bool, sourceIP, via string) (inviteResult, error) {
	s := h.store
	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}
	capLimit := h.invite.cap()
	now := s.now()

	cleartext, tokenID, hash, err := newTokenMaterial()
	if err != nil {
		return inviteResult{}, err
	}
	expiresAt := now.Add(ttl)

	out, err := s.mintInviteAtomically(ctx, inviteMintInput{
		ActorUserID: actorUserID,
		Target:      target,
		Capped:      capped,
		CapLimit:    capLimit,
		TokenID:     tokenID,
		TokenHash:   hash,
		ExpiresAt:   expiresAt,
		Now:         now,
	})
	if err != nil {
		return inviteResult{}, err
	}

	// target_detail names the INVITED user and the mint path; the inviter is
	// actor_user_id. No content, no token material — the token_id is the
	// non-secret lookup key already stored in the clear.
	detail := fmt.Sprintf("invited=%s token_id=%s via=%s", out.TargetUserID, tokenID, via)
	if err := rollup.WriteAudit(ctx, s.db, actorUserID, rollup.ActionInviteMinted, "", detail, sourceIP, now); err != nil {
		return inviteResult{}, err
	}

	return inviteResult{
		Token:      cleartext,
		TokenID:    tokenID,
		UserID:     out.TargetUserID,
		UserEmail:  out.TargetEmail,
		ExpiresAt:  expiresAt,
		MintedThis: out.MintedThisMonth,
		MonthlyCap: capLimit,
	}, nil
}

// inviteMintInput is everything mintInviteAtomically needs. The token
// material is pre-computed by the caller so argon2id never runs while the
// SQLite write lock is held.
type inviteMintInput struct {
	ActorUserID string
	Target      inviteTarget
	Capped      bool
	CapLimit    int
	TokenID     string
	TokenHash   string
	ExpiresAt   time.Time
	Now         time.Time
}

// inviteMintOutput is the resolved target plus this actor's post-mint count.
type inviteMintOutput struct {
	TargetUserID    string
	TargetEmail     string
	MintedThisMonth int
}

// mintInviteAtomically performs the whole delegated-mint decision inside ONE
// immediate transaction on a pinned connection:
//
//  1. prune + count this actor's failed resolutions in the rolling window;
//     over budget ⇒ ErrInviteAttemptBudget (regardless of hit or miss);
//  2. resolve the target; a miss RECORDS an attempt, commits it, and
//     returns ErrUserNotFound;
//  3. when capped, count this actor's mints this UTC month; at or over the
//     cap ⇒ ErrInviteCapReached;
//  4. insert the token row.
//
// Steps 1+2 and 3+4 are read-then-write pairs, and BOTH were previously
// non-atomic: N concurrent requests all read "9 of 10 used" and every one of
// them inserted, overshooting the cap by N-1. BEGIN IMMEDIATE takes SQLite's
// write lock UP FRONT, so the count is serialized with the insert it gates
// and concurrent minters queue behind busy_timeout instead of racing.
//
// The connection is pinned and the transaction opened with an explicit
// "BEGIN IMMEDIATE" rather than sql.DB.BeginTx: the DSN's _txlock=immediate
// is only applied for a file-backed path (an in-memory test DB opens without
// it), and this guarantee must not depend on how the DB was opened. Same
// pattern as orgserver/db.runMigrations.
func (s *store) mintInviteAtomically(ctx context.Context, in inviteMintInput) (inviteMintOutput, error) {
	var out inviteMintOutput

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return out, fmt.Errorf("api.store.mintInviteAtomically: acquire connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "PRAGMA busy_timeout = 30000"); err != nil {
		return out, fmt.Errorf("api.store.mintInviteAtomically: set busy_timeout: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return out, fmt.Errorf("api.store.mintInviteAtomically: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			// Detached from ctx: a cancelled request must still release the
			// write lock before the connection returns to the pool.
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		}
	}()

	now := in.Now.UTC()

	// 1. Attempt budget. Skipped for an unattributed actor: there is no
	// per-actor bucket to charge, and a shared global bucket would be a
	// denial-of-service lever rather than an anti-enumeration one. Every
	// authenticated surface supplies an actor.
	if in.ActorUserID != "" {
		cutoff := now.Add(-inviteAttemptWindow).Format(time.RFC3339Nano)
		if _, err := conn.ExecContext(ctx,
			`DELETE FROM invite_attempts WHERE created_at < ?`, cutoff); err != nil {
			return out, fmt.Errorf("api.store.mintInviteAtomically: prune attempts: %w", err)
		}
		var attempts int
		if err := conn.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM invite_attempts WHERE actor_user_id = ? AND created_at >= ?`,
			in.ActorUserID, cutoff).Scan(&attempts); err != nil {
			return out, fmt.Errorf("api.store.mintInviteAtomically: count attempts: %w", err)
		}
		if attempts >= inviteAttemptBudget {
			return out, ErrInviteAttemptBudget
		}
	}

	// 2. Resolve the target INSIDE the transaction — the 404/201 split is
	// the oracle, so the decision and the attempt row that bounds it must be
	// one atomic step.
	m, ok, err := resolveMemberTarget(ctx, conn, in.Target)
	if err != nil {
		return out, err
	}
	if !ok || !m.Active {
		if in.ActorUserID != "" {
			if _, err := conn.ExecContext(ctx,
				`INSERT INTO invite_attempts (actor_user_id, created_at) VALUES (?, ?)`,
				in.ActorUserID, now.Format(time.RFC3339Nano)); err != nil {
				return out, fmt.Errorf("api.store.mintInviteAtomically: record attempt: %w", err)
			}
			if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
				return out, fmt.Errorf("api.store.mintInviteAtomically: commit attempt: %w", err)
			}
			committed = true
		}
		return out, ErrUserNotFound
	}

	// 3. Monthly cap. Counted from enrolment_tokens, not audit_log: the cap
	// must survive an audit prune, and the token table is the thing being
	// bounded. Bounds are both-sided so a row with a skewed future timestamp
	// cannot be counted into the current month forever.
	used := 0
	if in.Capped && in.ActorUserID != "" {
		start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		end := start.AddDate(0, 1, 0)
		if err := conn.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM enrolment_tokens
			  WHERE minted_by = ? AND created_at >= ? AND created_at < ?`,
			in.ActorUserID, start.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano)).Scan(&used); err != nil {
			return out, fmt.Errorf("api.store.mintInviteAtomically: count mints: %w", err)
		}
		if used >= in.CapLimit {
			return out, ErrInviteCapReached
		}
	}

	// 4. Mint.
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO enrolment_tokens (id, token_hash, user_id, minted_by, created_at, expires_at)
		 VALUES (?, ?, ?, NULLIF(?, ''), ?, ?)`,
		in.TokenID, in.TokenHash, m.UserID, in.ActorUserID,
		now.Format(time.RFC3339Nano), in.ExpiresAt.Format(time.RFC3339Nano)); err != nil {
		return out, fmt.Errorf("api.store.mintInviteAtomically: insert token: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return out, fmt.Errorf("api.store.mintInviteAtomically: commit: %w", err)
	}
	committed = true

	return inviteMintOutput{
		TargetUserID:    m.UserID,
		TargetEmail:     m.Email,
		MintedThisMonth: used + 1,
	}, nil
}

// resolveMemberTarget looks up an invite target on the given querier (the
// pinned mint connection). Email is matched case-insensitively: the address
// arrives from a human typing it into an invite form while the stored one
// came from the IdP, and refusing a correct invite over letter case would be
// a bug, not a security property.
func resolveMemberTarget(ctx context.Context, q rowQuerier, t inviteTarget) (member, bool, error) {
	if t.UserID != "" {
		return memberByIDOn(ctx, q, t.UserID)
	}
	return memberByEmailFoldOn(ctx, q, t.Email)
}

// agentInviteRequest is the POST /api/agent/invite-token body: the teammate's
// email (they must already be a provisioned member) and an optional TTL.
//
// TTLDays is a pointer so an explicit 0 is rejected rather than silently
// meaning "server default" — see inviteTTL.
type agentInviteRequest struct {
	Email   string `json:"email"`
	TTLDays *int   `json:"ttl_days,omitempty"`
}

// agentInviteResponse mirrors mintTokenResponse and adds the cap counters, so
// the node dashboard can show "3 of 10 invites used this month" without a
// second round trip.
type agentInviteResponse struct {
	Token       string `json:"token"`
	TokenID     string `json:"token_id"`
	UserID      string `json:"user_id"`
	UserEmail   string `json:"user_email"`
	ExpiresAt   string `json:"expires_at"`
	MintedMonth int    `json:"minted_this_month"`
	MonthlyCap  int    `json:"monthly_cap"`
}

// MintInviteToken handles POST /api/agent/invite-token — the bearer-
// authenticated sibling of the SAML mint endpoint, added so the NODE
// dashboard can close the invite loop without a browser session on the org
// server (Arc 2 item 3a).
//
// It is a SEPARATE endpoint on purpose: the SAML mint route keeps its own
// (unchanged, admin-first) gate, and this one carries its own, strictly
// narrower authority —
//
//   - gated on the SAME [server].member_invites flag (403 when off);
//   - attributed to the ENROLLED user (the bearer's subject), never to a
//     user id the caller names;
//   - always capped (there is no admin exemption here: a node bearer is a
//     machine credential on a developer's laptop, so it gets the member
//     ceiling even if that developer is an org admin);
//   - audited with the same invite_minted row.
//
// Mounted behind auth.RequireBearer, which has already verified the bearer's
// signature, expiry, revocation state, and that the subject is an active
// member.
func (h *Handlers) MintInviteToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	claims, ok := auth.ClaimsFromContext(ctx)
	if !ok || claims.Sub == "" {
		// Unreachable behind RequireBearer; fail closed rather than mint
		// with an empty actor if this is ever mounted differently.
		auth.WriteError(w, http.StatusUnauthorized, "unauthorized", "enrolment bearer required")
		return
	}
	if !h.invite.MemberInvites {
		auth.WriteError(w, http.StatusForbidden, "invites_disabled",
			"this organisation does not allow member invites ([server].member_invites is false on the org server)")
		return
	}

	var req agentInviteRequest
	if err := orgcontract.DecodeCapped(r.Body, maxInviteRequestBytes, &req); err != nil {
		auth.WriteError(w, http.StatusBadRequest, "bad_request", "body must be exactly one JSON document within 64 KiB: {\"email\": \"…\"}")
		return
	}
	if strings.TrimSpace(req.Email) == "" {
		auth.WriteError(w, http.StatusBadRequest, "bad_request", "email is required")
		return
	}
	target, err := parseInviteTarget("", req.Email)
	if err != nil {
		auth.WriteError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	ttl, err := inviteTTL(req.TTLDays, h.enrolTokenTTL)
	if err != nil {
		auth.WriteError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	res, err := h.mintDelegated(ctx, claims.Sub, target, ttl, true, sourceIPOf(r), "agent")
	switch {
	case errors.Is(err, ErrUserNotFound):
		auth.WriteError(w, http.StatusNotFound, "not_found",
			"no active member with that email — an invite hands over a token for an EXISTING member; ask an admin to provision them first")
		return
	case errors.Is(err, ErrInviteAttemptBudget):
		auth.WriteError(w, http.StatusTooManyRequests, "invite_attempts_exceeded",
			fmt.Sprintf("too many invites for addresses that are not active members (%d per hour); wait for the window to roll", inviteAttemptBudget))
		return
	case errors.Is(err, ErrInviteCapReached):
		auth.WriteError(w, http.StatusTooManyRequests, "invite_cap_reached",
			fmt.Sprintf("monthly invite cap reached (%d per member); it resets at the start of next month", h.invite.cap()))
		return
	case err != nil:
		h.logger.Error("agent invite mint", "err", err)
		auth.WriteError(w, http.StatusInternalServerError, "internal", "could not mint invite")
		return
	}
	h.logger.Info("invite token minted", "actor", claims.Sub, "invited", res.UserID, "token_id", res.TokenID, "via", "agent")

	writeJSON(w, http.StatusCreated, agentInviteResponse{
		Token:       res.Token,
		TokenID:     res.TokenID,
		UserID:      res.UserID,
		UserEmail:   res.UserEmail,
		ExpiresAt:   res.ExpiresAt.UTC().Format(time.RFC3339),
		MintedMonth: res.MintedThis,
		MonthlyCap:  res.MonthlyCap,
	})
}

// sourceIPOf returns the audited peer address for an invite request.
//
// It is DELIBERATELY RemoteAddr-only (via clientIP): X-Forwarded-For is a
// request header any client can set, and this org server has no
// trusted-proxy allow-list to decide when it is authentic — so honouring it
// would let an attacker write an arbitrary address into the invite_minted
// audit row and point an investigation at someone else. If a deployment ever
// terminates TLS at a reverse proxy and needs the real client IP, the fix is
// a configured trusted-proxy list consulted here, not a bare header read.
func sourceIPOf(r *http.Request) string { return clientIP(r) }

// rowQuerier is the single-row read half shared by *sql.DB and *sql.Conn, so
// a member lookup can run either standalone or on the pinned connection of
// the mint transaction.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}
