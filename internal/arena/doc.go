// Package arena implements the Agent Arena engine: one prompt driven
// against N agent harnesses in isolated git worktrees of a selected
// project, judged by a chosen harness plus objective metrics, with an
// explicit keep step that merges the winner back onto the project's
// branch.
//
// Plan of record:
// docs/plans/agent-arena-terminal-multi-harness-2026-08-22.md.
// The package owns lifecycle orchestration only — persistence lives in
// internal/store (migration 088), the dashboard API in
// internal/intelligence/dashboard, and process-group kill helpers are
// local (the cmd/observer benchmark drivers are package main and cannot
// be imported; their proven argv shapes are mirrored here and pinned by
// tests).
package arena
