// Package db owns the SQLite connection pool and the embedded migration runner.
//
// Uses modernc.org/sqlite (pure Go, no CGO). Enables WAL mode and a busy
// timeout on open. Migrations live in internal/db/migrations and are embedded
// into the binary via embed.FS; they run sequentially inside transactions at
// startup, keyed on schema_meta.version.
package db
