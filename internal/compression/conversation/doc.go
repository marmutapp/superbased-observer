// Package conversation implements conversation-level compression in the API
// proxy: content-type detection, per-type compression, message importance
// scoring, budget enforcement, and cache alignment for Anthropic prefix
// caching. See spec §10 Layer 3. Implemented in Phase 5.
package conversation
