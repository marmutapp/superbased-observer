package audit

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"sync"
	"time"
)

// Row is one audit record. PathRequested is empty for tools that don't
// take a path argument; the writer stores NULL in that case. Ts is
// stamped at enqueue time so the flush latency (up to FlushInterval)
// doesn't smear timestamps across a batch.
type Row struct {
	Ts                time.Time
	Tool              string
	SessionID         string
	RequestHash       string
	PathRequested     string
	ResponseBytes     int
	ResponseTruncated bool
	ResponseOK        bool
	Reason            string
	Duration          time.Duration
}

// Writer records audit rows. Implementations are best-effort: a failed
// write must never propagate back to the MCP tool handler.
type Writer interface {
	Record(ctx context.Context, row Row)
}

// RequestHash returns sha256-hex of `${toolName}:${json(args)}`. Used
// as an idempotency key for "agent re-issued the same query N times"
// detection. Not cryptographically canonical — Go's default map-key
// sort during json.Marshal is canonical enough at the dedup scale we
// care about.
func RequestHash(toolName string, args any) string {
	h := sha256.New()
	h.Write([]byte(toolName + ":"))
	enc := json.NewEncoder(h)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(args)
	return hex.EncodeToString(h.Sum(nil))
}

// -----------------------------------------------------------------------------
// noop writer
// -----------------------------------------------------------------------------

type noopWriter struct{}

// NewNoopWriter returns a Writer that discards rows. Used when
// [intelligence.mcp.audit].enabled is false and in tests that don't
// exercise the audit table.
func NewNoopWriter() Writer { return noopWriter{} }

func (noopWriter) Record(context.Context, Row) {}

// -----------------------------------------------------------------------------
// sql writer (async, buffered, drop-oldest on overflow)
// -----------------------------------------------------------------------------

// SQLWriterOptions tunes the buffered async writer. Zero values pick
// defaults sized for typical operator workloads (a few hundred MCP
// calls per minute peak).
type SQLWriterOptions struct {
	// BufferSize is the channel capacity. Default 1024.
	BufferSize int
	// FlushInterval is the maximum time a buffered row waits before
	// being written. Default 100ms.
	FlushInterval time.Duration
	// BatchSize is the maximum rows per INSERT transaction. Default 256.
	BatchSize int
	// DropLogInterval is the minimum gap between overflow-drop log
	// lines. Default 1 minute.
	DropLogInterval time.Duration
}

func (o SQLWriterOptions) withDefaults() SQLWriterOptions {
	if o.BufferSize <= 0 {
		o.BufferSize = 1024
	}
	if o.FlushInterval <= 0 {
		o.FlushInterval = 100 * time.Millisecond
	}
	if o.BatchSize <= 0 {
		o.BatchSize = 256
	}
	if o.DropLogInterval <= 0 {
		o.DropLogInterval = time.Minute
	}
	return o
}

type sqlWriter struct {
	db     *sql.DB
	logger *slog.Logger
	opts   SQLWriterOptions
	ch     chan Row
	done   chan struct{}

	mu          sync.Mutex
	lastDropLog time.Time
	dropCount   int
}

// NewSQLWriter returns a Writer that INSERTs rows into mcp_audit
// asynchronously. Spawns a background goroutine that flushes either
// when BatchSize rows are buffered or FlushInterval elapses, whichever
// comes first. When the channel is full, Record drops the new row
// silently except for one log line per DropLogInterval.
//
// The writer never closes db — db lifecycle is owned by the caller.
// To stop the background goroutine call [Close].
func NewSQLWriter(db *sql.DB, logger *slog.Logger, opts SQLWriterOptions) *SQLWriter {
	if logger == nil {
		logger = slog.Default()
	}
	w := &sqlWriter{
		db:     db,
		logger: logger,
		opts:   opts.withDefaults(),
		ch:     make(chan Row, opts.withDefaults().BufferSize),
		done:   make(chan struct{}),
	}
	go w.run()
	return &SQLWriter{inner: w}
}

// SQLWriter is the public handle for the async SQL audit writer.
// Wraps the internal type so Close has a clean public surface without
// exposing channel internals.
type SQLWriter struct{ inner *sqlWriter }

// Record enqueues a row. Drops on full channel (logged at most once
// per DropLogInterval). Never blocks the caller.
func (w *SQLWriter) Record(ctx context.Context, row Row) { w.inner.Record(ctx, row) }

// Close stops the background flusher after draining buffered rows.
// Safe to call multiple times.
func (w *SQLWriter) Close() error { return w.inner.Close() }

func (w *sqlWriter) Record(_ context.Context, row Row) {
	if row.Ts.IsZero() {
		row.Ts = time.Now().UTC()
	}
	select {
	case w.ch <- row:
	default:
		w.recordDrop()
	}
}

func (w *sqlWriter) recordDrop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.dropCount++
	if time.Since(w.lastDropLog) < w.opts.DropLogInterval {
		return
	}
	w.lastDropLog = time.Now()
	dropped := w.dropCount
	w.dropCount = 0
	w.logger.Warn("mcp.audit: buffer full, dropping rows",
		"dropped_since_last_log", dropped,
		"buffer_size", w.opts.BufferSize)
}

func (w *sqlWriter) Close() error {
	select {
	case <-w.done:
		return nil
	default:
	}
	close(w.ch)
	<-w.done
	return nil
}

func (w *sqlWriter) run() {
	defer close(w.done)
	ticker := time.NewTicker(w.opts.FlushInterval)
	defer ticker.Stop()
	buf := make([]Row, 0, w.opts.BatchSize)
	flush := func() {
		if len(buf) == 0 {
			return
		}
		w.flush(buf)
		buf = buf[:0]
	}
	for {
		select {
		case row, ok := <-w.ch:
			if !ok {
				flush()
				return
			}
			buf = append(buf, row)
			if len(buf) >= w.opts.BatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (w *sqlWriter) flush(rows []Row) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		w.logger.Warn("mcp.audit: begin tx", "err", err, "lost_rows", len(rows))
		return
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO mcp_audit (
		    ts, session_id, tool_name, request_hash, path_requested,
		    response_size_bytes, response_truncated, response_ok,
		    reason, duration_us
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		w.logger.Warn("mcp.audit: prepare insert", "err", err, "lost_rows", len(rows))
		return
	}
	defer stmt.Close()
	for _, r := range rows {
		_, err := stmt.ExecContext(
			ctx,
			r.Ts.UTC().Format(time.RFC3339Nano),
			nullIfEmpty(r.SessionID),
			r.Tool,
			r.RequestHash,
			nullIfEmpty(r.PathRequested),
			r.ResponseBytes,
			boolToInt(r.ResponseTruncated),
			boolToInt(r.ResponseOK),
			nullIfEmpty(r.Reason),
			r.Duration.Microseconds(),
		)
		if err != nil {
			_ = tx.Rollback()
			w.logger.Warn("mcp.audit: insert row", "err", err, "lost_rows", len(rows))
			return
		}
	}
	if err := tx.Commit(); err != nil {
		w.logger.Warn("mcp.audit: commit", "err", err, "lost_rows", len(rows))
	}
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
