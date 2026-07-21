package orgserver

import (
	"database/sql"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/orgserver/auth"
	"github.com/marmutapp/superbased-observer/internal/orgserver/rollup"
)

// obsAdmissionHandler is the READ-ONLY org input-admission monitoring export
// (Plane-A admission org tier, gap-audit 2026-07-10 §1b), mounted behind the
// SAML admin session. It reports the admission posture + verdict timeline +
// would-block overlay + policy version history enrolled nodes shared over the
// trailing ?days= window (default 30). Manual route (no OpenAPI codegen),
// mirroring obsEndUserSpendHandler. Admin-only, so the rollup runs with an
// admin scope; the session user id (used as the rollup self id) rides the
// context injected by requireAdminSAML.
func obsAdmissionHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		days, ok := admissionDaysParam(w, r)
		if !ok {
			return
		}
		userID, _ := auth.UserIDFromContext(r.Context())
		res, err := rollup.ObsAdmission(r.Context(), db, rollup.Window{Days: days}, rollup.Scope{Admin: true}, userID, time.Now())
		if err != nil {
			auth.WriteError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
	}
}

// obsAdmissionReasonsHandler is the AUDITED admission-reason excerpt export. The
// human-readable verdict prose (reason_excerpt) may quote the end-user request,
// so this is a DEEPER disclosure than the content-free monitor: the handler
// writes a distinct view_admission_reasons audit row BEFORE the read and
// refuses the disclosure if that write fails (mirrors OrgObsTraceContent).
func obsAdmissionReasonsHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		days, ok := admissionDaysParam(w, r)
		if !ok {
			return
		}
		userID, _ := auth.UserIDFromContext(r.Context())
		if err := rollup.WriteAudit(r.Context(), db, userID, rollup.ActionViewAdmissionReasons, "", "", sourceIPOf(r), time.Now()); err != nil {
			auth.WriteError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		res, err := rollup.ObsAdmissionReasons(r.Context(), db, rollup.Window{Days: days}, rollup.Scope{Admin: true}, userID)
		if err != nil {
			auth.WriteError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
	}
}

// admissionDaysParam parses the trailing ?days= window (default 30). A present
// but non-positive/unparseable value is a 400 (mirrors obsEndUserSpendHandler).
func admissionDaysParam(w http.ResponseWriter, r *http.Request) (int, bool) {
	days := 30
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			auth.WriteError(w, http.StatusBadRequest, "bad_request", "days must be a positive integer")
			return 0, false
		}
		days = n
	}
	return days, true
}

// sourceIPOf returns the first X-Forwarded-For hop if present, else the
// request's remote host. (The dashboard package has an unexported twin; this is
// the org-server-package copy for the manual audited routes.)
func sourceIPOf(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first, _, found := strings.Cut(xff, ","); found {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(xff)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
