// Package migrations embeds the SQL migration files into the binary.
package migrations

import "embed"

// Files is the embedded filesystem of .sql migrations, sorted lexicographically
// by filename. The db.Open routine reads every *.sql entry, applies them in
// order inside a transaction, and records the highest applied version in
// schema_meta.
//
//go:embed *.sql
var Files embed.FS
