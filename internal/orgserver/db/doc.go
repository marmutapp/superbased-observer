// Package db owns the org server's SQLite connection and embedded migration
// runner. It mirrors the agent's internal/db package (WAL mode, BEGIN
// IMMEDIATE single-batch migration transaction, schema_meta version record)
// but is a wholly separate database: the server DB at
// /var/lib/observer-org/server.db is opened only by the observer-org binary.
//
// No CGO: the pure-Go modernc.org/sqlite driver is used, per project rule.
//
// EnsureOrg seeds the single-row `org` table on first boot, generating a
// stable random org_id that is independent of the (reconfigurable)
// external_url.
package db
