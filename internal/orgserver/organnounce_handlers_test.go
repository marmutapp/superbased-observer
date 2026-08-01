package orgserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/announce"
	"github.com/marmutapp/superbased-observer/internal/orgcontract"
	orgdb "github.com/marmutapp/superbased-observer/internal/orgserver/db"
	"github.com/marmutapp/superbased-observer/internal/orgserver/organnounce"
)

// openAnnounceDB opens a throwaway org-server DB with every migration
// applied (022 creates the announcement tables).
func openAnnounceDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := orgdb.Open(context.Background(), orgdb.Options{Path: filepath.Join(t.TempDir(), "org.db")})
	if err != nil {
		t.Fatalf("orgdb.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

// postAnnouncement drives the publish handler with a raw JSON body.
func postAnnouncement(t *testing.T, db *sql.DB, body string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	announcementPublishHandler(db)(rr, httptest.NewRequest(http.MethodPost, "/api/org/announcement", strings.NewReader(body)))
	return rr
}

// getAgentAnnouncement drives the agent fetch handler.
func getAgentAnnouncement(t *testing.T, db *sql.DB) (*httptest.ResponseRecorder, orgcontract.OrgAnnouncementDoc) {
	t.Helper()
	rr := httptest.NewRecorder()
	announcementGetHandler(db)(rr, httptest.NewRequest(http.MethodGet, "/api/agent/announcement", nil))
	var doc orgcontract.OrgAnnouncementDoc
	if rr.Code == http.StatusOK {
		if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
			t.Fatalf("decode agent doc %q: %v", rr.Body.String(), err)
		}
	}
	return rr, doc
}

const publishOnePayload = `{"announcement":{
  "id":"2026-08-01-maint",
  "severity":"notice",
  "title":"Fleet maintenance",
  "body":"The build cluster is down for maintenance until 14:00 UTC.",
  "url":"https://intranet.acme.example/status",
  "expires_at":"2030-01-01T00:00:00Z"
}}`

// TestAnnouncementPublishFetchRoundTrip is the arc that matters: an
// admin publishes, the agent endpoint serves a doc that verifies
// against the served key, and the body decodes back to the §1
// announcement with Source forced to "org".
func TestAnnouncementPublishFetchRoundTrip(t *testing.T) {
	t.Parallel()
	db := openAnnounceDB(t)

	// Nothing published yet → the agent gets a 404, never a 500.
	rr, _ := getAgentAnnouncement(t, db)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("agent fetch before publish = %d, want 404", rr.Code)
	}

	pub := postAnnouncement(t, db, publishOnePayload)
	if pub.Code != http.StatusOK {
		t.Fatalf("publish = %d: %s", pub.Code, pub.Body.String())
	}

	rr, doc := getAgentAnnouncement(t, db)
	if rr.Code != http.StatusOK {
		t.Fatalf("agent fetch = %d: %s", rr.Code, rr.Body.String())
	}
	if doc.Version != 1 || doc.PublicKey == "" {
		t.Fatalf("doc = %+v", doc)
	}
	if err := organnounce.Verify(doc, doc.PublicKey); err != nil {
		t.Fatalf("served doc does not verify: %v", err)
	}
	list, err := announce.Decode(doc.Body)
	if err != nil || len(list) != 1 {
		t.Fatalf("Decode(%q) = %v, %v", doc.Body, list, err)
	}
	got := list[0]
	if got.ID != "2026-08-01-maint" || got.Severity != announce.SeverityNotice {
		t.Errorf("decoded = %+v", got)
	}
	if got.Source != announce.SourceOrg {
		t.Errorf("source = %q, want org (the server asserts provenance, the caller does not)", got.Source)
	}
	if err := announce.Validate(got); err != nil {
		t.Errorf("published announcement does not satisfy §1: %v", err)
	}
}

// TestAnnouncementPublishForcesOrgSource pins that a caller claiming
// source "release" cannot smuggle release provenance into the SIGNED
// bytes — the node renders what the signature covers.
func TestAnnouncementPublishForcesOrgSource(t *testing.T) {
	t.Parallel()
	db := openAnnounceDB(t)
	pub := postAnnouncement(t, db, `{"announcement":{"id":"x","severity":"info","title":"t","body":"b","expires_at":"2030-01-01T00:00:00Z","source":"release"}}`)
	if pub.Code != http.StatusOK {
		t.Fatalf("publish = %d: %s", pub.Code, pub.Body.String())
	}
	_, doc := getAgentAnnouncement(t, db)
	if !strings.Contains(doc.Body, `"source":"org"`) {
		t.Errorf("signed body = %q, want source org", doc.Body)
	}
}

// TestAnnouncementPublishRejections pins the validation gate at the
// HTTP boundary: every bad shape is a 400 and nothing is published.
func TestAnnouncementPublishRejections(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		payload string
	}{
		{"not JSON", `nonsense`},
		{"empty object is not a retraction", `{}`},
		{"announcement and retract together", `{"retract":true,"announcement":{"id":"x","severity":"info","title":"t","body":"b","expires_at":"2030-01-01T00:00:00Z"}}`},
		{"missing expiry", `{"announcement":{"id":"x","severity":"info","title":"t","body":"b"}}`},
		{"body over 280 chars", `{"announcement":{"id":"x","severity":"info","title":"t","body":"` + strings.Repeat("a", 281) + `","expires_at":"2030-01-01T00:00:00Z"}}`},
		{"insecure url", `{"announcement":{"id":"x","severity":"info","title":"t","body":"b","url":"http://x.example","expires_at":"2030-01-01T00:00:00Z"}}`},
		{"unknown severity", `{"announcement":{"id":"x","severity":"urgent","title":"t","body":"b","expires_at":"2030-01-01T00:00:00Z"}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := openAnnounceDB(t)
			rr := postAnnouncement(t, db, tc.payload)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("publish = %d, want 400: %s", rr.Code, rr.Body.String())
			}
			var n int
			if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM org_announcements`).Scan(&n); err != nil || n != 0 {
				t.Errorf("rows after refused publish = %d err=%v, want 0", n, err)
			}
		})
	}
}

// TestAnnouncementRetraction pins the retraction path end to end: the
// admin retracts, the agent endpoint still answers 200 (the node needs
// the version bump to clear its banner) with an empty body, and the
// admin read reports retracted.
func TestAnnouncementRetraction(t *testing.T) {
	t.Parallel()
	db := openAnnounceDB(t)
	if rr := postAnnouncement(t, db, publishOnePayload); rr.Code != http.StatusOK {
		t.Fatalf("publish = %d: %s", rr.Code, rr.Body.String())
	}
	if rr := postAnnouncement(t, db, `{"retract":true}`); rr.Code != http.StatusOK {
		t.Fatalf("retract = %d: %s", rr.Code, rr.Body.String())
	}

	rr, doc := getAgentAnnouncement(t, db)
	if rr.Code != http.StatusOK {
		t.Fatalf("agent fetch after retraction = %d, want 200 (the node must receive the retraction)", rr.Code)
	}
	if doc.Version != 2 || doc.Body != "" {
		t.Fatalf("retraction doc = %+v", doc)
	}
	if err := organnounce.Verify(doc, doc.PublicKey); err != nil {
		t.Errorf("retraction does not verify: %v", err)
	}

	cur := httptest.NewRecorder()
	announcementCurrentHandler(db)(cur, httptest.NewRequest(http.MethodGet, "/api/org/announcement", nil))
	var current struct {
		Published     bool                    `json:"published"`
		Version       int64                   `json:"version"`
		Retracted     bool                    `json:"retracted"`
		Announcements []announce.Announcement `json:"announcements"`
	}
	if err := json.Unmarshal(cur.Body.Bytes(), &current); err != nil {
		t.Fatalf("decode current %q: %v", cur.Body.String(), err)
	}
	if !current.Published || current.Version != 2 || !current.Retracted || len(current.Announcements) != 0 {
		t.Errorf("current = %+v, want published v2, retracted, no announcements", current)
	}
}

// TestAnnouncementCurrentEmptyState pins that "nothing ever published"
// is a 200 with published=false and an EMPTY ARRAY (never null) — the
// composer's empty state is not an error and must not land as
// `.length` on null.
func TestAnnouncementCurrentEmptyState(t *testing.T) {
	t.Parallel()
	rr := httptest.NewRecorder()
	announcementCurrentHandler(openAnnounceDB(t))(rr, httptest.NewRequest(http.MethodGet, "/api/org/announcement", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("current = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"announcements":[]`) {
		t.Errorf("body = %q, want an empty announcements array", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"published":false`) {
		t.Errorf("body = %q, want published=false", rr.Body.String())
	}
}

// TestAnnouncementRoutesAreGated pins the MOUNT, which no handler test
// can see: publish + the admin read must sit behind requireAdminSAML
// (the gate that also mints enrolment tokens), and the agent fetch
// behind the enrolment bearer. A mis-mount would expose fleet-wide
// publish to any SSO-capable member — the exact regression the
// requireAdminSAML comment in server.go was written for.
func TestAnnouncementRoutesAreGated(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}
	want := []*regexp.Regexp{
		regexp.MustCompile(`mux\.Handle\("POST /api/org/announcement",\s*requireAdminSAML\(`),
		regexp.MustCompile(`mux\.Handle\("GET /api/org/announcement",\s*requireAdminSAML\(`),
		regexp.MustCompile(`mux\.Handle\("GET /api/agent/announcement",\s*auth\.RequireBearer\(`),
	}
	for _, re := range want {
		if !re.Match(src) {
			t.Errorf("server.go does not mount an announcement route as expected: %s", re)
		}
	}
}

// TestAnnouncementPublishRefusesTrailingBytes is security finding 5 on
// the server side: the 1 MiB LimitReader capped the READ, not the
// document, and json.Decode stops at the first value — so a request
// carrying a second document after the first was accepted, and which of
// the two got published was decided by decode order rather than by the
// admin. Publishing is the highest-privilege write on this server; it
// takes exactly one document.
func TestAnnouncementPublishRefusesTrailingBytes(t *testing.T) {
	t.Parallel()
	db := openAnnounceDB(t)

	tests := []struct {
		name string
		body string
	}{
		{"a second publish document", publishOnePayload + publishOnePayload},
		{"a retraction smuggled after the publish", publishOnePayload + `{"retract":true}`},
		{"one trailing byte", publishOnePayload + "x"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rr := postAnnouncement(t, db, tc.body)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("publish = %d, want 400: %s", rr.Code, rr.Body.String())
			}
		})
	}
	// Nothing landed, and the rail still works for a single document.
	var n int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM org_announcements`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows = %d err=%v, want 0", n, err)
	}
	if rr := postAnnouncement(t, db, publishOnePayload); rr.Code != http.StatusOK {
		t.Fatalf("a single well-formed publish was refused: %d %s", rr.Code, rr.Body.String())
	}
	// A trailing newline (what every JSON client and curl -d @file
	// produces) must still be fine — the guard is about DOCUMENTS, not
	// about whitespace.
	if rr := postAnnouncement(t, db, publishOnePayload+"\n"); rr.Code != http.StatusOK {
		t.Fatalf("a trailing newline was refused: %d %s", rr.Code, rr.Body.String())
	}
}

// TestAnnouncementPublishRefusesAmbiguousRetraction pins finding 6
// through the HTTP surface the web2 composer uses: retraction is asked
// for by name (`{"retract":true}`), and the several JSON spellings of
// "nothing" are 400s rather than three different signed documents that
// mean the same thing.
func TestAnnouncementPublishRefusesAmbiguousRetraction(t *testing.T) {
	t.Parallel()
	db := openAnnounceDB(t)

	for _, body := range []string{`{"announcement":null}`, `{}`, `{"announcement":null,"retract":false}`} {
		if rr := postAnnouncement(t, db, body); rr.Code != http.StatusBadRequest {
			t.Errorf("publish(%s) = %d, want 400: %s", body, rr.Code, rr.Body.String())
		}
	}
	rr := postAnnouncement(t, db, `{"retract":true}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("the named retraction was refused: %d %s", rr.Code, rr.Body.String())
	}
	var doc orgcontract.OrgAnnouncementDoc
	if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode publish response: %v", err)
	}
	if doc.Body != "" {
		t.Errorf("retraction body = %q, want the empty document (its ONE representation)", doc.Body)
	}
	if err := organnounce.Verify(doc, doc.PublicKey); err != nil {
		t.Errorf("published retraction does not verify: %v", err)
	}
}
