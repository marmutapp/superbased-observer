package orgclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// inviteServer serves POST /api/agent/invite-token with a scripted status +
// body, and records what the client actually sent.
type inviteServer struct {
	srv    *httptest.Server
	status int
	body   string
	// recorded request facts
	hits   int
	auth   string
	path   string
	method string
	reqRaw string
}

func newInviteServer(t *testing.T) *inviteServer {
	t.Helper()
	is := &inviteServer{status: http.StatusCreated}
	is.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		is.hits++
		is.auth = r.Header.Get("Authorization")
		is.path = r.URL.Path
		is.method = r.Method
		raw, _ := io.ReadAll(r.Body)
		is.reqRaw = string(raw)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(is.status)
		_, _ = w.Write([]byte(is.body))
	}))
	t.Cleanup(is.srv.Close)
	return is
}

// TestMintInviteToken_Happy covers the enabled path: the mint rides the
// enrolment credential to the enrolled server's invite endpoint, the request
// names ONLY the teammate's email (nothing about this node), and the org URL
// is filled in locally so the dashboard can render the enroll snippet.
func TestMintInviteToken_Happy(t *testing.T) {
	is := newInviteServer(t)
	is.body = `{"token":"tid.secret","token_id":"tid","user_id":"u-9","user_email":"mate@acme.example",` +
		`"expires_at":"2026-08-07T00:00:00Z","minted_this_month":2,"monthly_cap":10}`
	c, _, _ := enrolledClient(t, is.srv.URL)

	got, err := c.MintInviteToken(context.Background(), "  mate@acme.example  ", 3)
	if err != nil {
		t.Fatalf("MintInviteToken: %v", err)
	}
	if got.TokenID != "tid" || got.UserEmail != "mate@acme.example" {
		t.Fatalf("result = %+v", got)
	}
	if got.MintedMonth != 2 || got.MonthlyCap != 10 {
		t.Errorf("cap counters = %d/%d, want 2/10", got.MintedMonth, got.MonthlyCap)
	}
	if got.OrgServerURL != is.srv.URL {
		t.Errorf("org_server_url = %q, want the enrolled server %q", got.OrgServerURL, is.srv.URL)
	}
	if is.method != http.MethodPost || is.path != "/api/agent/invite-token" {
		t.Errorf("request = %s %s", is.method, is.path)
	}
	wantAuth := "Bear" + "er " + "bearer-xyz"
	if is.auth != wantAuth {
		t.Errorf("Authorization = %q, want %q (the enrolment credential)", is.auth, wantAuth)
	}
	// The request body describes the TEAMMATE and nothing else — no node
	// identity, no project, no activity. This is the whole privacy claim.
	var sent map[string]any
	if err := json.Unmarshal([]byte(is.reqRaw), &sent); err != nil {
		t.Fatalf("request body %q: %v", is.reqRaw, err)
	}
	if len(sent) != 2 || sent["email"] != "mate@acme.example" || sent["ttl_days"] != float64(3) {
		t.Errorf("request body = %v, want exactly {email, ttl_days}", sent)
	}
}

// TestMintInviteToken_ServerRefusals maps each server refusal onto a distinct,
// honest node-side error so the dashboard copy can name the real cause.
func TestMintInviteToken_ServerRefusals(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"invites disabled", http.StatusForbidden, `{"error":"invites_disabled"}`, ErrInvitesDisabled},
		{"unknown teammate", http.StatusNotFound, `{"error":"not_found"}`, ErrInviteTargetUnknown},
		{"older server (no route)", http.StatusNotFound, `404 page not found`, ErrInvitesDisabled},
		{"cap reached", http.StatusTooManyRequests, `{"error":"invite_cap_reached"}`, ErrInviteCapReached},
		{"bad credential", http.StatusUnauthorized, `{"error":"unauthorized"}`, ErrAuthFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			is := newInviteServer(t)
			is.status, is.body = tt.status, tt.body
			c, _, _ := enrolledClient(t, is.srv.URL)
			_, err := c.MintInviteToken(context.Background(), "mate@acme.example", 0)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

// TestMintInviteToken_NotEnrolled pins that a solo-local node never reaches
// the network: no enrolment row, no request.
func TestMintInviteToken_NotEnrolled(t *testing.T) {
	is := newInviteServer(t)
	s := newAgentStore(t)
	c := newTestClient(t, s, &memBearerStore{bearer: "bearer-xyz"})

	_, err := c.MintInviteToken(context.Background(), "mate@acme.example", 0)
	if !errors.Is(err, ErrNotEnrolled) {
		t.Fatalf("err = %v, want ErrNotEnrolled", err)
	}
	if is.hits != 0 {
		t.Errorf("a not-enrolled node made %d requests, want 0", is.hits)
	}
}

// TestMintInviteToken_RequiresEmail pins that an empty target is refused
// locally — an invite is addressed to a person, not broadcast.
func TestMintInviteToken_RequiresEmail(t *testing.T) {
	is := newInviteServer(t)
	c, _, _ := enrolledClient(t, is.srv.URL)
	if _, err := c.MintInviteToken(context.Background(), "   ", 0); err == nil {
		t.Fatal("empty email should error")
	}
	if is.hits != 0 {
		t.Errorf("empty email still made %d requests, want 0", is.hits)
	}
}

// TestMintInviteToken_RejectsEmptyToken pins that a server answering 201 with
// no token is an error, not a silently-empty snippet in the dashboard.
func TestMintInviteToken_RejectsEmptyToken(t *testing.T) {
	is := newInviteServer(t)
	is.body = `{"token_id":"tid"}`
	c, _, _ := enrolledClient(t, is.srv.URL)
	if _, err := c.MintInviteToken(context.Background(), "mate@acme.example", 0); err == nil {
		t.Fatal("empty token should error")
	}
}
