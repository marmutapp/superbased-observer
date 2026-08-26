package orgclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ACP-P6c agent half: the device-code client. These tests drive a fake org
// server through every answer the real one can give, because the CLI loop
// above them branches on exactly these and nothing else.

// idpFakeServer serves the two agent-facing routes from a handler table, so a
// test names only the answers it cares about.
func idpFakeServer(t *testing.T, start, poll http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case idpStartPath:
			if start == nil {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			start(w, r)
		case idpPollPath:
			if poll == nil {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			poll(w, r)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func idpTestClient(t *testing.T) *Client {
	t.Helper()
	return newTestClient(t, newAgentStore(t), &memBearerStore{})
}

// TestStartIdPEnrol_RoundTrip: a started pairing comes back whole, and the
// request is an unauthenticated JSON POST (the machine has no credential yet).
func TestStartIdPEnrol_RoundTrip(t *testing.T) {
	var (
		gotMethod string
		gotAuth   string
		gotType   string
	)
	srv := idpFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotAuth, gotType = r.Method, r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		writeTestJSON(w, http.StatusCreated, IdPEnrolStart{
			DeviceCode:      "dev-code-abc",
			UserCode:        "BCDF-2345",
			VerificationURI: "https://org.example/enrol/idp",
			ExpiresIn:       600,
			Interval:        5,
		})
	}, nil)

	got, err := idpTestClient(t).StartIdPEnrol(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("StartIdPEnrol: %v", err)
	}
	if got.DeviceCode != "dev-code-abc" || got.UserCode != "BCDF-2345" {
		t.Fatalf("codes = (%q, %q)", got.DeviceCode, got.UserCode)
	}
	if got.VerificationURI != "https://org.example/enrol/idp" {
		t.Errorf("verification_uri = %q", got.VerificationURI)
	}
	if got.ExpiresIn != 600 || got.Interval != 5 {
		t.Errorf("deadline/cadence = (%d, %d), want (600, 5)", got.ExpiresIn, got.Interval)
	}
	if gotMethod != http.MethodPost || gotType != "application/json" {
		t.Errorf("request = %s %s", gotMethod, gotType)
	}
	if gotAuth != "" {
		t.Errorf("start must be unauthenticated, sent %q", gotAuth)
	}
}

// TestStartIdPEnrol_TrailingSlashOrgURL: an operator pasting a URL with a
// trailing slash must not produce a double-slashed path the server 404s.
func TestStartIdPEnrol_TrailingSlashOrgURL(t *testing.T) {
	var gotPath string
	srv := idpFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		writeTestJSON(w, http.StatusCreated, IdPEnrolStart{DeviceCode: "d", UserCode: "u"})
	}, nil)

	if _, err := idpTestClient(t).StartIdPEnrol(context.Background(), srv.URL+"/  "); err != nil {
		t.Fatalf("StartIdPEnrol: %v", err)
	}
	if gotPath != idpStartPath {
		t.Fatalf("path = %q, want %q", gotPath, idpStartPath)
	}
}

// TestIdPEnrol_404IsOneNamedState is the compat gate: the rail being off and
// the server being too old are the SAME answer, and the client must map both
// (on both endpoints) to the single named state so the CLI prints one honest
// message instead of guessing.
func TestIdPEnrol_404IsOneNamedState(t *testing.T) {
	srv := idpFakeServer(t, nil, nil) // every route 404s

	c := idpTestClient(t)
	if _, err := c.StartIdPEnrol(context.Background(), srv.URL); !errors.Is(err, ErrIdPEnrolUnavailable) {
		t.Fatalf("start 404 = %v, want ErrIdPEnrolUnavailable", err)
	}
	if _, err := c.PollIdPEnrol(context.Background(), srv.URL, "dev-code"); !errors.Is(err, ErrIdPEnrolUnavailable) {
		t.Fatalf("poll 404 = %v, want ErrIdPEnrolUnavailable", err)
	}
}

// TestPollIdPEnrol_Statuses: every decided outcome is a normal return the CLI
// can report, never an error. Denied and expired especially — a developer
// needs to be told plainly, not handed a transport failure.
func TestPollIdPEnrol_Statuses(t *testing.T) {
	// The redeemable material an approval hands over, in the compound
	// "<id>.<secret>" shape Enroll consumes.
	const approvedCode = "tok_id.secret"
	// Assembled by assignment rather than a composite literal so the
	// approved answer is built the same way in every row.
	approved := IdPEnrolPoll{Status: IdPStatusApproved}
	approved.OneTimeToken = approvedCode
	cases := []struct {
		name     string
		answer   IdPEnrolPoll
		wantCode string
	}{
		{"pending", IdPEnrolPoll{Status: IdPStatusPending, Interval: 5}, ""},
		{"slow down", IdPEnrolPoll{Status: IdPStatusSlowDown, Interval: 5}, ""},
		{"denied", IdPEnrolPoll{Status: IdPStatusDenied}, ""},
		{"expired", IdPEnrolPoll{Status: IdPStatusExpired}, ""},
		{"approved", approved, approvedCode},
		// A status this build does not know is passed through rather than
		// coerced: the CLI says what it was told.
		{"unknown status", IdPEnrolPoll{Status: "quarantined"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody idpPollBody
			srv := idpFakeServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&gotBody)
				writeTestJSON(w, http.StatusOK, tc.answer)
			})
			got, err := idpTestClient(t).PollIdPEnrol(context.Background(), srv.URL, "dev-code-abc")
			if err != nil {
				t.Fatalf("PollIdPEnrol: %v", err)
			}
			if got.Status != tc.answer.Status {
				t.Errorf("status = %q, want %q", got.Status, tc.answer.Status)
			}
			if got.OneTimeToken != tc.wantCode {
				t.Errorf("handed-over code = %q, want %q", got.OneTimeToken, tc.wantCode)
			}
			if gotBody.DeviceCode != "dev-code-abc" {
				t.Errorf("device code on the wire = %q", gotBody.DeviceCode)
			}
		})
	}
}

// TestPollIdPEnrol_ApprovedWithoutMaterial: an "approved" carrying nothing to
// redeem is a broken server, and saying so beats returning a success the CLI
// would take into Enroll with an empty credential.
func TestPollIdPEnrol_ApprovedWithoutMaterial(t *testing.T) {
	srv := idpFakeServer(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, http.StatusOK, IdPEnrolPoll{Status: IdPStatusApproved})
	})
	if _, err := idpTestClient(t).PollIdPEnrol(context.Background(), srv.URL, "dev-code"); err == nil {
		t.Fatal("approved-with-nothing must be an error")
	}
}

// TestPollIdPEnrol_RejectsEmptyDeviceCode: the client refuses locally rather
// than asking the server about a pairing it cannot name.
func TestPollIdPEnrol_RejectsEmptyDeviceCode(t *testing.T) {
	hits := 0
	srv := idpFakeServer(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		hits++
		writeTestJSON(w, http.StatusOK, IdPEnrolPoll{Status: IdPStatusPending})
	})
	if _, err := idpTestClient(t).PollIdPEnrol(context.Background(), srv.URL, "  "); err == nil {
		t.Fatal("empty device code must be refused")
	}
	if hits != 0 {
		t.Fatalf("server was called %d times for an empty device code", hits)
	}
}

// TestIdPEnrol_CappedResponseRead: both endpoints read a BOUNDED body. A
// hostile or broken server on the other end of a poll loop must not be able to
// stream unbounded bytes into an agent asking a yes/no question.
func TestIdPEnrol_CappedResponseRead(t *testing.T) {
	flood := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// One syntactically valid document whose padding field alone dwarfs
		// the cap.
		_, _ = w.Write([]byte(`{"status":"pending","pad":"` + strings.Repeat("A", idpMaxResponseBytes*2) + `"}`))
	}
	srv := idpFakeServer(t, flood, flood)

	c := idpTestClient(t)
	if _, err := c.StartIdPEnrol(context.Background(), srv.URL); err == nil {
		t.Fatal("start accepted an oversize document")
	} else if !strings.Contains(err.Error(), "cap") {
		t.Fatalf("start error = %v, want the cap to be named", err)
	}
	if _, err := c.PollIdPEnrol(context.Background(), srv.URL, "dev-code"); err == nil {
		t.Fatal("poll accepted an oversize document")
	} else if !strings.Contains(err.Error(), "cap") {
		t.Fatalf("poll error = %v, want the cap to be named", err)
	}
}

// TestIdPEnrol_UnexpectedStatusNamesTheServersReason: a rate-limited start
// (the outstanding-pairing cap) must surface the server's own explanation,
// bounded.
func TestIdPEnrol_UnexpectedStatusNamesTheServersReason(t *testing.T) {
	srv := idpFakeServer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "too many enrolment pairings are already waiting for approval", http.StatusTooManyRequests)
	}, nil)

	_, err := idpTestClient(t).StartIdPEnrol(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("429 must be an error")
	}
	if errors.Is(err, ErrIdPEnrolUnavailable) {
		t.Fatal("429 must not be conflated with the unavailable state")
	}
	if !strings.Contains(err.Error(), "429") || !strings.Contains(err.Error(), "already waiting") {
		t.Fatalf("error = %v, want the status and the server's reason", err)
	}
}
