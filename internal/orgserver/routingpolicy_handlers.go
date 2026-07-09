package orgserver

import (
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/marmutapp/superbased-observer/internal/orgserver/auth"
	"github.com/marmutapp/superbased-observer/internal/orgserver/routingpolicy"
)

// routingPolicyPublishHandler is the §R19.2 admin publish endpoint
// (mounted behind the SAML session). Body: {"body": "<toml fragment>"}.
func routingPolicyPublishHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Body string `json:"body"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || req.Body == "" {
			auth.WriteError(w, http.StatusBadRequest, "bad_request", "body (TOML fragment) is required")
			return
		}
		actor, _ := auth.UserIDFromContext(r.Context())
		if actor == "" {
			actor = "admin"
		}
		doc, err := routingpolicy.Publish(r.Context(), db, req.Body, actor)
		if err != nil {
			auth.WriteError(w, http.StatusBadRequest, "invalid_policy", err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	}
}

// routingPolicyGetHandler is the agent fetch endpoint (mounted behind
// the enrolment bearer). 404 when no policy has been published.
func routingPolicyGetHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		doc, ok, err := routingpolicy.Latest(r.Context(), db)
		if err != nil {
			auth.WriteError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		if !ok {
			auth.WriteError(w, http.StatusNotFound, "not_found", "no routing policy published")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	}
}

// routingSummariesExportHandler is the §R19.5 org-side rollup export
// (mounted behind the SAML admin session): the aggregated
// routing_summaries table as CSV or JSON for finance / compliance.
func routingSummariesExportHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		format := r.URL.Query().Get("format")
		if format == "" {
			format = "csv"
		}
		rows, err := db.QueryContext(r.Context(), `
			SELECT org_id, user_email, day, tier, reason, mode,
			       decisions, applied, est_savings_usd, cache_forfeit_usd
			FROM routing_summaries ORDER BY day DESC, tier, reason`)
		if err != nil {
			auth.WriteError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		defer func() { _ = rows.Close() }()
		type expRow struct {
			OrgID           string  `json:"org_id"`
			UserEmail       string  `json:"user_email"`
			Day             string  `json:"day"`
			Tier            string  `json:"tier"`
			Reason          string  `json:"reason"`
			Mode            string  `json:"mode"`
			Decisions       int64   `json:"decisions"`
			Applied         int64   `json:"applied"`
			EstSavingsUSD   float64 `json:"est_savings_usd"`
			CacheForfeitUSD float64 `json:"cache_forfeit_usd"`
		}
		var out []expRow
		for rows.Next() {
			var e expRow
			if err := rows.Scan(&e.OrgID, &e.UserEmail, &e.Day, &e.Tier, &e.Reason, &e.Mode,
				&e.Decisions, &e.Applied, &e.EstSavingsUSD, &e.CacheForfeitUSD); err != nil {
				auth.WriteError(w, http.StatusInternalServerError, "internal", err.Error())
				return
			}
			out = append(out, e)
		}
		if err := rows.Err(); err != nil {
			auth.WriteError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		if format == "json" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(out)
			return
		}
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", `attachment; filename="routing_summaries.csv"`)
		cw := csv.NewWriter(w)
		_ = cw.Write([]string{
			"org_id", "user_email", "day", "tier", "reason", "mode",
			"decisions", "applied", "est_savings_usd", "cache_forfeit_usd",
		})
		for _, e := range out {
			_ = cw.Write([]string{
				e.OrgID, e.UserEmail, e.Day, e.Tier, e.Reason, e.Mode,
				strconv.FormatInt(e.Decisions, 10), strconv.FormatInt(e.Applied, 10),
				strconv.FormatFloat(e.EstSavingsUSD, 'f', 6, 64),
				strconv.FormatFloat(e.CacheForfeitUSD, 'f', 6, 64),
			})
		}
		cw.Flush()
	}
}
