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

// requestsServer serves the W6 agent-facing pair (POST/GET
// /api/agent/requests) with a scripted status + body, and records what the
// client actually sent.
type requestsServer struct {
	srv    *httptest.Server
	status int
	body   string
	// recorded request facts (last request seen)
	hits   int
	auth   string
	path   string
	method string
	reqRaw string
}

func newRequestsServer(t *testing.T) *requestsServer {
	t.Helper()
	rs := &requestsServer{status: http.StatusCreated}
	rs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs.hits++
		rs.auth = r.Header.Get("Authorization")
		rs.path = r.URL.Path
		rs.method = r.Method
		raw, _ := io.ReadAll(r.Body)
		rs.reqRaw = string(raw)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rs.status)
		_, _ = w.Write([]byte(rs.body))
	}))
	t.Cleanup(rs.srv.Close)
	return rs
}

// TestPostOrgRequest_Happy covers the enabled path: the ask rides the
// enrolment credential to the enrolled server's requests endpoint, and the
// request body carries exactly kind/target/message — no node identity.
func TestPostOrgRequest_Happy(t *testing.T) {
	rs := newRequestsServer(t)
	rs.body = `{"id":7,"kind":"enable_feature","target":"terminals","message":"please turn on terminals",` +
		`"status":"open","created_at":"2026-08-24T00:00:00Z"}`
	c, _, _ := enrolledClient(t, rs.srv.URL)

	got, err := c.PostOrgRequest(context.Background(), "enable_feature", "terminals", "  please turn on terminals  ")
	if err != nil {
		t.Fatalf("PostOrgRequest: %v", err)
	}
	if got.ID != 7 || got.Kind != "enable_feature" || got.Target != "terminals" || got.Status != "open" {
		t.Fatalf("result = %+v", got)
	}
	if rs.method != http.MethodPost || rs.path != "/api/agent/requests" {
		t.Errorf("request = %s %s", rs.method, rs.path)
	}
	wantAuth := "Bear" + "er " + "bearer-xyz"
	if rs.auth != wantAuth {
		t.Errorf("Authorization = %q, want %q (the enrolment credential)", rs.auth, wantAuth)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(rs.reqRaw), &sent); err != nil {
		t.Fatalf("request body %q: %v", rs.reqRaw, err)
	}
	if len(sent) != 3 || sent["kind"] != "enable_feature" || sent["target"] != "terminals" ||
		sent["message"] != "please turn on terminals" {
		t.Errorf("request body = %v, want exactly {kind, target, message}", sent)
	}
}

// TestPostOrgRequest_ServerRefusals maps each server refusal onto a
// distinct, honest node-side error.
func TestPostOrgRequest_ServerRefusals(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"open cap reached", http.StatusTooManyRequests, `{"error":"too_many_requests"}`, ErrOrgRequestCapReached},
		{"bad credential (401)", http.StatusUnauthorized, `{"error":"unauthorized"}`, ErrAuthFailed},
		{"bad credential (403)", http.StatusForbidden, `{"error":"forbidden"}`, ErrAuthFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rs := newRequestsServer(t)
			rs.status, rs.body = tt.status, tt.body
			c, _, _ := enrolledClient(t, rs.srv.URL)
			_, err := c.PostOrgRequest(context.Background(), "other", "", "ask")
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

// TestPostOrgRequest_NotEnrolled pins that a solo-local node never reaches
// the network.
func TestPostOrgRequest_NotEnrolled(t *testing.T) {
	rs := newRequestsServer(t)
	s := newAgentStore(t)
	c := newTestClient(t, s, &memBearerStore{bearer: "bearer-xyz"})

	_, err := c.PostOrgRequest(context.Background(), "other", "", "ask")
	if !errors.Is(err, ErrNotEnrolled) {
		t.Fatalf("err = %v, want ErrNotEnrolled", err)
	}
	if rs.hits != 0 {
		t.Errorf("a not-enrolled node made %d requests, want 0", rs.hits)
	}
}

// TestPostOrgRequest_RequiresMessage pins that an empty message is refused
// locally, with no network call.
func TestPostOrgRequest_RequiresMessage(t *testing.T) {
	rs := newRequestsServer(t)
	c, _, _ := enrolledClient(t, rs.srv.URL)
	if _, err := c.PostOrgRequest(context.Background(), "other", "", "   "); err == nil {
		t.Fatal("empty message should error")
	}
	if rs.hits != 0 {
		t.Errorf("empty message still made %d requests, want 0", rs.hits)
	}
}

// TestListMyOrgRequests_Happy covers the read-back path.
func TestListMyOrgRequests_Happy(t *testing.T) {
	rs := newRequestsServer(t)
	rs.status = http.StatusOK
	rs.body = `{"requests":[
		{"id":2,"kind":"allow_tool","target":"bash","message":"let me use bash","status":"resolved","created_at":"2026-08-24T00:00:00Z","resolved_at":"2026-08-24T01:00:00Z","resolved_by":"admin@acme.example","resolution_note":"granted via policy"},
		{"id":1,"kind":"other","message":"first ask","status":"open","created_at":"2026-08-23T00:00:00Z"}
	]}`
	c, _, _ := enrolledClient(t, rs.srv.URL)

	got, err := c.ListMyOrgRequests(context.Background())
	if err != nil {
		t.Fatalf("ListMyOrgRequests: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("requests = %d, want 2", len(got))
	}
	if got[0].ID != 2 || got[0].Status != "resolved" || got[0].ResolutionNote != "granted via policy" {
		t.Errorf("first request = %+v", got[0])
	}
	if rs.method != http.MethodGet || rs.path != "/api/agent/requests" {
		t.Errorf("request = %s %s", rs.method, rs.path)
	}
	wantAuth := "Bear" + "er " + "bearer-xyz"
	if rs.auth != wantAuth {
		t.Errorf("Authorization = %q, want %q (the enrolment credential)", rs.auth, wantAuth)
	}
}

// TestListMyOrgRequests_NotEnrolled pins the same local-first behaviour as
// the post path.
func TestListMyOrgRequests_NotEnrolled(t *testing.T) {
	rs := newRequestsServer(t)
	s := newAgentStore(t)
	c := newTestClient(t, s, &memBearerStore{bearer: "bearer-xyz"})

	_, err := c.ListMyOrgRequests(context.Background())
	if !errors.Is(err, ErrNotEnrolled) {
		t.Fatalf("err = %v, want ErrNotEnrolled", err)
	}
	if rs.hits != 0 {
		t.Errorf("a not-enrolled node made %d requests, want 0", rs.hits)
	}
}

// TestListMyOrgRequests_AuthFailed maps a rejected credential onto
// ErrAuthFailed.
func TestListMyOrgRequests_AuthFailed(t *testing.T) {
	rs := newRequestsServer(t)
	rs.status = http.StatusUnauthorized
	rs.body = `{"error":"unauthorized"}`
	c, _, _ := enrolledClient(t, rs.srv.URL)

	_, err := c.ListMyOrgRequests(context.Background())
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("err = %v, want ErrAuthFailed", err)
	}
}
