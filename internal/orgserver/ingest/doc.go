// Package ingest writes a content-free agent push envelope into the org
// server's data tables (spec §2.4.4). It is the server-side counterpart of the
// agent's store.SelectUnpushedSince read path: the orgcontract row types are
// the single wire source of truth, and this package only ever writes the
// columns those types carry — no prompt text, tool I/O, reasoning, or
// free-form error message can reach the server because none crosses the wire.
//
// Ingest is idempotent. Every row is keyed by a deterministic composite key
// (sessions: id+user_id; actions/token_usage: source_file+source_event_id+
// user_id; api_turns: user_id+session_id+request_id+timestamp) and inserted
// with INSERT OR IGNORE, so a re-pushed batch — whether from a retry, a
// re-enrol, or overlapping cursors — is deduplicated by the database rather
// than trusted from the client. The whole envelope lands in one transaction.
package ingest
