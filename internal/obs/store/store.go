// Package store is the obs subsystem's OWN persistence seam — the single
// owner of the node-local obs_* tables (CLAUDE.md rule #4). It owns its
// schema (migrate.go, decision D3) and exposes a small write surface
// (UpsertTrace / UpsertSpansBatch). The host's internal/store never touches
// obs_* tables; obs never writes the host's tables (api_turns reconciliation
// goes through the injected obs.TurnSink, not this package).
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/marmutapp/superbased-observer/internal/obs/span"
)

// Store wraps a database handle and owns the obs_* tables. It is constructed
// over the daemon's existing *sql.DB (same file, same WAL) but writes only
// its own prefixed tables.
type Store struct {
	db *sql.DB
}

// Open returns a Store over conn and brings the obs schema up to date. It is
// called only when [observability] is enabled, so a disabled node never runs
// these migrations (decision D3). Idempotent.
func Open(ctx context.Context, conn *sql.DB) (*Store, error) {
	if conn == nil {
		return nil, fmt.Errorf("obs/store.Open: nil db")
	}
	if err := migrate(ctx, conn); err != nil {
		return nil, err
	}
	return &Store{db: conn}, nil
}

// UpsertTrace inserts or updates one trace row by trace_id. Non-empty incoming
// scalar fields win; ended_at/status advance the row as a trace completes.
func (s *Store) UpsertTrace(ctx context.Context, t span.Trace) error {
	if t.TraceID == "" {
		return fmt.Errorf("obs/store.UpsertTrace: empty trace_id")
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO obs_traces (trace_id, session_id, thread_id, tenant, user, source, root_span_id, project_root, status, started_at, ended_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(trace_id) DO UPDATE SET
    session_id   = COALESCE(NULLIF(excluded.session_id, ''), obs_traces.session_id),
    thread_id    = COALESCE(NULLIF(excluded.thread_id, ''), obs_traces.thread_id),
    tenant       = COALESCE(NULLIF(excluded.tenant, ''), obs_traces.tenant),
    user         = COALESCE(NULLIF(excluded.user, ''), obs_traces.user),
    root_span_id = COALESCE(NULLIF(excluded.root_span_id, ''), obs_traces.root_span_id),
    project_root = COALESCE(NULLIF(excluded.project_root, ''), obs_traces.project_root),
    status       = excluded.status,
    ended_at     = COALESCE(NULLIF(excluded.ended_at, ''), obs_traces.ended_at)`,
		t.TraceID, t.SessionID, t.ThreadID, t.Tenant, t.User, string(t.Source),
		t.RootSpanID, t.ProjectRoot, string(t.Status), ts(t.StartedAt), ts(t.EndedAt))
	if err != nil {
		return fmt.Errorf("obs/store.UpsertTrace: %w", err)
	}
	return nil
}

// UpsertSpansBatch inserts or updates a batch of spans in one transaction,
// keyed on span_id. Nullable token/cost columns are authoritative-on-merge:
// a non-null incoming value wins, a nil incoming leaves the stored value
// (COALESCE) — so an approximate span followed by an exact one upgrades in
// place, and a later observation never clobbers a known value with NULL.
func (s *Store) UpsertSpansBatch(ctx context.Context, spans []span.Span) error {
	if len(spans) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("obs/store.UpsertSpansBatch: begin: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO obs_spans (span_id, trace_id, parent_span_id, kind, name, status, started_at, ended_at,
    model, provider, input_tokens, output_tokens, total_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens,
    cost_usd, cost_source, cost_detail, request_id, provider_response_id, tool_call_id, source)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(span_id) DO UPDATE SET
    parent_span_id       = COALESCE(NULLIF(excluded.parent_span_id, ''), obs_spans.parent_span_id),
    name                 = COALESCE(NULLIF(excluded.name, ''), obs_spans.name),
    status               = excluded.status,
    ended_at             = COALESCE(NULLIF(excluded.ended_at, ''), obs_spans.ended_at),
    model                = COALESCE(NULLIF(excluded.model, ''), obs_spans.model),
    provider             = COALESCE(NULLIF(excluded.provider, ''), obs_spans.provider),
    input_tokens         = COALESCE(excluded.input_tokens, obs_spans.input_tokens),
    output_tokens        = COALESCE(excluded.output_tokens, obs_spans.output_tokens),
    total_tokens         = COALESCE(excluded.total_tokens, obs_spans.total_tokens),
    cache_read_tokens    = COALESCE(excluded.cache_read_tokens, obs_spans.cache_read_tokens),
    cache_write_tokens   = COALESCE(excluded.cache_write_tokens, obs_spans.cache_write_tokens),
    reasoning_tokens     = COALESCE(excluded.reasoning_tokens, obs_spans.reasoning_tokens),
    cost_usd             = COALESCE(excluded.cost_usd, obs_spans.cost_usd),
    cost_source          = COALESCE(NULLIF(excluded.cost_source, ''), obs_spans.cost_source),
    cost_detail          = COALESCE(NULLIF(excluded.cost_detail, ''), obs_spans.cost_detail),
    request_id           = COALESCE(NULLIF(excluded.request_id, ''), obs_spans.request_id),
    provider_response_id = COALESCE(NULLIF(excluded.provider_response_id, ''), obs_spans.provider_response_id),
    tool_call_id         = COALESCE(NULLIF(excluded.tool_call_id, ''), obs_spans.tool_call_id)`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("obs/store.UpsertSpansBatch: prepare: %w", err)
	}
	defer stmt.Close()

	for _, sp := range spans {
		if sp.SpanID == "" || sp.TraceID == "" {
			_ = tx.Rollback()
			return fmt.Errorf("obs/store.UpsertSpansBatch: span missing span_id/trace_id")
		}
		if _, err := stmt.ExecContext(ctx,
			sp.SpanID, sp.TraceID, sp.ParentSpanID, string(sp.Kind), sp.Name, statusOr(sp.Status), ts(sp.StartedAt), ts(sp.EndedAt),
			sp.Model, sp.Provider, sp.InputTokens, sp.OutputTokens, sp.TotalTokens,
			sp.CacheReadTokens, sp.CacheWriteTokens, sp.ReasoningTokens, sp.CostUSD,
			string(sp.CostSource), costDetailJSON(sp.CostDetail),
			sp.RequestID, sp.ProviderResponseID, sp.ToolCallID, string(sp.Source)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("obs/store.UpsertSpansBatch: exec span %s: %w", sp.SpanID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("obs/store.UpsertSpansBatch: commit: %w", err)
	}
	return nil
}

// UpsertSpanEvents inserts span events (idempotent on the
// (span_id,time,name) unique key — events are immutable, so a re-export is a
// no-op insert). Empty input is a no-op.
func (s *Store) UpsertSpanEvents(ctx context.Context, events []span.SpanEvent) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("obs/store.UpsertSpanEvents: begin: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO obs_span_events (span_id, time, name, attributes)
VALUES (?, ?, ?, ?)
ON CONFLICT(span_id, time, name) DO NOTHING`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("obs/store.UpsertSpanEvents: prepare: %w", err)
	}
	defer stmt.Close()
	for _, e := range events {
		if e.SpanID == "" {
			continue
		}
		if _, err := stmt.ExecContext(ctx, e.SpanID, ts(e.Time), e.Name, nullIfEmpty(e.AttributesJSON)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("obs/store.UpsertSpanEvents: exec: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("obs/store.UpsertSpanEvents: commit: %w", err)
	}
	return nil
}

// UpsertSpanLinks inserts cross-trace links (idempotent on
// (span_id,linked_trace,linked_span)). Empty input is a no-op.
func (s *Store) UpsertSpanLinks(ctx context.Context, links []span.SpanLink) error {
	if len(links) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("obs/store.UpsertSpanLinks: begin: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO obs_span_links (span_id, linked_trace, linked_span, attributes)
VALUES (?, ?, ?, ?)
ON CONFLICT(span_id, linked_trace, linked_span) DO NOTHING`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("obs/store.UpsertSpanLinks: prepare: %w", err)
	}
	defer stmt.Close()
	for _, l := range links {
		if l.SpanID == "" || l.LinkedTrace == "" {
			continue
		}
		if _, err := stmt.ExecContext(ctx, l.SpanID, l.LinkedTrace, l.LinkedSpan, nullIfEmpty(l.AttributesJSON)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("obs/store.UpsertSpanLinks: exec: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("obs/store.UpsertSpanLinks: commit: %w", err)
	}
	return nil
}

// InsertSpanContent persists prompt/response/tool-io bodies for a batch of
// spans. content_hash is ALWAYS stored; the raw body is stored only when the
// caller (the ingestor, applying the injected ContentGate) left it set — a
// gated-off node passes Raw == "" so only the hash survives (§10 metadata-first
// default). Idempotent on the (content_hash, kind, span_id) unique key: a
// re-export is a no-op (bodies are immutable). Empty input is a no-op.
func (s *Store) InsertSpanContent(ctx context.Context, items []span.SpanContent) error {
	if len(items) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("obs/store.InsertSpanContent: begin: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO obs_span_content (span_id, trace_id, request_id, kind, content, content_hash, time)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(content_hash, kind, span_id) DO NOTHING`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("obs/store.InsertSpanContent: prepare: %w", err)
	}
	defer stmt.Close()
	for _, it := range items {
		if it.SpanID == "" || it.ContentHash == "" {
			continue
		}
		if _, err := stmt.ExecContext(ctx, it.SpanID, nullIfEmpty(it.TraceID), nullIfEmpty(it.RequestID),
			string(it.Kind), nullIfEmpty(it.Raw), it.ContentHash, ts(it.Time)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("obs/store.InsertSpanContent: exec span %s: %w", it.SpanID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("obs/store.InsertSpanContent: commit: %w", err)
	}
	return nil
}

// costDetailJSON marshals a span cost breakdown to a JSON string for storage,
// returning nil (SQL NULL) when there is nothing to store. Best-effort: a
// marshal error stores NULL rather than failing the span write.
func costDetailJSON(bd *span.CostBreakdown) any {
	if bd.Empty() {
		return nil
	}
	b, err := json.Marshal(bd)
	if err != nil {
		return nil
	}
	return string(b)
}

// nullIfEmpty maps "" to a SQL NULL so empty attribute blobs don't store "".
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ts formats a time as RFC3339 (matching existing TEXT timestamp columns),
// returning "" for the zero time so COALESCE leaves an existing value intact.
func ts(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// statusOr defaults an unset status to StatusUnset's string.
func statusOr(s span.Status) string {
	if s == "" {
		return string(span.StatusUnset)
	}
	return string(s)
}
