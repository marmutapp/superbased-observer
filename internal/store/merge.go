package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/turnmerge"
)

// Turn-provenance source tags persisted in api_turns.source (migration 047).
// They map to a turnmerge.Fidelity via fidelityForSource — the ONE place a
// source string becomes a fidelity rank, so the merge core
// (internal/turnmerge) stays blind to source identity (CLAUDE.md module
// rule #3).
const (
	// SourceProxy is a proxy intercept (exact tokens, cache-tier split, 1h
	// surcharge). Also the implicit value of a legacy NULL source — every
	// api_turns row written before migration 047 came from the proxy.
	SourceProxy = "proxy"
	// SourceCCOTel is Claude Code's native OTel (exact tokens, no proxy-only
	// fields). Written by the native-console ingest receiver.
	SourceCCOTel = "cc_otel"
	// SourceJSONL is a JSONL usage envelope (approximate). api_turns is not
	// written from the JSONL path today; reserved for completeness.
	SourceJSONL = "jsonl"
	// SourceObsSDK / SourceObsOTLP are the generalized-observability
	// subsystem's LLM-span sources (internal/obs; plan §5). SDK- and
	// instrumentor-reported token counts are estimates, so both rank
	// Approx — a proxy or native-OTel turn for the same request_id always
	// wins. These string values mirror internal/obs/span.Source. When obs
	// is removed they are simply unused strings; no edit is needed.
	SourceObsSDK  = "sdk_otlp"
	SourceObsOTLP = "otlp_trace"
)

// fidelityForSource maps a persisted source tag to its merge fidelity. An
// empty/unknown source is treated as proxy (the historical sole writer of
// api_turns), so legacy rows keep top rank without a backfill.
func fidelityForSource(source string) turnmerge.Fidelity {
	switch source {
	case SourceCCOTel:
		return turnmerge.FidelityNativeExact
	case SourceJSONL, SourceObsSDK, SourceObsOTLP:
		return turnmerge.FidelityApprox
	default: // "" (legacy) and SourceProxy
		return turnmerge.FidelityProxyExact
	}
}

// mergeTurnRow holds the existing api_turns columns that participate in
// cross-source merge, plus its id and source for the precedence decision.
type mergeTurnRow struct {
	id     int64
	source string
	turn   turnmerge.Turn
}

// UpsertTurnByRequestID records a turn observation, deduping against any
// existing api_turns row for the same request_id by source-derived fidelity
// (the native-console dedup core, Phase 2). It returns the action taken and the
// id of the affected row.
//
//   - No existing row (or t.RequestID == "", which has no dedup key) → the turn
//     is inserted via InsertAPITurn; action is turnmerge.ActionInsert. A
//     native-only turn (no proxy) lands here at full fidelity — the no-proxy
//     coverage-gap fill.
//   - An existing row → the observation is merged per turnmerge precedence:
//     authoritative token/cost fields take the higher fidelity (equal → MAX);
//     enrichment fields (ttft, total_response_ms, stop_reason) fill only if
//     absent. action is ActionUpdate when something changed, else
//     ActionNoChange.
//
// NOTE (Phase-3 hardening): the read-modify-write is not yet wrapped in a
// transaction, so two sources inserting the same request_id in the same instant
// could briefly produce two rows. Today the proxy is the only writer of
// api_turns; the native receiver (Phase 2b) is the first concurrent writer, and
// turn telemetry is at-least-once. Phase 3 adds a guarded unique index once
// legacy duplicate/empty request_ids are confirmed handled.
func (s *Store) UpsertTurnByRequestID(ctx context.Context, t models.APITurn) (turnmerge.Action, int64, error) {
	if t.RequestID == "" {
		id, err := s.InsertAPITurn(ctx, t)
		if err != nil {
			return turnmerge.ActionInsert, 0, fmt.Errorf("store.UpsertTurnByRequestID: %w", err)
		}
		return turnmerge.ActionInsert, id, nil
	}

	existing, err := s.loadMergeTurn(ctx, t.RequestID)
	if err != nil {
		return turnmerge.ActionNoChange, 0, fmt.Errorf("store.UpsertTurnByRequestID: %w", err)
	}

	incoming := turnFromAPITurn(t)
	var existingTurn *turnmerge.Turn
	if existing != nil {
		existingTurn = &existing.turn
	}

	res := turnmerge.Merge(existingTurn, incoming)
	switch res.Action {
	case turnmerge.ActionInsert:
		id, err := s.InsertAPITurn(ctx, t)
		if err != nil {
			return res.Action, 0, fmt.Errorf("store.UpsertTurnByRequestID: %w", err)
		}
		return res.Action, id, nil
	case turnmerge.ActionUpdate:
		if err := s.updateMergeTurn(ctx, existing.id, res.Turn); err != nil {
			return res.Action, existing.id, fmt.Errorf("store.UpsertTurnByRequestID: %w", err)
		}
		return res.Action, existing.id, nil
	default: // ActionNoChange
		return res.Action, existing.id, nil
	}
}

// turnFromAPITurn projects a full models.APITurn into the merge view, assigning
// fidelity from its source tag at the boundary.
func turnFromAPITurn(t models.APITurn) turnmerge.Turn {
	return turnmerge.Turn{
		RequestID:             t.RequestID,
		Fidelity:              fidelityForSource(t.Source),
		InputTokens:           t.InputTokens,
		OutputTokens:          t.OutputTokens,
		CacheReadTokens:       t.CacheReadTokens,
		CacheCreationTokens:   t.CacheCreationTokens,
		CacheCreation1hTokens: t.CacheCreation1hTokens,
		WebSearchRequests:     t.WebSearchRequests,
		CostUSD:               t.CostUSD,
		TimeToFirstTokenMS:    t.TimeToFirstTokenMS,
		TotalResponseMS:       t.TotalResponseMS,
		StopReason:            t.StopReason,
	}
}

// loadMergeTurn reads the oldest api_turns row for request_id (lowest id, the
// canonical one when legacy duplicates exist) into the merge view. Returns nil
// when no row exists.
func (s *Store) loadMergeTurn(ctx context.Context, requestID string) (*mergeTurnRow, error) {
	var (
		r   mergeTurnRow
		src sql.NullString
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, COALESCE(source,''),
		        input_tokens, output_tokens,
		        COALESCE(cache_read_tokens,0), COALESCE(cache_creation_tokens,0),
		        COALESCE(cache_creation_1h_tokens,0), COALESCE(web_search_requests,0),
		        COALESCE(cost_usd,0),
		        COALESCE(time_to_first_token_ms,0), COALESCE(total_response_ms,0),
		        COALESCE(stop_reason,'')
		   FROM api_turns
		  WHERE request_id = ? ORDER BY id ASC LIMIT 1`, requestID).Scan(
		&r.id, &src,
		&r.turn.InputTokens, &r.turn.OutputTokens,
		&r.turn.CacheReadTokens, &r.turn.CacheCreationTokens,
		&r.turn.CacheCreation1hTokens, &r.turn.WebSearchRequests,
		&r.turn.CostUSD,
		&r.turn.TimeToFirstTokenMS, &r.turn.TotalResponseMS,
		&r.turn.StopReason,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("loadMergeTurn: %w", err)
	}
	r.source = src.String
	r.turn.RequestID = requestID
	r.turn.Fidelity = fidelityForSource(r.source)
	return &r, nil
}

// updateMergeTurn writes the merged authoritative + enrichment columns back to
// the existing row, stamping the source that corresponds to the merged
// fidelity so a later read resolves the same rank.
func (s *Store) updateMergeTurn(ctx context.Context, id int64, merged turnmerge.Turn) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE api_turns SET
		    input_tokens = ?, output_tokens = ?,
		    cache_read_tokens = ?, cache_creation_tokens = ?,
		    cache_creation_1h_tokens = ?, web_search_requests = ?,
		    cost_usd = ?,
		    time_to_first_token_ms = ?, total_response_ms = ?,
		    stop_reason = ?, source = ?
		  WHERE id = ?`,
		merged.InputTokens, merged.OutputTokens,
		merged.CacheReadTokens, merged.CacheCreationTokens,
		merged.CacheCreation1hTokens, merged.WebSearchRequests,
		merged.CostUSD,
		merged.TimeToFirstTokenMS, merged.TotalResponseMS,
		nullableString(merged.StopReason), sourceForFidelity(merged.Fidelity),
		id)
	if err != nil {
		return fmt.Errorf("updateMergeTurn: %w", err)
	}
	return nil
}

// sourceForFidelity is the inverse of fidelityForSource for persisting a merged
// row's rank. ProxyExact persists as the explicit "proxy" tag (never empty) so
// a merged row's provenance is unambiguous after an update.
func sourceForFidelity(f turnmerge.Fidelity) string {
	switch f {
	case turnmerge.FidelityNativeExact:
		return SourceCCOTel
	case turnmerge.FidelityApprox:
		return SourceJSONL
	default:
		return SourceProxy
	}
}
