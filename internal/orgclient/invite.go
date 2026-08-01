package orgclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/orgcontract"
)

// ErrInvitesDisabled is returned by MintInviteToken when the org server
// refuses the mint because [server].member_invites is false there. It is a
// terminal, honest answer — not a retryable failure — and the dashboard
// renders it as disabled copy naming the exact server key.
var ErrInvitesDisabled = errors.New("orgclient: the organisation does not allow member invites")

// ErrInviteCapReached is returned when this user has already minted their
// monthly allowance of invites on the org server.
var ErrInviteCapReached = errors.New("orgclient: monthly invite cap reached")

// ErrInviteTargetUnknown is returned when no ACTIVE org member has the given
// email. An invite is a token handoff for an EXISTING member, never account
// creation — see the server's config.ServerConfig.MemberInvites.
var ErrInviteTargetUnknown = errors.New("orgclient: no active org member with that email")

// maxInviteResponseBytes caps the mint response document.
const maxInviteResponseBytes = 64 << 10

// InviteToken is one minted invite, as returned to the node dashboard. Token
// is the one-time compound enrolment token — it is shown to the operator
// exactly once and is NEVER persisted node-side (not in the DB, not in a
// log): the org server holds only its argon2id hash.
type InviteToken struct {
	Token       string `json:"token"`
	TokenID     string `json:"token_id"`
	UserID      string `json:"user_id"`
	UserEmail   string `json:"user_email"`
	ExpiresAt   string `json:"expires_at"`
	MintedMonth int    `json:"minted_this_month"`
	MonthlyCap  int    `json:"monthly_cap"`
	// OrgServerURL is filled in from the local enrolment so the dashboard can
	// render the paste-ready `observer enroll <org-url> <token>` snippet
	// without a second lookup. It is not part of the server's response.
	OrgServerURL string `json:"org_server_url"`
}

// MintInviteToken asks the org server to mint a one-time enrolment token for
// an existing teammate, over the enrolment credential this node already
// holds (POST /api/agent/invite-token).
//
// This adds NO network posture: it is the server the node is already
// enrolled with, over the credential it already presents on every push, and
// it is USER-INITIATED — nothing here runs on a timer and nothing about this
// node is described in the request beyond the teammate's email the operator
// typed. Nothing is added to the push envelope.
//
// The authority is entirely the server's: the endpoint is refused unless the
// org set [server].member_invites, the mint is attributed to THIS node's
// enrolled user (the server reads the identity from the bearer — the request
// cannot name an inviter), it is counted against that user's monthly cap, and
// it is audited org-side.
func (c *Client) MintInviteToken(ctx context.Context, email string, ttlDays int) (InviteToken, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return InviteToken{}, errors.New("orgclient.MintInviteToken: email is required")
	}
	enr, err := c.store.LoadEnrolment(ctx)
	if err != nil {
		return InviteToken{}, fmt.Errorf("orgclient.MintInviteToken: enrolment: %w", err)
	}
	if enr == nil {
		return InviteToken{}, ErrNotEnrolled
	}
	bearer, err := c.bearers.LoadBearer()
	if err != nil {
		return InviteToken{}, fmt.Errorf("orgclient.MintInviteToken: bearer: %w", err)
	}

	body, err := json.Marshal(struct {
		Email   string `json:"email"`
		TTLDays int    `json:"ttl_days,omitempty"`
	}{Email: email, TTLDays: ttlDays})
	if err != nil {
		return InviteToken{}, err
	}
	url := strings.TrimRight(enr.OrgServerURL, "/") + "/api/agent/invite-token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return InviteToken{}, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return InviteToken{}, fmt.Errorf("orgclient.MintInviteToken: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusCreated:
	case http.StatusForbidden:
		// Also the answer an OLDER server gives (no such route → 404), so
		// both are mapped to the same honest "your org doesn't allow this".
		return InviteToken{}, ErrInvitesDisabled
	case http.StatusNotFound:
		// 404 is ambiguous between "unknown teammate" and "server predates
		// this endpoint". The server's not_found body names the member case;
		// distinguish on the error code so the dashboard copy is truthful.
		if inviteErrorCode(resp) == "not_found" {
			return InviteToken{}, ErrInviteTargetUnknown
		}
		return InviteToken{}, ErrInvitesDisabled
	case http.StatusTooManyRequests:
		return InviteToken{}, ErrInviteCapReached
	case http.StatusUnauthorized:
		return InviteToken{}, ErrAuthFailed
	default:
		return InviteToken{}, fmt.Errorf("orgclient.MintInviteToken: server returned %d", resp.StatusCode)
	}

	var out InviteToken
	if err := orgcontract.DecodeCapped(resp.Body, maxInviteResponseBytes, &out); err != nil {
		return InviteToken{}, fmt.Errorf("orgclient.MintInviteToken: decode: %w", err)
	}
	if out.Token == "" {
		return InviteToken{}, errors.New("orgclient.MintInviteToken: server returned an empty token")
	}
	out.OrgServerURL = strings.TrimRight(enr.OrgServerURL, "/")
	// Deliberately NOT logged at any level: the compound token is the whole
	// credential. Only the non-secret token id is safe to record.
	c.logger.Info("invite token minted", "token_id", out.TokenID, "expires_at", out.ExpiresAt)
	return out, nil
}

// inviteErrorCode reads the {error,message} envelope's code, best-effort.
// An unreadable body yields "" and the caller falls back to its ambiguous
// interpretation.
func inviteErrorCode(resp *http.Response) string {
	var env struct {
		Error string `json:"error"`
	}
	if err := orgcontract.DecodeCapped(resp.Body, maxInviteResponseBytes, &env); err != nil {
		return ""
	}
	return env.Error
}
