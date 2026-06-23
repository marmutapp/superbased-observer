package main

import (
	"context"
	"log/slog"
	"time"

	otelexp "github.com/marmutapp/superbased-observer/internal/exporter/otel"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// Guard-event OTel feed ([guard.export].otel, guard spec §11.4 —
// G16). Like the §15 cloud dispatcher, this is a DAEMON-RESIDENT
// SWEEP of the guard_events tail — never a hot-path hook: verdicts
// recorded by ANY process (hook, watcher, proxy) reach the collector
// uniformly, with zero enforcement-path coupling and its own
// schema_meta cursor (guard_otel_cursor — independent of the cloud
// dispatcher's position).
//
// Gating (start.go): the tail runs only when [exporter.otel] is
// enabled (the exporter exists at all) AND [guard.export].otel is
// true AND guard is on — both export flags default off, so the
// solo-local no-network invariant holds twice over. Delivery is
// best-effort: the cursor advances at emission and the batching SDK
// owns retries; the audit chain is the record (§10.4), the collector
// is a convenience mirror.
//
// §15-boundary note: this feed deliberately does NOT route through
// notify.Egress — it joins the EXISTING Teams-M4 exporter rail
// ([exporter.otel]) per §11.4's placement, leaving §15.4 push
// alerting to the G15 webhook surface. One OTLP owner (the exporter),
// one guard-cloud owner (notify.Egress).

// guardOTelSweepBatch bounds one sweep's row read (the cloud
// dispatcher's batch size).
const guardOTelSweepBatch = 200

// guardSpanSink is the emission seam — satisfied by *otelexp.Exporter,
// stubbed by tests.
type guardSpanSink interface {
	ExportGuardEvent(ctx context.Context, ev otelexp.GuardEvent)
}

// guardOTelTail sweeps new guard_events rows into the OTel exporter.
type guardOTelTail struct {
	st       *store.Store
	sink     guardSpanSink
	logger   *slog.Logger
	interval time.Duration
}

// otelGuardEventFromRow maps a store guard_events row onto the
// exporter's GuardEvent at the boundary (the exporter never imports
// the store).
func otelGuardEventFromRow(r store.GuardEventRow) otelexp.GuardEvent {
	return otelexp.GuardEvent{
		ID: r.ID, TS: r.TS, SessionID: r.SessionID, Tool: r.Tool,
		EventKind: r.EventKind, RuleID: r.RuleID, Category: r.Category,
		Severity: r.Severity, Decision: r.Decision, DegradedFrom: r.DegradedFrom,
		Enforced: r.Enforced, Source: r.Source,
		TargetHash: r.TargetHash, ChainHash: r.ChainHash,
		Reason: r.Reason, TargetExcerpt: r.TargetExcerpt, TaintOrigin: r.TaintOrigin,
	}
}

// run is the daemon loop: anchor the cursor, then sweep on the
// exporter's poll cadence until ctx cancels.
func (t *guardOTelTail) run(ctx context.Context) {
	if err := t.anchorCursor(ctx); err != nil {
		t.logger.Warn("guard otel: cursor anchor failed; tail inert", "err", err)
		return
	}
	interval := t.interval
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		t.sweep(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// anchorCursor initialises the cursor at the CURRENT tail on first
// enable, so pre-existing history never floods the collector the day
// the operator turns the feed on (the cloud-dispatcher posture).
func (t *guardOTelTail) anchorCursor(ctx context.Context) error {
	cur, err := t.st.LoadGuardOTelCursor(ctx)
	if err != nil {
		return err
	}
	if cur != 0 {
		return nil
	}
	maxIDs, err := t.st.CurrentMaxIDs(ctx)
	if err != nil {
		return err
	}
	return t.st.SaveGuardOTelCursor(ctx, maxIDs.GuardEvents)
}

// sweep emits the new guard_events tail and advances the cursor.
func (t *guardOTelTail) sweep(ctx context.Context) {
	cur, err := t.st.LoadGuardOTelCursor(ctx)
	if err != nil {
		t.logger.Warn("guard otel: cursor load failed", "err", err)
		return
	}
	rows, err := t.st.GuardEventsAfter(ctx, cur, guardOTelSweepBatch)
	if err != nil {
		t.logger.Warn("guard otel: tail read failed", "err", err)
		return
	}
	for _, r := range rows {
		t.sink.ExportGuardEvent(ctx, otelGuardEventFromRow(r))
		cur = r.ID
	}
	if len(rows) > 0 {
		if err := t.st.SaveGuardOTelCursor(ctx, cur); err != nil {
			t.logger.Warn("guard otel: cursor save failed", "err", err)
		}
	}
}
