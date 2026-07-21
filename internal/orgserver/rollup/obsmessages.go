// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 SuperBased

package rollup

import (
	"context"
	"database/sql"
	"fmt"
)

// obsmessages.go is the OP3 AUDITED org span-content viewer (obs-org-tier T3),
// the deeper-disclosure analogue of rollup/messages.go::SessionMessages. It
// surfaces the raw prompt/response/tool-io bodies (obs_content) for one trace's
// spans — bodies that already arrive under the node's own
// [org_client.share].obs_content + full_content opt-in. The handler writes a
// DISTINCT view_span_content audit row BEFORE this read, and the scope check is
// identical to ObsTraceDetail (out-of-scope ≡ 404), so no existence leak.
//
// content is NULL unless the node shared full content — we SELECT it but the
// server cannot tell "stripped" from "never had one"; the content_hash always
// rides (the content-free signal).

// ObsContentEntry is one captured body on a span.
type ObsContentEntry struct {
	SpanID      string `json:"span_id"`
	Kind        string `json:"kind"`
	ContentHash string `json:"content_hash"`
	Content     string `json:"content"` // empty when the node did not share full content
	HasRaw      bool   `json:"has_raw"`
	Timestamp   string `json:"timestamp"`
}

// ObsContentResult is the GET /api/org/obs/trace/{id}/content body.
type ObsContentResult struct {
	TraceID string            `json:"trace_id"`
	Entries []ObsContentEntry `json:"entries"`
	AnyRaw  bool              `json:"any_raw"` // whether any entry carries a raw body
}

// ObsTraceContent returns the captured bodies for one trace's spans, or
// found=false when the trace is unknown or out of the caller's scope. The
// caller is responsible for writing the view_span_content audit row first.
func ObsTraceContent(ctx context.Context, db *sql.DB, traceID string, scope Scope, selfUserID string) (ObsContentResult, bool, error) {
	uScope, uArgs := peopleScopeSQL("pushed_by_user_id", scope, selfUserID)
	if uScope == falseScope {
		return ObsContentResult{}, false, nil
	}
	// Scope/existence probe via obs_traces (same visibility as ObsTraceDetail).
	var orgID string
	//nolint:gosec // G201: uScope is a parameterized fragment; values bind via ?.
	probe := `SELECT org_id FROM obs_traces WHERE trace_id = ? AND ` + uScope + ` LIMIT 1`
	if err := db.QueryRowContext(ctx, probe, append([]any{traceID}, uArgs...)...).Scan(&orgID); err != nil {
		if err == sql.ErrNoRows {
			return ObsContentResult{}, false, nil
		}
		return ObsContentResult{}, false, fmt.Errorf("rollup.ObsTraceContent: probe: %w", err)
	}

	res := ObsContentResult{TraceID: traceID, Entries: []ObsContentEntry{}}
	// Bodies are joined to spans of this trace (obs_content carries span_id; the
	// span carries the trace_id). Scoped to org for multi-tenant safety.
	q := `
SELECT c.span_id, COALESCE(c.kind,''), c.content_hash, c.content, COALESCE(c.timestamp,'')
  FROM obs_content c
  JOIN obs_spans s ON s.org_id = c.org_id AND s.span_id = c.span_id
 WHERE c.org_id = ? AND s.trace_id = ?
 ORDER BY COALESCE(c.timestamp,''), c.span_id`
	if err := eachRow(ctx, db, q, []any{orgID, traceID}, func(rows *sql.Rows) error {
		var e ObsContentEntry
		var content sql.NullString
		if err := rows.Scan(&e.SpanID, &e.Kind, &e.ContentHash, &content, &e.Timestamp); err != nil {
			return err
		}
		if content.Valid && content.String != "" {
			e.Content = content.String
			e.HasRaw = true
			res.AnyRaw = true
		}
		res.Entries = append(res.Entries, e)
		return nil
	}); err != nil {
		return ObsContentResult{}, false, fmt.Errorf("rollup.ObsTraceContent: bodies: %w", err)
	}
	return res, true, nil
}
