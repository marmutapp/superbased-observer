package orgserver

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/marmutapp/superbased-observer/internal/announce"
	"github.com/marmutapp/superbased-observer/internal/orgcontract"
	"github.com/marmutapp/superbased-observer/internal/orgserver/auth"
	"github.com/marmutapp/superbased-observer/internal/orgserver/organnounce"
)

// maxAnnouncementPublishBytes caps the publish request document. A §1
// announcement is a title, ≤280 chars of body and a URL — a megabyte is
// already four orders of magnitude of headroom, and it is enforced as a
// DOCUMENT cap (see orgcontract.DecodeCapped).
const maxAnnouncementPublishBytes = 1 << 20

// announcementPublishRequest is the POST /api/org/announcement body.
//
// Exactly one of the two fields is meaningful per call: `announcement`
// publishes, `retract` publishes the empty (retraction) document. They
// are separate fields rather than "publish a null announcement" so that
// a client bug which drops the payload can never be read as an intent
// to retract — an empty request is a 400, and retraction has to be
// asked for by name.
type announcementPublishRequest struct {
	Announcement *announce.Announcement `json:"announcement"`
	Retract      bool                   `json:"retract"`
}

// announcementPublishHandler is the admin publish endpoint (mounted
// behind requireAdminSAML — the same gate that mints enrolment tokens
// and publishes routing policy).
//
// The announcement is VALIDATED (plan §1 rules, through
// internal/announce.Validate inside organnounce.ValidateBody) before
// anything is signed, and Source is forced to "org" here: provenance is
// the server's to assert, not the caller's, and the signature must
// cover honest provenance.
func announcementPublishHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req announcementPublishRequest
		// DecodeCapped, not json.NewDecoder(io.LimitReader(...)): the
		// latter caps the READ, not the document, and accepts anything
		// following the first JSON value (a second document, padding).
		// Publishing is the highest-privilege write on this server; its
		// input should be exactly one document and nothing else.
		if err := orgcontract.DecodeCapped(r.Body, maxAnnouncementPublishBytes, &req); err != nil {
			auth.WriteError(w, http.StatusBadRequest, "bad_request", "body must be exactly one JSON document within 1 MiB: {\"announcement\": {…}} or {\"retract\": true}")
			return
		}
		if req.Announcement == nil && !req.Retract {
			auth.WriteError(w, http.StatusBadRequest, "bad_request", "announcement is required (or retract: true)")
			return
		}
		if req.Announcement != nil && req.Retract {
			auth.WriteError(w, http.StatusBadRequest, "bad_request", "announcement and retract are mutually exclusive")
			return
		}

		body := "" // the retraction document
		if req.Announcement != nil {
			a := *req.Announcement
			a.Source = announce.SourceOrg
			encoded, err := announce.Encode([]announce.Announcement{a})
			if err != nil {
				auth.WriteError(w, http.StatusBadRequest, "invalid_announcement", err.Error())
				return
			}
			body = encoded
		}

		actor, _ := auth.UserIDFromContext(r.Context())
		if actor == "" {
			actor = "admin"
		}
		doc, err := organnounce.Publish(r.Context(), db, body, actor)
		if errors.Is(err, organnounce.ErrInvalidBody) {
			auth.WriteError(w, http.StatusBadRequest, "invalid_announcement", err.Error())
			return
		}
		if err != nil {
			auth.WriteError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	}
}

// announcementGetHandler is the agent fetch endpoint (mounted behind
// the enrolment bearer). 404 when nothing has ever been published; a
// RETRACTION is a 200 with an empty body — the node must receive it to
// clear the banner it is currently showing.
func announcementGetHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		doc, ok, err := organnounce.Latest(r.Context(), db)
		if err != nil {
			auth.WriteError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		if !ok {
			auth.WriteError(w, http.StatusNotFound, "not_found", "no announcement published")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	}
}

// announcementCurrentHandler is the admin READ of what is currently
// published (mounted behind requireAdminSAML), so the web2 composer can
// show the live banner and offer Retract without going through the
// agent-bearer endpoint. Returns 200 with published=false when nothing
// has ever been published — the composer's empty state is not an error.
func announcementCurrentHandler(db *sql.DB) http.HandlerFunc {
	type response struct {
		Published     bool                    `json:"published"`
		Version       int64                   `json:"version"`
		Retracted     bool                    `json:"retracted"`
		Announcements []announce.Announcement `json:"announcements"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		doc, ok, err := organnounce.Latest(r.Context(), db)
		if err != nil {
			auth.WriteError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		resp := response{Announcements: []announce.Announcement{}}
		if ok {
			resp.Published = true
			resp.Version = doc.Version
			// A stored body always passed ValidateBody before it was
			// signed, so a decode failure here means the row was
			// tampered with underneath us. Report it as "nothing
			// readable is published" rather than 500 — the admin's next
			// action (publish a fresh version) fixes it either way.
			list, derr := announce.Decode(doc.Body)
			if derr == nil {
				resp.Announcements = append(resp.Announcements, list...)
			}
			resp.Retracted = len(resp.Announcements) == 0
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}
