// Package diag implements the diagnostics commands: `observer doctor`,
// `observer status`, and `observer tail`. Each command is exposed as a
// pure function over (config, *sql.DB) so the CLI layer stays thin and
// the logic stays testable.
//
// See spec §17 for the startup self-check requirements doctor implements.
package diag
