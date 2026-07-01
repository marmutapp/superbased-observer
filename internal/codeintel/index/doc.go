// Package index is the offline indexer orchestrator: it walks a
// project's files, calls parse + resolve, and persists results through
// the store seam. Unlike the pure subpackages it DOES perform I/O
// (filesystem walks, the source-watch via fsnotify mechanics, the store
// seam) — it is codeintel's single writer. It never runs on the proxy
// hot path (ADR-0002). codeintel owns this orchestrator rather than
// reusing the adapter session-file watcher (plan D9). See
// docs/codeintel/indexing.md and docs/codeintel/project-lifecycle.md.
package index
