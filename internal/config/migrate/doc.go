// Package migrate is the pure-logic core of the config auto-migration
// rail (docs/codeintel/migration-from-codegraph.md). It rewrites a
// config.toml's TEXT to rename deprecated keys onto their new homes
// (e.g. the decommissioned [compression.code_graph] /
// [intelligence.code_graph] blocks onto [codeintel]) while preserving
// comments, ordering, and every untouched line byte-for-byte.
//
// The package is pure: string in, string out. It performs NO file I/O
// (no os), no SQL, no HTTP, no fsnotify — the read/write boundary lives
// in internal/config (MigrateFile) and the daemon/CLI call sites. This
// is pinned by imports_test.go.
//
// Migrations are a versioned, table-driven registry (steps -> renames).
// A new field rename is one data row, never new control flow
// (CLAUDE.md module discipline #5). Apply is idempotent (guarded by a
// [observer] config_version stamp) and fail-safe: anything it cannot
// edit with confidence leaves the input untouched and is reported via
// Result.Skipped, so a migration can never corrupt an operator's file.
package migrate
