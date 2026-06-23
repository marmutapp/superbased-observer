// Package retention prunes old observer state per spec §19.
//
// On startup (when [observer.retention].prune_on_startup is true) the
// watcher / start commands call Run once. Run deletes:
//   - actions older than max_age_days, plus their dependent failure_context
//     and action_excerpts rows
//   - sessions with no remaining actions
//   - observer_log entries older than observer_log_max_age_days
//   - file_state rows whose last_seen_at is more than 30 days old
//
// After deletes it runs a wal_checkpoint(TRUNCATE) to reclaim WAL space.
// If max_db_size_mb is set and the DB still exceeds the cap, Run progressively
// shaves off the oldest 30-day window until under the cap (capped at a
// safety limit of 12 iterations).
//
// See spec §19.
package retention
