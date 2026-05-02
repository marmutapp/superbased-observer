// Package store is the storage layer: batched, idempotent inserts of Actions,
// TokenUsage, Sessions, Projects; incremental parse-cursor state; and the
// query helpers used by the MCP server and intelligence layer. Idempotency is
// enforced by the UNIQUE(source_file, source_event_id) index on actions.
package store
