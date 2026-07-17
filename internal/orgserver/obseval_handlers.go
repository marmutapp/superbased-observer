package orgserver

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"github.com/marmutapp/superbased-observer/internal/orgserver/auth"
	"github.com/marmutapp/superbased-observer/internal/orgserver/rollup"
)

// obseval_handlers.go is the READ-ONLY org per-item eval surface (Plane-A
// eval-run detail org tier, gap-audit 2026-07-10 §1 / §2.2 / §6), mounted behind
// the SAML admin session. It surfaces the per-item eval scores enrolled nodes
// share under [org_client.share.obs].eval_items: a run list, one run's per-item
// scores, and a run-vs-run comparison — all content-free. The item content
// excerpts are a DEEPER, server-audited disclosure on a distinct route. Manual
// routes (no OpenAPI codegen), mirroring obsAdmissionHandler / obsEndUserSpendHandler.

// obsEvalRunsHandler lists the per-item eval runs shared over the trailing
// ?days= window (default 30). Admin scope; the session user id rides the
// context injected by requireAdminSAML.
func obsEvalRunsHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		days, ok := admissionDaysParam(w, r)
		if !ok {
			return
		}
		userID, _ := auth.UserIDFromContext(r.Context())
		res, err := rollup.ObsEvalRuns(r.Context(), db, rollup.Window{Days: days}, rollup.Scope{Admin: true}, userID, time.Now())
		if err != nil {
			auth.WriteError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		writeJSON(w, res)
	}
}

// obsEvalRunHandler returns one run's summary + per-item scores. ?id= is the
// opaque run ref from the runs list; ?days= bounds the window for the summary
// context (default 30). Missing id → 400; unknown/out-of-scope ref → 404.
func obsEvalRunHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ref := r.URL.Query().Get("id")
		if ref == "" {
			auth.WriteError(w, http.StatusBadRequest, "bad_request", "id (run ref) is required")
			return
		}
		days, ok := admissionDaysParam(w, r)
		if !ok {
			return
		}
		userID, _ := auth.UserIDFromContext(r.Context())
		res, found, err := rollup.ObsEvalRunDetail(r.Context(), db, ref, rollup.Window{Days: days}, rollup.Scope{Admin: true}, userID)
		if err != nil {
			auth.WriteError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		if !found {
			auth.WriteError(w, http.StatusNotFound, "not_found", "run not found")
			return
		}
		writeJSON(w, res)
	}
}

// obsEvalCompareHandler returns two run summaries + per-scorer paired deltas.
// ?base= and ?compare= are opaque run refs. Missing either → 400; either
// unknown/out-of-scope → 404.
func obsEvalCompareHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		base := r.URL.Query().Get("base")
		compare := r.URL.Query().Get("compare")
		if base == "" || compare == "" {
			auth.WriteError(w, http.StatusBadRequest, "bad_request", "base and compare (run refs) are required")
			return
		}
		userID, _ := auth.UserIDFromContext(r.Context())
		res, found, err := rollup.ObsEvalCompare(r.Context(), db, base, compare, rollup.Scope{Admin: true}, userID)
		if err != nil {
			auth.WriteError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		if !found {
			auth.WriteError(w, http.StatusNotFound, "not_found", "run not found")
			return
		}
		writeJSON(w, res)
	}
}

// obsEvalRunContentHandler is the AUDITED per-item eval content export. The
// item input/expected/output excerpts + scorer rationale may quote the app
// request/response, so this is a DEEPER disclosure than the content-free
// scores: the handler writes a distinct view_eval_item_content audit row BEFORE
// the read and refuses the disclosure if that write fails (mirrors
// obsAdmissionReasonsHandler / OrgObsTraceContent). ?id= is the run ref.
func obsEvalRunContentHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ref := r.URL.Query().Get("id")
		if ref == "" {
			auth.WriteError(w, http.StatusBadRequest, "bad_request", "id (run ref) is required")
			return
		}
		userID, _ := auth.UserIDFromContext(r.Context())
		if err := rollup.WriteAudit(r.Context(), db, userID, rollup.ActionViewEvalItemContent, "", ref, sourceIPOf(r), time.Now()); err != nil {
			auth.WriteError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		res, found, err := rollup.ObsEvalItemContent(r.Context(), db, ref, rollup.Scope{Admin: true}, userID)
		if err != nil {
			auth.WriteError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		if !found {
			auth.WriteError(w, http.StatusNotFound, "not_found", "run not found")
			return
		}
		writeJSON(w, res)
	}
}

// writeJSON writes v as a JSON response. Small local helper for the manual obs
// eval routes (the org-server package's twin of the dashboard encoder).
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
