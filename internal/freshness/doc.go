// Package freshness classifies file and command accesses as fresh, stale,
// changed_by_self, changed_externally, or unknown, by hashing file contents
// (with an mtime+size fast path) and comparing against the file_state table.
// See spec §7.
package freshness
