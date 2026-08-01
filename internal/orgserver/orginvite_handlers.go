package orgserver

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"github.com/marmutapp/superbased-observer/internal/orgserver/api"
	"github.com/marmutapp/superbased-observer/internal/orgserver/auth"
)

// enrolmentTokenDTO is one row of GET /api/org/enrolment-tokens. It carries
// NO token material — the secret half is never stored (only its argon2id
// hash), so it cannot be listed. token_id is the non-secret lookup key.
//
// minted_by_email is the inviter, empty for an unattributed mint (the
// `observer-org invite` CLI, and every token predating server migration 023).
// With redeemed, it is the invite→enrolment conversion view the Arc-2 plan
// asks for: computed on read, in the org's own DB, on no wire.
type enrolmentTokenDTO struct {
	TokenID       string `json:"token_id"`
	UserEmail     string `json:"user_email"`
	MintedByEmail string `json:"minted_by_email,omitempty"`
	CreatedAt     string `json:"created_at"`
	ExpiresAt     string `json:"expires_at"`
	UsedAt        string `json:"used_at,omitempty"`
	Redeemed      bool   `json:"redeemed"`
	Expired       bool   `json:"expired"`
}

// enrolmentTokensListHandler is the ADMIN read of every enrolment token
// (mounted behind requireAdminSAML — the same gate as the other /api/org/*
// admin reads). It names every developer who has been invited, so it stays
// admin-only even with [server].member_invites on: a delegated member sees
// only the token they themselves just minted, in the mint response.
func enrolmentTokensListHandler(db *sql.DB) http.HandlerFunc {
	type response struct {
		Tokens []enrolmentTokenDTO `json:"tokens"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		toks, err := api.ListEnrolmentTokens(r.Context(), db)
		if err != nil {
			auth.WriteError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		now := time.Now().UTC()
		resp := response{Tokens: make([]enrolmentTokenDTO, 0, len(toks))}
		for _, t := range toks {
			dto := enrolmentTokenDTO{
				TokenID:       t.TokenID,
				UserEmail:     t.UserEmail,
				MintedByEmail: t.MintedByEmail,
				CreatedAt:     t.CreatedAt.UTC().Format(time.RFC3339),
				ExpiresAt:     t.ExpiresAt.UTC().Format(time.RFC3339),
				Redeemed:      t.Redeemed(),
				Expired:       t.Expired(now),
			}
			if t.UsedAt != nil {
				dto.UsedAt = t.UsedAt.UTC().Format(time.RFC3339)
			}
			resp.Tokens = append(resp.Tokens, dto)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}
