// Package contentcap caps free-form text bodies (tool inputs and outputs)
// at a safe upper bound before they reach the store, appending a marker
// when truncation occurred so downstream consumers can tell the
// difference between "exactly N bytes" and "≥ N bytes, the rest is gone".
//
// The cap is a defensive belt against pathological rows — an operator
// running `cat /var/log/syslog` through their AI tool can produce a
// multi-MB tool_output that, stored unbounded, would balloon SQLite's
// page count and slow every subsequent query. Adapters call Cap at
// emit time so the in-memory ToolEvent is bounded; the store layer
// trusts the bounded value.
//
// The marker is human-readable (mirrors cmd/observer/backfill_run.go's
// `…(output truncated at 1 MiB)…` pattern) so an operator copying
// the body sees immediately that they're looking at a truncated tail.
package contentcap

import "fmt"

// DefaultMaxBytes is the canonical upper bound for full tool
// input/output capture. Chosen to be ~256× the legacy 4000-byte cap
// while staying well below any practical SQLite row size concern. The
// observed P99 in production AI tool outputs sits around ~50 KB; the
// 1 MB ceiling only clips genuine outliers (large file dumps, full
// directory listings, multi-MB log greps).
const DefaultMaxBytes = 1 << 20 // 1 MiB

// Cap returns s if it fits within max bytes; otherwise returns
// s[:max] with a human-readable marker appended noting the truncation.
//
// max ≤ 0 is treated as DefaultMaxBytes. The marker is always
// counted OUTSIDE max — the returned slice can be at most
// max + len(marker) bytes. This keeps callers' contracts simple
// (max is the cap on the original payload, not the rendered output).
func Cap(s string, max int) string {
	if max <= 0 {
		max = DefaultMaxBytes
	}
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("\n…(content truncated at %d bytes)…", max)
}
