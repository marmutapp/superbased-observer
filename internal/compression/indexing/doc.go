// Package indexing captures tool-output excerpts (first 1KB + last 1KB) and
// stores them in the FTS5 action_excerpts virtual table, enabling the
// search_past_outputs MCP tool. See spec §10 Layer 2.
package indexing
