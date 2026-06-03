// Package discover surfaces optimization opportunities against captured
// action data (spec §15.1): stale file reads with token-waste estimates,
// repeated commands with no intervening changes, cross-tool redundancy,
// and a native-vs-Bash action breakdown.
//
// All numbers are deterministic reductions over the actions / file_state /
// failure_context tables — no live filesystem access.
package discover
