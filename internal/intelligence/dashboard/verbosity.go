package dashboard

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sort"

	"github.com/marmutapp/superbased-observer/internal/store"
	"github.com/marmutapp/superbased-observer/internal/verbosity"
)

// VerbosityResponse is the JSON payload for GET /api/session/<id>/verbosity —
// the Output Composition (Verbosity) session card
// (docs/plans/output-composition-verbosity-plan-2026-06-30.md). Read-side
// only: visible narrative/artifact bytes segmented from assistant text +
// authored code bytes (writes + shell commands) from actions.content_bytes.
type VerbosityResponse struct {
	SessionID    string  `json:"session_id"`
	TotalBytes   int64   `json:"total_bytes"`
	CodeBytes    int64   `json:"code_bytes"`
	ExplainBytes int64   `json:"explain_bytes"`
	CodePct      float64 `json:"code_pct"`
	ExplainPct   float64 `json:"explain_pct"`
	// CodeExplainRatio is code÷explanation; nil when there is no
	// explanation (the surface shows "—" rather than ∞).
	CodeExplainRatio *float64 `json:"code_explain_ratio,omitempty"`

	ByCategory map[string]int64  `json:"by_category"`
	Channels   verbosityChannels `json:"channels"`

	CodeByLanguage []langBytes `json:"code_by_language"`
	UnknownExt     []langBytes `json:"unknown_ext,omitempty"`

	// AuthoredCaptured is false when the session HAS write/edit/command
	// actions but none carry a content_bytes measurement yet (pre-feature
	// rows / a daemon not on the content_bytes build). The surface then
	// shows a "re-run `observer backfill` to include authored code" hint
	// instead of implying the session was explanation-only.
	AuthoredCaptured bool `json:"authored_captured"`

	// Cost is the ESTIMATED token/$ attribution (plan §7) — output_tokens
	// apportioned across the byte buckets by per-class chars/token, priced at
	// the model's output rate. CostEstimated is false when there's no
	// model/pricing/output tokens, and the surface then hides the $ entirely
	// rather than showing zeros. Every $ figure is labelled "est." on the card.
	CostEstimated      bool    `json:"cost_estimated"`
	Model              string  `json:"model,omitempty"`
	EstOutputTokens    int64   `json:"est_output_tokens,omitempty"`
	EstReasoningTokens int64   `json:"est_reasoning_tokens,omitempty"`
	EstCodeTokens      int64   `json:"est_code_tokens,omitempty"`
	EstExplainTokens   int64   `json:"est_explain_tokens,omitempty"`
	EstCodeUSD         float64 `json:"est_code_usd,omitempty"`
	EstExplainUSD      float64 `json:"est_explain_usd,omitempty"`
	EstTotalUSD        float64 `json:"est_total_usd,omitempty"`
}

// verbosityCost is the handler-resolved pricing input to the pure
// buildVerbosityResponse — the session's model, summed output/reasoning
// tokens, and the model's PER-TOKEN output rate (lookupRates already divides
// by 1e6). Nil when no priced model / no tokens.
type verbosityCost struct {
	Model              string
	OutputTokens       int64
	ReasoningTokens    int64
	OutputRatePerToken float64
}

type verbosityChannels struct {
	NarrativeBytes        int64 `json:"narrative_bytes"`
	ArtifactBytes         int64 `json:"artifact_bytes"`
	ArtifactUntaggedBytes int64 `json:"artifact_untagged_bytes"`
	WrittenBytes          int64 `json:"written_bytes"`
	CommandBytes          int64 `json:"command_bytes"`
}

type langBytes struct {
	Language string `json:"language"`
	Bytes    int64  `json:"bytes"`
	Category string `json:"category"`
}

// handleSessionVerbosity serves GET /api/session/<id>/verbosity. Sub-route
// under handleSessionDetail.
func (s *Server) handleSessionVerbosity(w http.ResponseWriter, r *http.Request, sessionID string) {
	if sessionID == "" {
		http.Error(w, "missing session id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	st := store.New(s.opts.DB)

	b, err := st.LoadSessionVerbosity(ctx, sessionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, fmt.Sprintf("load verbosity: %v", err), http.StatusInternalServerError)
		return
	}
	captured, totalAuthored, err := st.AuthoredCaptureStats(ctx, sessionID)
	if err != nil {
		http.Error(w, fmt.Sprintf("authored stats: %v", err), http.StatusInternalServerError)
		return
	}

	// Resolve the est token/$ cost input (best-effort — a missing model,
	// pricing entry, or token rows just omits the $, never errors the card).
	var vc *verbosityCost
	if model, outTok, reasonTok, terr := st.SessionTokenTotals(ctx, sessionID); terr == nil && model != "" && outTok > 0 {
		if rates, ok := lookupRates(s.opts.CostEngine, model); ok && rates.Output > 0 {
			vc = &verbosityCost{Model: model, OutputTokens: outTok, ReasoningTokens: reasonTok, OutputRatePerToken: rates.Output}
		}
	}

	writeJSON(w, buildVerbosityResponse(sessionID, b, captured, totalAuthored, vc))
}

// buildVerbosityResponse shapes a Breakdown into the API payload. Pure (no
// I/O) so it is unit-testable without a server. cost is nil when no priced
// model / no tokens — the est token/$ fields stay zero and CostEstimated=false.
func buildVerbosityResponse(sessionID string, b *verbosity.Breakdown, captured, totalAuthored int64, cost *verbosityCost) VerbosityResponse {
	cats := b.ByCategory()
	byCat := make(map[string]int64, len(cats))
	var total int64
	for c, v := range cats {
		byCat[string(c)] = v
		total += v
	}
	code := b.CodeBytes()
	explain := b.ExplainBytes()

	resp := VerbosityResponse{
		SessionID:    sessionID,
		TotalBytes:   total,
		CodeBytes:    code,
		ExplainBytes: explain,
		ByCategory:   byCat,
		Channels: verbosityChannels{
			NarrativeBytes:        b.Visible.NarrativeBytes,
			ArtifactBytes:         b.Visible.ArtifactBytes,
			ArtifactUntaggedBytes: b.Visible.ArtifactUntaggedBytes,
			WrittenBytes:          sumLangMap(b.Written) + sumLangMap(b.WrittenUnknownExt),
			CommandBytes:          sumLangMap(b.Command),
		},
		CodeByLanguage:   sortedLangBytes(b.CodeByLang()),
		UnknownExt:       sortedLangBytes(b.WrittenUnknownExt),
		AuthoredCaptured: totalAuthored == 0 || captured > 0,
	}
	if total > 0 {
		resp.CodePct = 100 * float64(code) / float64(total)
		resp.ExplainPct = 100 * float64(explain) / float64(total)
	}
	if explain > 0 {
		r := float64(code) / float64(explain)
		resp.CodeExplainRatio = &r
	}

	// Est token/$ split (plan §7): apportion the session's non-reasoning
	// output tokens across the byte buckets, price at the model's output rate.
	// Reasoning is billed at the same rate as a distinct slice, so total $
	// covers output + reasoning while the code/explain split covers only the
	// apportioned output.
	if cost != nil && cost.OutputTokens > 0 && cost.OutputRatePerToken > 0 {
		split := verbosity.EstimateTokens(b, cost.OutputTokens)
		codeUSD, explainUSD, totalUSD := split.Cost(cost.OutputRatePerToken, cost.ReasoningTokens)
		resp.CostEstimated = true
		resp.Model = cost.Model
		resp.EstOutputTokens = cost.OutputTokens
		resp.EstReasoningTokens = cost.ReasoningTokens
		resp.EstCodeTokens = split.CodeTokens
		resp.EstExplainTokens = split.ExplainTokens
		resp.EstCodeUSD = codeUSD
		resp.EstExplainUSD = explainUSD
		resp.EstTotalUSD = totalUSD
	}
	return resp
}

// sortedLangBytes turns a language→bytes map into a descending slice with
// the category attached (so the frontend gets a stable order + colouring).
func sortedLangBytes(m map[string]int64) []langBytes {
	out := make([]langBytes, 0, len(m))
	for lang, by := range m {
		out = append(out, langBytes{Language: lang, Bytes: by, Category: string(verbosity.CategoryOf(lang))})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Bytes != out[j].Bytes {
			return out[i].Bytes > out[j].Bytes
		}
		return out[i].Language < out[j].Language
	})
	return out
}

func sumLangMap(m map[string]int64) int64 {
	var s int64
	for _, v := range m {
		s += v
	}
	return s
}
