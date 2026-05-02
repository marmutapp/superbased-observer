// Package learn derives correction rules from paired failures → subsequent
// successes of the same command (spec §15.3). Entirely rule-based, zero LLM
// cost.
//
// The flow: walk failure_context, group by (project, command_hash). Within
// each group, pair each failure with the next successful run of the same
// command (if any) within the same session. The set of file edits between
// the failure and the success is the "correction delta" — emitted as a Rule
// that `observer learn --apply` can render into CLAUDE.md / AGENTS.md /
// .cursorrules.
package learn
