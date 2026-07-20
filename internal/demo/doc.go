// Package demo produces the sample dataset behind the dashboard's
// demo mode (usability arc P6.7).
//
// The package embeds a set of synthetic claude-code-shaped session
// transcripts (fixtures/*.jsonl — fictional projects under
// /home/demo/projects, generated deterministically; nothing in them
// derives from any real session) and parses them through the real
// claude-code adapter, so demo rows take the exact shape live capture
// produces. Timestamps are rebased at parse time so the newest event
// lands minutes before "now" and every time-windowed dashboard
// surface lights up.
//
// The package never touches a database: it returns parsed event
// slices and the caller (cmd/observer's demo seeder) owns the
// temp-database lifecycle. The real observer.db is never read or
// written on any demo path.
package demo
