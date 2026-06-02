// Package cowork parses Claude Cowork audit logs.
//
// Cowork is Anthropic's "knowledge-work" desktop product layered on
// top of Claude Code's CLI session model. Each Cowork local-instance
// writes an append-only audit.jsonl that mirrors the inner Claude
// Code transcript plus Cowork-specific metadata. See
// docs/cowork-adapter-plan.md for the schema reference and design
// rationale.
package cowork
