// Package compaction handles PreCompact/PostCompact hook events from the
// host AI tool (spec §15.5). PreCompact captures a file_state snapshot into
// compaction_events; PostCompact reconciles the snapshot against the first
// actions of the post-compaction conversation to populate ghost_files_after.
//
// The MCP tool `get_session_recovery_context` already reads
// compaction_events — this package is the *write* side.
package compaction
