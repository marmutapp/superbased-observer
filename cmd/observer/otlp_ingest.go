package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/marmutapp/superbased-observer/internal/adapter/ccotel"
	otlpingest "github.com/marmutapp/superbased-observer/internal/ingest/otlp"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/scrub"
	"github.com/marmutapp/superbased-observer/internal/store"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
)

// otlpLogsHandler composes the native-console ingest glue: parse an OTLP logs
// export into API-turn observations (ccotel) and dedup-merge each into the
// store by request_id (store.UpsertTurnByRequestID). It is the single seam
// between the network receiver (internal/ingest/otlp, schema-blind) and the
// store. Per-turn upsert errors are logged and skipped so one bad turn never
// fails the whole export (telemetry is at-least-once). It never returns an
// error today — the receiver maps a nil return to an OTLP success.
func otlpLogsHandler(st *store.Store, logger *slog.Logger, captureContent bool) otlpingest.Handler {
	scrubber := scrub.New()
	return func(ctx context.Context, req *collogspb.ExportLogsServiceRequest) error {
		turns, skipped := ccotel.ParseLogs(req)
		if skipped > 0 {
			logger.Warn("otlp ingest: skipped api_request events missing request_id", "count", skipped)
		}
		for i := range turns {
			if _, _, err := st.UpsertTurnByRequestID(ctx, turns[i]); err != nil {
				logger.Warn("otlp ingest: upsert failed", "request_id", turns[i].RequestID, "err", err)
			}
		}
		if captureContent {
			ingestOTelContent(ctx, st, scrubber, logger, req)
		}
		return nil
	}
}

// ingestOTelContent extracts content bodies, scrubs them for secrets at the
// boundary (ccotel stays pure), and stores them. Best-effort: a content-store
// failure logs and never fails the turn ingest above.
func ingestOTelContent(ctx context.Context, st *store.Store, scrubber *scrub.Scrubber, logger *slog.Logger, req *collogspb.ExportLogsServiceRequest) {
	recs := ccotel.ParseContent(req)
	if len(recs) == 0 {
		return
	}
	rows := make([]models.OTelContent, 0, len(recs))
	for _, r := range recs {
		var ts time.Time
		if r.TimeUnixNano != 0 {
			ts = time.Unix(0, int64(r.TimeUnixNano)).UTC()
		}
		rows = append(rows, models.OTelContent{
			RequestID: r.RequestID,
			SessionID: r.SessionID,
			ToolUseID: r.ToolUseID,
			Kind:      r.Kind,
			Content:   scrubber.String(r.Content),
			Timestamp: ts,
			Source:    ccotel.SourceTag,
		})
	}
	if _, err := st.InsertOTelContent(ctx, rows); err != nil {
		logger.Warn("otlp ingest: content store failed", "count", len(rows), "err", err)
	}
}
