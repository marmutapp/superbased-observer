package messagesummary

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// PricingLookup is the minimal contract the [DBRecorder] needs from
// the cost engine to convert a [SummaryCall]'s token counts into a
// USD figure for the summary_calls.cost_usd column. Mirrors
// (*cost.Engine).Lookup; declared here so messagesummary stays
// import-cycle-free.
type PricingLookup interface {
	Lookup(model string) (Pricing, bool)
}

// Pricing is the per-model rate set the [DBRecorder] consumes. Field
// units mirror the proxy / cost-engine contract: USD per 1M tokens.
type Pricing struct {
	Input           float64
	Output          float64
	CacheRead       float64
	CacheCreation   float64
	CacheCreation1h float64
}

// DBRecorder writes one row per summary call into the summary_calls
// table (migration 016). cost_usd is computed at insert time using
// the [PricingLookup] so the dashboard's cost-net surface can sum
// the column directly without re-pricing per query.
//
// Best-effort: errors propagate to the caller (the
// AnthropicSummarizer swallows them) so a transient DB issue or a
// closed handle on shutdown doesn't fail summarisation. Tests verify
// the row IS written when the DB is healthy.
type DBRecorder struct {
	db      *sql.DB
	pricing PricingLookup
}

// NewDBRecorder wraps db + pricing.
func NewDBRecorder(db *sql.DB, pricing PricingLookup) *DBRecorder {
	return &DBRecorder{db: db, pricing: pricing}
}

// Record implements [CallRecorder].
func (r *DBRecorder) Record(ctx context.Context, call SummaryCall) error {
	if r == nil || r.db == nil {
		return nil
	}
	cost := computeSummaryCost(call, r.pricing)
	var sessionID any
	if call.SessionID != "" {
		sessionID = call.SessionID
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO summary_calls
		   (session_id, timestamp, model, input_tokens, output_tokens,
		    cache_read_tokens, cache_creation_tokens, cost_usd)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, time.Now().UTC().Format(time.RFC3339Nano),
		call.Model, call.InputTokens, call.OutputTokens,
		call.CacheReadTokens, call.CacheCreationTokens, cost)
	if err != nil {
		return fmt.Errorf("messagesummary.DBRecorder.Record: %w", err)
	}
	return nil
}

// computeSummaryCost prices a call using the supplied lookup. Returns
// 0 when the model isn't recognised — the dashboard surfaces this as
// "missing pricing" rather than a billing claim.
func computeSummaryCost(call SummaryCall, lookup PricingLookup) float64 {
	if lookup == nil {
		return 0
	}
	p, ok := lookup.Lookup(call.Model)
	if !ok {
		return 0
	}
	const perMillion = 1_000_000.0
	return float64(call.InputTokens)*p.Input/perMillion +
		float64(call.OutputTokens)*p.Output/perMillion +
		float64(call.CacheReadTokens)*p.CacheRead/perMillion +
		float64(call.CacheCreationTokens)*p.CacheCreation/perMillion
}
