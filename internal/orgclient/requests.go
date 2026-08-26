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

// requests.go — W6 dev->org REQUEST/MESSAGE system, node-side client half
// (see docs/plans/org-parity-full-depth-plan-2026-08-24.md §0.4 + §4 "W6",
// internal/orgserver/api/orgrequests.go for the agent-facing server handlers,
// internal/orgserver/orgrequests_handlers.go for the admin-facing half). This
// is the node's ONLY ask channel to the org: it posts a short typed request
// and can read back its own requests + statuses. The admin never grants
// anything through this path directly — they respond by editing policy on
// the existing policy rail; PostOrgRequest / ListMyOrgRequests only carry the
// ask and its recorded status, never a grant.

// ErrOrgRequestCapReached is returned when this user already has the
// server's maximum number of OPEN requests outstanding. It is not
// retryable until an existing request is resolved or declined.
var ErrOrgRequestCapReached = errors.New("orgclient: too many open requests to the organisation")

// maxOrgRequestResponseBytes caps a single request-response document /
// list-of-requests document.
const maxOrgRequestResponseBytes = 256 << 10

// OrgRequest is one dev->org request/message, as the node sees it (its own
// requests only — the server never lets a node read another user's).
type OrgRequest struct {
	ID             int64  `json:"id"`
	Kind           string `json:"kind"`
	Target         string `json:"target,omitempty"`
	Message        string `json:"message"`
	Status         string `json:"status"`
	CreatedAt      string `json:"created_at"`
	ResolvedAt     string `json:"resolved_at,omitempty"`
	ResolvedBy     string `json:"resolved_by,omitempty"`
	ResolutionNote string `json:"resolution_note,omitempty"`
}

// PostOrgRequest sends a short typed ask to the org server (POST
// /api/agent/requests). kind is a closed vocabulary the server validates
// (enable_feature | raise_budget | allow_tool | other); an empty kind is
// server-normalised to "other", but any other unrecognised value is
// rejected -- pass "" or "other" rather than guessing at future kinds.
// target is the optional feature/tool/etc. the ask concerns; message is the
// free-form body the operator typed. Identity is stamped by the server from
// the bearer credential this node already presents on every push -- nothing
// here can spoof who is asking.
func (c *Client) PostOrgRequest(ctx context.Context, kind, target, message string) (OrgRequest, error) {
	message = strings.TrimSpace(message)
	if message == "" {
		return OrgRequest{}, errors.New("orgclient.PostOrgRequest: message is required")
	}
	enr, err := c.store.LoadEnrolment(ctx)
	if err != nil {
		return OrgRequest{}, fmt.Errorf("orgclient.PostOrgRequest: load enrolment: %w", err)
	}
	if enr == nil {
		return OrgRequest{}, ErrNotEnrolled
	}
	bearer, err := c.bearers.LoadBearer()
	if err != nil {
		return OrgRequest{}, fmt.Errorf("orgclient.PostOrgRequest: load bearer: %w", err)
	}

	body, err := json.Marshal(struct {
		Kind    string `json:"kind,omitempty"`
		Target  string `json:"target,omitempty"`
		Message string `json:"message"`
	}{Kind: strings.TrimSpace(kind), Target: strings.TrimSpace(target), Message: message})
	if err != nil {
		return OrgRequest{}, err
	}
	url := strings.TrimRight(enr.OrgServerURL, "/") + "/api/agent/requests"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return OrgRequest{}, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return OrgRequest{}, fmt.Errorf("orgclient.PostOrgRequest: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusCreated:
	case http.StatusTooManyRequests:
		return OrgRequest{}, ErrOrgRequestCapReached
	case http.StatusUnauthorized, http.StatusForbidden:
		return OrgRequest{}, ErrAuthFailed
	default:
		return OrgRequest{}, fmt.Errorf("orgclient.PostOrgRequest: server returned %d", resp.StatusCode)
	}

	var out OrgRequest
	if err := orgcontract.DecodeCapped(resp.Body, maxOrgRequestResponseBytes, &out); err != nil {
		return OrgRequest{}, fmt.Errorf("orgclient.PostOrgRequest: decode response: %w", err)
	}
	c.logger.Info("org request posted", "id", out.ID, "kind", out.Kind)
	return out, nil
}

// ListMyOrgRequests reads back this node's own requests and their current
// statuses (GET /api/agent/requests). The server scopes the list to the
// bearer's identity — there is no way to ask for anyone else's.
func (c *Client) ListMyOrgRequests(ctx context.Context) ([]OrgRequest, error) {
	enr, err := c.store.LoadEnrolment(ctx)
	if err != nil {
		return nil, fmt.Errorf("orgclient.ListMyOrgRequests: load enrolment: %w", err)
	}
	if enr == nil {
		return nil, ErrNotEnrolled
	}
	bearer, err := c.bearers.LoadBearer()
	if err != nil {
		return nil, fmt.Errorf("orgclient.ListMyOrgRequests: load bearer: %w", err)
	}

	url := strings.TrimRight(enr.OrgServerURL, "/") + "/api/agent/requests"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("orgclient.ListMyOrgRequests: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, ErrAuthFailed
	default:
		return nil, fmt.Errorf("orgclient.ListMyOrgRequests: server returned %d", resp.StatusCode)
	}

	var out struct {
		Requests []OrgRequest `json:"requests"`
	}
	if err := orgcontract.DecodeCapped(resp.Body, maxOrgRequestResponseBytes, &out); err != nil {
		return nil, fmt.Errorf("orgclient.ListMyOrgRequests: decode response: %w", err)
	}
	return out.Requests, nil
}
