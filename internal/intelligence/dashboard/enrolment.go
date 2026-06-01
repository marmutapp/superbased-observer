package dashboard

import (
	"net/http"
)

// enrolmentStatusResponse is the wire shape of GET /api/enrolment/status.
// Enrolled is always present; the rest are populated only when enrolled.
type enrolmentStatusResponse struct {
	Enrolled        bool         `json:"enrolled"`
	OrgID           string       `json:"org_id,omitempty"`
	OrgName         string       `json:"org_name,omitempty"`
	OrgServerURL    string       `json:"org_server_url,omitempty"`
	UserEmail       string       `json:"user_email,omitempty"`
	EnrolledAt      string       `json:"enrolled_at,omitempty"`
	CredentialStore string       `json:"credential_store,omitempty"`
	LastPush        *lastPushDTO `json:"last_push,omitempty"`
}

type lastPushDTO struct {
	PushedAt string `json:"pushed_at"`
	Status   string `json:"status"`
	RowCount int64  `json:"row_count"`
	Bytes    int64  `json:"bytes"`
	Error    string `json:"error,omitempty"`
}

// handleEnrolmentStatus serves GET /api/enrolment/status. When org mode is off
// (OrgClient nil) it reports {enrolled:false}, so the web UI hides the org
// surface on a solo-local install. Errors are isolated to this endpoint (the
// mux gives each handler its own request scope; a 500 here cannot affect any
// other endpoint — P1).
func (s *Server) handleEnrolmentStatus(w http.ResponseWriter, r *http.Request) {
	if s.opts.OrgClient == nil {
		writeJSON(w, enrolmentStatusResponse{Enrolled: false})
		return
	}
	st, err := s.opts.OrgClient.Status(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	resp := enrolmentStatusResponse{
		Enrolled:        st.Enrolled,
		OrgID:           st.OrgID,
		OrgName:         st.OrgName,
		OrgServerURL:    st.OrgServerURL,
		UserEmail:       st.UserEmail,
		EnrolledAt:      st.EnrolledAt,
		CredentialStore: st.Backend,
	}
	if st.LastPush != nil {
		resp.LastPush = &lastPushDTO{
			PushedAt: st.LastPush.PushedAt,
			Status:   st.LastPush.Status,
			RowCount: st.LastPush.RowCount,
			Bytes:    st.LastPush.Bytes,
			Error:    st.LastPush.Error,
		}
	}
	writeJSON(w, resp)
}

// handleEnrolmentLastPayload serves GET /api/enrolment/last-payload: the exact
// JSON of the most recent shared rollup, byte-for-byte (the transparency
// view — "show me precisely what was sent"). Returns the JSON literal null when
// org mode is off or nothing has been pushed yet.
func (s *Server) handleEnrolmentLastPayload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if s.opts.OrgClient == nil {
		_, _ = w.Write([]byte("null"))
		return
	}
	payload, err := s.opts.OrgClient.LastPayload(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	if len(payload) == 0 {
		_, _ = w.Write([]byte("null"))
		return
	}
	_, _ = w.Write(payload) // verbatim — must equal the pushed bytes
}

// handleEnrolmentUnenroll serves POST /api/enrolment/unenroll: leaves the org
// and clears local credentials. Idempotent — a no-op when not enrolled / org
// mode off.
func (s *Server) handleEnrolmentUnenroll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.opts.OrgClient == nil {
		writeJSON(w, map[string]bool{"unenrolled": false})
		return
	}
	if err := s.opts.OrgClient.Unenroll(r.Context()); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]bool{"unenrolled": true})
}
