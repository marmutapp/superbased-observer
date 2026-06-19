// Package migrations embeds the org server's SQL migration files into the
// observer-org binary. It mirrors the agent's internal/db/migrations package
// (same embed.FS + sequential-version convention) but is a separate schema:
// the server DB lives at /var/lib/observer-org/server.db and is never opened
// by the agent.
package migrations

import "embed"

// Files is the embedded filesystem of .sql migrations, applied in version
// order by orgserver/db.Open and recorded in schema_meta. Filenames are
// NNN_name.sql; the leading integer is the version.
//
//go:embed *.sql
var Files embed.FS
