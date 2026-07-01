// Package verbosity is the pure-logic core of the Output Composition
// (Verbosity) feature (docs/plans/output-composition-verbosity-plan-2026-06-30.md).
//
// It answers, for an assistant turn (and in aggregate): how much of the
// output is narrative explanation, how much is shown (fenced) display
// artifacts, and how much is code the model authored — split by language.
// The design uses two orthogonal axes:
//
//   - CHANNEL  — where content came through: narrative, shown-artifact
//     (a fenced block in visible text), command-executed (a Bash/
//     PowerShell/shell tool call), file-written (Write/Edit/...).
//   - CATEGORY — what the content is, a property of its language: code,
//     docs, config, data, prose, unknown. Shell/CLI languages (bash, sh,
//     powershell, ...) are category=code wherever they appear.
//
// This package owns only the language classifier (FileType / LangForTag /
// CategoryOf) and the visible-text fence segmenter (SegmentVisible). The
// authored-code cut rides the normalized internal/store actions layer and
// is assembled at the store seam; this package never sees a tool name, an
// adapter, an action_type, or a DB handle.
//
// Discipline mirrors internal/predict and internal/cachetrack: NO
// database/sql, NO net/http, NO fsnotify (pinned by imports_test.go).
// Every input is a plain string handed in at a seam; every output is a
// plain struct of integer byte counts. Content is classified then
// discarded by the caller — this package stores nothing.
package verbosity
