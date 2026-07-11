// Package sharecard composes a shareable, copy-pasteable summary of a
// developer's observed AI-agent spend — a Markdown block and a 1200×630 social
// card (SVG) — from an already-assembled, CONTENT-FREE Data value.
//
// It is a pure package (spec §24.1 discipline): no database/sql, no net/http,
// no fsnotify. The node CLI (cmd/observer/report.go) assembles Data from the
// cost engine and hands it here, mirroring internal/notify/digest's
// Data-injection pattern. The card carries ONLY aggregates — period totals,
// model ids, tool names, cache percentages — never project names, filesystem
// paths, or session titles.
//
// Honesty constraint: the artifact frames spend VISIBILITY (totals, model mix,
// cache economics). It never claims compression dollar-savings — that headline
// was retracted (see docs/compression.md and the marketing honesty rules).
package sharecard
