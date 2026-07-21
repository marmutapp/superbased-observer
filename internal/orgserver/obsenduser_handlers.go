package orgserver

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/marmutapp/superbased-observer/internal/orgserver/auth"
	"github.com/marmutapp/superbased-observer/internal/orgserver/rollup"
)

// obsEndUserSpendHandler is the T5 per-END-USER spend admin export (org-budget
// guardrails plan §2.1), mounted behind the SAML admin session. It reports the
// CROSS-INSTANCE spend attributed to each hosted-app end-user over the trailing
// ?days= window (default 30) — the aggregation the node-local budget surfaces
// can't provide. Manual route (no OpenAPI codegen), mirroring
// routingSummariesExportHandler.
func obsEndUserSpendHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		days := 30
		if v := r.URL.Query().Get("days"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				auth.WriteError(w, http.StatusBadRequest, "bad_request", "days must be a positive integer")
				return
			}
			days = n
		}
		res, err := rollup.ObsEndUserSpend(r.Context(), db, rollup.Window{Days: days}, time.Now())
		if err != nil {
			auth.WriteError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
	}
}
